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

// writeRec is one write target the statement has (the main statement's, or a
// data-modifying WITH item's), in analysis order: WITH items first, the statement last.
type writeRec struct {
	rel *schema.Relation
	r   *rte
	cmd string // "insert into" / "update" / "delete from" / "merge into"
}

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
func (a *analyzer) recordFacts(p *prover, as *scope, at int32) {
	if a.inView > 0 || a.inReturning {
		return
	}
	fs := a.scopeFacts(p, nil)
	fs.At = at
	a.factScopes = append(a.factScopes, factScope{sc: as, fs: fs})
	if sel := a.scopeSel[as]; sel != nil {
		a.factBySel[sel] = fs
		idx := map[*rte]int{}
		for i, l := range p.leaves {
			idx[l] = i
		}
		var out []*facts.ColRef
		for _, k := range p.outputKeys(sel.TargetList) {
			if k == nil {
				out = append(out, nil)
				continue
			}
			if i, ok := idx[k.r]; ok && k.i < len(k.r.cols) {
				out = append(out, &facts.ColRef{Leaf: i, Column: k.r.cols[k.i].name})
			} else {
				out = append(out, nil)
			}
		}
		a.factOutBySel[sel] = out
	}
}

// scopeFacts writes one level down. waived are the enclosing definition's opt-outs (a
// view's own directives); nil means the statement's, which are on the analyzer.
func (a *analyzer) scopeFacts(p *prover, waived map[string][]string) *facts.Scope {
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
				pr.Name = pol.Name
				pr.Restricts = []int{i}
				fs.Preds = append(fs.Preds, pr)
			}
		}
	}
	return fs
}

// leafFacts describes one FROM leaf.
func (a *analyzer) leafFacts(l *rte, waived map[string][]string) facts.Leaf {
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
		waived = a.waived
	}
	lf.Waived = append([]string{}, waived[rel.Name]...)
	if rel.FullName() != rel.Name {
		lf.Waived = append(lf.Waived, waived[rel.FullName()]...)
	}
	return lf
}

// viewFacts converts a view's defining query once per analysis; nil when the body did not
// analyze. The view's own directives are the waivers inside it.
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
	waived := rel.Waived
	if waived == nil {
		waived = map[string][]string{} // not the reading statement's
	}
	fs := a.scopeFacts(p, waived)
	fs.At = -1
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
	term := func(x *pg_query.Node) facts.Term {
		if o, ok := p.outerRef(x); ok {
			return facts.Term{Kind: facts.Outer, Col: o}
		}
		return termFacts(x)
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
			return facts.Pred{Op: facts.Eq, Col: lr, Term: term(r)}
		case rok && !lcol && known(l):
			return facts.Pred{Op: facts.Eq, Col: rr, Term: term(l)}
		}
	}
	if sub := n.GetSubLink(); sub != nil && !policy {
		if pr, ok := a.existsFacts(p, sub, ref); ok {
			return pr
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

// existsFacts turns an EXISTS (or a single-column `x IN (SELECT y ...)`) conjunct into an
// Exists predicate carrying the subquery's own facts; the body was recorded when the
// subquery was analyzed (factBySel). Anything else stays opaque.
func (a *analyzer) existsFacts(p *prover, sub *pg_query.SubLink, ref func(colKey) (facts.ColRef, bool)) (facts.Pred, bool) {
	sel := sub.Subselect.GetSelectStmt()
	body := a.factBySel[sel]
	if body == nil {
		return facts.Pred{}, false
	}
	switch sub.SubLinkType {
	case pg_query.SubLinkType_EXISTS_SUBLINK:
	case pg_query.SubLinkType_ANY_SUBLINK:
		// x IN (SELECT y FROM ...) / x = ANY (SELECT y ...): a witness row with y = x
		if names := strs(sub.OperName); len(names) > 0 && names[len(names)-1] != "=" {
			return facts.Pred{}, false
		}
		outs := a.factOutBySel[sel]
		if sub.Testexpr == nil || sub.Testexpr.GetRowExpr() != nil || len(outs) != 1 || outs[0] == nil {
			return facts.Pred{}, false
		}
		var t facts.Term
		if k, ok := p.resolve(sub.Testexpr); ok {
			r, ok := ref(k)
			if !ok {
				return facts.Pred{}, false
			}
			t = facts.Term{Kind: facts.Outer, Col: r}
		} else if p.isKnown(sub.Testexpr) {
			t = termFacts(sub.Testexpr)
		} else {
			return facts.Pred{}, false
		}
		with := *body
		with.Preds = append(append([]facts.Pred{}, body.Preds...), facts.Pred{Op: facts.Eq, Col: *outs[0], Term: t, Origin: facts.FromStatement})
		body = &with
	default:
		return facts.Pred{}, false
	}
	a.claimed[a.factBySel[sel]] = true
	return facts.Pred{Op: facts.Exists, Sub: body}, true
}

// outerRef places a column reference of an enclosing level: the leaf index it has in the
// parent level's facts and its name. False for anything that is not such a reference.
func (p *prover) outerRef(n *pg_query.Node) (facts.ColRef, bool) {
	cr := n.GetColumnRef()
	if cr == nil || p.sc == nil || p.sc.parent == nil {
		return facts.ColRef{}, false
	}
	if _, here := p.resolve(n); here {
		return facts.ColRef{}, false
	}
	parent := p.sc.parent.queryScope()
	pp := &prover{a: p.a, sc: parent, known: map[colKey]bool{}, single: map[*rte]bool{}, why: map[*rte]string{}}
	for _, it := range parent.items {
		pp.addItem(it)
	}
	k, ok := pp.resolve(n)
	if !ok || k.i >= len(k.r.cols) {
		return facts.ColRef{}, false
	}
	for i, l := range pp.leaves {
		if l == k.r {
			return facts.ColRef{Leaf: i, Column: k.r.cols[k.i].name}, true
		}
	}
	return facts.ColRef{}, false
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
		f.Top = &facts.Scope{At: -1}
		if a.writeLeaf != nil {
			f.Top.Leaves = []facts.Leaf{a.leafFacts(a.writeLeaf, nil)}
		}
		byScope[top] = f.Top
	}
	for _, r := range a.factScopes {
		if r.fs == f.Top || a.claimed[r.fs] {
			continue // a subquery body hangs off its EXISTS / IN predicate instead
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
	// writes: one per write target (WITH items first, the statement last), with the
	// columns assigned to that table
	for _, w := range a.writeRecs {
		name := w.r.alias
		switch {
		case w.r.rel != nil:
			name = w.r.rel.FullName()
		case w.r.viewRel != nil:
			name = w.r.viewRel.FullName()
		}
		fw := facts.Write{Table: name, Position: w.r.pos}
		switch w.cmd {
		case "insert into":
			fw.Kind = facts.Insert
		case "update":
			fw.Kind = facts.Update
		case "delete from":
			fw.Kind = facts.Delete
		case "merge into":
			fw.Kind = facts.Merge
		}
		for _, as := range a.assigned {
			if as.rel == nil || (as.rel != w.rel && as.rel.FullName() != name) {
				continue
			}
			dup := false
			for _, c := range fw.Assigned {
				dup = dup || c == as.col.Name
			}
			if dup {
				continue
			}
			fw.Assigned = append(fw.Assigned, as.col.Name)
			v := facts.Term{Kind: facts.Known, Text: "?"}
			if as.e != nil && as.e.node != nil {
				v = termFacts(as.e.node)
			}
			fw.Values = append(fw.Values, v)
		}
		f.Writes = append(f.Writes, fw)
	}
	for _, u := range a.uses {
		f.Uses = append(f.Uses, facts.Use{Table: u.Table, Column: u.Column, Position: u.Position - 1, Assigned: !a.readUses[u.Table+"."+u.Column]})
	}
	return f
}

// Lower reads a declared predicate over rel in the facts language: the expression is
// parsed and type-checked against the table alone (an error is the declaration's), and
// each conjunct becomes a facts.Pred whose ColRef.Leaf is 0, standing for the subject.
// This is internal/obligation's Lowerer for PostgreSQL.
func Lower(s *schema.Schema, expr string, rel *schema.Relation) ([]facts.Pred, error) {
	tree, err := pg_query.Parse("SELECT " + expr)
	if err != nil {
		return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
	}
	targets := tree.Stmts[0].Stmt.GetSelectStmt().GetTargetList()
	if len(tree.Stmts) != 1 || len(targets) != 1 {
		return nil, &Error{Code: codeSyntaxError, Message: "one boolean expression expected"}
	}
	n := targets[0].GetResTarget().GetVal()
	a := newAnalyzer(s, nil, nil)
	sc := newScope(nil)
	r, aerr := a.relationRTE(rel, nil, 0)
	if aerr != nil {
		return nil, aerr
	}
	sc.items = []*rte{r}
	if aerr := a.boolClause(n, sc, "WHERE"); aerr != nil {
		if aerr.Position > int32(len("SELECT ")) {
			aerr.Position -= int32(len("SELECT ")) // positions are the declaration's, not the wrapper's
		}
		return nil, aerr
	}
	p := a.newProver(sc, n)
	ref := func(k colKey) (facts.ColRef, bool) {
		if k.r != r || k.i >= len(r.cols) {
			return facts.ColRef{}, false
		}
		return facts.ColRef{Leaf: 0, Column: r.cols[k.i].name}, true
	}
	var out []facts.Pred
	for _, c := range p.conjuncts {
		pr := a.predFacts(p, c.n, ref, false)
		pr.Origin = facts.FromStatement
		out = append(out, pr)
	}
	return out, nil
}
