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

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlast"
	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/mysql/internal/placeholder"
	"github.com/kr9ly/sqlshape/mysql/internal/schema"
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
}

// relation is a table in scope, under its alias.
type relation struct {
	alias    string
	table    *schema.Table
	nullable bool // on the nullable side of an outer join
}

type scope struct {
	rels []relation
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
	qe, ok := n.Arg("qe").(*mysqlast.Node)
	if !ok || qe.Class != "PT_query_expression" {
		return fmt.Errorf("analyze: query expression not understood: %s", mysqlast.Sprint(n.Arg("qe")))
	}
	body, ok := qe.Arg("body").(*mysqlast.Node)
	if !ok || body.Class != "PT_query_specification" {
		return fmt.Errorf("analyze: only a single SELECT is supported yet (no UNION / INTERSECT / EXCEPT)")
	}
	sc, err := a.from(body.Arg("from_clause"))
	if err != nil {
		return err
	}
	if err := a.condition(sc, body.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	if err := a.condition(sc, body.Arg("opt_having_clause"), "having clause"); err != nil {
		return err
	}
	if err := a.items(sc, body.Arg("item_list")); err != nil {
		return err
	}
	if err := a.limit(qe.Arg("limit")); err != nil {
		return err
	}
	return nil
}

func (a *analyzer) insert(n *mysqlast.Node) error {
	rel, err := a.target(arg(n, "table_ident", 3), nil)
	if err != nil {
		return err
	}
	if q := arg(n, "insert_query_expression", 7); q != nil {
		return fmt.Errorf("analyze: INSERT ... SELECT is not supported yet")
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
	rows, _ := arg(n, "row_value_list", 6).(mysqlast.List)
	for _, row := range rows {
		vals, _ := row.(mysqlast.List)
		if len(vals) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		for i, v := range vals {
			a.assign(scope{[]relation{*rel}}, targets[i], v)
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
			a.assign(scope{[]relation{*rel}}, col, dupVals[i])
		}
	}
	return nil
}

func (a *analyzer) update(n *mysqlast.Node) error {
	sc, err := a.from(n.Arg("join_table_list"))
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
	rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"))
	if err != nil {
		return err
	}
	sc := scope{[]relation{*rel}}
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

// from builds the scope of a FROM / UPDATE table list.
func (a *analyzer) from(v mysqlast.Value) (scope, error) {
	var sc scope
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
		rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"))
		if err != nil {
			return err
		}
		rel.nullable = nullable
		for _, r := range sc.rels {
			if strings.EqualFold(r.alias, rel.alias) {
				return &Error{Message: fmt.Sprintf("Not unique table/alias: '%s'", rel.alias), Code: 1066, Position: a.ph.Back(n.Start)}
			}
		}
		sc.rels = append(sc.rels, *rel)
		return nil
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
	case "PT_derived_table":
		return fmt.Errorf("analyze: derived tables are not supported yet")
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

// target resolves a Table_ident to a schema table, under alias when given.
func (a *analyzer) target(ident, alias mysqlast.Value) (*relation, error) {
	n, ok := ident.(*mysqlast.Node)
	if !ok || n.Class != "Table_ident" {
		return nil, fmt.Errorf("analyze: table name not understood: %s", mysqlast.Sprint(ident))
	}
	name := str(n.Arg("table"))
	if name == "" && len(n.Args) > 0 {
		name = str(n.Args[len(n.Args)-1])
	}
	t := a.s.Table(name)
	if t == nil {
		if a.s.View(name) != nil {
			return nil, fmt.Errorf("analyze: views are not supported yet (%s)", name)
		}
		return nil, &Error{Message: fmt.Sprintf("Table '%s' doesn't exist", name), Code: 1146, Position: a.ph.Back(n.Start)}
	}
	rel := &relation{alias: t.Name, table: t}
	if s := str(alias); s != "" {
		rel.alias = s
	}
	return rel, nil
}

// targetColumn resolves a column name against one relation (INSERT's column list).
func (a *analyzer) targetColumn(rel *relation, v mysqlast.Value, where string) (*schema.Column, error) {
	ref, err := a.column(scope{[]relation{*rel}}, v, where)
	if err != nil {
		return nil, err
	}
	return ref.col, nil
}

// colRef is a resolved column reference.
type colRef struct {
	rel *relation
	col *schema.Column
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

func (a *analyzer) lookup(sc scope, table, field, where string, at int) (colRef, error) {
	var found []colRef
	for i := range sc.rels {
		rel := &sc.rels[i]
		if table != "" && !strings.EqualFold(rel.alias, table) {
			continue
		}
		if col := rel.table.Column(field); col != nil {
			found = append(found, colRef{rel, col})
		}
	}
	qualified := field
	if table != "" {
		qualified = table + "." + field
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if table != "" {
			known := false
			for _, rel := range sc.rels {
				if strings.EqualFold(rel.alias, table) {
					known = true
				}
			}
			if !known {
				return colRef{}, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", qualified, where), Code: 1054, Position: a.ph.Back(at)}
			}
		}
		return colRef{}, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", qualified, where), Code: 1054, Position: a.ph.Back(at)}
	}
	return colRef{}, &Error{Message: fmt.Sprintf("Column '%s' in %s is ambiguous", field, where), Code: 1052, Position: a.ph.Back(at)}
}

// items types the select list.
func (a *analyzer) items(sc scope, v mysqlast.Value) error {
	list, _ := v.(mysqlast.List)
	for _, item := range list {
		n, ok := item.(*mysqlast.Node)
		if !ok {
			return fmt.Errorf("analyze: select item not understood: %s", mysqlast.Sprint(item))
		}
		switch n.Class {
		case "Item_asterisk":
			table := str(arg(n, "opt_table_name", 1))
			matched := false
			for _, rel := range sc.rels {
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				matched = true
				for _, col := range rel.table.Columns {
					if col.Invisible {
						continue
					}
					a.columns = append(a.columns, Column{Name: col.Name, Type: col.Type, Known: true, Nullable: !col.NotNull || rel.nullable})
				}
			}
			if !matched {
				if table != "" {
					return &Error{Message: fmt.Sprintf("Unknown table '%s'", table), Code: 1051, Position: a.ph.Back(n.Start)}
				}
				return &Error{Message: "No tables used", Code: 1096, Position: a.ph.Back(n.Start)}
			}
		case "PTI_expr_with_alias":
			expr := n.Arg("expr")
			t, err := a.expr(sc, expr, "field list")
			if err != nil {
				return err
			}
			name := str(n.Arg("alias"))
			if name == "" {
				name = a.itemName(expr)
			}
			a.columns = append(a.columns, Column{Name: name, Type: t.typ, Known: t.known, Nullable: t.nullable})
		default:
			return fmt.Errorf("analyze: select item not understood: %s", n.Class)
		}
	}
	return nil
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
