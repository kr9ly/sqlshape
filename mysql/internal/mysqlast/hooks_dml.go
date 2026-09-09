package mysqlast

import (
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// Hand-written alternatives reachable from the statements sqlshape reads. Each mirrors
// what the server's action in sql_yacc.yy builds; the comment quotes the rule.
func init() {
	// select_item_list: '*' -> PT_select_item_list holding an Item_asterisk
	register("select_item_list", 2, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "Item_asterisk", Args: []Value{nil, nil}, Start: n.Start, End: n.End}}, nil
	})
	// window_spec_details: opt_existing_window_name opt_partition_clause opt_window_order_by_clause
	// opt_window_frame_clause -> PT_window(partition, order, frame, name); a missing frame is
	// the implicit RANGE UNBOUNDED PRECEDING .. CURRENT ROW (or UNBOUNDED FOLLOWING without ORDER BY)
	register("window_spec_details", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		frame := kids[3]
		if frame == nil {
			end := Const("WBT_UNBOUNDED_FOLLOWING")
			if kids[2] != nil {
				end = Const("WBT_CURRENT_ROW")
			}
			frame = &Node{Class: "PT_frame", Args: []Value{Const("WFU_RANGE"),
				&Node{Class: "PT_borders", Args: []Value{&Node{Class: "PT_border", Args: []Value{Const("WBT_UNBOUNDED_PRECEDING")}}, &Node{Class: "PT_border", Args: []Value{end}}}}, nil, Const("implicit")}}
		}
		return &Node{Class: "PT_window", Args: []Value{kids[1], kids[2], frame, kids[0]}, Start: n.Start, End: n.End}, nil
	})
	// user: CURRENT_USER optional_braces -> the current user
	register("user", 1, constant("CURRENT_USER"))
	// opt_explain_options: the EXPLAIN flavour; sqlshape does not read EXPLAIN, keep the format
	register("opt_explain_options", 0, pass(2))
	register("opt_explain_options", 1, pass(1))
}

func init() {
	// predicate: bit_expr IN_SYM '(' expr ',' expr_list ')' -> Item_func_in([bit_expr, expr, expr_list...], negated)
	register("predicate", 3, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[5].(List)
		return &Node{Class: "Item_func_in", Args: []Value{append(List{kids[0], kids[3]}, l...), Const("false")}, Start: n.Start, End: n.End}, nil
	})
	// predicate: bit_expr not IN_SYM '(' expr ',' expr_list ')' -> Item_func_in(..., true)
	register("predicate", 5, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[6].(List)
		return &Node{Class: "Item_func_in", Args: []Value{append(List{kids[0], kids[4]}, l...), Const("true")}, Start: n.Start, End: n.End}, nil
	})
	// update_list: update_elem | update_list ',' update_elem -> {column_list, value_list}
	register("update_list", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		e, _ := kids[0].(*Struct)
		st := &Struct{Fields: map[string]Value{"column_list": List{e.Fields["column"]}, "value_list": List{e.Fields["value"]}}, Order: []string{"column_list", "value_list"}}
		return st, nil
	})
	register("update_list", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		e, _ := kids[2].(*Struct)
		cols, _ := st.Fields["column_list"].(List)
		vals, _ := st.Fields["value_list"].(List)
		st.Fields["column_list"] = append(cols, e.Fields["column"])
		st.Fields["value_list"] = append(vals, e.Fields["value"])
		return st, nil
	})
	// table_ident: ident '.' ident -> Table_ident(schema, table)
	register("table_ident", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Table_ident", Args: []Value{field(kids[0], "str"), field(kids[2], "str")}, Start: n.Start, End: n.End}, nil
	})
	// sp_name: ident '.' ident | ident -> sp_name(db, name)
	register("sp_name", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Args: []Value{field(kids[0], "str"), field(kids[2], "str")}, Start: n.Start, End: n.End}, nil
	})
	register("sp_name", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Args: []Value{nil, field(kids[0], "str")}, Start: n.Start, End: n.End}, nil
	})
	// schema: ident (checked as a database name)
	register("schema", 0, pass(1))
	// charset_name / collation_name: the name resolved against the registry; keep the name
	register("charset_name", 0, pass(1))
	register("collation_name", 0, pass(1))
	// window_definition: window_name AS window_spec -> the spec, named
	register("window_definition", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		if w, ok := kids[2].(*Node); ok {
			w.Args = append(w.Args[:3:3], kids[0])
			return w, nil
		}
		return kids[2], nil
	})
	// column_attribute_list: column_attribute -> a list of one (enforcement checked by the server)
	register("column_attribute_list", 1, listOf(1))
	// opt_create_table_options_etc: create_table_options opt_create_partitioning_etc
	register("opt_create_table_options_etc", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, ok := kids[1].(*Struct)
		if !ok {
			st = &Struct{Fields: map[string]Value{}}
		}
		if _, dup := st.Fields["opt_create_table_options"]; !dup {
			st.Order = append(st.Order, "opt_create_table_options")
		}
		st.Fields["opt_create_table_options"] = kids[0]
		return st, nil
	})
}

func init() {
	// table_constraint_def: opt_constraint_name constraint_key_type opt_index_name_and_type '('
	// key_list_with_expression ')' opt_index_options -> PT_inline_index_definition(type, name, index type, parts, options)
	register("table_constraint_def", 3, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		name := kids[0]
		var indexType Value
		if st, ok := kids[2].(*Struct); ok {
			if v := st.Fields["0"]; v != nil {
				name = v
			}
			indexType = st.Fields["1"]
		}
		return &Node{Class: "PT_inline_index_definition", Args: []Value{kids[1], name, indexType, kids[4], kids[6]}, Start: n.Start, End: n.End}, nil
	})
	// column_attribute_list: column_attribute_list column_attribute (the server folds an
	// ENFORCED / NOT ENFORCED onto the previous CHECK; the list keeps both)
	register("column_attribute_list", 0, appendTo(1, 2))
	// opt_charset_with_opt_binary: character_set charset_name opt_bin_mod
	register("opt_charset_with_opt_binary", 4, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"charset": kids[1], "force_binary": Const("false"), "binary": kids[2]}, Order: []string{"charset", "force_binary", "binary"}}, nil
	})
	// reference_list: ident -> [Key_part_spec(ident, 0, ORDER_ASC)]
	register("reference_list", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "Key_part_spec", Args: []Value{field(kids[0], "str"), Number(0), Const("ORDER_ASC")}, Start: n.Start, End: n.End}}, nil
	})
	// ident_string_list: ident -> [ident]
	register("ident_string_list", 0, listOf(1))
	// xid: text_string -> XID(gtrid)
	register("xid", 0, build("XID", 1))
	// insert_stmt: INSERT ... SET update_list ... -> PT_insert with the SET pairs as one row
	register("insert_stmt", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		ul, _ := kids[7].(*Struct)
		vr, _ := kids[8].(*Struct)
		iu, _ := kids[9].(*Struct)
		get := func(st *Struct, k string) Value {
			if st == nil {
				return nil
			}
			return st.Fields[k]
		}
		return &Node{Class: "PT_insert", Start: n.Start, End: n.End, Args: []Value{Const("false"), kids[1], kids[2], kids[4], kids[5],
			get(ul, "column_list"), List{get(ul, "value_list")}, nil, get(vr, "table_alias"), get(vr, "column_list"), get(iu, "column_list"), get(iu, "value_list")}}, nil
	})
	// alter_list: alter_list_item | ... | create_table_options_space_separated -> {flags, actions}
	register("alter_list", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"flags": nil, "actions": List{kids[0]}}, Order: []string{"flags", "actions"}}, nil
	})
	register("alter_list", 3, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"flags": nil, "actions": kids[0]}, Order: []string{"flags", "actions"}}, nil
	})
}

func init() {
	// DROP TABLE / DROP VIEW: legacy LEX; keep what the statement says
	register("drop_table_stmt", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Sql_cmd_drop_table", Args: []Value{kids[1], kids[3], kids[4], kids[5]}, Start: n.Start, End: n.End}, nil
	})
	register("drop_view_stmt", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Sql_cmd_drop_view", Args: []Value{kids[2], kids[3], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// view_query_block: query_expression_with_opt_locking_clauses view_check_option
	register("view_query_block", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"query": kids[0], "check_option": kids[1]}, Order: []string{"query", "check_option"}}, nil
	})
	// alter_list: alter_list ',' alter_list_item | alter_list ',' alter_commands_modifier
	register("alter_list", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		acts, _ := st.Fields["actions"].(List)
		st.Fields["actions"] = append(acts, kids[2])
		return st, nil
	})
	register("alter_list", 2, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		fl, _ := st.Fields["flags"].(List)
		st.Fields["flags"] = append(fl, kids[2])
		return st, nil
	})
	// key_part: ident '(' NUM ')' opt_ordering_direction -> PT_key_part_specification(name, direction, prefix length)
	register("key_part", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		length, _ := number(kids[2])
		return &Node{Class: "PT_key_part_specification", Args: []Value{field(kids[0], "str"), kids[4], length}, Start: n.Start, End: n.End}, nil
	})
	// window_frame_extent: window_frame_start -> PT_borders(start, CURRENT ROW)
	register("window_frame_extent", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_borders", Args: []Value{kids[0], &Node{Class: "PT_border", Args: []Value{Const("WBT_CURRENT_ROW")}}}, Start: n.Start, End: n.End}, nil
	})
	// window_func_call: LEAD / LAG '(' expr opt_lead_lag_info ')' opt_null_treatment windowing_clause
	leadLag := func(isLead bool) Hook {
		return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			args := List{kids[2]}
			if st, ok := kids[3].(*Struct); ok {
				if v := st.Fields["offset"]; v != nil {
					args = append(args, v)
				}
				if v := st.Fields["default_value"]; v != nil {
					args = append(args, v)
				}
			}
			return &Node{Class: "Item_lead_lag", Args: []Value{Const(map[bool]string{true: "true", false: "false"}[isLead]), args, kids[5], kids[6]}, Start: n.Start, End: n.End}, nil
		}
	}
	register("window_func_call", 6, leadLag(true))
	register("window_func_call", 7, leadLag(false))
	// alter_algorithm_option_value / alter_lock_option_value: ident -> the matching enum
	register("alter_algorithm_option_value", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return Const("Alter_info::ALTER_TABLE_ALGORITHM_" + upperIdent(kids[0])), nil
	})
	register("alter_lock_option_value", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return Const("Alter_info::ALTER_TABLE_LOCK_" + upperIdent(kids[0])), nil
	})
}

func upperIdent(v Value) string {
	if t, ok := v.(Token); ok {
		s := t.Value
		if s == "" {
			s = t.Text
		}
		return strings.ToUpper(s)
	}
	return "?"
}

func init() {
	// param_marker: PARAM_MARKER -> Item_param(position)
	register("param_marker", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Item_param", Args: []Value{Number(n.Start)}, Start: n.Start, End: n.End}, nil
	})
	// joined_table: table_reference inner_join_type table_reference -> the cross join, attached
	// by the server to the innermost left of the right operand; kept flat here
	register("joined_table", 4, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_cross_join", Args: []Value{kids[0], kids[1], kids[2]}, Start: n.Start, End: n.End}, nil
	})
	// index_hint_definition: USE / IGNORE / FORCE key_or_index index_hint_clause '(' list ')'
	register("index_hint_definition", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Index_hints", Args: []Value{kids[0], kids[2], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	register("index_hint_definition", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Index_hints", Args: []Value{Const("INDEX_HINT_USE"), kids[2], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// common_table_expr: ident opt_derived_column_list AS table_subquery
	register("common_table_expr", 0, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		sub := n.Children[3]
		return &Node{Class: "PT_common_table_expr", Args: []Value{field(kids[0], "str"), sub.Text(b.SQL), Number(sub.Start), kids[3], kids[1]}, Start: n.Start, End: n.End}, nil
	})
	// cast_type: CHAR_SYM opt_field_length opt_charset_with_opt_binary
	register("cast_type", 1, func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		var charset Value
		binary := false
		if st, ok := kids[2].(*Struct); ok {
			charset = st.Fields["charset"]
			binary = st.Fields["binary"] != nil && st.Fields["binary"] != Const("false")
		}
		st := &Struct{Fields: map[string]Value{"target": Const("ITEM_CAST_CHAR"), "length": kids[1], "dec": nil, "charset": charset, "binary": Const(map[bool]string{true: "true", false: "false"}[binary])},
			Order: []string{"target", "length", "dec", "charset", "binary"}}
		return st, nil
	})
}
