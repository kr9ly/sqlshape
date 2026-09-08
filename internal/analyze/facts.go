package analyze

import (
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"

	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Facts production: the prover's equalities, the leaves it ran over and the write set,
// written down in the dialect-neutral form of internal/facts. Nothing here decides
// anything; internal/obligation does the judging. Every query level the statement has
// (recordFixed's callers, plus MERGE's ON) becomes one facts.Scope; view bodies are
// converted on demand and hung off the leaf that reads them.

// factScope pairs an analyzer scope with the facts derived at it.
type factScope struct {
	sc *scope
	fs *facts.Scope
}

// newProver builds the equality closure for one level: the leaves of sc, the ON / USING
// conjuncts of its joins and the conjuncts of where, with known values propagated along
// the equalities (no single-row reasoning: that is fixpoint's, for the One proof).
func (a *analyzer) newProver(sc *scope, where *pg_query.Node) *prover {
	p := &prover{a: a, sc: sc, known: map[colKey]bool{}, single: map[*rte]bool{}, why: map[*rte]string{}}
	for _, it := range sc.items {
		p.addItem(it)
	}
	p.addQuals(where, nil)
	for changed := true; changed; {
		changed = false
		for _, e := range p.edges {
			if p.known[e.from] && !p.known[e.to] {
				p.known[e.to] = true
				changed = true
			}
		}
	}
	return p
}

// recordFacts converts a proved level into facts and remembers it, under the scope as
// (the level's own scope, or the statement's for MERGE whose ON lives in a helper scope),
// for the tree built at the end of the analysis. Levels inside a view body are not the statement's (they are
// reached through the leaf that reads the view); a RETURNING list's rows are the ones
// just written and carry no obligations.
func (a *analyzer) recordFacts(p *prover, as *scope) {
	if a.inView > 0 || a.inReturning {
		return
	}
	a.factScopes = append(a.factScopes, factScope{sc: as, fs: a.scopeFacts(p, nil)})
}

// scopeFacts writes one level down. waived names the tables the enclosing definition
// opted out for (a view's own `unfiltered` directive); the statement's directives are on
// the analyzer.
func (a *analyzer) scopeFacts(p *prover, waived map[string]bool) *facts.Scope {
	fs := &facts.Scope{}
	idx := map[*rte]int{}
	for i, l := range p.leaves {
		idx[l] = i
		fs.Leaves = append(fs.Leaves, a.leafFacts(l, waived))
	}
	ref := func(k colKey) (facts.ColRef, bool) {
		i, ok := idx[k.r]
		if !ok || k.i >= len(k.r.cols) {
			return facts.ColRef{}, false
		}
		return facts.ColRef{Leaf: i, Column: k.r.cols[k.i].name}, true
	}
	for _, c := range p.conjuncts {
		pr := a.predFacts(p, c.n, ref, false)
		if c.allow != nil {
			for l := range c.allow {
				if i, ok := idx[l]; ok {
					pr.Restricts = append(pr.Restricts, i)
				}
			}
			sort.Ints(pr.Restricts)
			if pr.Restricts == nil {
				pr.Restricts = []int{} // restricts nothing of this level, as opposed to everything
			}
		}
		pr.Origin = facts.FromStatement
		fs.Preds = append(fs.Preds, pr)
	}
	for k := range p.known {
		if r, ok := ref(k); ok {
			fs.Fixed = append(fs.Fixed, r)
		}
	}
	sortRefs(fs.Fixed)
	for _, e := range p.edges {
		from, ok1 := ref(e.from)
		to, ok2 := ref(e.to)
		if ok1 && ok2 {
			fs.Edges = append(fs.Edges, facts.Edge{From: from, To: to})
		}
	}
	seen := map[facts.ColRef]bool{}
	for _, k := range a.nullRejected(p) {
		if r, ok := ref(k); ok && !seen[r] {
			seen[r] = true
			fs.NotNull = append(fs.NotNull, r)
		}
	}
	sortRefs(fs.NotNull)
	// row-security policies: what the database itself guarantees about the rows of a table
	// leaf, for roles subject to row security
	for i, l := range p.leaves {
		if l.rel == nil || !l.rel.RowSecurity {
			continue
		}
		for _, pol := range l.rel.Policies {
			if !pol.Permissive || (pol.Command != "all" && pol.Command != "select") || pol.Using == nil {
				continue
			}
			pp := &prover{a: a, sc: &scope{items: []*rte{l}}, known: map[colKey]bool{}, single: map[*rte]bool{}, why: map[*rte]string{}}
			pp.leaves = []*rte{l}
			for _, c := range conjuncts(pol.Using) {
				pr := a.predFacts(pp, c, func(k colKey) (facts.ColRef, bool) {
					if k.r != l || k.i >= len(l.cols) {
						return facts.ColRef{}, false
					}
					return facts.ColRef{Leaf: i, Column: l.cols[k.i].name}, true
				}, true)
				pr.Origin = facts.FromPolicy
				pr.Restricts = []int{i}
				fs.Preds = append(fs.Preds, pr)
			}
		}
	}
	return fs
}

// leafFacts describes one FROM leaf.
func (a *analyzer) leafFacts(l *rte, waived map[string]bool) facts.Leaf {
	lf := facts.Leaf{Alias: l.alias, Kind: facts.Derived, Role: facts.Read, Position: l.pos}
	if l.target {
		lf.Role = facts.Target
	}
	var rel *schema.Relation
	switch {
	case l.rel != nil && l.rel.Kind == schema.Table:
		rel, lf.Kind = l.rel, facts.Table
	case l.viewRel != nil:
		rel = l.viewRel
		lf.Kind = facts.View
		if rel.Kind == schema.MatView {
			lf.Kind = facts.MatView
		}
		lf.View = a.viewFacts(rel)
	}
	if rel == nil {
		return lf
	}
	lf.Table = rel.FullName()
	if waived == nil {
		waived = a.unfiltered
	}
	if waived[rel.Name] || waived[rel.FullName()] {
		lf.Waived = []string{"unfiltered"}
	}
	return lf
}

// viewFacts converts a view's defining query once per analysis; nil when the body did not
// analyze. The view's own `unfiltered` directive is the waiver inside it.
func (a *analyzer) viewFacts(rel *schema.Relation) *facts.Scope {
	if fs, ok := a.viewFactScopes[rel]; ok {
		return fs
	}
	a.viewFactScopes[rel] = nil // a view reached again through itself gets no body
	sub := a.viewScopes[rel]
	if sub == nil || sub.sel == nil {
		return nil
	}
	p := a.newProver(sub.sc, sub.sel.WhereClause)
	fs := a.scopeFacts(p, rel.Unfiltered)
	clearPositions(fs) // offsets into the view's definition mean nothing to the statement
	a.viewFactScopes[rel] = fs
	return fs
}

func clearPositions(fs *facts.Scope) {
	for i := range fs.Leaves {
		fs.Leaves[i].Position = -1
	}
	for _, c := range fs.Children {
		clearPositions(c)
	}
}

// predFacts normalizes one conjunct. policy relaxes "known value" to "reads no column":
// a policy's session-setting call was not analyzed by this analyzer, so its volatility is
// not on record, but pinning by a policy has always meant exactly that (vet's former
// pinsColumn).
func (a *analyzer) predFacts(p *prover, n *pg_query.Node, ref func(colKey) (facts.ColRef, bool), policy bool) facts.Pred {
	known := func(x *pg_query.Node) bool {
		if policy {
			return !readsColumn(x)
		}
		return p.isKnown(x)
	}
	if l, r := equalitySides(n); l != nil {
		lk, lcol := p.resolve(l)
		rk, rcol := p.resolve(r)
		lr, lok := facts.ColRef{}, false
		rr, rok := facts.ColRef{}, false
		if lcol {
			lr, lok = ref(lk)
		}
		if rcol {
			rr, rok = ref(rk)
		}
		switch {
		case lok && rok:
			return facts.Pred{Op: facts.Eq, Col: lr, Term: facts.Term{Kind: facts.Column, Col: rr}}
		case lok && !rcol && known(r):
			return facts.Pred{Op: facts.Eq, Col: lr, Term: termFacts(r)}
		case rok && !lcol && known(l):
			return facts.Pred{Op: facts.Eq, Col: rr, Term: termFacts(l)}
		}
	}
	if nt := n.GetNullTest(); nt != nil {
		if k, ok := p.resolve(nt.Arg); ok {
			if r, ok := ref(k); ok {
				op := facts.IsNull
				if nt.Nulltesttype == pg_query.NullTestType_IS_NOT_NULL {
					op = facts.IsNotNull
				}
				return facts.Pred{Op: op, Col: r}
			}
		}
	}
	// opaque: the text with this level's column references reduced to bare names
	clone := proto.Clone(n).(*pg_query.Node)
	var cols []facts.ColRef
	schema.WalkNodes(clone, func(m *pg_query.Node) {
		cr := m.GetColumnRef()
		if cr == nil || len(cr.Fields) < 2 {
			if cr != nil && len(cr.Fields) == 1 {
				if k, ok := p.resolve(m); ok {
					if r, ok := ref(k); ok {
						cols = append(cols, r)
					}
				}
			}
			return
		}
		if k, ok := p.resolve(m); ok {
			if r, ok := ref(k); ok {
				cols = append(cols, r)
				cr.Fields = cr.Fields[len(cr.Fields)-1:]
			}
		}
	})
	sortRefs(cols)
	return facts.Pred{Op: facts.Opaque, Text: strings.TrimPrefix(deparse(clone), "SELECT "), Cols: cols}
}

// termFacts classifies the known side of an equality.
func termFacts(n *pg_query.Node) facts.Term {
	inner := n
	for {
		tc := inner.GetTypeCast()
		if tc == nil {
			break
		}
		inner = tc.Arg
	}
	switch v := inner.Node.(type) {
	case *pg_query.Node_ParamRef:
		return facts.Term{Kind: facts.Param, Param: v.ParamRef.Number}
	case *pg_query.Node_AConst:
		return facts.Term{Kind: facts.Const, Const: constText(v.AConst)}
	}
	return facts.Term{Kind: facts.Known, Text: strings.TrimPrefix(deparse(n), "SELECT ")}
}

// readsColumn reports whether an expression references any column.
func readsColumn(n *pg_query.Node) bool {
	found := false
	schema.WalkNodes(n, func(m *pg_query.Node) {
		if m.GetColumnRef() != nil {
			found = true
		}
	})
	return found
}

func sortRefs(rs []facts.ColRef) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Leaf != rs[j].Leaf {
			return rs[i].Leaf < rs[j].Leaf
		}
		return rs[i].Column < rs[j].Column
	})
}

// buildFacts assembles the statement's facts: the top level, the nested levels under
// their nearest recorded ancestor, and the write set.
func (a *analyzer) buildFacts(stmt *pg_query.Node, top *scope) *facts.Facts {
	f := &facts.Facts{}
	switch stmt.Node.(type) {
	case *pg_query.Node_SelectStmt:
		f.Kind = facts.Select
	case *pg_query.Node_InsertStmt:
		f.Kind = facts.Insert
	case *pg_query.Node_UpdateStmt:
		f.Kind = facts.Update
	case *pg_query.Node_DeleteStmt:
		f.Kind = facts.Delete
	case *pg_query.Node_MergeStmt:
		f.Kind = facts.Merge
	default:
		return nil
	}
	byScope := map[*scope]*facts.Scope{}
	for _, r := range a.factScopes {
		byScope[r.sc] = r.fs
	}
	f.Top = byScope[top]
	if f.Top == nil {
		// INSERT (and a MERGE whose ON did not record): the target alone at the top
		f.Top = &facts.Scope{}
		if a.writeLeaf != nil {
			f.Top.Leaves = []facts.Leaf{a.leafFacts(a.writeLeaf, nil)}
		}
		byScope[top] = f.Top
	}
	for _, r := range a.factScopes {
		if r.fs == f.Top {
			continue
		}
		parent := f.Top
		for s := r.sc.parent; s != nil; s = s.parent {
			if fs, ok := byScope[s]; ok {
				parent = fs
				break
			}
		}
		parent.Children = append(parent.Children, r.fs)
	}
	// writes: one entry per assigned relation, in first-assignment order
	kind := f.Kind
	if kind == facts.Select {
		kind = 0 // a data-modifying CTE under a SELECT still reports its assignments
	}
	pos := int32(-1)
	if a.writeLeaf != nil {
		pos = a.writeLeaf.pos
	}
	order := map[string]int{}
	for _, as := range a.assigned {
		if as.rel == nil {
			continue
		}
		name := as.rel.FullName()
		i, ok := order[name]
		if !ok {
			i = len(f.Writes)
			order[name] = i
			f.Writes = append(f.Writes, facts.Write{Table: name, Kind: kind, Position: pos})
		}
		dup := false
		for _, c := range f.Writes[i].Assigned {
			dup = dup || c == as.col.Name
		}
		if !dup {
			f.Writes[i].Assigned = append(f.Writes[i].Assigned, as.col.Name)
		}
	}
	if f.Kind == facts.Delete && a.writeLeaf != nil {
		name := a.writeLeaf.alias
		if a.writeLeaf.rel != nil {
			name = a.writeLeaf.rel.FullName()
		} else if a.writeLeaf.viewRel != nil {
			name = a.writeLeaf.viewRel.FullName()
		}
		f.Writes = append(f.Writes, facts.Write{Table: name, Kind: facts.Delete, Position: pos})
	}
	return f
}
