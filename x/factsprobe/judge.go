package factsprobe

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/cardinality"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// ---- judging: the facts' claims against the rows the server returned -----------------

// Verdict is the judgment of one statement. Kind is "" when every claim held; otherwise
// the class of failure, Detail what was claimed and what the server showed.
type Verdict struct {
	Kind   string
	Detail string
	SQL    string
	Schema string // the DDL and the rows
	Params []Value
	// Hit are the alphabet entries the statement's facts exercised.
	Hit []string
	// Unverified counts the claims the judge could not evaluate (an Opaque predicate
	// whose text matched no generated conjunct, a Known term): reported, not failed.
	Unverified []string
}

// judge compares f, the facts the analyzer gave for q, with rows, what the server
// returned for it. m is the model the rows came from (for EXISTS witnesses).
func judge(q *query, f *facts.Facts, rows []row, m *Schema) Verdict {
	v := Verdict{SQL: q.sql(), Params: q.params}
	if f == nil {
		v.Kind, v.Detail = "no facts", "the analyzer recorded no facts for the statement"
		return v
	}
	v.Hit = alphabetHits(f)
	sc := f.Top
	if sc == nil {
		v.Kind, v.Detail = "no facts", "the facts have no top scope"
		return v
	}
	aliases := make([]string, len(sc.Leaves))
	for i, l := range sc.Leaves {
		aliases[i] = l.Alias
	}
	col := func(c facts.ColRef, r row) (Value, bool) {
		if c.Leaf < 0 || c.Leaf >= len(aliases) {
			return Value{}, false
		}
		val, ok := r[aliases[c.Leaf]+"."+c.Column]
		return val, ok
	}
	fail := func(kind, format string, args ...any) Verdict {
		v.Kind, v.Detail = kind, fmt.Sprintf(format, args...)
		return v
	}
	// at most one row: the whole level for a SELECT, the target leaf alone for a write
	// (x/obligation judges `single` on an UPDATE / DELETE that way)
	if q.kind == "update" || q.kind == "delete" {
		if ok, _ := cardinality.TargetSingle(sc, 0); ok && len(rows) > 1 {
			return fail("at most one row refuted", "the proof says the target is at most one row; the server matched %d", len(rows))
		}
	} else if ok, _ := cardinality.AtMostOne(f); ok && len(rows) > 1 {
		return fail("at most one row refuted", "the proof says at most one row; the server returned %d", len(rows))
	}
	// present: the row has leaf i's columns (not the all-NULL side of an outer join
	// without a match): what Fixed / NotNull / an Edge on that leaf speak about
	present := func(i int, r row) bool {
		if !nullableLeaf(sc, i) {
			return true
		}
		return restrictedRowPresent(facts.Pred{Restricts: []int{i}}, r, aliases, sc)
	}
	// predicates: every returned row satisfies each conjunct of the top scope
	for _, p := range sc.Preds {
		for _, r := range rows {
			// an outer join's ON restricts only its nullable side: a row where that side
			// is all NULL (no match) is not one the predicate speaks about
			if p.Restricts != nil && !restrictedRowPresent(p, r, aliases, sc) {
				continue
			}
			res, why := evalPred(p, r, q, m, col, aliases, sc)
			if why != "" {
				v.Unverified = append(v.Unverified, why)
				break
			}
			if res != yes {
				return fail("predicate refuted", "the facts say every row satisfies %s; the server returned %s (the predicate is %v there)", describePred(p, aliases), r, res)
			}
		}
	}
	// fixed columns: one non-NULL value across every returned row
	for _, c := range sc.Fixed {
		var seen *Value
		for _, r := range rows {
			val, ok := col(c, r)
			if !ok {
				v.Unverified = append(v.Unverified, notProjected)
				break
			}
			if !present(c.Leaf, r) {
				continue
			}
			if val.Null {
				return fail("fixed column refuted", "the facts say %s.%s is fixed to a known value; the server returned a row with it NULL: %s", aliases[c.Leaf], c.Column, r)
			}
			if seen == nil {
				seen = &val
			} else if seen.S != val.S {
				return fail("fixed column refuted", "the facts say %s.%s is fixed to one value; the server returned both %s and %s", aliases[c.Leaf], c.Column, seen.S, val.S)
			}
		}
	}
	// not null
	for _, c := range sc.NotNull {
		for _, r := range rows {
			if val, ok := col(c, r); ok && present(c.Leaf, r) && val.Null {
				return fail("not null refuted", "the facts say %s.%s is not NULL in every row; the server returned %s", aliases[c.Leaf], c.Column, r)
			}
		}
	}
	// edges: From fixed implies To fixed -- over the returned rows, To is a function of From
	for _, e := range sc.Edges {
		seen := map[string]string{}
		for _, r := range rows {
			from, ok1 := col(e.From, r)
			to, ok2 := col(e.To, r)
			if !ok1 || !ok2 || from.Null || !present(e.To.Leaf, r) {
				continue
			}
			if prev, ok := seen[from.S]; ok && prev != to.String() {
				return fail("edge refuted", "the facts say %s.%s fixed implies %s.%s fixed; the server returned %s = %s with %s.%s both %s and %s", aliases[e.From.Leaf], e.From.Column, aliases[e.To.Leaf], e.To.Column, e.From.Column, from.S, aliases[e.To.Leaf], e.To.Column, prev, to)
			}
			seen[from.S] = to.String()
		}
	}
	return v
}

// notProjected is the claim about a column the statement does not return: the other
// leaf's columns under GROUP BY t0.id.
const notProjected = "a column of a leaf the GROUP BY does not project"

// nullableLeaf: some predicate restricts itself to leaf i alone -- the mark of an outer
// join's nullable side.
func nullableLeaf(sc *facts.Scope, i int) bool {
	for _, p := range sc.Preds {
		if len(p.Restricts) == 1 && p.Restricts[0] == i {
			return true
		}
	}
	return false
}

// restrictedRowPresent: the leaves p restricts have a row (not the all-NULL side of an
// outer join without a match) in r.
func restrictedRowPresent(p facts.Pred, r row, aliases []string, sc *facts.Scope) bool {
	for _, i := range p.Restricts {
		if i < 0 || i >= len(aliases) {
			continue
		}
		allNull := true
		for k, v := range r {
			if strings.HasPrefix(k, aliases[i]+".") && !v.Null {
				allNull = false
				break
			}
		}
		if allNull {
			return false
		}
	}
	return true
}

// evalPred evaluates one predicate over a row. why is set when it cannot be evaluated.
func evalPred(p facts.Pred, r row, q *query, m *Schema, col func(facts.ColRef, row) (Value, bool), aliases []string, sc *facts.Scope) (tri, string) {
	term := func(t facts.Term) (Value, string) {
		switch t.Kind {
		case facts.Param:
			if int(t.Param) < 1 || int(t.Param) > len(q.params) {
				return Value{}, fmt.Sprintf("parameter $%d out of range", t.Param)
			}
			return q.params[t.Param-1], ""
		case facts.Const:
			return constValue(t.Const), ""
		case facts.Column:
			v, ok := col(t.Col, r)
			if !ok {
				return Value{}, notProjected
			}
			return v, ""
		case facts.Known:
			if v, ok := q.known[normText(t.Text)]; ok {
				return v, ""
			}
		}
		return Value{}, "term of kind " + termKind(t.Kind) + " (" + t.Text + ") is not evaluated"
	}
	switch p.Op {
	case facts.Eq:
		a, ok := col(p.Col, r)
		if !ok {
			return unknown, notProjected
		}
		b, why := term(p.Term)
		if why != "" {
			return unknown, why
		}
		return eqValues(a, b), ""
	case facts.In:
		a, ok := col(p.Col, r)
		if !ok {
			return unknown, notProjected
		}
		var set []Value
		for _, t := range p.Terms {
			b, why := term(t)
			if why != "" {
				return unknown, why
			}
			set = append(set, b)
		}
		return inValues(a, set), ""
	case facts.IsNull:
		a, ok := col(p.Col, r)
		if !ok {
			return unknown, notProjected
		}
		return bool3(a.Null), ""
	case facts.IsNotNull:
		a, ok := col(p.Col, r)
		if !ok {
			return unknown, notProjected
		}
		return bool3(!a.Null), ""
	case facts.Exists:
		return existsWitness(p.Sub, r, q, m, col)
	case facts.Opaque:
		// the generator's own conjunct with the same text, its qualifiers stripped
		for _, c := range append(append([]conj(nil), q.where...), onConjs(q)...) {
			if bareText(c.sql) == bareText(p.Text) {
				return c.eval(r, m, q.params), ""
			}
		}
		return unknown, "opaque predicate " + strconv.Quote(p.Text) + " matches no generated conjunct"
	}
	return unknown, fmt.Sprintf("predicate op %d is not evaluated", p.Op)
}

func onConjs(q *query) []conj {
	var out []conj
	for _, l := range q.leaves {
		out = append(out, l.on...)
	}
	return out
}

// bareText strips alias qualifiers and spaces from a predicate's text, the way the
// producers reduce column references to their bare names.
func bareText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			continue
		}
		b.WriteByte(s[i])
	}
	out := b.String()
	for _, a := range []string{"t0.", "t1.", "s0."} {
		out = strings.ReplaceAll(out, a, "")
	}
	return strings.Trim(out, "()")
}

// existsWitness looks for a row of the sub-scope's leaves satisfying its predicates, with
// Outer terms bound to the enclosing row.
func existsWitness(sub *facts.Scope, outer row, q *query, m *Schema, outerCol func(facts.ColRef, row) (Value, bool)) (tri, string) {
	if sub == nil {
		return unknown, "EXISTS without a body"
	}
	var tables []*Table
	for _, l := range sub.Leaves {
		t := m.table(l.Table)
		if t == nil {
			return unknown, "EXISTS body leaf " + l.Table + " is not a model table"
		}
		tables = append(tables, t)
	}
	aliases := make([]string, len(sub.Leaves))
	for i, l := range sub.Leaves {
		aliases[i] = l.Alias
	}
	innerCol := func(c facts.ColRef, r row) (Value, bool) {
		if c.Leaf < 0 || c.Leaf >= len(aliases) {
			return Value{}, false
		}
		v, ok := r[aliases[c.Leaf]+"."+c.Column]
		return v, ok
	}
	term := func(t facts.Term, r row) (Value, string) {
		switch t.Kind {
		case facts.Param:
			if int(t.Param) < 1 || int(t.Param) > len(q.params) {
				return Value{}, "parameter out of range"
			}
			return q.params[t.Param-1], ""
		case facts.Const:
			return constValue(t.Const), ""
		case facts.Column:
			v, ok := innerCol(t.Col, r)
			if !ok {
				return Value{}, "column term not in the body row"
			}
			return v, ""
		case facts.Outer:
			v, ok := outerCol(t.Col, outer)
			if !ok {
				return Value{}, "outer term not in the enclosing row"
			}
			return v, ""
		}
		return Value{}, "term of kind " + termKind(t.Kind) + " in an EXISTS body is not evaluated"
	}
	result := no
	var walk func(i int, r row) string
	walk = func(i int, r row) string {
		if i == len(tables) {
			all := yes
			for _, p := range sub.Preds {
				var res tri
				a, ok := innerCol(p.Col, r)
				switch p.Op {
				case facts.Eq:
					if !ok {
						return "body column not in the row"
					}
					b, why := term(p.Term, r)
					if why != "" {
						return why
					}
					res = eqValues(a, b)
				case facts.In:
					if !ok {
						return "body column not in the row"
					}
					var set []Value
					for _, t := range p.Terms {
						b, why := term(t, r)
						if why != "" {
							return why
						}
						set = append(set, b)
					}
					res = inValues(a, set)
				case facts.IsNull:
					res = bool3(ok && a.Null)
				case facts.IsNotNull:
					res = bool3(ok && !a.Null)
				default:
					return fmt.Sprintf("predicate op %d in an EXISTS body is not evaluated", p.Op)
				}
				if res == no {
					return ""
				}
				if res == unknown {
					all = unknown
				}
			}
			if all == yes {
				result = yes
			}
			return ""
		}
		for _, tr := range tables[i].Rows {
			nr := make(row, len(r)+len(tr))
			for k, v := range r {
				nr[k] = v
			}
			for k, c := range tables[i].Cols {
				nr[aliases[i]+"."+c.Name] = tr[k]
			}
			if why := walk(i+1, nr); why != "" {
				return why
			}
			if result == yes {
				return ""
			}
		}
		return ""
	}
	if why := walk(0, row{}); why != "" {
		return unknown, why
	}
	return result, ""
}

// constValue reads a Const term's tagged spelling ("i5", "sx", "btrue", "NULL").
func constValue(c string) Value {
	if c == "NULL" || c == "" {
		return null
	}
	return Value{S: c[1:]}
}

func termKind(k facts.TermKind) string {
	switch k {
	case facts.Param:
		return "param"
	case facts.Const:
		return "const"
	case facts.Column:
		return "column"
	case facts.Known:
		return "known"
	case facts.Outer:
		return "outer"
	case facts.Expr:
		return "expr"
	}
	return fmt.Sprint(k)
}

func predOp(op facts.PredOp) string {
	switch op {
	case facts.Eq:
		return "eq"
	case facts.IsNull:
		return "isnull"
	case facts.IsNotNull:
		return "isnotnull"
	case facts.Exists:
		return "exists"
	case facts.In:
		return "in"
	case facts.Contains:
		return "contains"
	case facts.Opaque:
		return "opaque"
	}
	return fmt.Sprint(op)
}

func describePred(p facts.Pred, aliases []string) string {
	name := func(c facts.ColRef) string {
		if c.Leaf >= 0 && c.Leaf < len(aliases) {
			return aliases[c.Leaf] + "." + c.Column
		}
		return fmt.Sprintf("leaf%d.%s", c.Leaf, c.Column)
	}
	t := func(t facts.Term) string {
		switch t.Kind {
		case facts.Param:
			return fmt.Sprintf("$%d", t.Param)
		case facts.Const:
			return t.Const
		case facts.Column:
			return name(t.Col)
		case facts.Outer:
			return "outer " + name(t.Col)
		}
		return termKind(t.Kind) + "(" + t.Text + ")"
	}
	switch p.Op {
	case facts.Eq:
		return name(p.Col) + " = " + t(p.Term)
	case facts.In:
		var ts []string
		for _, x := range p.Terms {
			ts = append(ts, t(x))
		}
		return name(p.Col) + " IN (" + strings.Join(ts, ", ") + ")"
	case facts.IsNull:
		return name(p.Col) + " IS NULL"
	case facts.IsNotNull:
		return name(p.Col) + " IS NOT NULL"
	case facts.Exists:
		return "EXISTS (...)"
	case facts.Opaque:
		return "opaque " + strconv.Quote(p.Text)
	}
	return predOp(p.Op)
}

// ---- the alphabet: what the facts can say, and what a statement's facts said ----------

// Alphabet lists every shape of claim the facts language has, as the probe names them.
// The gate compares it with what the generated statements' facts exercised.
var Alphabet = []string{
	"pred eq param", "pred eq const", "pred eq column", "pred eq known", "pred eq outer",
	"pred in", "pred isnull", "pred isnotnull", "pred exists", "pred contains", "pred opaque",
	"pred restricted", "pred origin view", "pred origin policy",
	"fixed", "edge", "notnull", "scope single", "scope many", "groups",
	"leaf table", "leaf view", "leaf derived", "leaf cte", "leaf function", "leaf body", "leaf single", "leaf key partial", "leaf key temporal",
	"at most one", "write insert", "write update", "write delete", "value const", "value param", "value known", "kind select", "kind insert", "kind update", "kind delete", "kind call",
}

// alphabetHits lists the alphabet entries f exercises, at any depth.
func alphabetHits(f *facts.Facts) []string {
	hit := map[string]bool{}
	switch f.Kind {
	case facts.Select:
		hit["kind select"] = true
	case facts.Insert:
		hit["kind insert"] = true
	case facts.Update:
		hit["kind update"] = true
	case facts.Delete:
		hit["kind delete"] = true
	case facts.Call:
		hit["kind call"] = true
	}
	if ok, _ := cardinality.AtMostOne(f); ok {
		hit["at most one"] = true
	}
	for _, w := range f.Writes {
		switch w.Kind {
		case facts.Insert:
			hit["write insert"] = true
		case facts.Update:
			hit["write update"] = true
		case facts.Delete:
			hit["write delete"] = true
		}
		for _, v := range w.Values {
			switch v.Kind {
			case facts.Const:
				hit["value const"] = true
			case facts.Param:
				hit["value param"] = true
			case facts.Known:
				hit["value known"] = true
			}
		}
	}
	var walk func(sc *facts.Scope)
	walk = func(sc *facts.Scope) {
		if sc == nil {
			return
		}
		for _, p := range sc.Preds {
			switch p.Op {
			case facts.Eq:
				hit["pred eq "+termKind(p.Term.Kind)] = true
			default:
				hit["pred "+predOp(p.Op)] = true
			}
			if p.Restricts != nil {
				hit["pred restricted"] = true
			}
			switch p.Origin {
			case facts.FromView:
				hit["pred origin view"] = true
			case facts.FromPolicy:
				hit["pred origin policy"] = true
			}
			walk(p.Sub)
		}
		if len(sc.Fixed) > 0 {
			hit["fixed"] = true
		}
		if len(sc.Edges) > 0 {
			hit["edge"] = true
		}
		if len(sc.NotNull) > 0 {
			hit["notnull"] = true
		}
		if sc.Single {
			hit["scope single"] = true
		}
		if sc.Many != "" {
			hit["scope many"] = true
		}
		if sc.Groups != nil {
			hit["groups"] = true
		}
		for _, l := range sc.Leaves {
			switch l.Kind {
			case facts.Table:
				hit["leaf table"] = true
			case facts.View, facts.MatView:
				hit["leaf view"] = true
			case facts.Derived:
				hit["leaf derived"] = true
			case facts.CTE:
				hit["leaf cte"] = true
			case facts.Function:
				hit["leaf function"] = true
			}
			if l.Body != nil {
				hit["leaf body"] = true
				walk(l.Body)
			}
			if l.Single {
				hit["leaf single"] = true
			}
			for _, k := range l.Keys {
				if len(k.Where) > 0 {
					hit["leaf key partial"] = true
				}
				if k.Temporal {
					hit["leaf key temporal"] = true
				}
			}
		}
		for _, ch := range sc.Children {
			walk(ch)
		}
	}
	walk(f.Top)
	var out []string
	for k := range hit {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
