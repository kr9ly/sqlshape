// Package analyze types a MySQL statement against a loaded schema: the result columns
// with their types and nullability, the parameters with the types their context gives
// them, and the errors MySQL itself would raise (unknown table or column, ambiguity).
//
// It reads the statement through mysqlast, the server's own parse tree, so the shapes it
// switches on are MySQL's PT_ / Item_ classes. What it does not know yet it leaves
// untyped rather than guessing: a column whose Known is false is accepted by the checker
// with a note.
package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/check/mysql/internal/placeholder"
	"github.com/kr9ly/sqlshape/check/mysql/internal/schema"
)

// Result is the analysis of one statement.
type Result struct {
	// Params are the placeholders by number ($1 is Params[0]).
	Params  []Param
	Columns []Column
}

// Param is one placeholder: the type its context gives it, when the context is one the
// analyzer reads (compared with or assigned to a column, LIMIT).
type Param struct {
	Type  schema.Type
	Known bool
}

// Column is one result column.
type Column struct {
	Name     string
	Type     schema.Type
	Known    bool // the type was inferred; false leaves Type empty
	Nullable bool

	base *schema.Column // the base table column this is a plain reference to, if any
}

// Error is what MySQL would raise for the statement.
type Error struct {
	Message  string
	Code     int // MySQL's error number: 1064 syntax, 1146 no table, 1054 no column, 1052 ambiguous
	Position int // 0-based byte offset into the statement as given ($n form); -1 when unknown
}

func (e *Error) Error() string { return fmt.Sprintf("%s (MySQL error %d)", e.Message, e.Code) }

// Analyze types sql, whose placeholders are `$n`, against s.
func Analyze(s *schema.Schema, sql string) (*Result, error) {
	text, ph := placeholder.Rewrite(sql)
	cst, err := mysqlparse.Parse(text, 0)
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			return nil, &Error{Message: pe.Message, Code: 1064, Position: ph.Back(pe.Offset)}
		}
		return nil, err
	}
	root, err := mysqlast.Build(text, cst)
	if err != nil {
		return nil, err
	}
	a := &analyzer{s: s, text: text, ph: ph, params: make([]Param, ph.Count())}
	if err := a.statement(root); err != nil {
		return nil, err
	}
	return &Result{Params: a.params, Columns: a.columns}, nil
}

type analyzer struct {
	s       *schema.Schema
	text    string // the statement with `?` placeholders, what mysqlparse saw
	ph      placeholder.Map
	params  []Param
	columns []Column
	views   map[string]bool // the views being expanded, against a cycle
}

// relation is a table in scope, under its alias: a base table, or a derived one (a
// derived table, a view, a common table expression) whose columns are its query's.
type relation struct {
	alias     string
	table     *schema.Table // nil for a derived relation
	cols      []Column      // the derived relation's columns
	updatable bool          // a derived relation whose plain column references write through (a mergeable view)
	nullable  bool          // on the nullable side of an outer join
}

// columns lists the relation's columns as a SELECT * expands them (a base table's
// invisible columns are left out).
func (r *relation) columns() []Column {
	if r.table == nil {
		out := make([]Column, len(r.cols))
		for i, c := range r.cols {
			c.Nullable = c.Nullable || r.nullable
			out[i] = c
		}
		return out
	}
	var out []Column
	for _, col := range r.table.Columns {
		if col.Invisible {
			continue
		}
		out = append(out, Column{Name: col.Name, Type: col.Type, Known: true, Nullable: !col.NotNull || r.nullable, base: col})
	}
	return out
}

// column resolves a column name in the relation.
func (r *relation) column(name string) (colRef, bool) {
	if r.table == nil {
		for _, c := range r.cols {
			if strings.EqualFold(c.Name, name) {
				c.Nullable = c.Nullable || r.nullable
				ref := colRef{rel: r, c: c}
				if r.updatable {
					ref.col = c.base
				}
				return ref, true
			}
		}
		return colRef{}, false
	}
	col := r.table.Column(name)
	if col == nil {
		return colRef{}, false
	}
	return colRef{rel: r, col: col, c: Column{Name: col.Name, Type: col.Type, Known: true, Nullable: !col.NotNull || r.nullable, base: col}}, true
}

// scope is the relations a name resolves against: the query's own, then, for a
// correlated subquery, the enclosing queries'. ctes are the common table expressions in
// force, by name, for the FROM clauses of this query and its subqueries.
type scope struct {
	rels  []relation
	outer *scope
	ctes  []relation
}

// derived makes a scope for a nested query: its relations start empty, the enclosing
// scope is outer, and the enclosing CTEs stay visible.
func (sc *scope) derived() scope {
	if sc == nil {
		return scope{}
	}
	return scope{outer: sc, ctes: sc.ctes}
}

func (a *analyzer) statement(v mysqlast.Value) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return fmt.Errorf("analyze: statement not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PT_select_stmt":
		return a.selectStmt(n)
	case "PT_insert":
		return a.insert(n)
	case "PT_update":
		return a.update(n)
	case "PT_delete":
		return a.delete(n)
	}
	return fmt.Errorf("analyze: %s is not supported yet", strings.TrimPrefix(n.Class, "PT_"))
}

func (a *analyzer) selectStmt(n *mysqlast.Node) error {
	cols, err := a.queryExpression(n.Arg("qe"), nil)
	if err != nil {
		return err
	}
	a.columns = cols
	return nil
}

func (a *analyzer) insert(n *mysqlast.Node) error {
	rel, err := a.target(arg(n, "table_ident", 3), nil, nil)
	if err != nil {
		return err
	}
	if rel.table == nil {
		return fmt.Errorf("analyze: INSERT into a view is not supported yet")
	}
	// the column list names the targets; without one the row lists every column in order
	var targets []*schema.Column
	if cols, ok := arg(n, "column_list", 5).(mysqlast.List); ok && len(cols) > 0 {
		for _, c := range cols {
			col, err := a.targetColumn(rel, c, "field list")
			if err != nil {
				return err
			}
			targets = append(targets, col)
		}
	} else {
		targets = rel.table.Columns
	}
	if q := arg(n, "insert_query_expression", 7); q != nil {
		// INSERT ... SELECT: the query's columns feed the targets in order; a bare
		// placeholder in its select list takes the target's type
		cols, err := a.queryExpression(q, nil)
		if err != nil {
			return err
		}
		if len(cols) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		if qe, ok := q.(*mysqlast.Node); ok {
			if body, ok := qe.Arg("body").(*mysqlast.Node); ok && body.Class == "PT_query_specification" {
				items, _ := body.Arg("item_list").(mysqlast.List)
				for i, item := range items {
					if it, ok := item.(*mysqlast.Node); ok && it.Class == "PTI_expr_with_alias" && isParam(it.Arg("expr")) && i < len(targets) {
						a.setParam(it.Arg("expr"), targets[i].Type)
					}
				}
			}
		}
	}
	rows, _ := arg(n, "row_value_list", 6).(mysqlast.List)
	for _, row := range rows {
		vals, _ := row.(mysqlast.List)
		if len(vals) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		for i, v := range vals {
			a.assign(scope{rels: []relation{*rel}}, targets[i], v)
		}
	}
	dupCols, _ := arg(n, "opt_on_duplicate_column_list", 10).(mysqlast.List)
	dupVals, _ := arg(n, "opt_on_duplicate_value_list", 11).(mysqlast.List)
	for i, c := range dupCols {
		col, err := a.targetColumn(rel, c, "field list")
		if err != nil {
			return err
		}
		if i < len(dupVals) {
			a.assign(scope{rels: []relation{*rel}}, col, dupVals[i])
		}
	}
	return nil
}

func (a *analyzer) update(n *mysqlast.Node) error {
	ctes, err := a.with(n.Arg("with_clause"), nil)
	if err != nil {
		return err
	}
	sc, err := a.from(n.Arg("join_table_list"), scope{ctes: ctes})
	if err != nil {
		return err
	}
	cols, _ := n.Arg("column_list").(mysqlast.List)
	vals, _ := n.Arg("value_list").(mysqlast.List)
	for i, c := range cols {
		col, err := a.column(sc, c, "field list")
		if err != nil {
			return err
		}
		if col.col == nil {
			return &Error{Message: fmt.Sprintf("The target table %s of the UPDATE is not updatable", col.rel.alias), Code: 1288, Position: a.ph.Back(nodeStart(c))}
		}
		if i < len(vals) {
			a.assign(sc, col.col, vals[i])
		}
	}
	if err := a.condition(sc, n.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	return a.limit(n.Arg("opt_limit_clause"))
}

func (a *analyzer) delete(n *mysqlast.Node) error {
	ctes, err := a.with(n.Arg("with_clause"), nil)
	if err != nil {
		return err
	}
	rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"), &scope{ctes: ctes})
	if err != nil {
		return err
	}
	if rel.table == nil {
		return fmt.Errorf("analyze: DELETE from a view or a common table expression is not supported yet")
	}
	sc := scope{rels: []relation{*rel}, ctes: ctes}
	if err := a.condition(sc, n.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	return a.limit(n.Arg("opt_delete_limit_clause"))
}

// arg reads a named argument, or the positional one when the node carries no names (a
// hook that built the node by position; PT_insert's positional form, INSERT ... SET, has
// no opt_hints, so its indices are the named ones less one from table_ident on).
func arg(n *mysqlast.Node, name string, i int) mysqlast.Value {
	if len(n.Names) > 0 {
		return n.Arg(name)
	}
	if i < len(n.Args) {
		return n.Args[i]
	}
	return nil
}

// from builds the scope of a FROM / UPDATE table list, starting from sc (the enclosing
// scope and the CTEs in force).
func (a *analyzer) from(v mysqlast.Value, sc scope) (scope, error) {
	list, _ := v.(mysqlast.List)
	for _, t := range list {
		if err := a.tableRef(&sc, t, false); err != nil {
			return sc, err
		}
	}
	return sc, nil
}

func (a *analyzer) tableRef(sc *scope, v mysqlast.Value, nullable bool) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return fmt.Errorf("analyze: table reference not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PT_table_factor_table_ident":
		rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"), sc)
		if err != nil {
			return err
		}
		rel.nullable = nullable
		return a.addRelation(sc, rel, n.Start)
	case "PT_derived_table":
		// (SELECT ...) AS alias [(col, ...)]: the columns are the query's, renamed by the
		// list when given; LATERAL sees the relations to its left
		alias := str(n.Arg("table_alias"))
		if alias == "" {
			return &Error{Message: "Every derived table must have its own alias", Code: 1248, Position: a.ph.Back(n.Start)}
		}
		outer := sc.outer
		if isTrue(n.Arg("lateral")) {
			outer = sc
		}
		cols, err := a.subquery(n.Arg("subquery"), outer)
		if err != nil {
			return err
		}
		if !mergeable(subqueryExpression(n.Arg("subquery"))) {
			cols = materialized(cols, subqueryExpression(n.Arg("subquery")))
		}
		names, _ := n.Arg("column_names").(mysqlast.List)
		if cols, err = renamed(cols, names, alias, a.ph.Back(n.Start)); err != nil {
			return err
		}
		return a.addRelation(sc, &relation{alias: alias, cols: cols, nullable: nullable}, n.Start)
	case "PT_joined_table_on", "PT_joined_table_using", "PT_cross_join":
		jt := str(n.Arg("type"))
		left, right := nullable, nullable
		switch {
		case strings.Contains(jt, "LEFT"):
			right = true
		case strings.Contains(jt, "RIGHT"):
			left = true
		}
		if err := a.tableRef(sc, n.Arg("tab1_node"), left); err != nil {
			return err
		}
		if err := a.tableRef(sc, n.Arg("tab2_node"), right); err != nil {
			return err
		}
		if n.Class == "PT_joined_table_on" {
			return a.condition(*sc, n.Arg("on"), "on clause")
		}
		if fields, ok := n.Arg("using_fields").(mysqlast.List); ok {
			for _, f := range fields {
				if _, err := a.column(*sc, f, "from clause"); err != nil {
					return err
				}
			}
		}
		return nil
	case "PT_table_reference_list_parens":
		list, _ := n.Arg("table_list").(mysqlast.List)
		for _, t := range list {
			if err := a.tableRef(sc, t, nullable); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("analyze: %s is not supported yet", n.Class)
}

// addRelation puts rel into sc, rejecting a second relation under the same alias.
func (a *analyzer) addRelation(sc *scope, rel *relation, at int) error {
	for _, r := range sc.rels {
		if strings.EqualFold(r.alias, rel.alias) {
			return &Error{Message: fmt.Sprintf("Not unique table/alias: '%s'", rel.alias), Code: 1066, Position: a.ph.Back(at)}
		}
	}
	sc.rels = append(sc.rels, *rel)
	return nil
}

// target resolves a Table_ident, under alias when given: a common table expression in
// force (the innermost wins, as in the server), else a base table, else a view (analyzed
// on the spot, its columns are its query's).
func (a *analyzer) target(ident, alias mysqlast.Value, sc *scope) (*relation, error) {
	n, ok := ident.(*mysqlast.Node)
	if !ok || n.Class != "Table_ident" {
		return nil, fmt.Errorf("analyze: table name not understood: %s", mysqlast.Sprint(ident))
	}
	name := str(n.Arg("table"))
	if name == "" && len(n.Args) > 0 {
		name = str(n.Args[len(n.Args)-1])
	}
	var rel *relation
	if sc != nil && str(n.Arg("db")) == "" {
		for i := len(sc.ctes) - 1; i >= 0; i-- {
			if strings.EqualFold(sc.ctes[i].alias, name) {
				r := sc.ctes[i]
				rel = &r
				break
			}
		}
	}
	if rel == nil {
		if t := a.s.Table(name); t != nil {
			rel = &relation{alias: t.Name, table: t}
		} else if v := a.s.View(name); v != nil {
			cols, err := a.view(v)
			if err != nil {
				return nil, err
			}
			// a view is merged into the query unless it says TEMPTABLE or its query cannot be
			// merged; a merged view's plain column references are updatable
			rel = &relation{alias: v.Name, cols: cols, updatable: true}
			if v.Algorithm == "TEMPTABLE" || !mergeable(v.Query) {
				rel.cols, rel.updatable = materialized(cols, v.Query), false
			}
		} else {
			return nil, &Error{Message: fmt.Sprintf("Table '%s' doesn't exist", name), Code: 1146, Position: a.ph.Back(n.Start)}
		}
	}
	if s := str(alias); s != "" {
		rel.alias = s
	}
	return rel, nil
}

// view types a view's query, in a scope of its own (a view sees no CTE and no outer query).
func (a *analyzer) view(v *schema.View) ([]Column, error) {
	if a.views[v.Name] {
		return nil, fmt.Errorf("analyze: view %s refers to itself", v.Name)
	}
	if a.views == nil {
		a.views = map[string]bool{}
	}
	a.views[v.Name] = true
	defer delete(a.views, v.Name)
	// the view's placeholders, if any, are not ours: keep the statement's parameter table
	saved := a.params
	a.params = nil
	defer func() { a.params = saved }()
	cols, err := a.queryExpression(v.Query, nil)
	if err != nil {
		if e, ok := err.(*Error); ok {
			return nil, fmt.Errorf("analyze: view %s: %s (MySQL error %d)", v.Name, e.Message, e.Code)
		}
		return nil, fmt.Errorf("analyze: view %s: %w", v.Name, err)
	}
	var names mysqlast.List
	for _, c := range v.Columns {
		names = append(names, c)
	}
	return renamed(cols, names, v.Name, -1)
}

// renamed applies a derived relation's column list: the same count, the new names.
func renamed(cols []Column, names mysqlast.List, alias string, at int) ([]Column, error) {
	if len(names) == 0 {
		return cols, nil
	}
	if len(names) != len(cols) {
		return nil, &Error{Message: fmt.Sprintf("View's SELECT and view's field list have different column counts"), Code: 1353, Position: at}
	}
	out := make([]Column, len(cols))
	for i, c := range cols {
		c.Name = str(names[i])
		out[i] = c
	}
	return out, nil
}

// targetColumn resolves a column name against one relation (INSERT's column list).
func (a *analyzer) targetColumn(rel *relation, v mysqlast.Value, where string) (*schema.Column, error) {
	ref, err := a.column(scope{rels: []relation{*rel}}, v, where)
	if err != nil {
		return nil, err
	}
	return ref.col, nil
}

// colRef is a resolved column reference: the relation, the schema column when the
// relation is a base table, and the column as the query sees it.
type colRef struct {
	rel *relation
	col *schema.Column
	c   Column
}

// column resolves a PTI_simple_ident_* node in sc. where names the clause the way MySQL's
// message does ("field list", "where clause", "on clause").
func (a *analyzer) column(sc scope, v mysqlast.Value, where string) (colRef, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		if s := str(v); s != "" { // a bare identifier token (USING (id))
			return a.lookup(sc, "", s, where, 0)
		}
		return colRef{}, fmt.Errorf("analyze: column reference not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		return a.lookup(sc, "", str(n.Arg("ident")), where, n.Start)
	case "PTI_simple_ident_q_2d":
		return a.lookup(sc, str(n.Arg("table")), str(n.Arg("field")), where, n.Start)
	case "PTI_simple_ident_q_3d":
		return a.lookup(sc, str(n.Arg("table")), str(n.Arg("field")), where, n.Start)
	}
	return colRef{}, fmt.Errorf("analyze: column reference not understood: %s", mysqlast.Sprint(v))
}

// lookup resolves table.field (table may be "") in sc: the innermost query whose
// relations know the name wins, an outer query is tried only when none of the inner one's
// do (a correlated reference); two matches at one level are ambiguous.
func (a *analyzer) lookup(sc scope, table, field, where string, at int) (colRef, error) {
	for s := &sc; s != nil; s = s.outer {
		var found []colRef
		for i := range s.rels {
			rel := &s.rels[i]
			if table != "" && !strings.EqualFold(rel.alias, table) {
				continue
			}
			if ref, ok := rel.column(field); ok {
				found = append(found, ref)
			}
		}
		switch len(found) {
		case 1:
			return found[0], nil
		case 0:
			continue
		}
		return colRef{}, &Error{Message: fmt.Sprintf("Column '%s' in %s is ambiguous", field, where), Code: 1052, Position: a.ph.Back(at)}
	}
	qualified := field
	if table != "" {
		qualified = table + "." + field
	}
	return colRef{}, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", qualified, where), Code: 1054, Position: a.ph.Back(at)}
}

// items types the select list.
func (a *analyzer) items(sc scope, v mysqlast.Value) ([]Column, error) {
	var out []Column
	list, _ := v.(mysqlast.List)
	for _, item := range list {
		n, ok := item.(*mysqlast.Node)
		if !ok {
			return nil, fmt.Errorf("analyze: select item not understood: %s", mysqlast.Sprint(item))
		}
		switch n.Class {
		case "Item_asterisk":
			table := str(arg(n, "opt_table_name", 1))
			matched := false
			for i := range sc.rels {
				rel := &sc.rels[i]
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				matched = true
				out = append(out, rel.columns()...)
			}
			if !matched {
				if table != "" {
					return nil, &Error{Message: fmt.Sprintf("Unknown table '%s'", table), Code: 1051, Position: a.ph.Back(n.Start)}
				}
				return nil, &Error{Message: "No tables used", Code: 1096, Position: a.ph.Back(n.Start)}
			}
		case "PTI_expr_with_alias":
			expr := n.Arg("expr")
			t, err := a.expr(sc, expr, "field list")
			if err != nil {
				return nil, err
			}
			name := str(n.Arg("alias"))
			if name == "" {
				name = a.itemName(expr)
			}
			c := Column{Name: name, Type: t.typ, Known: t.known, Nullable: t.nullable}
			if ref, ok := a.plainColumn(sc, expr); ok {
				c.base = ref.c.base
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("analyze: select item not understood: %s", n.Class)
		}
	}
	return out, nil
}

// plainColumn resolves expr when it is a bare column reference (already typed without
// error by the caller).
func (a *analyzer) plainColumn(sc scope, expr mysqlast.Value) (colRef, bool) {
	n, ok := expr.(*mysqlast.Node)
	if !ok {
		return colRef{}, false
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
		ref, err := a.column(sc, n, "field list")
		return ref, err == nil
	}
	return colRef{}, false
}

// itemName is the name MySQL gives an unaliased select item: the column's name for a
// column reference, else the expression's text.
func (a *analyzer) itemName(v mysqlast.Value) string {
	if n, ok := v.(*mysqlast.Node); ok {
		switch n.Class {
		case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
			return str(n.Arg("ident"))
		case "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
			return str(n.Arg("field"))
		case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
			if tok, ok := n.Arg("literal").(mysqlast.Token); ok {
				return tok.Value // a string literal is named by its value, without the quotes
			}
		}
		if n.Start >= 0 && n.End <= len(a.text) && n.Start < n.End {
			return a.text[n.Start:n.End]
		}
	}
	return ""
}

// condition types a WHERE / HAVING / ON clause for its errors and its parameters.
func (a *analyzer) condition(sc scope, v mysqlast.Value, where string) error {
	if v == nil {
		return nil
	}
	if n, ok := v.(*mysqlast.Node); ok && (n.Class == "PTI_where" || n.Class == "PTI_having") {
		v = n.Arg("expr")
	}
	_, err := a.expr(sc, v, where)
	return err
}

// limit types LIMIT / OFFSET placeholders as bigint unsigned. SELECT and UPDATE / DELETE
// differ in shape: a PT_limit_clause with a limit_options struct, or the bare option.
func (a *analyzer) limit(v mysqlast.Value) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	options := []mysqlast.Value{n}
	if opts, ok := n.Arg("limit_options").(*mysqlast.Struct); ok {
		options = []mysqlast.Value{opts.Fields["limit"], opts.Fields["opt_offset"]}
	}
	for _, opt := range options {
		if o, ok := opt.(*mysqlast.Node); ok && o.Class == "PTI_limit_option_param_marker" {
			a.setParam(o.Arg("param_marker"), schema.Type{Name: "bigint", Unsigned: true, Length: -1, Dec: -1})
		}
	}
	return nil
}

// assign types v, stored into col: a placeholder takes the column's type.
func (a *analyzer) assign(sc scope, col *schema.Column, v mysqlast.Value) {
	if isParam(v) {
		a.setParam(v, col.Type)
		return
	}
	a.expr(sc, v, "field list") //nolint:errcheck // an INSERT's expressions: errors surface as unknown types
}

// nodeStart is v's start offset in the text, -1 when it has none.
func nodeStart(v mysqlast.Value) int {
	switch x := v.(type) {
	case *mysqlast.Node:
		return x.Start
	case mysqlast.Token:
		return x.Start
	}
	return -1
}

// str reads a Token's value or a Const's text; "" for anything else.
func str(v mysqlast.Value) string {
	switch x := v.(type) {
	case mysqlast.Token:
		if x.Value != "" {
			return x.Value
		}
		return strings.Trim(x.Text, "`")
	case mysqlast.Const:
		return string(x)
	case string:
		return x
	}
	return ""
}
