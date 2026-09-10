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
	qe, ok := v.(*mysqlast.Node)
	if !ok || qe.Class != "PT_query_expression" {
		return nil, nil, fmt.Errorf("analyze: query expression not understood: %s", mysqlast.Sprint(v))
	}
	ctes, err := a.with(qe.Arg("with_clause"), outer)
	if err != nil {
		return nil, nil, err
	}
	sc := outer.derived()
	sc.ctes = append(append([]relation{}, sc.ctes...), ctes...)
	cols, block, body, err := a.bodyScope(qe.Arg("body"), sc)
	if err != nil {
		return nil, nil, err
	}
	// ORDER BY of the whole expression: against the result columns, and for a single
	// SELECT also against its tables
	if err := a.orderBy(qe.Arg("order"), cols, block, "order clause"); err != nil {
		return nil, nil, err
	}
	if block != nil {
		if err := a.orderCheck(block, qe.Arg("order")); err != nil {
			return nil, nil, err
		}
	}
	if err := a.limit(qe.Arg("limit")); err != nil {
		return nil, nil, err
	}
	if body != nil {
		if limitOne(qe.Arg("limit")) {
			body.Single = true
		}
		body.Children = append(body.Children, cteBodies(ctes)...)
	}
	return cols, body, nil
}

// subqueryFacts is subquery, with the block's facts (see queryExpressionFacts).
func (a *analyzer) subqueryFacts(v mysqlast.Value, outer *scope) ([]Column, *facts.Scope, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "PT_subquery" {
		return nil, nil, fmt.Errorf("analyze: subquery not understood: %s", mysqlast.Sprint(v))
	}
	a.depth++
	defer func() { a.depth-- }()
	return a.queryExpressionFacts(n.Arg("query_expression"), outer)
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
		for _, c := range cols {
			if strings.EqualFold(c.Name, name) {
				matches++
			}
		}
		if block != nil && where == "group statement" {
			// a table column of the same name shadows a select-list alias (with a warning)
			if _, err := a.lookup(*block, "", name, where, n.Start); err == nil {
				return nil
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
		return fmt.Errorf("analyze: an expression in the ORDER BY of a set operation is not supported yet")
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
	a.depth++
	defer func() { a.depth-- }()
	cols, body, err := a.queryExpressionFacts(n.Arg("query_expression"), outer)
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
	sc, err := a.from(body.Arg("from_clause"), sc)
	if err != nil {
		return nil, nil, err
	}
	if err := a.condition(sc, body.Arg("opt_where_clause"), "where clause"); err != nil {
		return nil, nil, err
	}
	cols, err := a.items(sc, body.Arg("item_list"))
	if err != nil {
		return nil, nil, err
	}
	sc.items = cols
	if err := a.orderBy(body.Arg("opt_group_clause"), cols, &sc, "group statement"); err != nil {
		return nil, nil, err
	}
	a.inHaving = append(a.inHaving, blockID(&sc))
	err = a.condition(sc, body.Arg("opt_having_clause"), "having clause")
	a.inHaving = a.inHaving[:len(a.inHaving)-1]
	if err != nil {
		return nil, nil, err
	}
	sc.facts = a.block(&sc, body)
	if err := a.groupCheck(&sc, body); err != nil {
		return nil, nil, err
	}
	// the statement's own block (not a subquery's, not a set operation's arm) is what the
	// contracts are judged on
	if a.depth == 0 && a.setOp == 0 && a.facts != nil && a.facts.Kind == facts.Select && a.facts.Top == nil {
		a.facts.Top = sc.facts
		if body.Arg("opt_group_clause") == nil && len(cols) > 0 && a.allAggregates(body.Arg("item_list")) {
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
		return len(from) == 0 && n.Arg("opt_where_clause") == nil && n.Arg("opt_having_clause") == nil
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

// mergeable reports whether the server merges a derived table or view of this query
// expression into the outer query (Query_expression::is_mergeable and merge_heuristic):
// a single SELECT over at least one table, without GROUP BY, HAVING, DISTINCT, LIMIT,
// window functions, or a subquery in its select list. What is not merged is
// materialized into a temporary table, whose columns are typed by materialized.
func mergeable(v mysqlast.Value) bool {
	qe, ok := v.(*mysqlast.Node)
	if !ok || qe.Class != "PT_query_expression" {
		return false
	}
	if qe.Arg("limit") != nil {
		return false
	}
	body, ok := qe.Arg("body").(*mysqlast.Node)
	if !ok {
		return false
	}
	if body.Class == "PT_query_expression" {
		return mergeable(body)
	}
	if body.Class != "PT_query_specification" {
		return false
	}
	from, _ := body.Arg("from_clause").(mysqlast.List)
	if len(from) == 0 || body.Arg("opt_group_clause") != nil || body.Arg("opt_having_clause") != nil || body.Arg("opt_window_clause") != nil {
		return false
	}
	if strings.Contains(fmt.Sprint(body.Arg("options")), "SELECT_DISTINCT") {
		return false
	}
	items, _ := body.Arg("item_list").(mysqlast.List)
	for _, item := range items {
		if containsClass(item, "PT_window") || containsClass(item, "PT_subquery") {
			return false
		}
	}
	return true
}

// containsClass reports whether a node of the class occurs in v.
func containsClass(v mysqlast.Value, class string) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		if x.Class == class {
			return true
		}
		for _, a := range x.Args {
			if containsClass(a, class) {
				return true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if containsClass(e, class) {
				return true
			}
		}
	case *mysqlast.Struct:
		for _, e := range x.Fields {
			if containsClass(e, class) {
				return true
			}
		}
	}
	return false
}

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
