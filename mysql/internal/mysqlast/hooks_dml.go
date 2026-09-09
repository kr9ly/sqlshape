package mysqlast

import (
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// Hand-written alternatives reachable from the statements sqlshape reads. Each mirrors
// what the server's action in sql_yacc.yy builds; the comment quotes the rule.
func init() {
	// select_item_list: '*' -> PT_select_item_list holding an Item_asterisk
	register("select_item_list", "'*'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "Item_asterisk", Args: []Value{nil, nil}, Start: n.Start, End: n.End}}, nil
	})
	// window_spec_details: opt_existing_window_name opt_partition_clause opt_window_order_by_clause
	// opt_window_frame_clause -> PT_window(partition, order, frame, name); a missing frame is
	// the implicit RANGE UNBOUNDED PRECEDING .. CURRENT ROW (or UNBOUNDED FOLLOWING without ORDER BY)
	register("window_spec_details", "opt_existing_window_name opt_partition_clause opt_window_order_by_clause opt_window_frame_clause", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		frame := kids[3]
		if frame == nil {
			end := Const("WBT_UNBOUNDED_FOLLOWING")
			if kids[2] != nil {
				end = Const("WBT_CURRENT_ROW")
			}
			frame = &Node{Class: "PT_frame", Args: []Value{Const("WFU_RANGE"),
				&Node{Class: "PT_borders", Args: []Value{&Node{Class: "PT_border", Args: []Value{Const("WBT_UNBOUNDED_PRECEDING")}}, &Node{Class: "PT_border", Args: []Value{end}}}}, nil, Const("implicit")}}
		}
		return &Node{Class: "PT_window", Names: []string{"partition_by", "order_by", "frame", "inherit"}, Args: []Value{kids[1], kids[2], frame, kids[0]}, Start: n.Start, End: n.End}, nil
	})
	// user: CURRENT_USER optional_braces -> the current user
	register("user", "CURRENT_USER optional_braces", constant("CURRENT_USER"))
	// opt_explain_options: the EXPLAIN flavour; sqlshape does not read EXPLAIN, keep the format
	register("opt_explain_options", "ANALYZE_SYM opt_explain_format", pass(2))
	register("opt_explain_options", "opt_explain_format opt_explain_into", pass(1))
}

func init() {
	// predicate: bit_expr IN_SYM '(' expr ',' expr_list ')' -> Item_func_in([bit_expr, expr, expr_list...], negated)
	register("predicate", "bit_expr IN_SYM '(' expr ',' expr_list ')'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[5].(List)
		return &Node{Class: "Item_func_in", Names: []string{"list", "is_negation"}, Args: []Value{append(List{kids[0], kids[3]}, l...), Const("false")}, Start: n.Start, End: n.End}, nil
	})
	// predicate: bit_expr not IN_SYM '(' expr ',' expr_list ')' -> Item_func_in(..., true)
	register("predicate", "bit_expr not IN_SYM '(' expr ',' expr_list ')'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[6].(List)
		return &Node{Class: "Item_func_in", Names: []string{"list", "is_negation"}, Args: []Value{append(List{kids[0], kids[4]}, l...), Const("true")}, Start: n.Start, End: n.End}, nil
	})
	// update_list: update_elem | update_list ',' update_elem -> {column_list, value_list}
	register("update_list", "update_elem", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		e, _ := kids[0].(*Struct)
		st := &Struct{Fields: map[string]Value{"column_list": List{e.Fields["column"]}, "value_list": List{e.Fields["value"]}}, Order: []string{"column_list", "value_list"}}
		return st, nil
	})
	register("update_list", "update_list ',' update_elem", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		e, _ := kids[2].(*Struct)
		cols, _ := st.Fields["column_list"].(List)
		vals, _ := st.Fields["value_list"].(List)
		st.Fields["column_list"] = append(cols, e.Fields["column"])
		st.Fields["value_list"] = append(vals, e.Fields["value"])
		return st, nil
	})
	// table_ident: ident '.' ident -> Table_ident(schema, table)
	register("table_ident", "ident '.' ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Table_ident", Names: []string{"db", "table"}, Args: []Value{field(kids[0], "str"), field(kids[2], "str")}, Start: n.Start, End: n.End}, nil
	})
	// sp_name: ident '.' ident | ident -> sp_name(db, name)
	register("sp_name", "ident '.' ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Args: []Value{field(kids[0], "str"), field(kids[2], "str")}, Start: n.Start, End: n.End}, nil
	})
	register("sp_name", "ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Args: []Value{nil, field(kids[0], "str")}, Start: n.Start, End: n.End}, nil
	})
	// schema: ident (checked as a database name)
	register("schema", "ident", pass(1))
	// charset_name / collation_name: the name resolved against the registry; keep the name
	register("charset_name", "ident_or_text", pass(1))
	register("collation_name", "ident_or_text", pass(1))
	// window_definition: window_name AS window_spec -> the spec, named
	register("window_definition", "window_name AS window_spec", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		if w, ok := kids[2].(*Node); ok {
			for i, k := range w.Names {
				if k == "name" {
					w.Args[i] = kids[0]
					return w, nil
				}
			}
			w.Names = append(w.Names, "name")
			w.Args = append(w.Args, kids[0])
			return w, nil
		}
		return kids[2], nil
	})
	// column_attribute_list: column_attribute -> a list of one (enforcement checked by the server)
	register("column_attribute_list", "column_attribute", listOf(1))
	// opt_create_table_options_etc: create_table_options opt_create_partitioning_etc
	register("opt_create_table_options_etc", "create_table_options opt_create_partitioning_etc", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
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
	register("table_constraint_def", "opt_constraint_name constraint_key_type opt_index_name_and_type '(' key_list_with_expression ')' opt_index_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		name := kids[0]
		var indexType Value
		if st, ok := kids[2].(*Struct); ok {
			if v := st.Fields["name"]; v != nil {
				name = v
			}
			indexType = st.Fields["type"]
		}
		return &Node{Class: "PT_inline_index_definition", Names: []string{"type_par", "name", "type", "cols", "options"}, Args: []Value{kids[1], name, indexType, kids[4], kids[6]}, Start: n.Start, End: n.End}, nil
	})
	// column_attribute_list: column_attribute_list column_attribute; `CHECK (...) [NOT] ENFORCED`
	// arrives as two attributes and the server folds the enforcement onto the CHECK
	register("column_attribute_list", "column_attribute_list column_attribute", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[0].(List)
		if enf, ok := kids[1].(*Node); ok && enf.Class == "PT_constraint_enforcement_attr" && len(l) > 0 {
			if chk, ok := l[len(l)-1].(*Node); ok && chk.Class == "PT_check_constraint_column_attr" {
				chk.Names = append(chk.Names, "enforced")
				chk.Args = append(chk.Args, enf.Arg("enforced"))
				return l, nil
			}
			return nil, &Unsupported{Rule: n.Kind.String(), Alt: n.Alt, Start: enf.Start, End: enf.End, Text: "ENFORCED without a preceding CHECK"}
		}
		return append(l, kids[1]), nil
	})
	// opt_charset_with_opt_binary: character_set charset_name opt_bin_mod
	register("opt_charset_with_opt_binary", "character_set charset_name opt_bin_mod", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"charset": kids[1], "force_binary": Const("false"), "binary": kids[2]}, Order: []string{"charset", "force_binary", "binary"}}, nil
	})
	// reference_list: ident -> [Key_part_spec(ident, 0, ORDER_ASC)]
	register("reference_list", "ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "Key_part_spec", Names: []string{"column_name", "prefix_length", "order"}, Args: []Value{field(kids[0], "str"), Number(0), Const("ORDER_ASC")}, Start: n.Start, End: n.End}}, nil
	})
	// ident_string_list: ident -> [ident]
	register("ident_string_list", "ident", listOf(1))
	// xid: text_string -> XID(gtrid)
	register("xid", "text_string", build("XID", 1))
	// insert_stmt: INSERT ... SET update_list ... -> PT_insert with the SET pairs as one row
	register("insert_stmt", "INSERT_SYM insert_lock_option opt_ignore opt_INTO table_ident opt_use_partition SET_SYM update_list opt_values_reference opt_insert_update_list", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
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
	register("alter_list", "alter_list_item", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"flags": nil, "actions": List{kids[0]}}, Order: []string{"flags", "actions"}}, nil
	})
	register("alter_list", "create_table_options_space_separated", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"flags": nil, "actions": kids[0]}, Order: []string{"flags", "actions"}}, nil
	})
}

func init() {
	// DROP TABLE / DROP VIEW: legacy LEX; keep what the statement says
	register("drop_table_stmt", "DROP opt_temporary table_or_tables if_exists table_list opt_restrict", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Sql_cmd_drop_table", Names: []string{"temporary", "if_exists", "tables", "restrict"}, Args: []Value{kids[1], kids[3], kids[4], kids[5]}, Start: n.Start, End: n.End}, nil
	})
	register("drop_view_stmt", "DROP VIEW_SYM if_exists table_list opt_restrict", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Sql_cmd_drop_view", Names: []string{"if_exists", "views", "restrict"}, Args: []Value{kids[2], kids[3], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// view_query_block: query_expression_with_opt_locking_clauses view_check_option
	register("view_query_block", "query_expression_with_opt_locking_clauses view_check_option", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"query": kids[0], "check_option": kids[1]}, Order: []string{"query", "check_option"}}, nil
	})
	// alter_list: alter_list ',' alter_list_item | alter_list ',' alter_commands_modifier
	register("alter_list", "alter_list ',' alter_list_item", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		acts, _ := st.Fields["actions"].(List)
		st.Fields["actions"] = append(acts, kids[2])
		return st, nil
	})
	register("alter_list", "alter_list ',' alter_commands_modifier", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		fl, _ := st.Fields["flags"].(List)
		st.Fields["flags"] = append(fl, kids[2])
		return st, nil
	})
	// key_part: ident '(' NUM ')' opt_ordering_direction -> PT_key_part_specification(name, direction, prefix length)
	register("key_part", "ident '(' NUM ')' opt_ordering_direction", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		length, _ := number(kids[2])
		return &Node{Class: "PT_key_part_specification", Names: []string{"column_name", "order", "prefix_length"}, Args: []Value{field(kids[0], "str"), kids[4], length}, Start: n.Start, End: n.End}, nil
	})
	// window_frame_extent: window_frame_start -> PT_borders(start, CURRENT ROW)
	register("window_frame_extent", "window_frame_start", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
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
	register("window_func_call", "LEAD_SYM '(' expr opt_lead_lag_info ')' opt_null_treatment windowing_clause", leadLag(true))
	register("window_func_call", "LAG_SYM '(' expr opt_lead_lag_info ')' opt_null_treatment windowing_clause", leadLag(false))
	// alter_algorithm_option_value / alter_lock_option_value: ident -> the matching enum; the
	// server rejects any other word (ER_UNKNOWN_ALTER_ALGORITHM / _LOCK)
	enumOf := func(prefix string, allowed ...string) Hook {
		return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			word := upperIdent(kids[0])
			for _, a := range allowed {
				if word == a {
					return Const(prefix + word), nil
				}
			}
			return nil, &Unsupported{Rule: n.Kind.String(), Alt: n.Alt, Start: n.Start, End: n.End, Text: "unknown " + word}
		}
	}
	register("alter_algorithm_option_value", "ident", enumOf("Alter_info::ALTER_TABLE_ALGORITHM_", "INPLACE", "INSTANT", "COPY"))
	register("alter_lock_option_value", "ident", enumOf("Alter_info::ALTER_TABLE_LOCK_", "NONE", "SHARED", "EXCLUSIVE"))
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
	// constraint_enforcement: opt_not ENFORCED_SYM -> !not
	register("constraint_enforcement", "opt_not ENFORCED_SYM", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		if kids[0] == Const("true") {
			return Const("false"), nil
		}
		return Const("true"), nil
	})
	// param_marker: PARAM_MARKER -> Item_param(position)
	register("param_marker", "PARAM_MARKER", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Item_param", Names: []string{"pos_in_query"}, Args: []Value{Number(n.Start)}, Start: n.Start, End: n.End}, nil
	})
	// joined_table: table_reference inner_join_type table_reference -> PT_cross_join(a, type, b)
	// attached where the server's add_cross_join puts it: at the leftmost table of the right
	// operand, so that `a JOIN b JOIN c ON p` reads ((a JOIN b) JOIN c ON p)
	register("joined_table", "table_reference inner_join_type table_reference", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		cross := func(right Value) Value {
			return &Node{Class: "PT_cross_join", Names: []string{"tab1_node", "type", "tab2_node"}, Args: []Value{kids[0], kids[1], right}, Start: n.Start, End: n.End}
		}
		right, ok := kids[2].(*Node)
		if !ok || !strings.HasPrefix(right.Class, "PT_joined_table") && right.Class != "PT_cross_join" {
			return cross(kids[2]), nil
		}
		// descend along the left operands to the leftmost table reference
		leftmost := right
		for {
			l, ok := leftmost.Arg("tab1_node").(*Node)
			if !ok || !strings.HasPrefix(l.Class, "PT_joined_table") && l.Class != "PT_cross_join" {
				break
			}
			leftmost = l
		}
		for i, k := range leftmost.Names {
			if k == "tab1_node" {
				leftmost.Args[i] = cross(leftmost.Args[i])
				return right, nil
			}
		}
		return cross(kids[2]), nil
	})
	// index_hint_definition: USE / IGNORE / FORCE key_or_index index_hint_clause '(' list ')'
	register("index_hint_definition", "index_hint_type key_or_index index_hint_clause '(' key_usage_list ')'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Index_hints", Args: []Value{kids[0], kids[2], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	register("index_hint_definition", "USE_SYM key_or_index index_hint_clause '(' opt_key_usage_list ')'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Index_hints", Args: []Value{Const("INDEX_HINT_USE"), kids[2], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// common_table_expr: ident opt_derived_column_list AS table_subquery
	register("common_table_expr", "ident opt_derived_column_list AS table_subquery", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		sub := n.Children[3]
		return &Node{Class: "PT_common_table_expr", Args: []Value{field(kids[0], "str"), sub.Text(b.SQL), Number(sub.Start), kids[3], kids[1]}, Start: n.Start, End: n.End}, nil
	})
	// cast_type: CHAR_SYM opt_field_length opt_charset_with_opt_binary
	register("cast_type", "CHAR_SYM opt_field_length opt_charset_with_opt_binary", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
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

func init() {
	// CREATE VIEW is legacy LEX all the way down: view_tail names the view and its column
	// list, view_query_block carries the query and the check option, the outer alternative
	// the OR REPLACE / ALGORITHM / DEFINER. They fold into one node here.
	register("view_tail", "view_suid VIEW_SYM table_ident opt_derived_column_list AS view_query_block", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		var query, check Value
		if st, ok := kids[5].(*Struct); ok {
			query, check = st.Fields["query"], st.Fields["check_option"]
		}
		var suid Value
		if st, ok := kids[0].(*Struct); ok {
			suid = st.Fields["create_view_suid"]
		}
		return &Node{Class: "Sql_cmd_create_view", Names: []string{"suid", "name", "column_list", "query", "check_option"},
			Args: []Value{suid, kids[2], kids[3], query, check}, Start: n.Start, End: n.End}, nil
	})
	viewHead := func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		view, ok := kids[len(kids)-1].(*Node)
		if !ok || view.Class != "Sql_cmd_create_view" {
			return kids[len(kids)-1], nil // a trigger, routine or event: left as the server's own nodes
		}
		var mode, algorithm Value
		if len(kids) == 4 {
			if st, ok := kids[0].(*Struct); ok {
				mode, algorithm = st.Fields["create_view_mode"], st.Fields["create_view_algorithm"]
			}
		}
		view.Names = append(view.Names, "replace", "algorithm", "definer")
		view.Args = append(view.Args, mode, algorithm, kids[len(kids)-3])
		view.Start, view.End = n.Start, n.End
		return view, nil
	}
	register("view_or_trigger_or_sp_or_event", "view_replace_or_algorithm definer_opt init_lex_create_info view_tail", viewHead)
	register("view_or_trigger_or_sp_or_event", "definer init_lex_create_info definer_tail", viewHead)
	register("view_or_trigger_or_sp_or_event", "no_definer init_lex_create_info no_definer_tail", viewHead)
}
