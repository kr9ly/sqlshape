// Package cardinality proves, from a statement's facts alone, that it touches at most one
// row: the One contract, written once for every dialect.
//
// An application that calls One asserts an interpretation ("this lookup hits at most one
// row") that the database can back with its constraints. The proof is the classic
// functional-dependency argument over the leaves of a scope: a leaf is single once some
// unique key of it (a partial one only where the statement repeats its predicate) is fixed
// by equalities to values known before the statement runs, and a single leaf makes all its
// columns known, which can fix other leaves through the join equalities (Edges). When
// every leaf is single the level is at most one row. Join direction is already in the
// facts: an outer join's ON fixes only its nullable side. A derived leaf is proved through
// its Body, seeded with the outputs the enclosing level fixes. Besides the key argument, a
// level says of itself when it is one row (Single: LIMIT 1, one VALUES row, an aggregate
// without GROUP BY) or many (Many: a set operation, several VALUES rows, FULL JOIN), and a
// GROUP BY level is one row when every grouping expression is pinned. A producer that can
// prove more sets Facts.AtMostOne itself.
package cardinality

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

const notAnalyzed = "the statement's shape is not analyzed for its cardinality"

// AtMostOne reports whether f proves the statement touches at most one row; when it does
// not, why says what blocked the proof, the way the checker reports it.
func AtMostOne(f *facts.Facts) (bool, string) {
	if f == nil {
		return false, notAnalyzed
	}
	if f.AtMostOne {
		return true, ""
	}
	sc := f.Top
	if f.Kind == facts.Insert && f.Source != nil {
		sc = f.Source
	}
	if sc == nil {
		return false, notAnalyzed
	}
	return scopeSingle(sc, nil)
}

// ScopeSingle is the proof over one scope, on its own.
func ScopeSingle(sc *facts.Scope) (bool, string) { return scopeSingle(sc, nil) }

// scopeSingle proves one level; seeds are its leaves' columns fixed from outside (the
// enclosing level's equalities on a derived leaf's outputs).
func scopeSingle(sc *facts.Scope, seeds []facts.ColRef) (bool, string) {
	if sc.Single {
		return true, "" // read first: LIMIT 1 over a UNION, count(*) over a FULL JOIN are one row
	}
	if sc.Many != "" {
		return false, sc.Many
	}
	if len(sc.Leaves) == 0 {
		return true, "" // a level without FROM is one row
	}
	p := newProof(sc, seeds)
	p.fixpoint()
	if sc.Groups != nil {
		if sc.GroupingSets {
			return false, "GROUPING SETS yield one row per set"
		}
		for _, g := range sc.Groups {
			switch g.Kind {
			case facts.Column:
				if p.isKnown(g.Col) {
					continue
				}
				return false, "GROUP BY yields one row per group (" + g.Col.Column + " is not pinned)"
			case facts.Expr:
				return false, "GROUP BY yields one row per group (" + g.Text + " is not pinned)"
			}
		}
		return true, ""
	}
	for i, l := range sc.Leaves {
		if !p.single[i] {
			return false, p.describe(l, i)
		}
	}
	return true, ""
}

type proof struct {
	sc     *facts.Scope
	known  map[facts.ColRef]bool
	single []bool
	// pointIn: range columns holding a known point (a Contains predicate whose point is
	// known, or another column once that is known)
	pointIn map[facts.ColRef]bool
	why     map[int]string // derived leaf → why its body is not proved single
}

func newProof(sc *facts.Scope, seeds []facts.ColRef) *proof {
	p := &proof{sc: sc, known: map[facts.ColRef]bool{}, single: make([]bool, len(sc.Leaves)), pointIn: map[facts.ColRef]bool{}, why: map[int]string{}}
	for _, c := range sc.Fixed {
		p.known[c] = true
	}
	for _, c := range seeds {
		p.known[c] = true
	}
	return p
}

// isKnown: the column is fixed, or belongs to a leaf already fixed to one row.
func (p *proof) isKnown(c facts.ColRef) bool {
	return p.known[c] || (c.Leaf >= 0 && c.Leaf < len(p.single) && p.single[c.Leaf])
}

func (p *proof) fixpoint() {
	for changed := true; changed; {
		changed = false
		for _, e := range p.sc.Edges {
			if !p.known[e.To] && p.isKnown(e.From) {
				p.known[e.To] = true
				changed = true
			}
		}
		for _, pr := range p.sc.Preds {
			if pr.Op != facts.Contains || p.pointIn[pr.Col] || !applies(pr, pr.Col.Leaf) {
				continue
			}
			if pr.Term.Kind == facts.Column && !p.isKnown(pr.Term.Col) {
				continue
			}
			p.pointIn[pr.Col] = true
			changed = true
		}
		for i, l := range p.sc.Leaves {
			if !p.single[i] && p.leafSingle(l, i) {
				p.single[i] = true
				changed = true
			}
		}
	}
}

// applies: the predicate restricts leaf i.
func applies(pr facts.Pred, i int) bool {
	if pr.Restricts == nil {
		return true
	}
	for _, r := range pr.Restricts {
		if r == i {
			return true
		}
	}
	return false
}

// leafSingle decides whether the known columns fix at most one row of the leaf.
func (p *proof) leafSingle(l facts.Leaf, i int) bool {
	if l.Single {
		return true
	}
	for _, key := range l.Keys {
		if p.keyFixed(key, i) {
			return true
		}
	}
	if l.Body != nil {
		var seeds []facts.ColRef
		for _, o := range l.Outputs {
			if p.isKnown(facts.ColRef{Leaf: i, Column: o.Name}) && !ambiguous(l.Outputs, o.Name) {
				seeds = append(seeds, o.Col)
			}
		}
		ok, why := scopeSingle(l.Body, seeds)
		if !ok {
			p.why[i] = why
		}
		return ok
	}
	return false
}

func ambiguous(outs []facts.Output, name string) bool {
	n := 0
	for _, o := range outs {
		if o.Name == name {
			n++
		}
	}
	return n > 1
}

func (p *proof) keyFixed(key facts.Key, i int) bool {
	if len(key.Columns) == 0 {
		return false
	}
	for k, col := range key.Columns {
		c := facts.ColRef{Leaf: i, Column: col}
		if p.isKnown(c) {
			continue
		}
		// a temporal key's range column: a row whose range holds a known point is the only
		// one for those key values, since no two rows' ranges overlap
		if key.Temporal && k == len(key.Columns)-1 && p.pointIn[c] {
			continue
		}
		return false
	}
	// a partial unique index applies only where the statement repeats its predicate
	for _, w := range key.Where {
		found := false
		for _, pr := range p.sc.Preds {
			if applies(pr, i) && samePred(pr, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// samePred: two predicates in the facts language say the same thing (a statement conjunct
// and an index predicate's).
func samePred(a, b facts.Pred) bool {
	if a.Op != b.Op || a.Col != b.Col {
		return false
	}
	switch a.Op {
	case facts.Eq, facts.Contains:
		return sameTerm(a.Term, b.Term)
	case facts.IsNull, facts.IsNotNull:
		return true
	case facts.In:
		if len(a.Terms) != len(b.Terms) {
			return false
		}
		for _, t := range a.Terms {
			found := false
			for _, u := range b.Terms {
				found = found || sameTerm(t, u)
			}
			if !found {
				return false
			}
		}
		return true
	case facts.Opaque:
		if a.Text != b.Text || len(a.Cols) != len(b.Cols) {
			return false
		}
		for k := range a.Cols {
			if a.Cols[k] != b.Cols[k] {
				return false
			}
		}
		return true
	}
	return false
}

func sameTerm(a, b facts.Term) bool {
	return a.Kind == b.Kind && a.Param == b.Param && a.Const == b.Const && a.Col == b.Col && a.Text == b.Text
}

// describe says why a leaf could not be proved single.
func (p *proof) describe(l facts.Leaf, i int) string {
	name := l.Alias
	if name == "" {
		name = l.Table
	}
	switch l.Kind {
	case facts.Table:
		if l.Body != nil {
			return "view " + name + ": " + p.why[i] // written through an updatable view: the alias is the view's
		}
		if short := l.Table[strings.LastIndexByte(l.Table, '.')+1:]; short != name && l.Table != "" {
			name = short + " " + name
		}
		if len(l.Keys) == 0 {
			return name + " has no unique key"
		}
		keys := make([]string, len(l.Keys))
		for k, key := range l.Keys {
			keys[k] = "(" + strings.Join(key.Columns, ", ") + ")"
			if len(key.Where) > 0 {
				keys[k] += " WHERE ..."
			}
		}
		return fmt.Sprintf("%s: no unique key is fixed by equality (keys: %s)", name, strings.Join(keys, ", "))
	case facts.View, facts.MatView:
		if l.Body == nil {
			return "view " + name + ": its definition is not analyzed"
		}
		return "view " + name + ": " + p.why[i]
	case facts.CTE:
		if l.Body == nil {
			return "CTE " + name + ": its body is not analyzed"
		}
		return "CTE " + name + ": " + p.why[i]
	case facts.Function:
		return "function " + name + " may return many rows"
	}
	if l.Body == nil {
		return name + ": not proved to return one row"
	}
	return "subquery " + name + ": " + p.why[i]
}
