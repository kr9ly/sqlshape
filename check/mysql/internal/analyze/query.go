package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// queryExpression types a PT_query_expression (WITH, a body, ORDER BY, LIMIT) and returns
// its result columns. outer is the enclosing scope for a subquery, nil at the top.
func (a *analyzer) queryExpression(v mysqlast.Value, outer *scope) ([]Column, error) {
	cols, _, err := a.queryExpressionFacts(v, outer)
	return cols, err
}

// queryExpressionFacts is queryExpression, also returning the block's facts when the body
// is one SELECT (nil for a set operation): what a derived table or a view contributes to
// the proof.
func (a *analyzer) queryExpressionFacts(v mysqlast.Value, outer *scope) ([]Column, *facts.Scope, error) {
	cols, body, _, err := a.queryExpressionBlock(v, outer)
	return cols, body, err
}

// queryExpressionBlock is queryExpressionFacts with the block's scope too, when the
// expression is one SELECT (nil for a set operation): INSERT ... SELECT ... ON DUPLICATE
// KEY UPDATE resolves its assignments against the SELECT's tables as well as the target.
func (a *analyzer) queryExpressionBlock(v mysqlast.Value, outer *scope) ([]Column, *facts.Scope, *scope, error) {
	qe, ok := v.(*mysqlast.Node)
	if !ok || qe.Class != "PT_query_expression" {
		return nil, nil, nil, fmt.Errorf("analyze: query expression not understood: %s", mysqlast.Sprint(v))
	}
	ctes, err := a.with(qe.Arg("with_clause"), outer)
	if err != nil {
		return nil, nil, nil, err
	}
	sc := outer.derived()
	sc.ctes = append(append([]relation{}, sc.ctes...), ctes...)
	a.limitZero = limitIsZero(qe.Arg("limit")) // LIMIT 0: the select list is never evaluated (fold.go)
	cols, block, body, err := a.bodyScope(qe.Arg("body"), sc)
	a.limitZero = false
	if err != nil {
		return nil, nil, nil, err
	}
	// ORDER BY of the whole expression: against the result columns, and for a single
	// SELECT also against its tables and its select list
	if block != nil {
		a.pushList(block, "order clause", cols, block.itemList)
	}
	err = a.orderBy(qe.Arg("order"), cols, block, "order clause")
	if block != nil {
		a.popList()
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if block != nil {
		if err := a.orderCheck(block, qe.Arg("order")); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := a.limit(qe.Arg("limit")); err != nil {
		return nil, nil, nil, err
	}
	if body != nil {
		if limitOne(qe.Arg("limit")) {
			body.Single = true
		}
		body.Children = append(body.Children, cteBodies(ctes)...)
	}
	return cols, body, block, nil
}

// subqueryFacts is subquery, with the block's facts (see queryExpressionFacts).
func (a *analyzer) subqueryFacts(v mysqlast.Value, outer *scope) ([]Column, *facts.Scope, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "PT_subquery" {
		return nil, nil, fmt.Errorf("analyze: subquery not understood: %s", mysqlast.Sprint(v))
	}
	a.depth++
	defer func() { a.depth-- }()
	cols, body, err := a.queryExpressionFacts(n.Arg("query_expression"), outer)
	if err == nil && body != nil {
		if a.subFacts == nil {
			a.subFacts = map[*mysqlast.Node]*facts.Scope{}
		}
		a.subFacts[n] = body
	}
	return cols, body, err
}

// orderBy resolves the items of an ORDER BY / GROUP BY list the way the server does
// (find_order_in_list): an integer is a 1-based position in the select list; an
// unqualified name is looked up in the select list first (an alias, or the column an
// item is), then, for GROUP BY, a table column of the same name wins; what the select list
// does not have resolves against the tables of the block (block nil: a set operation, whose
// ORDER BY sees only the result columns). Any other expression is typed against the block.
func (a *analyzer) orderBy(v mysqlast.Value, cols []Column, block *scope, where string) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	list, _ := n.Arg("order_list").(mysqlast.List)
	if list == nil {
		list, _ = n.Arg("group_list").(mysqlast.List)
	}
	a.noFold++ // a constant item is dropped from the list, never evaluated (fold.go)
	defer func() { a.noFold-- }()
	for k, it := range list {
		item := it
		if oe, ok := it.(*mysqlast.Node); ok && oe.Class == "PT_order_expr" {
			item = oe.Arg("item")
		}
		if err := a.orderItem(item, cols, block, where, k+1); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) orderItem(item mysqlast.Value, cols []Column, block *scope, where string, num int) error {
	n, ok := item.(*mysqlast.Node)
	if !ok {
		return nil
	}
	if block == nil && where == "order clause" && (isAggregateLike(n) || isWindowFunction(n) || containsAggregate(n)) {
		return &Error{Message: fmt.Sprintf("Expression #%d of ORDER BY contains aggregate function and applies to a UNION, EXCEPT or INTERSECT", num), Code: 3028, Position: a.ph.Back(n.Start)}
	}
	if block != nil && where == "group statement" && !isColumnRef(n) && n.Class != "Item_int" && n.Class != "Item_uint" {
		// an aggregate cannot be grouped on (measured): one that is a select item as
		// written is 1056 with its text, an alias of one inside the expression is 1056
		// with the server's '???', any other is 1111
		if containsOwn(n, func(x *mysqlast.Node) bool { return x.Class == "Item_func_grouping" }) {
			return &Error{Message: "Can't group on 'GROUPING function'", Code: 1056, Position: a.ph.Back(n.Start)}
		}
		if containsOwn(n, func(x *mysqlast.Node) bool { return isAggregate(x) }) {
			for _, it := range block.itemList {
				if ewa, ok := it.(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" {
					if e, ok := ewa.Arg("expr").(*mysqlast.Node); ok && strings.EqualFold(a.textOf(e), a.textOf(n)) {
						return &Error{Message: fmt.Sprintf("Can't group on '%s'", a.textOf(n)), Code: 1056, Position: a.ph.Back(n.Start)}
					}
				}
			}
			return &Error{Message: "Invalid use of group function", Code: 1111, Position: a.ph.Back(n.Start)}
		}
		if a.namesAggregateAlias(*block, n) {
			return &Error{Message: "Can't group on '???'", Code: 1056, Position: a.ph.Back(n.Start)}
		}
	}
	switch n.Class {
	case "Item_int", "Item_uint":
		pos := intOr(n.Arg("i"), 0)
		if n.Class == "Item_uint" {
			pos = intOr(n.Arg("str"), 0)
		}
		if pos < 1 || pos > len(cols) {
			return &Error{Message: fmt.Sprintf("Unknown column '%d' in '%s'", pos, where), Code: 1054, Position: a.ph.Back(n.Start)}
		}
		return nil
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		name := str(n.Arg("ident"))
		matches := 0
		var first *Column
		for i := range cols {
			if strings.EqualFold(cols[i].Name, name) {
				if first != nil && first.base != nil && first.base == cols[i].base && first.leaf1 == cols[i].leaf1 {
					continue // the same table column twice is one item (find_item_in_list)
				}
				matches++
				if first == nil {
					first = &cols[i]
				}
			}
		}
		if block != nil && where == "group statement" {
			// a table column of the same name shadows a select-list alias (with a warning):
			// the lookup is of the tables alone, so not under the clause's own name
			if _, err := a.lookup(*block, "", name, "group statement (tables)", n.Start); err == nil {
				return nil
			}
			if matches == 1 && aliasOfAggregate(block.itemList, name) {
				return &Error{Message: fmt.Sprintf("Can't group on '%s'", name), Code: 1056, Position: a.ph.Back(n.Start)}
			}
		}
		if matches == 1 {
			return nil
		}
		if matches > 1 {
			return &Error{Message: fmt.Sprintf("Column '%s' in %s is ambiguous", name, where), Code: 1052, Position: a.ph.Back(n.Start)}
		}
		if block == nil {
			return &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", name, where), Code: 1054, Position: a.ph.Back(n.Start)}
		}
		_, err := a.expr(*block, n, where)
		return err
	}
	if block == nil {
		// a set operation's ORDER BY sees only the result columns: type the expression
		// against them as a relation of their own
		result := scope{rels: []relation{{cols: cols}}}
		_, err := a.expr(result, n, where)
		return err
	}
	_, err := a.expr(*block, n, where)
	return err
}

// subquery types a PT_subquery (a parenthesized query expression).
func (a *analyzer) subquery(v mysqlast.Value, outer *scope) ([]Column, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "PT_subquery" {
		return nil, fmt.Errorf("analyze: subquery not understood: %s", mysqlast.Sprint(v))
	}
	cols, body, err := a.subqueryFacts(n, outer)
	outer.child(body) // a subquery of a condition or a select list is a nested block of the enclosing one
	return cols, err
}

// body types a query expression body: a single SELECT, a set operation over bodies, or a
// parenthesized query expression. sc is the scope the body's FROM clauses start from.
func (a *analyzer) body(v mysqlast.Value, sc scope) ([]Column, error) {
	cols, _, _, err := a.bodyScope(v, sc)
	return cols, err
}

// bodyScope is body, also returning the block's scope when the body is one SELECT (nil
// for a set operation), for the ORDER BY at the expression level, and the body's facts: the
// block's, or a set operation's level (which may combine rows) with the arms under it.
func (a *analyzer) bodyScope(v mysqlast.Value, sc scope) ([]Column, *scope, *facts.Scope, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil, nil, nil, fmt.Errorf("analyze: query body not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PT_query_specification":
		cols, block, err := a.querySpecification(n, sc)
		if block == nil {
			return cols, nil, nil, err
		}
		return cols, block, block.facts, err
	case "PT_query_expression":
		cols, body, err := a.queryExpressionFacts(n, sc.outer)
		return cols, nil, body, err
	case "PT_union", "PT_except", "PT_intersect":
		cols, fs, err := a.setOperation(n, sc)
		return cols, nil, fs, err
	}
	return nil, nil, nil, fmt.Errorf("analyze: %s is not supported yet", n.Class)
}

// querySpecification types one SELECT block and returns its columns and its scope. HAVING
// and GROUP BY see the select list's names as well as the tables' (a MySQL extension).
func (a *analyzer) querySpecification(body *mysqlast.Node, sc scope) ([]Column, *scope, error) {
	sc.kids = new([]*facts.Scope)
	sc.tag = new(byte)
	sc, err := a.from(body.Arg("from_clause"), sc)
	if err != nil {
		return nil, nil, err
	}
	// the select list is resolved before the WHERE (setup_fields, then setup_conds): its
	// errors come first. A constant select item runs per row over a FROM (its 1690 is a
	// violation, fold.go), once without one (the statement's error); an EXISTS never
	// evaluates its subquery's select list
	perRow, off := a.foldPerRow, a.existsList || a.limitZero
	a.existsList, a.limitZero = false, false
	// a select item over a FROM runs per row; so does a subquery's without one when the
	// subquery itself sits in a per-row position (measured: `SELECT (SELECT <constant>)
	// FROM t` over no rows runs, `WHERE (SELECT <constant>)` fails before any)
	a.foldPerRow = len(sc.rels) > 0 || perRow
	if len(sc.rels) == 0 {
		// without a FROM, a constant false WHERE returns no row before the select list is
		// evaluated (measured: `SELECT 9223372036854775807 + 1 FROM DUAL WHERE 1 = 0` runs)
		w := body.Arg("opt_where_clause")
		if wn, ok := w.(*mysqlast.Node); ok && wn.Class == "PTI_where" {
			w = wn.Arg("expr")
		}
		if b, _, ok := a.constBool(w); ok && !b {
			off = true
		}
	}
	if off {
		a.noFold++
	}
	itemList, _ := body.Arg("item_list").(mysqlast.List)
	a.pushList(&sc, "field list", nil, itemList)
	cols, err := a.items(sc, body.Arg("item_list"))
	a.popList()
	if off {
		a.noFold--
	}
	a.foldPerRow = perRow
	if err != nil {
		return nil, nil, err
	}
	sc.items = cols
	sc.itemList = itemList
	if err := a.condition(sc, body.Arg("opt_where_clause"), "where clause"); err != nil {
		return nil, nil, err
	}
	a.pushList(&sc, "group statement", cols, itemList)
	err = a.orderBy(body.Arg("opt_group_clause"), cols, &sc, "group statement")
	a.popList()
	if err != nil {
		return nil, nil, err
	}
	a.pushList(&sc, "having clause", cols, itemList)
	err = a.condition(sc, body.Arg("opt_having_clause"), "having clause")
	a.popList()
	if err != nil {
		return nil, nil, err
	}
	if q, ok := body.Arg("opt_qualify_clause").(*mysqlast.Node); ok {
		// 8.4 accepts the clause only under the hypergraph optimizer, which is off
		return nil, nil, &Error{Message: "'QUALIFY clause' can be used only if the hypergraph optimizer is enabled.", Code: 6037, Position: a.ph.Back(q.Start)}
	}
	sc.facts = a.block(&sc, body)
	if err := a.groupCheck(&sc, body); err != nil {
		return nil, nil, err
	}
	if sc.info != nil && sc.info.aggregated && !sc.info.explicit {
		// an aggregate of the block in its select list or HAVING without GROUP BY groups
		// the whole input into one row, whatever the aggregate sits inside (COUNT(*),
		// COALESCE(SUM(total), 0), MAX(id) + 1 alike); an aggregate a subquery owns
		// (its own columns only) does not count, as groupCheck's ownership says
		sc.facts.Single = true
	}
	if sc.info != nil && sc.info.rollup {
		// the super-aggregate rows of ROLLUP hold NULL in every non-aggregated column that
		// reads the grouped columns (an Item_rollup_group_item wraps each occurrence of a
		// GROUP BY expression; a constant item such as `1+1` is left alone and keeps its
		// nullability, measured on mysqld 8.4 -- TestOracle, the corpus probe's olap file)
		k := 0
		for _, it := range sc.itemList {
			n, ok := it.(*mysqlast.Node)
			if !ok {
				continue
			}
			switch n.Class {
			case "PTI_expr_with_alias":
				if k < len(cols) && !isAggregateLike(exprNode(n.Arg("expr"))) && containsColumnRef(n.Arg("expr")) {
					cols[k].Nullable = true
				}
				k++
			case "Item_asterisk":
				for ; k < len(cols); k++ {
					cols[k].Nullable = true
				}
			}
		}
		sc.items = cols
	}
	// the statement's own block (not a subquery's, not a set operation's arm) is what the
	// contracts are judged on
	if a.depth == 0 && a.setOp == 0 && a.facts != nil && a.facts.Kind == facts.Select && a.facts.Top == nil {
		a.facts.Top = sc.facts
		if sc.facts.Single {
			a.facts.AtMostOne = true // an aggregate over the whole input is one row
		}
	}
	return cols, &sc, nil
}

// setOperation types UNION / EXCEPT / INTERSECT: the column names are the first
// operand's, each column's type is the operands' aggregated as the server does for a
// UNION (Item_type_holder over field_type_merge), and it is nullable when any operand's is.
func (a *analyzer) setOperation(n *mysqlast.Node, sc scope) ([]Column, *facts.Scope, error) {
	// the level has no leaves of its own: its facts say a set operation may combine rows,
	// and the arms are the blocks under it
	fs := &facts.Scope{At: -1, Many: setOpName(n.Class) + " may combine rows"}
	if a.depth == 0 && a.setOp == 0 && a.facts != nil && a.facts.Kind == facts.Select && a.facts.Top == nil {
		a.facts.Top = fs
	}
	a.setOp++
	defer func() { a.setOp-- }()
	list, _ := n.Arg("list").(mysqlast.List)
	var out []Column
	for i, arm := range list {
		cols, _, armFacts, err := a.bodyScope(arm, sc)
		if err != nil {
			return nil, nil, err
		}
		if armFacts != nil {
			fs.Children = append(fs.Children, armFacts)
		}
		if i == 0 {
			out = cols
			continue
		}
		if len(cols) != len(out) {
			return nil, nil, &Error{Message: "The used SELECT statements have a different number of columns", Code: 1222, Position: a.ph.Back(n.Start)}
		}
		for j := range out {
			out[j] = mergeColumn(out[j], cols[j])
		}
	}
	return out, fs, nil
}

// setOpName spells a set operation's node class the way the statement does.
func setOpName(class string) string {
	switch class {
	case "PT_except":
		return "EXCEPT"
	case "PT_intersect":
		return "INTERSECT"
	}
	return "UNION"
}

// mergeColumn is the type of a set operation's column over two operands' columns.
func mergeColumn(x, y Column) Column {
	if !x.Known || !y.Known {
		return Column{Name: x.Name, Nullable: x.Nullable || y.Nullable}
	}
	t := aggregate([]typed{{typ: x.Type, known: true, nullable: x.Nullable}, {typ: y.Type, known: true, nullable: y.Nullable}})
	return Column{Name: x.Name, Type: t.typ, Known: t.known, Nullable: x.Nullable || y.Nullable}
}

// with types a WITH clause and returns its common table expressions as relations, in
// order; a later one sees the earlier ones. A recursive CTE takes its columns from the
// arms of its UNION that do not refer to it (the anchor), which the server also requires
// to come first; the recursive arms are then checked with the CTE in scope.
func (a *analyzer) with(v mysqlast.Value, outer *scope) ([]relation, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil, nil
	}
	recursive := isTrue(n.Arg("r"))
	list, _ := n.Arg("l").(mysqlast.List)
	var ctes []relation
	for _, c := range list {
		cte, ok := c.(*mysqlast.Node)
		if !ok || cte.Class != "PT_common_table_expr" || len(cte.Args) < 5 {
			return nil, fmt.Errorf("analyze: common table expression not understood: %s", mysqlast.Sprint(c))
		}
		name := str(cte.Args[0])
		names, _ := cte.Args[4].(mysqlast.List)
		sc := outer.derived()
		sc.ctes = append(append([]relation{}, sc.ctes...), ctes...)
		var cols []Column
		var body *facts.Scope
		var merged bool
		var err error
		if recursive {
			cols, err = a.recursiveCTE(name, names, cte.Args[3], sc)
		} else {
			sc.kids = new([]*facts.Scope) // the body's facts, to hang under the query that has the WITH
			if cols, err = a.subquery(cte.Args[3], &sc); err == nil {
				if len(*sc.kids) > 0 {
					body = (*sc.kids)[0]
				}
				merged = mergeable(subqueryExpression(cte.Args[3]))
				if !merged {
					cols = materialized(cols, subqueryExpression(cte.Args[3]))
				}
				cols, err = renamed(cols, names, name, a.ph.Back(cte.Start))
			}
		}
		if err != nil {
			return nil, err
		}
		ctes = append(ctes, relation{alias: name, cols: cols, body: body, cte: true, merged: merged})
	}
	return ctes, nil
}

// recursiveCTE types a recursive CTE's body: anchor arms first, without the CTE in
// scope; the recursive arms are checked with it in scope (under its declared column
// names) and do not change the types. Every column of a recursive CTE is nullable
// (Query_expression::prepare: "Always nullable, per SQL standard").
func (a *analyzer) recursiveCTE(name string, names mysqlast.List, sub mysqlast.Value, sc scope) ([]Column, error) {
	cols, err := a.recursiveCTEBody(name, names, sub, sc)
	if err != nil {
		return nil, err
	}
	for i := range cols {
		cols[i].Nullable = true
	}
	return cols, nil
}

func (a *analyzer) recursiveCTEBody(name string, names mysqlast.List, sub mysqlast.Value, sc scope) ([]Column, error) {
	n, ok := sub.(*mysqlast.Node)
	if !ok || n.Class != "PT_subquery" {
		return nil, fmt.Errorf("analyze: subquery not understood: %s", mysqlast.Sprint(sub))
	}
	qe, ok := n.Arg("query_expression").(*mysqlast.Node)
	if !ok {
		return nil, fmt.Errorf("analyze: query expression not understood: %s", mysqlast.Sprint(n.Arg("query_expression")))
	}
	union, ok := qe.Arg("body").(*mysqlast.Node)
	if !ok || union.Class != "PT_union" {
		cols, err := a.subquery(sub, &sc) // no self-reference possible without a UNION
		if err != nil {
			return nil, err
		}
		return renamed(cols, names, name, a.ph.Back(n.Start))
	}
	arms, _ := union.Arg("list").(mysqlast.List)
	inner := sc.derived()
	inner.outer = sc.outer
	var out []Column
	for i, arm := range arms {
		if out == nil {
			cols, err := a.body(arm, inner)
			if err == nil {
				if i > 0 {
					if len(cols) != len(out) {
						return nil, &Error{Message: "The used SELECT statements have a different number of columns", Code: 1222, Position: a.ph.Back(union.Start)}
					}
				}
				if out, err = renamed(cols, names, name, a.ph.Back(n.Start)); err != nil {
					return nil, err
				}
				continue
			}
			if e, isDB := err.(*Error); !isDB || e.Code != 1146 {
				return nil, err
			}
			// the arm refers to a table that does not exist: itself, if it is the CTE
			if i == 0 {
				return nil, &Error{Message: fmt.Sprintf("Recursive Common Table Expression '%s' should contain a UNION", name), Code: 3573, Position: a.ph.Back(union.Start)}
			}
		}
		// a recursive arm: the CTE, typed so far, is in scope
		with := inner
		with.ctes = append(append([]relation{}, inner.ctes...), relation{alias: name, cols: out})
		cols, err := a.body(arm, with)
		if err != nil {
			return nil, err
		}
		if len(cols) != len(out) {
			return nil, &Error{Message: "The used SELECT statements have a different number of columns", Code: 1222, Position: a.ph.Back(union.Start)}
		}
	}
	return out, nil
}

// scalarSubquery types a subquery used as a value: its single column, NULL when the
// column can be or when the query may produce no row (Item_singlerow_subselect::resolve_type:
// a query block without tables, WHERE, HAVING or LIMIT is guaranteed one row, and a UNION
// with such an arm is never empty).
func (a *analyzer) scalarSubquery(v mysqlast.Value, sc scope, at int) (typed, error) {
	cols, err := a.subquery(v, &sc)
	if err != nil {
		return unknown, err
	}
	if len(cols) != 1 {
		return unknown, &Error{Message: "Operand should contain 1 column(s)", Code: 1241, Position: a.ph.Back(at)}
	}
	return typed{typ: cols[0].Type, known: cols[0].Known, nullable: cols[0].Nullable || possiblyEmpty(subqueryExpression(v))}, nil
}

// subqueryExpression is the PT_query_expression under a PT_subquery.
func subqueryExpression(v mysqlast.Value) mysqlast.Value {
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "PT_subquery" {
		return n.Arg("query_expression")
	}
	return v
}

// possiblyEmpty reports whether a query expression may produce no row.
func possiblyEmpty(v mysqlast.Value) bool {
	qe, ok := v.(*mysqlast.Node)
	if !ok || qe.Class != "PT_query_expression" {
		return true
	}
	if qe.Arg("limit") != nil {
		return true
	}
	return !oneRow(qe.Arg("body"))
}

// oneRow reports whether a query body is guaranteed a row: a SELECT without tables,
// WHERE or HAVING, or a UNION with such an arm (an INTERSECT / EXCEPT may always be empty).
func oneRow(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "PT_query_specification":
		from, _ := n.Arg("from_clause").(mysqlast.List)
		return len(from) == 0 && constTrue(whereExpr(n.Arg("opt_where_clause"))) && n.Arg("opt_having_clause") == nil
	case "PT_query_expression":
		return !possiblyEmpty(n)
	case "PT_union":
		list, _ := n.Arg("list").(mysqlast.List)
		for _, arm := range list {
			if oneRow(arm) {
				return true
			}
		}
	}
	return false
}

// mergeable is mysqlast.Mergeable: shared with the schema loader, which refuses WITH CHECK
// OPTION on a view the server would not merge (1368) the way the server does at CREATE.
func mergeable(v mysqlast.Value) bool { return mysqlast.Mergeable(v) }

// containsClass is mysqlast.ContainsClass.
func containsClass(v mysqlast.Value, class string) bool { return mysqlast.ContainsClass(v, class) }

// materialized retypes the columns of a query the server materializes into a temporary
// table. For a single SELECT the temporary table's fields come from its items
// (create_tmp_field_from_item): a column that is not a plain table column and has an
// integer result becomes an int when its display width is under 10 digits, a bigint
// otherwise; widths the analyzer does not compute leave the type as it is. A set
// operation's fields come from Item_type_holder over field_type_merge, which keeps the
// types the operands were merged to.
func materialized(cols []Column, qe mysqlast.Value) []Column {
	single := singleBlock(qe)
	out := make([]Column, len(cols))
	for i, c := range cols {
		if single && c.base == nil && c.Known && kindOf(c.Type) == "INT_RESULT" && c.Type.Length > 0 && c.Type.Length < 10 && c.Type.Name != "year" {
			c.Type = schema.Type{Name: "int", Unsigned: c.Type.Unsigned, Length: c.Type.Length, Dec: -1}
		}
		c.base = nil // the temporary table's column is nobody's
		out[i] = c
	}
	return out
}

// singleBlock reports whether a query expression is one SELECT (however parenthesized).
func singleBlock(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "PT_query_expression":
		return singleBlock(n.Arg("body"))
	case "PT_query_specification":
		return true
	}
	return false
}

// typeOfColumn is a result column as a typed value.
func typeOfColumn(c Column) typed {
	return typed{typ: c.Type, known: c.Known, nullable: c.Nullable}
}

// aliasOfAggregate reports a select alias whose expression is an aggregate: the server
// cannot group on it (ER_WRONG_GROUP_FIELD, "Can't group on 'c'").
func aliasOfAggregate(items mysqlast.List, name string) bool {
	for _, it := range items {
		ewa, ok := it.(*mysqlast.Node)
		if !ok || ewa.Class != "PTI_expr_with_alias" || !strings.EqualFold(str(ewa.Arg("alias")), name) {
			continue
		}
		return containsAggregate(ewa.Arg("expr"))
	}
	return false
}

// containsOwn reports a node pred accepts anywhere in v outside any subquery (whose
// aggregates are its own).
func containsOwn(v mysqlast.Value, pred func(*mysqlast.Node) bool) bool {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if containsOwn(e, pred) {
				return true
			}
		}
	case *mysqlast.Node:
		if x.Class == "PT_subquery" {
			return false
		}
		if pred(x) {
			return true
		}
		for _, arg := range x.Args {
			if containsOwn(arg, pred) {
				return true
			}
		}
	}
	return false
}

// namesAggregateAlias reports an unqualified name in v (outside any subquery) that is no
// table column of the block but the alias of an aggregate select item.
func (a *analyzer) namesAggregateAlias(block scope, v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if a.namesAggregateAlias(block, e) {
				return true
			}
		}
	case *mysqlast.Node:
		if x.Class == "PT_subquery" {
			return false
		}
		if x.Class == "PTI_simple_ident_ident" || x.Class == "PTI_simple_ident_nospvar_ident" {
			name := str(x.Arg("ident"))
			if _, err := a.lookup(block, "", name, "group statement (tables)", x.Start); err != nil && aliasOfAggregate(block.itemList, name) {
				return true
			}
			return false
		}
		for _, arg := range x.Args {
			if a.namesAggregateAlias(block, arg) {
				return true
			}
		}
	}
	return false
}

// aliasOfWindow reports a select alias whose expression carries a window function: an
// outer reference to it is 3594.
func aliasOfWindow(items mysqlast.List, name string) bool {
	for _, it := range items {
		ewa, ok := it.(*mysqlast.Node)
		if !ok || ewa.Class != "PTI_expr_with_alias" || !strings.EqualFold(str(ewa.Arg("alias")), name) {
			continue
		}
		return containsWindow(ewa.Arg("expr"))
	}
	return false
}

// containsWindow reports a window function anywhere in v.
func containsWindow(v mysqlast.Value) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		if isWindowFunction(x) {
			return true
		}
		for _, arg := range x.Args {
			if containsWindow(arg) {
				return true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if containsWindow(e) {
				return true
			}
		}
	}
	return false
}

// exprNode is v as a node (an empty node otherwise).
func exprNode(v mysqlast.Value) *mysqlast.Node {
	if n, ok := v.(*mysqlast.Node); ok {
		return n
	}
	return &mysqlast.Node{}
}

// containsColumnRef reports a column reference (a bare or qualified identifier, or a
// `*`) anywhere under v, outside of aggregates or not.
func containsColumnRef(v mysqlast.Value) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		switch x.Class {
		case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d", "Item_asterisk":
			return true
		}
		for _, arg := range x.Args {
			if containsColumnRef(arg) {
				return true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if containsColumnRef(e) {
				return true
			}
		}
	}
	return false
}
