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
		e, err := a.analyzeExpr(t.Val, sc)
		if err != nil {
			return nil, err
		}
		// unresolved unknown in an output column becomes text
		if e.oid() == catalog.Unknown {
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
	for _, g := range sel.GroupClause {
		if err := a.orderOrGroupItem(g, sc, cols, "GROUP BY"); err != nil {
			return nil, err
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
	for _, cn := range w.Ctes {
		c := cn.GetCommonTableExpr()
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
			default:
				return errAt(codeFeatureNotSupported, c.Location, "unsupported CTE query %T", c.Ctequery.Node)
			}
			if err != nil {
				return err
			}
			a.dmlCTEs = append(a.dmlCTEs, c.Ctequery)
			sc.ctes[c.Ctename] = &cte{name: c.Ctename, cols: a.aliasCols(cols, c.Aliascolnames)}
			continue
		}
		def := &cte{name: c.Ctename, recursive: w.Recursive}
		if w.Recursive && sel.Op == pg_query.SetOperation_SETOP_UNION {
			// analyze the non-recursive term first, then expose the CTE with those types for the recursive term
			left, err := a.selectStmt(sel.Larg, newScope(sc))
			if err != nil {
				return err
			}
			def.cols = a.aliasCols(left, c.Aliascolnames)
			sc.ctes[c.Ctename] = def
			right, err := a.selectStmt(sel.Rarg, newScope(sc))
			if err != nil {
				return err
			}
			if len(right) != len(left) {
				return errAt(codeSyntaxError, c.Location, "each UNION query must have the same number of columns")
			}
			for i := range def.cols {
				def.cols[i].nullable = def.cols[i].nullable || right[i].nullable
				def.cols[i].src = nil
			}
			continue
		}
		csc := newScope(sc)
		cols, err := a.selectStmt(sel, csc)
		if err != nil {
			return err
		}
		def.cols = a.aliasCols(cols, c.Aliascolnames)
		def.sub = &subquery{what: "CTE", sel: sel, sc: csc}
		sc.ctes[c.Ctename] = def
	}
	return nil
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
	left, err := a.selectStmt(sel.Larg, newScope(sc))
	if err != nil {
		return nil, err
	}
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
				r := &rte{alias: rv.Relname, cols: append([]rteCol{}, c.cols...), sub: c.sub}
				if rv.Alias != nil {
					if rv.Alias.Aliasname != "" {
						r.alias = rv.Alias.Aliasname
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
			return nil, errAt(codeSyntaxError, -1, "subquery in FROM must have an alias")
		}
		r := &rte{alias: sub.Alias.Aliasname, sub: &subquery{what: "subquery", sel: sub.Subquery.GetSelectStmt(), sc: child}}
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
	case *pg_query.Node_JoinExpr:
		return a.joinExpr(v.JoinExpr, sc)
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
	r := &rte{}
	var cols []rteCol
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
		switch {
		case t != nil && t.Kind == 'c' && t.RelID != 0:
			rel := a.relByRowType(t.OID)
			for _, c := range rel.Columns {
				cols = append(cols, rteCol{name: c.Name, typ: c.Type, nullable: !c.NotNull})
			}
		case e.typ.OID == catalog.Record:
			// RETURNS TABLE / OUT params of a user function
			if uf := a.lastUserFunc; uf != nil {
				for _, arg := range uf.Args {
					if arg.Mode == 't' || arg.Mode == 'o' || arg.Mode == 'b' {
						cols = append(cols, rteCol{name: arg.Name, typ: arg.Type, nullable: true})
					}
				}
			}
			if len(cols) == 0 && len(coldefs) == 0 {
				return nil, errAt(codeSyntaxError, fc.Location, "a column definition list is required for functions returning \"record\"")
			}
		default:
			cols = []rteCol{{name: fname, typ: e.typ, nullable: true}}
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
		for i, cn := range rf.Alias.Colnames {
			if i < len(cols) {
				cols[i].name = cn.GetString_().GetSval()
			} else if i == len(cols) && len(cols) == 1 {
				// alias(colname) on a scalar function renames the single column
			}
		}
		if len(rf.Alias.Colnames) == 0 && len(cols) == 1 && fc != nil && a.typ(cols[0].typ.OID) != nil && a.typ(cols[0].typ.OID).Kind != 'c' {
			// a scalar-returning function's alias names its single column too
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
	a.assigned = append(a.assigned, assignment{rel: a.relByFullName(relName), col: col, e: e})
	if e.param > 0 {
		if _, done := a.paramSrc[e.param]; !done {
			a.paramSrc[e.param] = &Source{Table: relName, Column: col.Name, NotNull: col.NotNull, Assigned: true}
		}
	}
	if e.oid() == catalog.Unknown {
		return a.bind(e, col.Type.OID, at)
	}
	if !a.canCoerce(e.oid(), col.Type.OID, assignmentCoercion) {
		return errAt(codeDatatypeMismatch, at, "column %q is of type %s but expression is of type %s", col.Name, a.s.Types.Format(col.Type), a.s.Types.Format(e.typ))
	}
	a.domainAssign(e, col.Type.OID, relName, col.Name, at)
	return nil
}

func (a *analyzer) returning(list []*pg_query.Node, sc *scope) ([]rteCol, *Error) {
	if len(list) == 0 {
		return nil, nil
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
	rel, target, err := a.targetRTE(ins.Relation, sc)
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
			src, err := a.selectStmt(sel, a.insertSelScope)
			if err != nil {
				return nil, err
			}
			if len(src) > len(cols) {
				return nil, errAt(codeSyntaxError, -1, "INSERT has more expressions than target columns")
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
	for _, tn := range targets {
		t := tn.GetResTarget()
		col := rel.Column(t.Name)
		if col == nil {
			return errAt(codeUndefinedColumn, t.Location, "column %q of relation %q does not exist", t.Name, rel.Name)
		}
		if t.Val.GetMultiAssignRef() != nil {
			return errAt(codeFeatureNotSupported, t.Location, "multi-column SET is not supported yet")
		}
		e, err := a.analyzeExpr(t.Val, sc)
		if err != nil {
			return err
		}
		if err := a.assign(e, col, rel.FullName(), loc(t.Val)); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) updateStmt(upd *pg_query.UpdateStmt, sc *scope) ([]rteCol, *Error) {
	if upd.WithClause != nil {
		if err := a.withClause(upd.WithClause, sc); err != nil {
			return nil, err
		}
	}
	rel, target, err := a.targetRTE(upd.Relation, sc)
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
	_, target, err := a.targetRTE(del.Relation, sc)
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
	switch v := n.Node.(type) {
	case *pg_query.Node_ColumnRef:
		f := v.ColumnRef.Fields
		for i := len(f) - 1; i >= 0; i-- {
			if s := f[i].GetString_(); s != nil {
				return s.Sval
			}
		}
	case *pg_query.Node_AIndirection:
		ind := v.AIndirection.Indirection
		for i := len(ind) - 1; i >= 0; i-- {
			if s := ind[i].GetString_(); s != nil {
				return s.Sval
			}
			if ind[i].GetAIndices() != nil {
				continue
			}
		}
		return a.figureColnameInternal(v.AIndirection.Arg)
	case *pg_query.Node_FuncCall:
		names := strs(v.FuncCall.Funcname)
		return names[len(names)-1]
	case *pg_query.Node_TypeCast:
		if inner := a.figureColnameInternal(v.TypeCast.Arg); inner != "" {
			return inner
		}
		names := strs(v.TypeCast.TypeName.Names)
		return names[len(names)-1]
	case *pg_query.Node_CollateClause:
		return a.figureColnameInternal(v.CollateClause.Arg)
	case *pg_query.Node_CaseExpr:
		if v.CaseExpr.Defresult != nil {
			if inner := a.figureColnameInternal(v.CaseExpr.Defresult); inner != "" && v.CaseExpr.Defresult.GetAConst() == nil {
				return inner
			}
		}
		return "case"
	case *pg_query.Node_CoalesceExpr:
		return "coalesce"
	case *pg_query.Node_MinMaxExpr:
		if v.MinMaxExpr.Op == pg_query.MinMaxOp_IS_LEAST {
			return "least"
		}
		return "greatest"
	case *pg_query.Node_AArrayExpr:
		return "array"
	case *pg_query.Node_RowExpr:
		return "row"
	case *pg_query.Node_SubLink:
		switch v.SubLink.SubLinkType {
		case pg_query.SubLinkType_EXISTS_SUBLINK:
			return "exists"
		case pg_query.SubLinkType_ARRAY_SUBLINK:
			return "array"
		case pg_query.SubLinkType_EXPR_SUBLINK:
			if sel := v.SubLink.Subselect.GetSelectStmt(); sel != nil && len(sel.TargetList) == 1 {
				t := sel.TargetList[0].GetResTarget()
				if t.Name != "" {
					return t.Name
				}
				return a.figureColnameInternal(t.Val)
			}
		}
	case *pg_query.Node_SqlvalueFunction:
		s := strings.ToLower(strings.TrimPrefix(v.SqlvalueFunction.Op.String(), "SVFOP_"))
		return strings.TrimSuffix(s, "_n")
	case *pg_query.Node_GroupingFunc:
		return "grouping"
	case *pg_query.Node_NamedArgExpr:
		return a.figureColnameInternal(v.NamedArgExpr.Arg)
	}
	return ""
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
