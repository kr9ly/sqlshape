package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// The equality structure of a query level, for the facts (facts.go) and the advisories.
//
// The One proof itself ("at most one row") is x/cardinality's, over the facts: this file
// only decides what is known before the statement runs -- literals, parameters, outer
// references, uncorrelated scalar subqueries, stable functions and casts / arithmetic
// over those -- and which columns the predicates fix to such values. Join direction
// matters: an ON condition of a LEFT JOIN restricts only the right side, so its
// equalities may only make right-side columns known; a FULL JOIN keeps unmatched rows of
// both sides, which the level's facts say (Many).

// subquery is the defining query of a FROM leaf that is not a table.
type subquery struct {
	what string // "subquery" / "view" / "CTE"
	sel  *pgparse.SelectStmt
	sc   *scope
}

type colKey struct {
	r *rte
	i int
}

type edge struct{ from, to colKey }

type conjunct struct {
	n     *pgparse.Node
	allow map[*rte]bool // leaves this predicate restricts (nil: all)
}

// prover collects one query level's equality structure: its leaves, the conjuncts that
// restrict them (with the leaves an outer join's ON may restrict), the columns fixed to
// known values and the equalities between columns. It is the source of the level's facts
// (facts.go), of the nullability refinement and of the plan advisories.
type prover struct {
	a         *analyzer
	sc        *scope
	leaves    []*rte
	conjuncts []conjunct
	known     map[colKey]bool
	edges     []edge
	fail      string // structural reason the level may yield many rows (FULL JOIN)
}

func (a *analyzer) newProverFor(sc *scope) *prover {
	return &prover{a: a, sc: sc, known: map[colKey]bool{}}
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
	case pgparse.JoinType_JOIN_FULL:
		p.fail = "FULL JOIN may keep unmatched rows of both sides"
		return
	case pgparse.JoinType_JOIN_LEFT:
		allow = leavesOf(j.right)
	case pgparse.JoinType_JOIN_RIGHT:
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
func (p *prover) addQuals(n *pgparse.Node, allow map[*rte]bool) {
	for _, c := range conjuncts(n) {
		p.conjuncts = append(p.conjuncts, conjunct{n: c, allow: allow})
		l, r, notDistinct := equalitySides(c)
		if l == nil {
			continue
		}
		lk, lcol := p.resolve(l)
		rk, rcol := p.resolve(r)
		switch {
		case lcol && rcol:
			if !notDistinct {
				p.equate(lk, rk, allow)
			}
		case lcol && p.isKnown(r) && !nullConst(r) && (!notDistinct || p.notNull(lk)): // col = NULL is never true: it fixes nothing
			p.fix(lk, allow)
		case rcol && p.isKnown(l) && !nullConst(l) && (!notDistinct || p.notNull(rk)):
			p.fix(rk, allow)
		}
	}
}

// containment reads `col @> point` / `point <@ col` where col is a column of this level
// and the point is of a non-range type (a range on the point side could be empty, which
// every range contains).
func (p *prover) containment(c *pgparse.Node) (colKey, *pgparse.Node, bool) {
	x := c.GetAExpr()
	if x == nil || x.Kind != pgparse.A_Expr_Kind_AEXPR_OP || x.Lexpr == nil || x.Rexpr == nil {
		return colKey{}, nil, false
	}
	parts := strs(x.Name)
	var col, point *pgparse.Node
	switch parts[len(parts)-1] {
	case "@>":
		col, point = x.Lexpr, x.Rexpr
	case "<@":
		col, point = x.Rexpr, x.Lexpr
	default:
		return colKey{}, nil, false
	}
	k, ok := p.resolve(col)
	if !ok || !p.scalarTyped(point) {
		return colKey{}, nil, false
	}
	return k, point, true
}

// scalarTyped reports whether n's type is visibly not a range or multirange: a cast to
// such a type, or a column of one. A bare parameter or literal could be a range.
func (p *prover) scalarTyped(n *pgparse.Node) bool {
	var oid catalog.OID
	switch {
	case n.GetTypeCast() != nil:
		t, err := p.a.s.ResolveType(n.GetTypeCast().TypeName)
		if err != nil {
			return false
		}
		oid = t.OID
	default:
		k, ok := p.resolve(n)
		if !ok {
			return false
		}
		oid = k.r.cols[k.i].typ.OID
	}
	t := p.a.typ(p.a.baseType(oid))
	return t != nil && t.Kind != 'r' && t.Kind != 'm'
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

func conjuncts(n *pgparse.Node) []*pgparse.Node {
	if n == nil {
		return nil
	}
	if b := n.GetBoolExpr(); b != nil && b.Boolop == pgparse.BoolExprType_AND_EXPR {
		var out []*pgparse.Node
		for _, arg := range b.Args {
			out = append(out, conjuncts(arg)...)
		}
		return out
	}
	return []*pgparse.Node{n}
}

// equalitySides returns the operands of `x = y` (also `x IN (y)` with a single item, and
// `x = ANY(ARRAY[y])` with a single-element array literal -- PostgreSQL never returns more
// than one match for a singleton array any more than for `x = y` itself). notDistinct
// reports whether n is `x IS NOT DISTINCT FROM y`: unlike `=`, that predicate also matches
// a NULL x against a NULL y, so a caller may only treat it as pinning when it also knows y
// cannot be NULL, or x cannot be NULL.
func equalitySides(n *pgparse.Node) (l, r *pgparse.Node, notDistinct bool) {
	x := n.GetAExpr()
	if x == nil || x.Lexpr == nil {
		return nil, nil, false
	}
	switch x.Kind {
	case pgparse.A_Expr_Kind_AEXPR_OP:
		if parts := strs(x.Name); len(parts) > 0 && parts[len(parts)-1] == "=" {
			return x.Lexpr, x.Rexpr, false
		}
	case pgparse.A_Expr_Kind_AEXPR_OP_ANY:
		if parts := strs(x.Name); len(parts) > 0 && parts[len(parts)-1] == "=" {
			if arr := x.Rexpr.GetAArrayExpr(); arr != nil && len(arr.Elements) == 1 {
				return x.Lexpr, arr.Elements[0], false
			}
		}
	case pgparse.A_Expr_Kind_AEXPR_NOT_DISTINCT:
		return x.Lexpr, x.Rexpr, true
	case pgparse.A_Expr_Kind_AEXPR_IN:
		if items := x.Rexpr.GetList().GetItems(); len(items) == 1 {
			return x.Lexpr, items[0], false
		}
	}
	return nil, nil, false
}

// stripCasts unwraps TypeCast nodes to the underlying expression: a GROUP BY item that
// casts a column (or an alias for that cast, already resolved to the cast by groupExpr)
// names the same group as the column itself, since every row's cast is a deterministic
// function of that one value.
func stripCasts(n *pgparse.Node) *pgparse.Node {
	for {
		tc := n.GetTypeCast()
		if tc == nil {
			return n
		}
		n = tc.Arg
	}
}

// notNull reports whether k's column is proven never NULL (the table's own NOT NULL, or a
// NOT NULL domain -- see rte construction in scope.go): the only case where
// `col IS NOT DISTINCT FROM v` behaves like `col = v` regardless of whether v itself is
// NULL (both then agree the row does not match).
func (p *prover) notNull(k colKey) bool {
	return k.i < len(k.r.cols) && !k.r.cols[k.i].nullable
}

// resolve maps a column reference to a leaf column of this level; col is false for
// anything else (a known value, an outer reference, an expression).
func (p *prover) resolve(n *pgparse.Node) (colKey, bool) {
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
func (p *prover) outputKeys(targets []*pgparse.Node) []*colKey {
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
func (p *prover) isKnown(n *pgparse.Node) bool {
	switch v := n.Node.(type) {
	case *pgparse.Node_AConst, *pgparse.Node_ParamRef:
		return true
	case *pgparse.Node_TypeCast:
		return p.isKnown(v.TypeCast.Arg)
	case *pgparse.Node_ColumnRef:
		if _, ok := p.resolve(n); ok {
			return false
		}
		// an outer reference is a constant for this level
		return p.sc.parent != nil && p.resolvesOutside(v.ColumnRef)
	case *pgparse.Node_SubLink:
		return v.SubLink.SubLinkType == pgparse.SubLinkType_EXPR_SUBLINK && !p.correlated(v.SubLink.Subselect)
	case *pgparse.Node_AExpr:
		x := v.AExpr
		return x.Kind == pgparse.A_Expr_Kind_AEXPR_OP && (x.Lexpr == nil || p.isKnown(x.Lexpr)) && p.isKnown(x.Rexpr)
	case *pgparse.Node_CoalesceExpr:
		for _, arg := range v.CoalesceExpr.Args {
			if !p.isKnown(arg) {
				return false
			}
		}
		return true
	case *pgparse.Node_FuncCall:
		// a stable / immutable function of known values has one value per query;
		// volatile ones (random(), nextval()) are evaluated per row
		f := v.FuncCall
		vol, ok := p.a.funcVolatility[f]
		if !ok || vol == 'v' || p.a.funcRetSet[f] || f.AggStar || f.Over != nil || p.a.isAggregateName(strs(f.Funcname)) {
			return false // a set-returning function yields a row per element, not one value
		}
		for _, arg := range f.Args {
			if !p.isKnown(arg) {
				return false
			}
		}
		return true
	}
	return false
}

func (p *prover) resolvesOutside(cr *pgparse.ColumnRef) bool {
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
func (p *prover) correlated(sub *pgparse.Node) bool {
	found := false
	inner := p.a.fromColumns(sub.GetSelectStmt())
	schema.WalkNodes(sub, func(n *pgparse.Node) {
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
func (a *analyzer) fromColumns(sel *pgparse.SelectStmt) map[string]bool {
	out := map[string]bool{}
	if sel == nil {
		return out
	}
	var walk func(n *pgparse.Node)
	walk = func(n *pgparse.Node) {
		switch v := n.Node.(type) {
		case *pgparse.Node_RangeVar:
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
		case *pgparse.Node_JoinExpr:
			walk(v.JoinExpr.Larg)
			walk(v.JoinExpr.Rarg)
		}
	}
	for _, f := range sel.FromClause {
		walk(f)
	}
	return out
}

func constText(c *pgparse.A_Const) string {
	if c.Isnull {
		return "NULL"
	}
	switch v := c.Val.(type) {
	case *pgparse.A_Const_Ival:
		return fmt.Sprint("i", v.Ival.GetIval())
	case *pgparse.A_Const_Fval:
		return "f" + v.Fval.GetFval()
	case *pgparse.A_Const_Sval:
		return "s" + v.Sval.GetSval()
	case *pgparse.A_Const_Boolval:
		return fmt.Sprint("b", v.Boolval.GetBoolval())
	case *pgparse.A_Const_Bsval:
		return "x" + v.Bsval.GetBsval()
	}
	return "?"
}

// constInt returns the value of an integer constant node.
func constInt(n *pgparse.Node) (int32, bool) {
	c := n.GetAConst()
	if c == nil {
		return 0, false
	}
	if v, ok := c.Val.(*pgparse.A_Const_Ival); ok {
		return v.Ival.GetIval(), true
	}
	return 0, false
}

// recordFixed notes which table columns of this level are pinned to a known value by
// the predicate (WHERE plus join conditions), for Result.Fixed, checks the visibility
// policies and the plan advisories, and refines nullability from the predicate. Inside a
// view body only the nullability refinement applies (the view's own columns follow its
// WHERE; the fixed columns and the policies are the reader's concern). A RETURNING list
// sees the rows just written: the policies were checked on the statement's own WHERE, or
// do not apply to an INSERT.
func (a *analyzer) recordFixed(sc *scope, where *pgparse.Node) {
	p := a.newProver(sc, where)
	a.recordFacts(p, sc, loc(where))
	if a.inView == 0 {
		for k := range p.known {
			if k.r.rel != nil {
				a.fixed = append(a.fixed, Source{Table: k.r.rel.FullName(), Column: k.r.cols[k.i].name, NotNull: k.r.cols[k.i].src != nil && k.r.cols[k.i].src.NotNull})
			}
		}
		if !a.inReturning {
			a.advisePlans(p)
		}
	}
	a.rejectNulls(p)
}

// rejectNulls refines nullability from the predicate: a column tested IS NOT NULL, or
// compared with a strict operator, in WHERE or an inner join's ON cannot be NULL in the
// rows that survive. (An outer join's ON does not count: the join reintroduces NULLs.)
func (a *analyzer) rejectNulls(p *prover) {
	for _, k := range a.nullRejected(p) {
		k.r.cols[k.i].nullable = false
		if k.r.outerNullable {
			// the null-extended rows of an outer join are gone: base NOT NULL columns hold again
			for i := range k.r.cols {
				if src := k.r.cols[i].src; src != nil && src.NotNull {
					k.r.cols[i].nullable = false
				}
			}
		}
	}
}

// nullRejected lists the columns the prover's unconditional conjuncts prove non-NULL.
func (a *analyzer) nullRejected(p *prover) []colKey {
	var out []colKey
	for _, c := range p.conjuncts {
		if c.allow != nil {
			continue
		}
		var operands []*pgparse.Node
		switch v := c.n.Node.(type) {
		case *pgparse.Node_NullTest:
			if v.NullTest.Nulltesttype == pgparse.NullTestType_IS_NOT_NULL {
				operands = []*pgparse.Node{v.NullTest.Arg}
			}
		case *pgparse.Node_AExpr:
			switch v.AExpr.Kind {
			case pgparse.A_Expr_Kind_AEXPR_OP, pgparse.A_Expr_Kind_AEXPR_LIKE, pgparse.A_Expr_Kind_AEXPR_ILIKE,
				pgparse.A_Expr_Kind_AEXPR_BETWEEN, pgparse.A_Expr_Kind_AEXPR_IN, pgparse.A_Expr_Kind_AEXPR_OP_ANY:
				// strict operators: NULL operands yield NULL, which WHERE rejects (IS DISTINCT FROM is not strict)
				if v.AExpr.Lexpr != nil {
					operands = append(operands, v.AExpr.Lexpr)
				}
				if v.AExpr.Kind == pgparse.A_Expr_Kind_AEXPR_OP {
					operands = append(operands, v.AExpr.Rexpr)
				}
			}
		}
		for _, o := range operands {
			if k, ok := p.resolve(o); ok {
				out = append(out, k)
			}
		}
	}
	return out
}

// underCondition analyzes n as if cond held: the columns cond proves non-NULL are not
// nullable while n is typed (CASE WHEN x IS NOT NULL THEN x ...).
func (a *analyzer) underCondition(cond, n *pgparse.Node, sc *scope) (*expr, *Error) {
	p := a.newProverFor(sc)
	for _, it := range sc.items {
		p.addItem(it)
	}
	p.addQuals(cond, nil)
	var restore []colKey
	for _, k := range a.nullRejected(p) {
		if k.r.cols[k.i].nullable {
			k.r.cols[k.i].nullable = false
			restore = append(restore, k)
		}
	}
	e, err := a.analyzeExpr(n, sc)
	for _, k := range restore {
		k.r.cols[k.i].nullable = true
	}
	return e, err
}

// advisePlans emits the structural performance advisories (no EXPLAIN, no statistics):
// a table predicate no index leads with, and a view predicate the planner cannot push
// into the view (LIMIT / OFFSET / set operation / window function, or a non-grouping
// column of a GROUP BY view), so the view is computed in full before filtering.
func (a *analyzer) advisePlans(p *prover) {
	for _, l := range p.leaves {
		cols := p.predicateColumns(l)
		if len(cols) == 0 {
			continue
		}
		switch {
		case l.rel != nil:
			if !a.indexLeads(l.rel, cols) {
				a.note(noteNoIndex, p.firstPredicatePos(l), "no index on "+l.rel.Name+" leads with any of ("+strings.Join(cols, ", ")+"): this predicate scans the whole table")
			}
		case l.sub != nil && l.sub.what == "view":
			if why := a.pushdownBlocker(l, cols); why != "" {
				a.note(noteViewPushdown, p.firstPredicatePos(l), "predicate on view "+l.alias+" ("+strings.Join(cols, ", ")+") is not pushed into the view: "+why+"; the view is computed in full, then filtered")
			}
		}
	}
}

// predicateColumns lists the leaf's columns that predicates restricting it compare or test.
func (p *prover) predicateColumns(l *rte) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range p.conjuncts {
		if c.allow != nil && !c.allow[l] {
			continue
		}
		var sides []*pgparse.Node
		switch v := c.n.Node.(type) {
		case *pgparse.Node_AExpr:
			sides = []*pgparse.Node{v.AExpr.Lexpr, v.AExpr.Rexpr}
		case *pgparse.Node_NullTest:
			sides = []*pgparse.Node{v.NullTest.Arg}
		case *pgparse.Node_BooleanTest:
			sides = []*pgparse.Node{v.BooleanTest.Arg}
		}
		for _, sd := range sides {
			if sd == nil {
				continue
			}
			if k, ok := p.resolve(sd); ok && k.r == l && !seen[l.cols[k.i].name] {
				seen[l.cols[k.i].name] = true
				out = append(out, l.cols[k.i].name)
			}
		}
	}
	return out
}

func (p *prover) firstPredicatePos(l *rte) int32 {
	for _, c := range p.conjuncts {
		if c.allow == nil || c.allow[l] {
			return loc(c.n)
		}
	}
	return -1
}

// indexLeads reports whether some index or key of rel starts with one of the columns.
func (a *analyzer) indexLeads(rel *schema.Relation, cols []string) bool {
	leads := map[string]bool{}
	for _, idx := range rel.Indexes {
		if len(idx.Columns) > 0 {
			leads[idx.Columns[0]] = true
		}
	}
	for _, con := range rel.Constraints {
		if (con.Kind == schema.PrimaryKey || con.Kind == schema.Unique) && len(con.Columns) > 0 {
			leads[con.Columns[0]] = true
		}
	}
	for _, c := range cols {
		if leads[c] {
			return true
		}
	}
	return false
}

// pushdownBlocker says why a predicate on the view's columns cannot move inside it.
func (a *analyzer) pushdownBlocker(l *rte, cols []string) string {
	sel := l.sub.sel
	switch {
	case sel.Op != pgparse.SetOperation_SETOP_NONE && sel.Op != pgparse.SetOperation_SET_OPERATION_UNDEFINED:
		return "the view is a set operation"
	case sel.LimitCount != nil || sel.LimitOffset != nil:
		return "the view has LIMIT / OFFSET"
	}
	for _, tn := range sel.TargetList {
		hasWindow := false
		schema.WalkNodes(tn, func(n *pgparse.Node) {
			if f := n.GetFuncCall(); f != nil && f.Over != nil {
				hasWindow = true
			}
		})
		if hasWindow {
			return "the view has a window function"
		}
	}
	if len(sel.GroupClause) == 0 {
		return ""
	}
	groups := map[string]bool{}
	for _, g := range sel.GroupClause {
		if g.GetGroupingSet() != nil {
			return "the view uses GROUPING SETS"
		}
		groups[deparse(a.groupExpr(g, sel, l.sub.sc, l.cols))] = true
	}
	for _, c := range cols {
		for i, vc := range l.cols {
			if vc.name != c || i >= len(sel.TargetList) {
				continue
			}
			if !groups[deparse(sel.TargetList[i].GetResTarget().GetVal())] {
				return c + " is an aggregate, not a grouping column"
			}
		}
	}
	return ""
}
