package analyze

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// Cardinality: proving "at most one row".
//
// An application that calls One asserts an interpretation ("this lookup hits at most one
// row") that the database can back with its constraints. The proof is the classic
// functional-dependency argument over the FROM leaves: a leaf is single once some unique
// key of it (PK / UNIQUE / unique index, partial ones only when the query repeats the
// predicate) is fixed by equalities to known values, and a single leaf makes all its
// columns known, which can fix other leaves through join equalities. Known values are
// literals, parameters, outer references, uncorrelated scalar subqueries and casts /
// arithmetic over those. When every leaf is single the result is at most one row.
//
// Join direction matters: an ON condition of a LEFT JOIN restricts only the right side,
// so its equalities may only make right-side columns known. FULL JOIN is never single
// (two unmatched sides give two rows). Subqueries, views and CTEs are proved recursively
// with the outer-fixed output columns as seeds. Besides the key argument, a constant
// LIMIT 0/1, an aggregate without GROUP BY, no FROM at all and a one-row VALUES /
// INSERT are single.

// subquery is the defining query of a FROM leaf that is not a table.
type subquery struct {
	what string // "subquery" / "view" / "CTE"
	sel  *pg_query.SelectStmt
	sc   *scope
}

type colKey struct {
	r *rte
	i int
}

type edge struct{ from, to colKey }

type conjunct struct {
	n     *pg_query.Node
	allow map[*rte]bool // leaves this predicate restricts (nil: all)
}

type prover struct {
	a         *analyzer
	sc        *scope
	leaves    []*rte
	conjuncts []conjunct
	known     map[colKey]bool
	edges     []edge
	single    map[*rte]bool
	why       map[*rte]string // leaf → why it could not be proved single
	fail      string          // structural reason (FULL JOIN)
}

// cardinality decides whether the analyzed statement returns at most one row.
func (a *analyzer) cardinality(stmt *pg_query.Node, sc *scope) (bool, string) {
	switch st := stmt.Node.(type) {
	case *pg_query.Node_SelectStmt:
		return a.selectSingle(st.SelectStmt, sc, nil)
	case *pg_query.Node_InsertStmt:
		ins := st.InsertStmt
		if ins.SelectStmt == nil {
			return true, ""
		}
		sel := ins.SelectStmt.GetSelectStmt()
		if len(sel.ValuesLists) > 0 && sel.Op == pg_query.SetOperation_SETOP_NONE && len(sel.FromClause) == 0 {
			if len(sel.ValuesLists) == 1 {
				return true, ""
			}
			return false, fmt.Sprintf("VALUES has %d rows", len(sel.ValuesLists))
		}
		return a.selectSingle(sel, a.insertSelScope, nil)
	case *pg_query.Node_UpdateStmt:
		return a.fromSingle(sc, st.UpdateStmt.WhereClause, nil, nil)
	case *pg_query.Node_DeleteStmt:
		return a.fromSingle(sc, st.DeleteStmt.WhereClause, nil, nil)
	case *pg_query.Node_CallStmt:
		return true, ""
	}
	return false, "unsupported statement"
}

// selectSingle proves one SELECT level; knownOut are output columns fixed from outside.
func (a *analyzer) selectSingle(sel *pg_query.SelectStmt, sc *scope, knownOut []int) (bool, string) {
	if sel.Op != pg_query.SetOperation_SETOP_NONE && sel.Op != pg_query.SetOperation_SET_OPERATION_UNDEFINED {
		return false, setOpName(sel.Op) + " may combine rows"
	}
	if len(sel.ValuesLists) > 0 {
		if len(sel.ValuesLists) == 1 {
			return true, ""
		}
		return false, fmt.Sprintf("VALUES has %d rows", len(sel.ValuesLists))
	}
	if n, ok := constInt(sel.LimitCount); ok && n <= 1 {
		return true, ""
	}
	if len(sel.FromClause) == 0 {
		return true, ""
	}
	if len(sel.GroupClause) > 0 {
		return false, "GROUP BY yields one row per group"
	}
	if sc.agg {
		return true, ""
	}
	return a.fromSingle(sc, sel.WhereClause, sel.TargetList, knownOut)
}

// fromSingle runs the functional-dependency argument over sc.items.
func (a *analyzer) fromSingle(sc *scope, where *pg_query.Node, targets []*pg_query.Node, knownOut []int) (bool, string) {
	p := &prover{a: a, sc: sc, known: map[colKey]bool{}, single: map[*rte]bool{}, why: map[*rte]string{}}
	for _, it := range sc.items {
		p.addItem(it)
	}
	if p.fail != "" {
		return false, p.fail
	}
	p.addQuals(where, nil)
	if len(knownOut) > 0 {
		keys := p.outputKeys(targets)
		for _, i := range knownOut {
			if i < len(keys) && keys[i] != nil {
				p.known[*keys[i]] = true
			}
		}
	}
	p.fixpoint()
	for _, l := range p.leaves {
		if !p.single[l] {
			return false, p.describe(l)
		}
	}
	return true, ""
}

func leavesOf(r *rte) map[*rte]bool {
	m := map[*rte]bool{}
	for _, l := range r.leaves() {
		m[l] = true
	}
	return m
}

func (p *prover) addItem(r *rte) {
	if r.join == nil {
		p.leaves = append(p.leaves, r)
		return
	}
	j := r.join
	p.addItem(j.left)
	p.addItem(j.right)
	var allow map[*rte]bool
	switch j.jointype {
	case pg_query.JoinType_JOIN_FULL:
		p.fail = "FULL JOIN may keep unmatched rows of both sides"
		return
	case pg_query.JoinType_JOIN_LEFT:
		allow = leavesOf(j.right)
	case pg_query.JoinType_JOIN_RIGHT:
		allow = leavesOf(j.left)
	}
	p.addQuals(j.quals, allow)
	for _, u := range j.using {
		lk, rk := p.resolveIn(j.left, u), p.resolveIn(j.right, u)
		if len(lk) == 1 && len(rk) == 1 {
			p.equate(lk[0], rk[0], allow)
		}
	}
}

// addQuals records the equality conjuncts of a predicate; allow limits which leaves it
// may restrict (an outer join's ON clause restricts only its nullable side).
func (p *prover) addQuals(n *pg_query.Node, allow map[*rte]bool) {
	for _, c := range conjuncts(n) {
		p.conjuncts = append(p.conjuncts, conjunct{n: c, allow: allow})
		l, r := equalitySides(c)
		if l == nil {
			continue
		}
		lk, lcol := p.resolve(l)
		rk, rcol := p.resolve(r)
		switch {
		case lcol && rcol:
			p.equate(lk, rk, allow)
		case lcol && p.isKnown(r):
			p.fix(lk, allow)
		case rcol && p.isKnown(l):
			p.fix(rk, allow)
		}
	}
}

func (p *prover) fix(k colKey, allow map[*rte]bool) {
	if allow == nil || allow[k.r] {
		p.known[k] = true
	}
}

func (p *prover) equate(x, y colKey, allow map[*rte]bool) {
	if allow == nil || allow[y.r] {
		p.edges = append(p.edges, edge{from: x, to: y})
	}
	if allow == nil || allow[x.r] {
		p.edges = append(p.edges, edge{from: y, to: x})
	}
}

func conjuncts(n *pg_query.Node) []*pg_query.Node {
	if n == nil {
		return nil
	}
	if b := n.GetBoolExpr(); b != nil && b.Boolop == pg_query.BoolExprType_AND_EXPR {
		var out []*pg_query.Node
		for _, arg := range b.Args {
			out = append(out, conjuncts(arg)...)
		}
		return out
	}
	return []*pg_query.Node{n}
}

// equalitySides returns the operands of `x = y` (also `x IN (y)` with a single item).
func equalitySides(n *pg_query.Node) (*pg_query.Node, *pg_query.Node) {
	x := n.GetAExpr()
	if x == nil || x.Lexpr == nil {
		return nil, nil
	}
	switch x.Kind {
	case pg_query.A_Expr_Kind_AEXPR_OP:
		if parts := strs(x.Name); len(parts) > 0 && parts[len(parts)-1] == "=" {
			return x.Lexpr, x.Rexpr
		}
	case pg_query.A_Expr_Kind_AEXPR_IN:
		if items := x.Rexpr.GetList().GetItems(); len(items) == 1 {
			return x.Lexpr, items[0]
		}
	}
	return nil, nil
}

// resolve maps a column reference to a leaf column of this level; col is false for
// anything else (a known value, an outer reference, an expression).
func (p *prover) resolve(n *pg_query.Node) (colKey, bool) {
	cr := n.GetColumnRef()
	if cr == nil {
		return colKey{}, false
	}
	var names []string
	for _, f := range cr.Fields {
		if f.GetAStar() != nil {
			return colKey{}, false
		}
		names = append(names, f.GetString_().GetSval())
	}
	switch len(names) {
	case 1:
		var hits []colKey
		for _, it := range p.sc.items {
			hits = append(hits, p.resolveIn(it, names[0])...)
		}
		if len(hits) == 1 {
			return hits[0], true
		}
	case 2, 3:
		tbl, col := names[len(names)-2], names[len(names)-1]
		if leaf := p.sc.byAlias(tbl); leaf != nil {
			for i, c := range leaf.cols {
				if c.name == col {
					return colKey{leaf, i}, true
				}
			}
		}
	}
	return colKey{}, false
}

// resolveIn mirrors rte.find with column identities; a USING column stands for its left side.
func (p *prover) resolveIn(r *rte, name string) []colKey {
	if r.join == nil {
		var out []colKey
		for i, c := range r.cols {
			if c.name == name && !r.hidden[name] {
				out = append(out, colKey{r, i})
			}
		}
		return out
	}
	for _, u := range r.join.using {
		if u == name {
			return p.resolveIn(r.join.left, name)
		}
	}
	return append(p.resolveIn(r.join.left, name), p.resolveIn(r.join.right, name)...)
}

// outputKeys maps each output column of a target list to the leaf column it is a plain
// reference to (nil otherwise), expanding stars like the analyzer does.
func (p *prover) outputKeys(targets []*pg_query.Node) []*colKey {
	var out []*colKey
	var expand func(r *rte)
	expand = func(r *rte) {
		if r.join == nil {
			for i := range r.cols {
				k := colKey{r, i}
				out = append(out, &k)
			}
			return
		}
		j := r.join
		skip := map[string]bool{}
		for _, u := range j.using {
			skip[u] = true
			ks := p.resolveIn(j.left, u)
			if len(ks) == 1 {
				k := ks[0]
				out = append(out, &k)
			} else {
				out = append(out, nil)
			}
		}
		var side func(r *rte)
		side = func(r *rte) {
			if r.join == nil {
				for i, c := range r.cols {
					if !skip[c.name] {
						k := colKey{r, i}
						out = append(out, &k)
					}
				}
				return
			}
			side(r.join.left)
			side(r.join.right)
		}
		side(j.left)
		side(j.right)
	}
	for _, tn := range targets {
		val := tn.GetResTarget().GetVal()
		if cr := val.GetColumnRef(); cr != nil && isStar(cr) {
			if len(cr.Fields) == 1 {
				for _, it := range p.sc.items {
					expand(it)
				}
			} else if leaf := p.sc.byAlias(cr.Fields[len(cr.Fields)-2].GetString_().GetSval()); leaf != nil {
				expand(leaf)
			}
			continue
		}
		if k, ok := p.resolve(val); ok {
			out = append(out, &k)
		} else {
			out = append(out, nil)
		}
	}
	return out
}

// isKnown reports whether n has one value for the whole query (or per outer row).
func (p *prover) isKnown(n *pg_query.Node) bool {
	switch v := n.Node.(type) {
	case *pg_query.Node_AConst, *pg_query.Node_ParamRef:
		return true
	case *pg_query.Node_TypeCast:
		return p.isKnown(v.TypeCast.Arg)
	case *pg_query.Node_ColumnRef:
		if _, ok := p.resolve(n); ok {
			return false
		}
		// an outer reference is a constant for this level
		return p.sc.parent != nil && p.resolvesOutside(v.ColumnRef)
	case *pg_query.Node_SubLink:
		return v.SubLink.SubLinkType == pg_query.SubLinkType_EXPR_SUBLINK && !p.correlated(v.SubLink.Subselect)
	case *pg_query.Node_AExpr:
		x := v.AExpr
		return x.Kind == pg_query.A_Expr_Kind_AEXPR_OP && (x.Lexpr == nil || p.isKnown(x.Lexpr)) && p.isKnown(x.Rexpr)
	case *pg_query.Node_CoalesceExpr:
		for _, arg := range v.CoalesceExpr.Args {
			if !p.isKnown(arg) {
				return false
			}
		}
		return true
	}
	return false
}

func (p *prover) resolvesOutside(cr *pg_query.ColumnRef) bool {
	var names []string
	for _, f := range cr.Fields {
		names = append(names, f.GetString_().GetSval())
	}
	tbl, col := "", names[len(names)-1]
	if len(names) > 1 {
		tbl = names[len(names)-2]
	}
	_, err := p.a.resolveColumn(p.sc.parent, tbl, col, -1)
	return err == nil
}

// correlated reports whether a subquery may refer to this level's leaves (conservatively:
// any qualified reference to one of our aliases, or an unqualified name one of our leaves
// has that the subquery's own FROM tables do not provide).
func (p *prover) correlated(sub *pg_query.Node) bool {
	found := false
	inner := p.a.fromColumns(sub.GetSelectStmt())
	schema.WalkNodes(sub, func(n *pg_query.Node) {
		cr := n.GetColumnRef()
		if cr == nil || found {
			return
		}
		var names []string
		for _, f := range cr.Fields {
			if f.GetAStar() == nil {
				names = append(names, f.GetString_().GetSval())
			}
		}
		if len(names) == 0 {
			return
		}
		if len(names) >= 2 {
			found = p.sc.byAlias(names[len(names)-2]) != nil
			return
		}
		if inner[names[0]] {
			return
		}
		for _, it := range p.sc.items {
			if len(p.resolveIn(it, names[0])) > 0 {
				found = true
			}
		}
	})
	return found
}

// fromColumns is the set of column names the tables named directly in sel's FROM provide.
func (a *analyzer) fromColumns(sel *pg_query.SelectStmt) map[string]bool {
	out := map[string]bool{}
	if sel == nil {
		return out
	}
	var walk func(n *pg_query.Node)
	walk = func(n *pg_query.Node) {
		switch v := n.Node.(type) {
		case *pg_query.Node_RangeVar:
			if rel := a.s.Relation(v.RangeVar.Schemaname, v.RangeVar.Relname); rel != nil {
				for _, c := range rel.Columns {
					out[c.Name] = true
				}
				if rel.Kind == schema.View || rel.Kind == schema.MatView {
					for _, c := range a.viewCache[rel] {
						out[c.name] = true
					}
				}
			}
		case *pg_query.Node_JoinExpr:
			walk(v.JoinExpr.Larg)
			walk(v.JoinExpr.Rarg)
		}
	}
	for _, f := range sel.FromClause {
		walk(f)
	}
	return out
}

func (p *prover) fixpoint() {
	for changed := true; changed; {
		changed = false
		for _, e := range p.edges {
			if p.known[e.from] && !p.known[e.to] {
				p.known[e.to] = true
				changed = true
			}
		}
		for _, l := range p.leaves {
			if p.single[l] {
				continue
			}
			if p.leafSingle(l) {
				p.single[l] = true
				for i := range l.cols {
					p.known[colKey{l, i}] = true
				}
				changed = true
			}
		}
	}
}

// leafSingle decides whether the known columns fix at most one row of the leaf.
func (p *prover) leafSingle(r *rte) bool {
	switch {
	case r.single:
		return true
	case r.rel != nil:
		for _, con := range r.rel.Constraints {
			if con.Kind != schema.PrimaryKey && con.Kind != schema.Unique {
				continue
			}
			if p.keyFixed(r, con) {
				return true
			}
		}
		return false
	case r.sub != nil:
		var knownOut []int
		for i := range r.cols {
			if p.known[colKey{r, i}] {
				knownOut = append(knownOut, i)
			}
		}
		ok, why := p.a.selectSingle(r.sub.sel, r.sub.sc, knownOut)
		if !ok {
			p.why[r] = why
		}
		return ok
	}
	return false
}

func (p *prover) keyFixed(r *rte, con *schema.Constraint) bool {
	if len(con.Columns) == 0 {
		return false
	}
	for _, name := range con.Columns {
		idx := -1
		for i, c := range r.cols {
			if c.name == name {
				idx = i
				break
			}
		}
		if idx < 0 || !p.known[colKey{r, idx}] {
			return false
		}
	}
	if con.Predicate == nil {
		return true
	}
	// a partial unique index applies only where the query repeats its predicate
	for _, c := range p.conjuncts {
		if (c.allow == nil || c.allow[r]) && sameExpr(con.Predicate, c.n, r) {
			return true
		}
	}
	return false
}

// sameExpr compares an index predicate (unqualified column names) with a query
// predicate whose columns may be qualified with the leaf's alias.
func sameExpr(pred, q *pg_query.Node, r *rte) bool {
	if pred == nil || q == nil {
		return pred == nil && q == nil
	}
	switch pv := pred.Node.(type) {
	case *pg_query.Node_ColumnRef:
		qc := q.GetColumnRef()
		if qc == nil {
			return false
		}
		pn, qn := strs(pv.ColumnRef.Fields), strs(qc.Fields)
		if len(qn) == 2 && qn[0] != r.alias {
			return false
		}
		return len(pn) > 0 && len(qn) > 0 && pn[len(pn)-1] == qn[len(qn)-1]
	case *pg_query.Node_AConst:
		qc := q.GetAConst()
		return qc != nil && constText(pv.AConst) == constText(qc)
	case *pg_query.Node_AExpr:
		qx := q.GetAExpr()
		return qx != nil && qx.Kind == pv.AExpr.Kind && strings.Join(strs(qx.Name), ".") == strings.Join(strs(pv.AExpr.Name), ".") &&
			sameExpr(pv.AExpr.Lexpr, qx.Lexpr, r) && sameExpr(pv.AExpr.Rexpr, qx.Rexpr, r)
	case *pg_query.Node_BoolExpr:
		qb := q.GetBoolExpr()
		if qb == nil || qb.Boolop != pv.BoolExpr.Boolop || len(qb.Args) != len(pv.BoolExpr.Args) {
			return false
		}
		for i := range qb.Args {
			if !sameExpr(pv.BoolExpr.Args[i], qb.Args[i], r) {
				return false
			}
		}
		return true
	case *pg_query.Node_NullTest:
		qn := q.GetNullTest()
		return qn != nil && qn.Nulltesttype == pv.NullTest.Nulltesttype && sameExpr(pv.NullTest.Arg, qn.Arg, r)
	case *pg_query.Node_BooleanTest:
		qb := q.GetBooleanTest()
		return qb != nil && qb.Booltesttype == pv.BooleanTest.Booltesttype && sameExpr(pv.BooleanTest.Arg, qb.Arg, r)
	case *pg_query.Node_TypeCast:
		qt := q.GetTypeCast()
		return qt != nil && strings.Join(strs(qt.TypeName.Names), ".") == strings.Join(strs(pv.TypeCast.TypeName.Names), ".") && sameExpr(pv.TypeCast.Arg, qt.Arg, r)
	}
	return false
}

func constText(c *pg_query.A_Const) string {
	if c.Isnull {
		return "NULL"
	}
	switch v := c.Val.(type) {
	case *pg_query.A_Const_Ival:
		return fmt.Sprint("i", v.Ival.GetIval())
	case *pg_query.A_Const_Fval:
		return "f" + v.Fval.GetFval()
	case *pg_query.A_Const_Sval:
		return "s" + v.Sval.GetSval()
	case *pg_query.A_Const_Boolval:
		return fmt.Sprint("b", v.Boolval.GetBoolval())
	case *pg_query.A_Const_Bsval:
		return "x" + v.Bsval.GetBsval()
	}
	return "?"
}

// constInt returns the value of an integer constant node.
func constInt(n *pg_query.Node) (int32, bool) {
	c := n.GetAConst()
	if c == nil {
		return 0, false
	}
	if v, ok := c.Val.(*pg_query.A_Const_Ival); ok {
		return v.Ival.GetIval(), true
	}
	return 0, false
}

// describe says why a leaf could not be proved single.
func (p *prover) describe(r *rte) string {
	name := r.alias
	switch {
	case r.rel != nil:
		if r.rel.Name != r.alias {
			name = r.rel.Name + " " + r.alias
		}
		var keys []string
		for _, con := range r.rel.Constraints {
			if con.Kind == schema.PrimaryKey || con.Kind == schema.Unique {
				k := "(" + strings.Join(con.Columns, ", ") + ")"
				if con.Predicate != nil {
					k += " WHERE ..."
				}
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return name + " has no unique key"
		}
		return name + ": no unique key is fixed by equality (keys: " + strings.Join(keys, ", ") + ")"
	case r.sub != nil:
		return r.sub.what + " " + name + ": " + p.why[r]
	}
	return "function " + name + " may return many rows"
}

// recordFixed notes which table columns of this level are pinned to a known value by
// the predicate (WHERE plus join conditions), for Result.Fixed. View bodies are skipped.
func (a *analyzer) recordFixed(sc *scope, where *pg_query.Node) {
	if a.inView > 0 || len(sc.items) == 0 {
		return
	}
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
	for k := range p.known {
		if k.r.rel != nil {
			a.fixed = append(a.fixed, Source{Table: k.r.rel.FullName(), Column: k.r.cols[k.i].name, NotNull: k.r.cols[k.i].src != nil && k.r.cols[k.i].src.NotNull})
		}
	}
}
