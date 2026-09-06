package analyze

import (
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// selectStmt analyzes a SELECT (incl. set operations and VALUES) and returns its output columns.
func (a *analyzer) selectStmt(sel *pg_query.SelectStmt, sc *scope) ([]rteCol, *Error) {
	keepUnknown := a.keepUnknown
	a.keepUnknown = false
	// a subquery is its own level for aggregate nesting
	savedAggArgs := a.inAggArgs
	a.inAggArgs = 0
	defer func() { a.inAggArgs = savedAggArgs }()
	if sel.WithClause != nil {
		if err := a.withClause(sel.WithClause, sc); err != nil {
			return nil, err
		}
	}
	if sel.Op != pg_query.SetOperation_SETOP_NONE && sel.Op != pg_query.SetOperation_SET_OPERATION_UNDEFINED {
		return a.setOp(sel, sc)
	}
	if len(sel.ValuesLists) > 0 {
		return a.values(sel.ValuesLists, sc)
	}
	// FROM
	for _, item := range sel.FromClause {
		r, err := a.fromItem(item, sc)
		if err != nil {
			return nil, err
		}
		sc.items = append(sc.items, r)
	}
	// WINDOW clause
	for _, wn := range sel.WindowClause {
		wd := wn.GetWindowDef()
		if wd == nil {
			continue
		}
		if sc.windows == nil {
			sc.windows = map[string]*pg_query.WindowDef{}
		}
		if _, dup := sc.windows[wd.Name]; dup {
			return nil, errAt(codeWindowingError, wd.Location, "window %q is already defined", wd.Name)
		}
		if wd.Refname != "" {
			if _, ok := sc.windows[wd.Refname]; !ok {
				return nil, errAt(codeUndefinedObject, wd.Location, "window %q does not exist", wd.Refname)
			}
		}
		sc.windows[wd.Name] = wd
	}

	// WHERE / GROUP BY / HAVING
	if err := a.boolClause(sel.WhereClause, sc, "WHERE"); err != nil {
		return nil, err
	}
	a.recordFixed(sc, sel.WhereClause)
	// target list
	var cols []rteCol
	for _, tn := range sel.TargetList {
		t := tn.GetResTarget()
		if cr := t.Val.GetColumnRef(); cr != nil && isStar(cr) {
			expanded, err := a.expandStar(cr, sc)
			if err != nil {
				return nil, err
			}
			cols = append(cols, expanded...)
			continue
		}
		if ind := t.Val.GetAIndirection(); ind != nil && len(ind.Indirection) > 0 && ind.Indirection[len(ind.Indirection)-1].GetAStar() != nil {
			// (composite expression).* expands to the row type's columns
			expanded, err := a.expandCompositeStar(ind, sc)
			if err != nil {
				return nil, err
			}
			cols = append(cols, expanded...)
			continue
		}
		e, err := a.analyzeExpr(t.Val, sc)
		if err != nil {
			return nil, err
		}
		// unresolved unknown in an output column becomes text (INSERT ... SELECT keeps it
		// for the target column to type)
		if e.oid() == catalog.Unknown && !keepUnknown {
			if err := a.bind(e, catalog.Text, t.Location); err != nil {
				return nil, err
			}
		}
		name := t.Name
		if name == "" {
			name = a.figureColname(t.Val)
		}
		cols = append(cols, rteCol{name: name, typ: e.typ, nullable: e.nullable, src: e.src, lit: isLit(e), fields: e.fields, coll: e.coll})
	}
	for _, g := range groupingLeaves(sel.GroupClause) {
		if err := a.orderOrGroupItem(g, sc, cols, "GROUP BY"); err != nil {
			return nil, err
		}
	}
	if hasGroupingSets(sel.GroupClause) {
		// GROUPING SETS / ROLLUP / CUBE: a grouped column is NULL in the sets it is not part
		// of, so every output column is nullable except count(...), which is never NULL
		for i, tn := range sel.TargetList {
			if i >= len(cols) {
				break
			}
			if fc := tn.GetResTarget().GetVal().GetFuncCall(); fc != nil && strings.HasPrefix(strs(fc.Funcname)[len(fc.Funcname)-1], "count") {
				continue
			}
			cols[i].nullable = true
		}
	}
	if err := a.boolClause(sel.HavingClause, sc, "HAVING"); err != nil {
		return nil, err
	}
	for _, s := range sel.SortClause {
		if err := a.orderOrGroupItem(s.GetSortBy().GetNode(), sc, cols, "ORDER BY"); err != nil {
			return nil, err
		}
		a.noteEnumSort(s.GetSortBy().GetNode(), sc, cols)
	}
	for _, d := range sel.DistinctClause {
		if d.Node == nil {
			// plain DISTINCT compares every output column
			for _, c := range cols {
				a.noteCollConflict(c.coll, loc(d), "DISTINCT")
			}
			continue
		}
		if err := a.orderOrGroupItem(d, sc, cols, "DISTINCT ON"); err != nil {
			return nil, err
		}
	}
	if err := a.checkGrouping(sel, sc, cols); err != nil {
		return nil, err
	}
	for _, lim := range []*pg_query.Node{sel.LimitCount, sel.LimitOffset} {
		if lim == nil {
			continue
		}
		e, err := a.analyzeExpr(lim, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Int8, loc(lim)); err != nil {
			return nil, err
		}
		if !a.canCoerce(e.oid(), catalog.Int8, implicitCoercion) {
			return nil, errAt(codeDatatypeMismatch, loc(lim), "argument of LIMIT must be type bigint, not type %s", a.s.Types.Format(e.typ))
		}
	}
	return cols, nil
}

// orderOrGroupItem types an ORDER BY / GROUP BY item; a bare integer constant is an
// output-column ordinal. Sorting or grouping compares values, so an indeterminate
// collation is noted here (what names the clause).
func (a *analyzer) orderOrGroupItem(n *pg_query.Node, sc *scope, cols []rteCol, what string) *Error {
	if c := n.GetAConst(); c != nil {
		if iv, ok := c.Val.(*pg_query.A_Const_Ival); ok {
			if i := int(iv.Ival.Ival); i >= 1 && i <= len(cols) {
				a.noteCollConflict(cols[i-1].coll, loc(n), what)
			}
			return nil
		}
	}
	// an unqualified name may refer to an output column alias first
	if cr := n.GetColumnRef(); cr != nil && len(cr.Fields) == 1 {
		name := cr.Fields[0].GetString_().GetSval()
		for _, c := range cols {
			if c.name == name {
				a.noteCollConflict(c.coll, loc(n), what)
				return nil
			}
		}
	}
	e, err := a.analyzeExpr(n, sc)
	if err != nil {
		return err
	}
	a.noteCollConflict(e.coll, loc(n), what)
	return nil
}

func (a *analyzer) boolClause(n *pg_query.Node, sc *scope, what string) *Error {
	if n == nil {
		return nil
	}
	if what == "WHERE" || what == "ON" {
		where := "WHERE"
		if what == "ON" {
			where = "JOIN conditions"
		}
		if agg := a.aggregateIn(n); agg != nil && !a.outerLevelAggregate(agg, sc) {
			return errAt(codeGroupingError, agg.Location, "aggregate functions are not allowed in %s", where)
		}
		if w := windowIn(n); w != nil {
			return errAt(codeWindowingError, w.Location, "window functions are not allowed in %s", where)
		}
	}
	e, err := a.analyzeExpr(n, sc)
	if err != nil {
		return err
	}
	if err := a.bind(e, catalog.Bool, loc(n)); err != nil {
		return err
	}
	if !a.canCoerce(e.oid(), catalog.Bool, implicitCoercion) {
		return errAt(codeDatatypeMismatch, loc(n), "argument of %s must be type boolean, not type %s", what, a.s.Types.Format(e.typ))
	}
	return nil
}

func isStar(cr *pg_query.ColumnRef) bool {
	return len(cr.Fields) > 0 && cr.Fields[len(cr.Fields)-1].GetAStar() != nil
}

func (a *analyzer) expandStar(cr *pg_query.ColumnRef, sc *scope) ([]rteCol, *Error) {
	if len(cr.Fields) == 1 {
		var out []rteCol
		for _, it := range sc.items {
			out = append(out, it.expand()...)
		}
		if len(out) == 0 && len(sc.items) == 0 {
			return nil, errAt(codeSyntaxError, cr.Location, "SELECT * with no tables specified is not valid")
		}
		return out, nil
	}
	alias := cr.Fields[len(cr.Fields)-2].GetString_().GetSval()
	r := sc.wholeRow(alias)
	if r == nil {
		return nil, errAt(codeUndefinedTable, cr.Location, "missing FROM-clause entry for table %q", alias)
	}
	return r.cols, nil
}

func (a *analyzer) withClause(w *pg_query.WithClause, sc *scope) *Error {
	pending := w.Ctes
	for len(pending) > 0 {
		var deferred []*pg_query.Node
		var firstErr *Error
		progress := false
		for _, cn := range pending {
			c := cn.GetCommonTableExpr()
			err := a.defineCTE(c, w, sc)
			if err == nil {
				progress = true
				continue
			}
			// WITH RECURSIVE lets a query name a CTE defined further down: try it after the rest
			if w.Recursive && err.Code == codeUndefinedTable && a.namesLaterCTE(err, w) {
				deferred = append(deferred, cn)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			return err
		}
		if !progress {
			return firstErr
		}
		pending = deferred
	}
	return nil
}

// namesLaterCTE reports whether a "relation does not exist" error names one of w's CTEs.
func (a *analyzer) namesLaterCTE(err *Error, w *pg_query.WithClause) bool {
	for _, cn := range w.Ctes {
		if strings.Contains(err.Message, "\""+cn.GetCommonTableExpr().Ctename+"\"") {
			return true
		}
	}
	return false
}

// defineCTE analyzes one WITH item and exposes it in sc.
func (a *analyzer) defineCTE(c *pg_query.CommonTableExpr, w *pg_query.WithClause, sc *scope) *Error {
	sel := c.Ctequery.GetSelectStmt()
	if sel == nil {
		// data-modifying CTE: its RETURNING rows are the CTE's columns
		csc := newScope(sc)
		var cols []rteCol
		var err *Error
		switch st := c.Ctequery.Node.(type) {
		case *pg_query.Node_InsertStmt:
			cols, err = a.insertStmt(st.InsertStmt, csc)
		case *pg_query.Node_UpdateStmt:
			cols, err = a.updateStmt(st.UpdateStmt, csc)
		case *pg_query.Node_DeleteStmt:
			cols, err = a.deleteStmt(st.DeleteStmt, csc)
		case *pg_query.Node_MergeStmt:
			cols, err = a.mergeStmt(st.MergeStmt, csc)
		default:
			return errAt(codeFeatureNotSupported, c.Location, "unsupported CTE query %T", c.Ctequery.Node)
		}
		if w.Recursive && (&recursionWalker{a: a, name: c.Ctename}).mentions(c.Ctequery) {
			return errAt(codeInvalidRecursion, c.Location, "recursive query %q must not contain data-modifying statements", c.Ctename)
		}
		if err != nil {
			return err
		}
		a.dmlCTEs = append(a.dmlCTEs, c.Ctequery)
		sc.ctes[c.Ctename] = &cte{name: c.Ctename, cols: a.aliasCols(cols, c.Aliascolnames)}
		return nil
	}
	def := &cte{name: c.Ctename, recursive: w.Recursive}
	if w.Recursive && sel.Op != pg_query.SetOperation_SETOP_UNION && (&recursionWalker{a: a, name: c.Ctename}).mentions(c.Ctequery) {
		return errAt(codeInvalidRecursion, c.Location, "recursive query %q does not have the form non-recursive-term UNION [ALL] recursive-term", c.Ctename)
	}
	if w.Recursive && sel.Op == pg_query.SetOperation_SETOP_UNION {
		// a WITH on the whole recursive union is visible to both arms
		armSc := sc
		if sel.WithClause != nil {
			armSc = newScope(sc)
			if err := a.withClause(sel.WithClause, armSc); err != nil {
				return err
			}
		}
		// analyze the non-recursive term first (the CTE may not appear in it), then expose
		// the CTE with those types for the recursive term
		def.forbidden = true
		sc.ctes[c.Ctename] = def
		left, err := a.selectStmt(sel.Larg, newScope(armSc))
		if err != nil {
			return err
		}
		def.forbidden = false
		def.cols = a.aliasCols(left, c.Aliascolnames)
		if err := a.checkRecursiveTerm(c.Ctename, selNode(sel.Rarg)); err != nil {
			return err
		}
		// SEARCH / CYCLE columns are visible to the recursive term (WHERE NOT is_cycle)
		a.addSearchCycleCols(def, c, sc)
		right, err := a.selectStmt(sel.Rarg, newScope(armSc))
		if err != nil {
			return err
		}
		if len(right) != len(left) {
			return errAt(codeSyntaxError, c.Location, "each UNION query must have the same number of columns")
		}
		for i := range left {
			def.cols[i].nullable = def.cols[i].nullable || right[i].nullable
			def.cols[i].src = nil
		}
		return nil
	}
	csc := newScope(sc)
	cols, err := a.selectStmt(sel, csc)
	if err != nil {
		return err
	}
	def.cols = a.aliasCols(cols, c.Aliascolnames)
	def.sub = &subquery{what: "CTE", sel: sel, sc: csc}
	sc.ctes[c.Ctename] = def
	return nil
}

// addSearchCycleCols appends the columns a SEARCH / CYCLE clause adds to a recursive CTE.
func (a *analyzer) addSearchCycleCols(def *cte, c *pg_query.CommonTableExpr, sc *scope) {
	{
		if sc2 := c.SearchClause; sc2 != nil {
			// SEARCH DEPTH FIRST ... SET seq is a record[] path, BREADTH FIRST a record
			seq := ref(a.s.Types.ArrayOf(catalog.Record))
			if sc2.SearchBreadthFirst {
				seq = ref(catalog.Record)
			}
			def.cols = append(def.cols, rteCol{name: sc2.SearchSeqColumn, typ: seq})
		}
		if cy := c.CycleClause; cy != nil {
			mark := ref(catalog.Bool)
			if cy.CycleMarkValue != nil {
				if me, err := a.analyzeExpr(cy.CycleMarkValue, newScope(sc)); err == nil && me.oid() != catalog.Unknown {
					mark = me.typ
				}
			}
			def.cols = append(def.cols, rteCol{name: cy.CycleMarkColumn, typ: mark}, rteCol{name: cy.CyclePathColumn, typ: ref(a.s.Types.ArrayOf(catalog.Record))})
		}
	}
}

func (a *analyzer) aliasCols(cols []rteCol, aliases []*pg_query.Node) []rteCol {
	out := make([]rteCol, len(cols))
	copy(out, cols)
	for i, n := range aliases {
		if i < len(out) {
			out[i].name = n.GetString_().GetSval()
		}
	}
	return out
}

func (a *analyzer) setOp(sel *pg_query.SelectStmt, sc *scope) ([]rteCol, *Error) {
	// each arm keeps its unknown literals (select null, 42 union all select x, y): the
	// common type of the pair types them, text only when every arm is unknown
	a.keepUnknown = true
	left, err := a.selectStmt(sel.Larg, newScope(sc))
	if err != nil {
		return nil, err
	}
	a.keepUnknown = true
	right, err := a.selectStmt(sel.Rarg, newScope(sc))
	if err != nil {
		return nil, err
	}
	if len(left) != len(right) {
		return nil, errAt(codeSyntaxError, -1, "each %s query must have the same number of columns", setOpName(sel.Op))
	}
	out := make([]rteCol, len(left))
	for i := range left {
		l, r := left[i], right[i]
		t, ok := a.commonType([]catalog.OID{l.typ.OID, r.typ.OID})
		if !ok {
			return nil, errAt(codeDatatypeMismatch, -1, "%s types %s and %s cannot be matched", setOpName(sel.Op), a.s.Types.Format(l.typ), a.s.Types.Format(r.typ))
		}
		typmod := int32(-1)
		if l.typ.OID == r.typ.OID && l.typ.Typmod == r.typ.Typmod {
			typmod = l.typ.Typmod
		}
		t = a.domainUnify([]*expr{{typ: l.typ, lit: l.lit}, {typ: r.typ, lit: r.lit}}, t, -1, setOpName(sel.Op))
		coll, err := a.setOpColl(l.coll, r.coll, sel.Op, sel.All)
		if err != nil {
			return nil, err
		}
		out[i] = rteCol{name: l.name, typ: schema.TypeRef{OID: t, Typmod: typmod}, nullable: l.nullable || r.nullable, lit: l.lit && r.lit, coll: a.resultColl(coll, t)}
	}
	// ORDER BY / LIMIT on the whole set operation
	for _, lim := range []*pg_query.Node{sel.LimitCount, sel.LimitOffset} {
		if lim == nil {
			continue
		}
		e, err := a.analyzeExpr(lim, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Int8, loc(lim)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func setOpName(op pg_query.SetOperation) string {
	switch op {
	case pg_query.SetOperation_SETOP_INTERSECT:
		return "INTERSECT"
	case pg_query.SetOperation_SETOP_EXCEPT:
		return "EXCEPT"
	}
	return "UNION"
}

func (a *analyzer) values(lists []*pg_query.Node, sc *scope) ([]rteCol, *Error) {
	var rows [][]*expr
	for _, ln := range lists {
		row, err := a.analyzeList(ln.GetList().GetItems(), sc)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 && len(row) != len(rows[0]) {
			return nil, errAt(codeSyntaxError, -1, "VALUES lists must all be the same length")
		}
		rows = append(rows, row)
	}
	n := len(rows[0])
	out := make([]rteCol, n)
	for i := 0; i < n; i++ {
		col := make([]*expr, len(rows))
		for j := range rows {
			col[j] = rows[j][i]
		}
		t, coll, err := a.unify(col, loc(col[0].node), "VALUES")
		if err != nil {
			return nil, err
		}
		nullable := false
		for _, e := range col {
			nullable = nullable || e.nullable
		}
		out[i] = rteCol{name: "column" + strconv.Itoa(i+1), typ: t, nullable: nullable, coll: coll}
	}
	return out, nil
}

// fromItem builds the rte for one FROM entry.
func (a *analyzer) fromItem(n *pg_query.Node, sc *scope) (*rte, *Error) {
	switch v := n.Node.(type) {
	case *pg_query.Node_RangeVar:
		rv := v.RangeVar
		if rv.Schemaname == "" {
			if c := sc.findCTE(rv.Relname); c != nil {
				if c.forbidden {
					return nil, errAt(codeInvalidRecursion, rv.Location, "recursive reference to query %q must not appear within its non-recursive term", rv.Relname)
				}
				r := &rte{alias: rv.Relname, cols: append([]rteCol{}, c.cols...), sub: c.sub}
				if rv.Alias != nil {
					if rv.Alias.Aliasname != "" {
						r.alias = rv.Alias.Aliasname
					}
					if len(rv.Alias.Colnames) > len(r.cols) {
						return nil, errAt(codeInvalidColumnRef, rv.Location, "WITH query %q has %d columns available but %d columns specified", rv.Relname, len(r.cols), len(rv.Alias.Colnames))
					}
					for i, cn := range rv.Alias.Colnames {
						if i < len(r.cols) {
							r.cols[i].name = cn.GetString_().GetSval()
						}
					}
				}
				return r, nil
			}
		}
		rel := a.s.Relation(rv.Schemaname, rv.Relname)
		if rel == nil {
			return nil, errAt(codeUndefinedTable, rv.Location, "relation %q does not exist", qualName(rv))
		}
		return a.relationRTE(rel, rv.Alias, rv.Location)
	case *pg_query.Node_RangeSubselect:
		sub := v.RangeSubselect
		child := newScope(sc)
		if !sub.Lateral {
			// non-lateral subqueries cannot see sibling FROM items; they can see outer levels
			child = newScope(sc.parent)
			child.ctes = sc.ctes
			for s := sc; s != nil; s = s.parent {
				for k, c := range s.ctes {
					if _, ok := child.ctes[k]; !ok {
						child.ctes[k] = c
					}
				}
			}
		}
		cols, err := a.selectStmt(sub.Subquery.GetSelectStmt(), child)
		if err != nil {
			return nil, err
		}
		if sub.Alias == nil {
			sub.Alias = &pg_query.Alias{Aliasname: "unnamed_subquery"} // optional since PG 16
		}
		r := &rte{alias: sub.Alias.Aliasname, sub: &subquery{what: "subquery", sel: sub.Subquery.GetSelectStmt(), sc: child}}
		if len(sub.Alias.Colnames) > len(cols) {
			return nil, errAt(codeInvalidColumnRef, -1, "table %q has %d columns available but %d columns specified", sub.Alias.Aliasname, len(cols), len(sub.Alias.Colnames))
		}
		for i, c := range cols {
			// PG traces column origins through subqueries and CTEs (but not views)
			if i < len(sub.Alias.Colnames) {
				c.name = sub.Alias.Colnames[i].GetString_().GetSval()
			}
			r.cols = append(r.cols, c)
		}
		return r, nil
	case *pg_query.Node_RangeFunction:
		return a.rangeFunction(v.RangeFunction, sc)
	case *pg_query.Node_RangeTableFunc:
		return a.xmlTable(v.RangeTableFunc, sc)
	case *pg_query.Node_JoinExpr:
		r, err := a.joinExpr(v.JoinExpr, sc)
		if err != nil {
			return nil, err
		}
		if ua := v.JoinExpr.JoinUsingAlias; ua != nil {
			r.join.usingAlias = &rte{alias: ua.Aliasname, cols: r.join.usingCols}
		}
		if al := v.JoinExpr.Alias; al != nil {
			r.alias = al.Aliasname
			if n := len(r.expand()); len(al.Colnames) > n {
				return nil, errAt(codeInvalidColumnRef, -1, "join expression %q has %d columns available but %d columns specified", al.Aliasname, n, len(al.Colnames))
			}
			r.join.colAliases = strs(al.Colnames)
		}
		return r, nil
	case *pg_query.Node_JsonTable:
		return a.jsonTable(v.JsonTable, sc)
	case *pg_query.Node_RangeTableSample:
		ts := v.RangeTableSample
		r, err := a.fromItem(ts.Relation, sc)
		if err != nil {
			return nil, err
		}
		if r.rel == nil || (r.rel.Kind != schema.Table && r.rel.Kind != schema.MatView) {
			return nil, errAt(codeFeatureNotSupported, loc(ts.Relation), "TABLESAMPLE clause can only be applied to tables and materialized views")
		}
		// the built-in methods take a real (percentage / limit); REPEATABLE takes a double
		for i, arg := range append(append([]*pg_query.Node{}, ts.Args...), ts.Repeatable) {
			if arg == nil {
				continue
			}
			want := catalog.Float4
			if i == len(ts.Args) {
				want = catalog.Float8
			}
			e, err := a.analyzeExpr(arg, sc)
			if err != nil {
				return nil, err
			}
			if err := a.bind(e, want, loc(arg)); err != nil {
				return nil, err
			}
			if !a.canCoerce(e.oid(), want, assignmentCoercion) {
				return nil, errAt(codeDatatypeMismatch, loc(arg), "TABLESAMPLE argument must be of type %s, not type %s", a.s.Types.Format(ref(want)), a.s.Types.Format(e.typ))
			}
		}
		r.single = false
		return r, nil
	}
	return nil, errAt(codeFeatureNotSupported, -1, "unsupported FROM item %T", n.Node)
}

func qualName(rv *pg_query.RangeVar) string {
	if rv.Schemaname != "" {
		return rv.Schemaname + "." + rv.Relname
	}
	return rv.Relname
}

func (a *analyzer) rangeFunction(rf *pg_query.RangeFunction, sc *scope) (*rte, *Error) {
	if len(rf.Functions) > 1 {
		// ROWS FROM (f1(), f2() AS (...)): the functions run in lockstep, columns side by side
		r := &rte{alias: "rows_from"}
		for _, fn := range rf.Functions {
			items := fn.GetList().GetItems()
			one := &pg_query.RangeFunction{Functions: []*pg_query.Node{fn}, Ordinality: false}
			if len(items) > 1 {
				one.Coldeflist = items[1].GetList().GetItems() // per-function column definition list
			}
			sub, err := a.rangeFunction(one, sc)
			if err != nil {
				return nil, err
			}
			r.cols = append(r.cols, sub.cols...)
			r.single = r.single && sub.single
		}
		if rf.Alias != nil {
			if rf.Alias.Aliasname != "" {
				r.alias = rf.Alias.Aliasname
			}
			for i, cn := range rf.Alias.Colnames {
				if i < len(r.cols) {
					r.cols[i].name = cn.GetString_().GetSval()
				}
			}
		}
		if rf.Ordinality {
			r.cols = append(r.cols, rteCol{name: "ordinality", typ: ref(catalog.Int8)})
			applyColnames(r.cols, rf.Alias)
		}
		return r, nil
	}
	items := rf.Functions[0].GetList().GetItems()
	fnode := items[0]
	fc := fnode.GetFuncCall()
	if fc != nil && len(fc.Args) > 1 && len(rf.Coldeflist) == 0 && (len(items) == 1 || len(items[1].GetList().GetItems()) == 0) {
		if names := strs(fc.Funcname); names[len(names)-1] == "unnest" && (len(names) == 1 || names[0] == "pg_catalog") {
			// unnest(a, b, ...) in FROM is shorthand for ROWS FROM (unnest(a), unnest(b), ...)
			multi := &pg_query.RangeFunction{Alias: rf.Alias, Ordinality: rf.Ordinality, Lateral: rf.Lateral}
			for _, arg := range fc.Args {
				one := &pg_query.FuncCall{Funcname: fc.Funcname, Args: []*pg_query.Node{arg}, Location: fc.Location}
				multi.Functions = append(multi.Functions, &pg_query.Node{Node: &pg_query.Node_List{List: &pg_query.List{Items: []*pg_query.Node{{Node: &pg_query.Node_FuncCall{FuncCall: one}}}}}})
			}
			return a.rangeFunction(multi, sc)
		}
	}
	r := &rte{}
	var cols []rteCol
	scalar, outName := false, "" // a scalar result: its one column is named by the OUT parameter, the alias, or the function
	coldefs := rf.Coldeflist
	if len(items) > 1 && len(coldefs) == 0 {
		coldefs = items[1].GetList().GetItems()
	}
	if fc == nil {
		// e.g. a bare column reference or sublink used as a function-in-FROM (unnest of array column etc.)
		e, err := a.analyzeExpr(fnode, sc)
		if err != nil {
			return nil, err
		}
		cols = []rteCol{{name: "?column?", typ: e.typ, nullable: true}}
	} else {
		e, err := a.funcCall(fc, sc)
		if err != nil {
			return nil, err
		}
		r.single = !a.lastFuncRetSet
		names := strs(fc.Funcname)
		fname := names[len(names)-1]
		r.alias = fname
		t := a.typ(e.typ.OID)
		scalar = !(t != nil && t.Kind == 'c' && t.RelID != 0) && e.typ.OID != catalog.Record
		outName = a.singleOutName()
		switch {
		case t != nil && t.Kind == 'c' && t.RelID != 0:
			rel := a.relByRowType(t.OID)
			r.rowType = t.OID
			for _, c := range rel.Columns {
				cols = append(cols, rteCol{name: c.Name, typ: c.Type, nullable: !c.NotNull})
			}
		case e.typ.OID == catalog.Record:
			// RETURNS TABLE / OUT params of a user function, or of a catalog function
			// (jsonb_each, json_each_text, ...)
			cols = a.outParamCols()
			if len(cols) == 0 && len(coldefs) == 0 {
				return nil, errAt(codeSyntaxError, fc.Location, "a column definition list is required for functions returning \"record\"")
			}
		default:
			cols = []rteCol{{name: fname, typ: e.typ, nullable: true}}
			if outName != "" {
				cols[0].name = outName
			}
		}
	}
	for _, cd := range coldefs {
		def := cd.GetColumnDef()
		tr, err := a.s.ResolveType(def.TypeName)
		if err != nil {
			return nil, errAt(codeUndefinedObject, def.Location, "%v", err)
		}
		cols = append(cols, rteCol{name: def.Colname, typ: tr, nullable: true})
	}
	if rf.Alias != nil {
		if rf.Alias.Aliasname != "" {
			r.alias = rf.Alias.Aliasname
		}
		if n := len(cols); len(rf.Alias.Colnames) > n && !rf.Ordinality {
			return nil, errAt(codeInvalidColumnRef, -1, "table %q has %d columns available but %d columns specified", r.alias, n, len(rf.Alias.Colnames))
		}
		for i, cn := range rf.Alias.Colnames {
			if i < len(cols) {
				cols[i].name = cn.GetString_().GetSval()
			}
		}
		if len(rf.Alias.Colnames) == 0 && len(coldefs) == 0 && len(cols) == 1 && scalar && outName == "" {
			// a scalar-returning function's alias names its single column too
			// (chooseScalarFunctionAlias: a named OUT parameter wins over the alias)
			cols[0].name = rf.Alias.Aliasname
		}
	}
	if rf.Ordinality {
		cols = append(cols, rteCol{name: "ordinality", typ: ref(catalog.Int8)})
		applyColnames(cols, rf.Alias)
	}
	r.cols = cols
	return r, nil
}

// applyColnames renames columns after an alias column list (AS t(a, b, c)).
func applyColnames(cols []rteCol, alias *pg_query.Alias) {
	if alias == nil {
		return
	}
	for i, cn := range alias.Colnames {
		if i < len(cols) {
			cols[i].name = cn.GetString_().GetSval()
		}
	}
}

func (a *analyzer) joinExpr(j *pg_query.JoinExpr, sc *scope) (*rte, *Error) {
	left, err := a.fromItem(j.Larg, sc)
	if err != nil {
		return nil, err
	}
	// the right side may be LATERAL and see the left side
	inner := newScope(sc)
	inner.items = []*rte{left}
	right, err := a.fromItem(j.Rarg, inner)
	if err != nil {
		return nil, err
	}
	// outer-join nullability
	switch j.Jointype {
	case pg_query.JoinType_JOIN_LEFT:
		markNullable(right)
	case pg_query.JoinType_JOIN_RIGHT:
		markNullable(left)
	case pg_query.JoinType_JOIN_FULL:
		markNullable(left)
		markNullable(right)
	}
	r := &rte{join: &joinInfo{left: left, right: right, jointype: j.Jointype, quals: j.Quals}}
	// qualification / USING / NATURAL are checked in a scope holding both sides
	both := newScope(sc)
	both.items = []*rte{left, right}
	var using []string
	if j.IsNatural {
		lnames := map[string]bool{}
		for _, c := range left.expand() {
			lnames[c.name] = true
		}
		for _, c := range right.expand() {
			if lnames[c.name] {
				using = append(using, c.name)
			}
		}
	} else {
		using = strs(j.UsingClause)
	}
	for _, u := range using {
		lc := left.find(u)
		rc := right.find(u)
		if len(lc) != 1 {
			return nil, errAt(codeUndefinedColumn, -1, "column %q specified in USING clause does not exist in left table", u)
		}
		if len(rc) != 1 {
			return nil, errAt(codeUndefinedColumn, -1, "column %q specified in USING clause does not exist in right table", u)
		}
		l, rr := &expr{typ: lc[0].typ, nullable: lc[0].nullable}, &expr{typ: rc[0].typ, nullable: rc[0].nullable}
		if _, err := a.applyOperator("=", l, rr, -1, nil); err != nil {
			return nil, err
		}
		merged := lc[0]
		merged.nullable = lc[0].nullable && rc[0].nullable
		if j.Jointype == pg_query.JoinType_JOIN_FULL {
			merged.src = nil
		}
		if j.Jointype == pg_query.JoinType_JOIN_RIGHT {
			merged = rc[0]
		}
		if a.baseType(lc[0].typ.OID) != a.baseType(rc[0].typ.OID) {
			// sides of different types: the merged column is their common type, coerced
			ct, _, err := a.unify([]*expr{l, rr}, -1, "JOIN/USING")
			if err != nil {
				return nil, err
			}
			merged.typ = ct
			merged.src = nil
		}
		r.join.usingCols = append(r.join.usingCols, merged)
	}
	r.join.using = using
	if j.Quals != nil {
		if err := a.boolClause(j.Quals, both, "JOIN/ON"); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func markNullable(r *rte) {
	if r.join != nil {
		markNullable(r.join.left)
		markNullable(r.join.right)
		for i := range r.join.usingCols {
			r.join.usingCols[i].nullable = true
		}
		return
	}
	r.outerNullable = true
	for i := range r.cols {
		r.cols[i].nullable = true
	}
}

// --- DML -------------------------------------------------------------------

func (a *analyzer) targetRTE(rv *pg_query.RangeVar, sc *scope) (*schema.Relation, *rte, *Error) {
	rel := a.s.Relation(rv.Schemaname, rv.Relname)
	if rel == nil {
		return nil, nil, errAt(codeUndefinedTable, rv.Location, "relation %q does not exist", qualName(rv))
	}
	r, err := a.relationRTE(rel, rv.Alias, rv.Location)
	if err != nil {
		return nil, nil, err
	}
	return rel, r, nil
}

// assign coerces a value expression to a target column in assignment context.
func (a *analyzer) assign(e *expr, col *schema.Column, relName string, at int32) *Error {
	if err := a.checkViewColumnWritable(col, at); err != nil {
		return err
	}
	if col.Generated != nil && (e.node == nil || e.node.GetSetToDefault() == nil) {
		return errAt(codeGeneratedAlways, at, "cannot insert a non-DEFAULT value into column %q", col.Name)
	}
	a.assigned = append(a.assigned, assignment{rel: a.relByFullName(relName), col: col, e: e})
	if e.param > 0 {
		if _, done := a.paramSrc[e.param]; !done {
			a.paramSrc[e.param] = &Source{Table: relName, Column: col.Name, NotNull: col.NotNull, Assigned: true}
		}
	}
	if e.oid() == catalog.Unknown {
		if err := a.bind(e, col.Type.OID, at); err != nil {
			return err
		}
		if c := e.node.GetAConst(); c != nil {
			if sv, ok := c.Val.(*pg_query.A_Const_Sval); ok {
				return a.validateAssignLength(sv.Sval.GetSval(), col.Type, c.Location)
			}
		}
		return nil
	}
	if !a.canCoerce(e.oid(), col.Type.OID, assignmentCoercion) {
		if ct := a.typ(a.baseType(col.Type.OID)); e.oid() == catalog.Record && ct != nil && ct.Kind == 'c' {
			// row(...) into a composite column: coerce_record_to_complex, field by field
			if rel := relByRowType(a.s, ct.OID); rel != nil && (len(e.fields) == 0 || len(rel.Columns) == len(e.fields)) {
				return nil
			}
		}
		return errAt(codeDatatypeMismatch, at, "column %q is of type %s but expression is of type %s", col.Name, a.s.Types.Format(col.Type), a.s.Types.Format(e.typ))
	}
	a.domainAssign(e, col.Type.OID, relName, col.Name, at)
	return nil
}

func (a *analyzer) returning(list []*pg_query.Node, sc *scope) ([]rteCol, *Error) {
	if len(list) == 0 {
		return nil, nil
	}
	for _, n := range list {
		if w := windowIn(n.GetResTarget().GetVal()); w != nil {
			return nil, errAt(codeWindowingError, w.Location, "window functions are not allowed in RETURNING")
		}
	}
	sel := &pg_query.SelectStmt{TargetList: list}
	return a.selectStmt(sel, sc)
}

func (a *analyzer) insertStmt(ins *pg_query.InsertStmt, sc *scope) ([]rteCol, *Error) {
	if ins.WithClause != nil {
		if err := a.withClause(ins.WithClause, sc); err != nil {
			return nil, err
		}
	}
	rel, target, err := a.writeTarget(ins.Relation, sc, "insert into")
	if err != nil {
		return nil, err
	}
	var cols []*schema.Column
	if len(ins.Cols) == 0 {
		cols = rel.Columns
	} else {
		for _, cn := range ins.Cols {
			rt := cn.GetResTarget()
			c := rel.Column(rt.Name)
			if c == nil {
				return nil, errAt(codeUndefinedColumn, rt.Location, "column %q of relation %q does not exist", rt.Name, rel.Name)
			}
			if len(rt.Indirection) > 0 {
				var err *Error
				if c, err = a.indirectTarget(c, rt.Indirection, rt.Location); err != nil {
					return nil, err
				}
			}
			cols = append(cols, c)
		}
	}
	if ins.SelectStmt != nil {
		sel := ins.SelectStmt.GetSelectStmt()
		if len(sel.ValuesLists) > 0 && sel.Op == pg_query.SetOperation_SETOP_NONE && len(sel.FromClause) == 0 {
			// VALUES: coerce each expression directly to its column (assignment context)
			for _, ln := range sel.ValuesLists {
				items := ln.GetList().GetItems()
				if len(items) > len(cols) {
					return nil, errAt(codeSyntaxError, loc(items[len(cols)]), "INSERT has more expressions than target columns")
				}
				if len(items) < len(cols) && len(ins.Cols) > 0 {
					return nil, errAt(codeSyntaxError, -1, "INSERT has more target columns than expressions")
				}
				for i, it := range items {
					if cols[i].Identity == 'a' && it.GetSetToDefault() == nil && ins.Override == pg_query.OverridingKind_OVERRIDING_NOT_SET {
						return nil, errAt(codeGeneratedAlways, loc(it), "cannot insert a non-DEFAULT value into column %q", cols[i].Name)
					}
					e, err := a.analyzeExpr(it, sc)
					if err != nil {
						return nil, err
					}
					if err := a.assign(e, cols[i], rel.FullName(), loc(it)); err != nil {
						return nil, err
					}
				}
			}
		} else {
			a.insertSelScope = newScope(sc)
			a.keepUnknown = true
			src, err := a.selectStmt(sel, a.insertSelScope)
			a.keepUnknown = false
			if err != nil {
				return nil, err
			}
			if len(src) > len(cols) {
				return nil, errAt(codeSyntaxError, -1, "INSERT has more expressions than target columns")
			}
			for i := range src {
				if cols[i].Identity == 'a' && ins.Override == pg_query.OverridingKind_OVERRIDING_NOT_SET {
					return nil, errAt(codeGeneratedAlways, -1, "cannot insert a non-DEFAULT value into column %q", cols[i].Name)
				}
			}
			for i, c := range src {
				e := &expr{typ: c.typ, nullable: c.nullable}
				if err := a.assign(e, cols[i], rel.FullName(), -1); err != nil {
					return nil, err
				}
			}
		}
	}
	// ON CONFLICT
	inner := newScope(sc)
	inner.items = []*rte{target}
	if oc := ins.OnConflictClause; oc != nil {
		if oc.Infer != nil {
			for _, ie := range oc.Infer.IndexElems {
				if ex := ie.GetIndexElem().GetExpr(); ex != nil {
					if _, err := a.analyzeExpr(ex, inner); err != nil {
						return nil, err
					}
				}
			}
			if err := a.boolClause(oc.Infer.WhereClause, inner, "WHERE"); err != nil {
				return nil, err
			}
		}
		if oc.Action == pg_query.OnConflictAction_ONCONFLICT_UPDATE {
			if oc.Infer == nil {
				return nil, errAt(codeSyntaxError, oc.Location, "ON CONFLICT DO UPDATE requires inference specification or constraint name")
			}
			excluded := &rte{alias: "excluded", cols: append([]rteCol{}, target.cols...)}
			for i := range excluded.cols {
				excluded.cols[i].src = nil
				excluded.cols[i].nullable = true
			}
			upd := newScope(sc)
			upd.items = []*rte{target, excluded}
			if err := a.setClause(oc.TargetList, rel, upd); err != nil {
				return nil, err
			}
			if err := a.boolClause(oc.WhereClause, upd, "WHERE"); err != nil {
				return nil, err
			}
		}
	}
	return a.returning(ins.ReturningList, inner)
}

func (a *analyzer) setClause(targets []*pg_query.Node, rel *schema.Relation, sc *scope) *Error {
	// SET (a, b) = (x, y) / (SELECT ...): one source, analyzed once, one value per column
	sources := map[*pg_query.Node][]*expr{}
	assignedCols := map[string]bool{}
	for _, tn := range targets {
		t := tn.GetResTarget()
		col := rel.Column(t.Name)
		if col == nil {
			return errAt(codeUndefinedColumn, t.Location, "column %q of relation %q does not exist", t.Name, rel.Name)
		}
		if len(t.Indirection) == 0 {
			if assignedCols[t.Name] {
				return errAt(codeSyntaxError, t.Location, "multiple assignments to same column %q", t.Name)
			}
			assignedCols[t.Name] = true
		}
		if col.Identity == 'a' && t.Val.GetSetToDefault() == nil {
			return errAt(codeGeneratedAlways, t.Location, "column %q can only be updated to DEFAULT", col.Name)
		}
		if len(t.Indirection) > 0 {
			var err *Error
			if t.Val.GetSetToDefault() != nil {
				return errAt(codeFeatureNotSupported, t.Location, "cannot set an array element to DEFAULT")
			}
			if col, err = a.indirectTarget(col, t.Indirection, t.Location); err != nil {
				return err
			}
		}
		var e *expr
		if ma := t.Val.GetMultiAssignRef(); ma != nil {
			vals, ok := sources[ma.Source]
			if !ok {
				var err *Error
				if vals, err = a.multiAssignSource(ma.Source, int(ma.Ncolumns), sc); err != nil {
					return err
				}
				sources[ma.Source] = vals
			}
			e = vals[ma.Colno-1]
		} else {
			var err *Error
			if e, err = a.analyzeExpr(t.Val, sc); err != nil {
				return err
			}
		}
		if err := a.assign(e, col, rel.FullName(), loc(t.Val)); err != nil {
			return err
		}
	}
	return nil
}

// multiAssignSource types the right-hand side of SET (a, b, ...) = source: a row of
// expressions, or a subquery whose columns are taken as the values.
func (a *analyzer) multiAssignSource(src *pg_query.Node, n int, sc *scope) ([]*expr, *Error) {
	if row := src.GetRowExpr(); row != nil {
		if len(row.Args) == 1 && row.Args[0].GetColumnRef() != nil && len(row.Args[0].GetColumnRef().Fields) > 0 &&
			row.Args[0].GetColumnRef().Fields[len(row.Args[0].GetColumnRef().Fields)-1].GetAStar() != nil {
			// ROW(t.*): the row's fields are the values
			e, err := a.analyzeExpr(row.Args[0], sc)
			if err != nil {
				return nil, err
			}
			if len(e.fields) != n {
				return nil, errAt(codeSyntaxError, loc(src), "number of columns does not match number of values")
			}
			var out []*expr
			for _, f := range e.fields {
				out = append(out, &expr{typ: f.typ, nullable: f.nullable, src: f.src})
			}
			return out, nil
		}
		if len(row.Args) != n {
			return nil, errAt(codeSyntaxError, loc(src), "number of columns does not match number of values")
		}
		return a.analyzeList(row.Args, sc)
	}
	if sl := src.GetSubLink(); sl != nil {
		sel := sl.Subselect.GetSelectStmt()
		if sel == nil {
			return nil, errAt(codeFeatureNotSupported, sl.Location, "unsupported subquery")
		}
		cols, err := a.selectStmt(sel, newScope(sc))
		if err != nil {
			return nil, err
		}
		if len(cols) != n {
			return nil, errAt(codeSyntaxError, sl.Location, "number of columns does not match number of values")
		}
		out := make([]*expr, len(cols))
		for i, c := range cols {
			out[i] = &expr{typ: c.typ, nullable: true, coll: c.coll.asVar(), lit: c.lit}
		}
		return out, nil
	}
	return nil, errAt(codeFeatureNotSupported, loc(src), "unsupported multi-column assignment source %T", src.Node)
}

func (a *analyzer) updateStmt(upd *pg_query.UpdateStmt, sc *scope) ([]rteCol, *Error) {
	if upd.WithClause != nil {
		if err := a.withClause(upd.WithClause, sc); err != nil {
			return nil, err
		}
	}
	rel, target, err := a.writeTarget(upd.Relation, sc, "update")
	if err != nil {
		return nil, err
	}
	sc.items = append(sc.items, target)
	for _, item := range upd.FromClause {
		r, err := a.fromItem(item, sc)
		if err != nil {
			return nil, err
		}
		sc.items = append(sc.items, r)
	}
	if err := a.setClause(upd.TargetList, rel, sc); err != nil {
		return nil, err
	}
	if err := a.boolClause(upd.WhereClause, sc, "WHERE"); err != nil {
		return nil, err
	}
	a.recordFixed(sc, upd.WhereClause)
	return a.returning(upd.ReturningList, sc)
}

func (a *analyzer) deleteStmt(del *pg_query.DeleteStmt, sc *scope) ([]rteCol, *Error) {
	if del.WithClause != nil {
		if err := a.withClause(del.WithClause, sc); err != nil {
			return nil, err
		}
	}
	_, target, err := a.writeTarget(del.Relation, sc, "delete from")
	if err != nil {
		return nil, err
	}
	sc.items = append(sc.items, target)
	for _, item := range del.UsingClause {
		r, err := a.fromItem(item, sc)
		if err != nil {
			return nil, err
		}
		sc.items = append(sc.items, r)
	}
	if err := a.boolClause(del.WhereClause, sc, "WHERE"); err != nil {
		return nil, err
	}
	a.recordFixed(sc, del.WhereClause)
	return a.returning(del.ReturningList, sc)
}

// figureColname implements PG's FigureColname for unaliased target entries.
func (a *analyzer) figureColname(n *pg_query.Node) string {
	if name := a.figureColnameInternal(n); name != "" {
		return name
	}
	return "?column?"
}

func (a *analyzer) figureColnameInternal(n *pg_query.Node) string {
	name, _ := a.figureColnameStrength(n)
	return name
}

// figureColnameStrength is FigureColnameInternal: the name and how strongly the node
// claims it (2 a real name, 1 a fallback such as a cast's type name, 0 none). A cast
// only names the column when what it casts has no strong name of its own.
func (a *analyzer) figureColnameStrength(n *pg_query.Node) (string, int) {
	switch v := n.Node.(type) {
	case *pg_query.Node_ColumnRef:
		f := v.ColumnRef.Fields
		for i := len(f) - 1; i >= 0; i-- {
			if s := f[i].GetString_(); s != nil {
				return s.Sval, 2
			}
		}
	case *pg_query.Node_AIndirection:
		ind := v.AIndirection.Indirection
		for i := len(ind) - 1; i >= 0; i-- {
			if s := ind[i].GetString_(); s != nil {
				return s.Sval, 2
			}
			if ind[i].GetAIndices() != nil {
				continue
			}
		}
		return a.figureColnameStrength(v.AIndirection.Arg)
	case *pg_query.Node_FuncCall:
		names := strs(v.FuncCall.Funcname)
		return names[len(names)-1], 2
	case *pg_query.Node_TypeCast:
		inner, strength := a.figureColnameStrength(v.TypeCast.Arg)
		if strength > 1 {
			return inner, strength
		}
		if v.TypeCast.TypeName != nil {
			names := strs(v.TypeCast.TypeName.Names)
			return names[len(names)-1], 1
		}
		return inner, strength
	case *pg_query.Node_CollateClause:
		return a.figureColnameStrength(v.CollateClause.Arg)
	case *pg_query.Node_CaseExpr:
		if v.CaseExpr.Defresult != nil {
			if inner, strength := a.figureColnameStrength(v.CaseExpr.Defresult); strength > 0 {
				return inner, strength
			}
		}
		return "case", 1
	case *pg_query.Node_AArrayExpr:
		return "array", 2
	case *pg_query.Node_RowExpr:
		return "row", 2
	case *pg_query.Node_AExpr:
		if v.AExpr.Kind == pg_query.A_Expr_Kind_AEXPR_NULLIF {
			return "nullif", 2
		}
	case *pg_query.Node_SubLink:
		switch v.SubLink.SubLinkType {
		case pg_query.SubLinkType_EXISTS_SUBLINK:
			return "exists", 2
		case pg_query.SubLinkType_ARRAY_SUBLINK:
			return "array", 2
		case pg_query.SubLinkType_EXPR_SUBLINK:
			if sel := v.SubLink.Subselect.GetSelectStmt(); sel != nil && len(sel.TargetList) == 1 {
				t := sel.TargetList[0].GetResTarget()
				if t.Name != "" {
					return t.Name, 2
				}
				return a.figureColnameStrength(t.Val)
			}
		}
	case *pg_query.Node_SqlvalueFunction:
		s := strings.ToLower(strings.TrimPrefix(v.SqlvalueFunction.Op.String(), "SVFOP_"))
		return strings.TrimSuffix(s, "_n"), 2
	case *pg_query.Node_GroupingFunc:
		return "grouping", 2
	case *pg_query.Node_NamedArgExpr:
		return a.figureColnameStrength(v.NamedArgExpr.Arg)
	case *pg_query.Node_CoalesceExpr:
		return "coalesce", 2
	case *pg_query.Node_MinMaxExpr:
		if v.MinMaxExpr.Op == pg_query.MinMaxOp_IS_LEAST {
			return "least", 2
		}
		return "greatest", 2
	case *pg_query.Node_MergeSupportFunc:
		return "merge_action", 2
	case *pg_query.Node_XmlExpr:
		switch v.XmlExpr.Op {
		case pg_query.XmlExprOp_IS_XMLCONCAT:
			return "xmlconcat", 2
		case pg_query.XmlExprOp_IS_XMLELEMENT:
			return "xmlelement", 2
		case pg_query.XmlExprOp_IS_XMLFOREST:
			return "xmlforest", 2
		case pg_query.XmlExprOp_IS_XMLPARSE:
			return "xmlparse", 2
		case pg_query.XmlExprOp_IS_XMLPI:
			return "xmlpi", 2
		case pg_query.XmlExprOp_IS_XMLROOT:
			return "xmlroot", 2
		case pg_query.XmlExprOp_IS_XMLSERIALIZE:
			return "xmlserialize", 2
		case pg_query.XmlExprOp_IS_DOCUMENT:
			return "is_document", 2
		}
	case *pg_query.Node_XmlSerialize:
		return "xmlserialize", 2
	case *pg_query.Node_JsonParseExpr:
		return "json", 2
	case *pg_query.Node_JsonScalarExpr:
		return "json_scalar", 2
	case *pg_query.Node_JsonSerializeExpr:
		return "json_serialize", 2
	case *pg_query.Node_JsonObjectConstructor:
		return "json_object", 2
	case *pg_query.Node_JsonArrayConstructor, *pg_query.Node_JsonArrayQueryConstructor:
		return "json_array", 2
	case *pg_query.Node_JsonObjectAgg:
		return "json_objectagg", 2
	case *pg_query.Node_JsonArrayAgg:
		return "json_arrayagg", 2
	case *pg_query.Node_JsonFuncExpr:
		switch v.JsonFuncExpr.Op {
		case pg_query.JsonExprOp_JSON_EXISTS_OP:
			return "json_exists", 2
		case pg_query.JsonExprOp_JSON_QUERY_OP:
			return "json_query", 2
		case pg_query.JsonExprOp_JSON_VALUE_OP:
			return "json_value", 2
		}
	}
	return "", 0
}

// noteEnumSort flags ORDER BY on an enum: it sorts by declaration order, which surprises
// readers expecting the labels' alphabetical order (advisory).
func (a *analyzer) noteEnumSort(n *pg_query.Node, sc *scope, cols []rteCol) {
	var typ schema.TypeRef
	if cr := n.GetColumnRef(); cr != nil && len(cr.Fields) == 1 {
		name := cr.Fields[0].GetString_().GetSval()
		for _, c := range cols {
			if c.name == name {
				typ = c.typ
			}
		}
	}
	if typ.OID == 0 {
		if cr := n.GetColumnRef(); cr != nil {
			saved := len(a.notes)
			e, err := a.columnRef(cr, sc)
			a.notes = a.notes[:saved]
			if err != nil {
				return
			}
			typ = e.typ
		}
	}
	if t := a.typ(a.baseType(typ.OID)); t != nil && t.Kind == 'e' {
		a.note(noteEnumOrder, loc(n), "ORDER BY enum "+t.Name+" sorts in declaration order, not alphabetically")
	}
}

// callStmt analyzes CALL procedure(args): the arguments bind like a function call and
// the OUT / INOUT parameters come back as one result row.
func (a *analyzer) callStmt(call *pg_query.CallStmt, sc *scope) ([]rteCol, *Error) {
	a.inCall = true
	e, err := a.funcCall(call.Funccall, sc)
	a.inCall = false
	if err != nil {
		return nil, err
	}
	_ = e
	fn := a.lastUserFunc
	if fn == nil || !fn.IsProc {
		names := strs(call.Funccall.Funcname)
		return nil, errAt(codeWrongObjectType, call.Funccall.Location, "%s is not a procedure", names[len(names)-1])
	}
	var cols []rteCol
	for _, arg := range fn.Args {
		if arg.Mode == 'o' || arg.Mode == 'b' {
			cols = append(cols, rteCol{name: arg.Name, typ: arg.Type, nullable: true})
		}
	}
	return cols, nil
}

// mergeStmt analyzes MERGE INTO target USING source ON cond WHEN ... (PG 15; RETURNING and
// WHEN NOT MATCHED BY SOURCE are PG 17). A WHEN MATCHED / NOT MATCHED BY SOURCE action
// sees both relations, a WHEN NOT MATCHED [BY TARGET] action sees the source only.
func (a *analyzer) mergeStmt(m *pg_query.MergeStmt, sc *scope) ([]rteCol, *Error) {
	if m.WithClause != nil {
		if err := a.withClause(m.WithClause, sc); err != nil {
			return nil, err
		}
	}
	writes := false
	for _, wn := range m.MergeWhenClauses {
		if wn.GetMergeWhenClause().CommandType != pg_query.CmdType_CMD_NOTHING {
			writes = true
		}
	}
	var rel *schema.Relation
	var target *rte
	var err *Error
	if writes {
		rel, target, err = a.writeTarget(m.Relation, sc, "merge into")
	} else {
		rel, target, err = a.targetRTE(m.Relation, sc)
	}
	if err != nil {
		return nil, err
	}
	if orig := a.s.Relation(m.Relation.Schemaname, m.Relation.Relname); orig != nil && writes {
		// MERGE refuses an action kind that has a rule, and a view whose INSTEAD OF triggers
		// cover only some of the action kinds used (a computed view column fails where an
		// action assigns it)
		used := map[string]bool{}
		for _, wn := range m.MergeWhenClauses {
			switch wn.GetMergeWhenClause().CommandType {
			case pg_query.CmdType_CMD_INSERT:
				used["insert into"] = true
			case pg_query.CmdType_CMD_UPDATE:
				used["update"] = true
			case pg_query.CmdType_CMD_DELETE:
				used["delete from"] = true
			}
		}
		covered, uncovered := 0, 0
		for cmd := range used {
			ev := map[string]string{"insert into": "insert", "update": "update", "delete from": "delete"}[cmd]
			if orig.RuleEvents[ev] {
				return nil, errAt(codeFeatureNotSupported, m.Relation.Location, "cannot execute MERGE on relation %q", orig.Name)
			}
			if orig.Kind == schema.View {
				if a.hasInsteadOfTrigger(orig, cmd) {
					covered++
				} else {
					uncovered++
				}
			}
		}
		if covered > 0 && uncovered > 0 {
			return nil, errAt(codeFeatureNotSupported, m.Relation.Location, "cannot execute MERGE on relation %q", orig.Name)
		}
	}
	source, err := a.fromItem(m.SourceRelation, sc)
	if err != nil {
		return nil, err
	}
	both := newScope(sc)
	both.items = []*rte{target, source}
	srcOnly := newScope(sc)
	srcOnly.items = []*rte{source}
	if err := a.boolClause(m.JoinCondition, both, "ON"); err != nil {
		return nil, err
	}
	a.inMerge = true
	defer func() { a.inMerge = false }()
	unconditional := map[pg_query.MergeMatchKind]bool{}
	for _, wn := range m.MergeWhenClauses {
		w := wn.GetMergeWhenClause()
		if unconditional[w.MatchKind] {
			return nil, errAt(codeSyntaxError, -1, "unreachable WHEN clause specified after unconditional WHEN clause")
		}
		if w.Condition == nil {
			unconditional[w.MatchKind] = true
		}
		wsc := both
		if w.MatchKind == pg_query.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_TARGET {
			wsc = srcOnly
		}
		if err := a.boolClause(w.Condition, wsc, "WHEN"); err != nil {
			return nil, err
		}
		switch w.CommandType {
		case pg_query.CmdType_CMD_UPDATE:
			if w.MatchKind == pg_query.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_TARGET {
				return nil, errAt(codeSyntaxError, -1, "UPDATE is not allowed in WHEN NOT MATCHED clause")
			}
			if err := a.setClause(w.TargetList, rel, wsc); err != nil {
				return nil, err
			}
			a.mergeActions |= mergeUpdate
		case pg_query.CmdType_CMD_DELETE:
			if w.MatchKind == pg_query.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_TARGET {
				return nil, errAt(codeSyntaxError, -1, "DELETE is not allowed in WHEN NOT MATCHED clause")
			}
			a.mergeActions |= mergeDelete
		case pg_query.CmdType_CMD_INSERT:
			if w.MatchKind != pg_query.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_TARGET {
				return nil, errAt(codeSyntaxError, -1, "INSERT is not allowed in WHEN MATCHED clause")
			}
			var cols []*schema.Column
			if len(w.TargetList) == 0 {
				cols = rel.Columns
			}
			for _, tn := range w.TargetList {
				rt := tn.GetResTarget()
				c := rel.Column(rt.Name)
				if c == nil {
					return nil, errAt(codeUndefinedColumn, rt.Location, "column %q of relation %q does not exist", rt.Name, rel.Name)
				}
				cols = append(cols, c)
				a.mergeInserted = append(a.mergeInserted, c.Name)
			}
			if len(w.TargetList) == 0 && len(w.Values) > 0 {
				for _, c := range rel.Columns {
					a.mergeInserted = append(a.mergeInserted, c.Name)
				}
			}
			if len(w.Values) > len(cols) {
				return nil, errAt(codeSyntaxError, loc(w.Values[len(cols)]), "INSERT has more expressions than target columns")
			}
			if len(w.Values) > 0 && len(w.Values) < len(cols) && len(w.TargetList) > 0 {
				return nil, errAt(codeSyntaxError, -1, "INSERT has more target columns than expressions")
			}
			for i, vn := range w.Values {
				e, err := a.analyzeExpr(vn, srcOnly)
				if err != nil {
					return nil, err
				}
				if err := a.assign(e, cols[i], rel.FullName(), loc(vn)); err != nil {
					return nil, err
				}
			}
			a.mergeActions |= mergeInsert
		case pg_query.CmdType_CMD_NOTHING:
		}
	}
	return a.returning(m.ReturningList, both)
}

const (
	mergeInsert = 1 << iota
	mergeUpdate
	mergeDelete
)

// expandCompositeStar expands (expr).* in a target list to the columns of expr's row type.
func (a *analyzer) expandCompositeStar(ind *pg_query.A_Indirection, sc *scope) ([]rteCol, *Error) {
	inner := &pg_query.A_Indirection{Arg: ind.Arg, Indirection: ind.Indirection[:len(ind.Indirection)-1]}
	var e *expr
	var err *Error
	if len(inner.Indirection) == 0 {
		e, err = a.analyzeExpr(inner.Arg, sc)
	} else {
		e, err = a.indirection(inner, sc)
	}
	if err != nil {
		return nil, err
	}
	if len(e.fields) > 0 {
		out := make([]rteCol, len(e.fields))
		copy(out, e.fields)
		return out, nil
	}
	if e.oid() == catalog.Record && inner.Arg.GetFuncCall() != nil {
		// (f(...)).* of a function returning record through OUT parameters
		if cols := a.outParamCols(); len(cols) > 0 {
			return cols, nil
		}
	}
	t := a.typ(e.oid())
	if t == nil || t.Kind != 'c' {
		return nil, errAt(codeWrongObjectType, loc(ind.Arg), "type %s is not composite", a.s.Types.Format(e.typ))
	}
	rel := a.relByRowType(t.OID)
	if rel == nil {
		return nil, errAt(codeWrongObjectType, loc(ind.Arg), "type %s is not composite", a.s.Types.Format(e.typ))
	}
	var out []rteCol
	for _, c := range rel.Columns {
		out = append(out, rteCol{name: c.Name, typ: c.Type, nullable: true})
	}
	return out, nil
}

// singleOutName is the name of the one OUT parameter of the function the last funcCall
// resolved, "" when it has none or more than one (get_func_result_name).
func (a *analyzer) singleOutName() string {
	var names []string
	if uf := a.lastUserFunc; uf != nil {
		for _, arg := range uf.Args {
			if arg.Mode == 'o' || arg.Mode == 'b' || arg.Mode == 't' {
				names = append(names, arg.Name)
			}
		}
	} else if cf := a.lastCatFunc; cf != nil {
		for i, m := range cf.ArgModes {
			if m == 'o' || m == 'b' || m == 't' {
				name := ""
				if i < len(cf.ArgNames) {
					name = cf.ArgNames[i]
				}
				names = append(names, name)
			}
		}
	}
	if len(names) == 1 && names[0] != "" {
		return names[0]
	}
	return ""
}

// aggregateIn returns an aggregate call directly in the expression (not inside a
// subquery), or nil.
func (a *analyzer) aggregateIn(n *pg_query.Node) *pg_query.FuncCall {
	if n == nil || n.GetSubLink() != nil {
		return nil
	}
	if f := n.GetFuncCall(); f != nil && f.Over == nil && a.isAggregateName(strs(f.Funcname)) {
		return f
	}
	for _, c := range children(n) {
		if f := a.aggregateIn(c); f != nil {
			return f
		}
	}
	return nil
}

// xmlTable is XMLTABLE(... PASSING doc COLUMNS ...) in FROM: the declared columns (FOR
// ORDINALITY is integer), or one xml column when none are declared.
func (a *analyzer) xmlTable(x *pg_query.RangeTableFunc, sc *scope) (*rte, *Error) {
	inner := sc
	if x.Lateral {
		inner = newScope(sc)
		inner.items = sc.items
	}
	for _, n := range append([]*pg_query.Node{x.Docexpr, x.Rowexpr}, x.Namespaces...) {
		if n == nil {
			continue
		}
		if rn := n.GetResTarget(); rn != nil {
			n = rn.Val
		}
		e, err := a.analyzeExpr(n, inner)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Text, loc(n)); err != nil {
			return nil, err
		}
	}
	r := &rte{alias: "xmltable"}
	for _, cn := range x.Columns {
		c := cn.GetRangeTableFuncCol()
		if c.ForOrdinality {
			r.cols = append(r.cols, rteCol{name: c.Colname, typ: ref(catalog.Int4)})
			continue
		}
		tr, err := a.s.ResolveType(c.TypeName)
		if err != nil {
			return nil, errAt(codeUndefinedObject, c.Location, "%v", err)
		}
		for _, n := range []*pg_query.Node{c.Colexpr, c.Coldefexpr} {
			if n == nil {
				continue
			}
			e, err := a.analyzeExpr(n, inner)
			if err != nil {
				return nil, err
			}
			if err := a.bind(e, catalog.Text, loc(n)); err != nil {
				return nil, err
			}
		}
		r.cols = append(r.cols, rteCol{name: c.Colname, typ: tr, nullable: !c.IsNotNull})
	}
	if len(x.Columns) == 0 {
		r.cols = []rteCol{{name: "xmltable", typ: ref(catalog.XML), nullable: true}}
	}
	if x.Alias != nil {
		if x.Alias.Aliasname != "" {
			r.alias = x.Alias.Aliasname
		}
		applyColnames(r.cols, x.Alias)
	}
	return r, nil
}

// indirectTarget is the column an INSERT / UPDATE target with subscripts or field
// selection (f2[1], f3.if1, f4[1].if2[2]) assigns: a copy of the column typed as that
// element or field.
func (a *analyzer) indirectTarget(col *schema.Column, ind []*pg_query.Node, at int32) (*schema.Column, *Error) {
	cur := a.baseType(col.Type.OID)
	for _, n := range ind {
		switch v := n.Node.(type) {
		case *pg_query.Node_AIndices:
			if cur == catalog.JSONB {
				continue // jsonb subscripting assigns a jsonb value at the path
			}
			t := a.typ(cur)
			if t == nil || t.Elem == 0 {
				return nil, errAt(codeDatatypeMismatch, at, "cannot subscript type %s because it does not support subscripting", a.s.Types.Format(ref(cur)))
			}
			if !v.AIndices.IsSlice {
				cur = a.baseType(t.Elem)
			}
		case *pg_query.Node_String_:
			t := a.typ(cur)
			if t == nil || t.Kind != 'c' {
				return nil, errAt(codeDatatypeMismatch, at, "column notation .%s applied to type %s, which is not a composite type", v.String_.Sval, a.s.Types.Format(ref(cur)))
			}
			rel := a.relByRowType(cur)
			fc := rel.Column(v.String_.Sval)
			if fc == nil {
				return nil, errAt(codeUndefinedColumn, at, "column %q not found in data type %s", v.String_.Sval, t.Name)
			}
			cur = a.baseType(fc.Type.OID)
		default:
			return nil, errAt(codeFeatureNotSupported, at, "unsupported indirection in assignment target")
		}
	}
	cp := *col
	cp.Type = ref(cur)
	cp.NotNull = false
	return &cp, nil
}

// outParamCols are the OUT / INOUT / TABLE parameters of the function the last funcCall
// resolved, as result columns (a function returning record through them).
func (a *analyzer) outParamCols() []rteCol {
	var cols []rteCol
	if uf := a.lastUserFunc; uf != nil {
		for _, arg := range uf.Args {
			if arg.Mode == 't' || arg.Mode == 'o' || arg.Mode == 'b' {
				cols = append(cols, rteCol{name: arg.Name, typ: arg.Type, nullable: true})
			}
		}
	} else if cf := a.lastCatFunc; cf != nil && len(cf.ArgModes) == len(cf.AllArgTypes) {
		for i, m := range cf.ArgModes {
			if m == 'o' || m == 't' || m == 'b' {
				name := ""
				if i < len(cf.ArgNames) {
					name = cf.ArgNames[i]
				}
				cols = append(cols, rteCol{name: name, typ: ref(cf.AllArgTypes[i]), nullable: true})
			}
		}
	}
	for i := range cols {
		if cols[i].name == "" {
			cols[i].name = "column" + strconv.Itoa(i+1) // an unnamed OUT parameter
		}
		if t := a.typ(cols[i].typ.OID); t != nil && t.IsPolymorphic() {
			if r, ok := a.resolvePolymorphic(a.lastCallArgs, a.lastCallActual, cols[i].typ.OID); ok {
				cols[i].typ = ref(r)
			}
		}
	}
	return cols
}

// windowIn returns a window function call directly in the expression (not inside a
// subquery), or nil.
func windowIn(n *pg_query.Node) *pg_query.FuncCall {
	if n == nil || n.GetSubLink() != nil {
		return nil
	}
	if f := n.GetFuncCall(); f != nil && f.Over != nil {
		return f
	}
	for _, c := range children(n) {
		if f := windowIn(c); f != nil {
			return f
		}
	}
	return nil
}

// outerLevelAggregate reports whether an aggregate in a subquery belongs to an enclosing
// query: none of its column references resolve at this level (they are all outer
// references), so it is that query's aggregate and allowed here.
func (a *analyzer) outerLevelAggregate(f *pg_query.FuncCall, sc *scope) bool {
	if sc.parent == nil {
		return false
	}
	local := false
	sawRef := false
	here := &scope{items: sc.items, ctes: sc.ctes}
	for _, arg := range f.Args {
		schema.WalkNodes(arg, func(n *pg_query.Node) {
			if cr := n.GetColumnRef(); cr != nil {
				sawRef = true
				if _, err := a.columnRef(cr, here); err == nil {
					local = true
				}
			}
		})
	}
	return sawRef && !local
}
