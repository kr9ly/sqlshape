package mysqlast

import "github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"

// LOAD DATA / SELECT ... INTO OUTFILE separators and LOCK TABLES: the server's actions fill
// by-value structs (Field_separators / Line_separators, `$$.cleanup()` and field stores) or
// call add_table_to_list on the LEX, none of which parsegen reads.
func init() {
	// field_term: one FIELDS clause item, as a Struct with the fields sql_yacc.yy stores
	sep := func(fields ...string) Hook {
		return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			st := &Struct{Fields: map[string]Value{}}
			for i := 0; i < len(fields); i += 2 {
				k, v := fields[i], fields[i+1]
				if v == "true" {
					st.Fields[k] = Const("true")
				} else {
					st.Fields[k] = kids[len(kids)-1] // the text_string is always last
				}
				st.Order = append(st.Order, k)
			}
			return st, nil
		}
	}
	register("field_term", "TERMINATED BY text_string", sep("field_term", ""))
	register("field_term", "OPTIONALLY ENCLOSED BY text_string", sep("enclosed", "", "opt_enclosed", "true"))
	register("field_term", "ENCLOSED BY text_string", sep("enclosed", ""))
	register("field_term", "ESCAPED BY text_string", sep("escaped", ""))
	register("line_term", "TERMINATED BY text_string", sep("line_term", ""))
	register("line_term", "STARTING BY text_string", sep("line_start", ""))
	// *_list: merge the items into one Struct (merge_field_separators / merge_line_separators:
	// a later item's field wins)
	merge := func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		out := &Struct{Fields: map[string]Value{}}
		for _, k := range kids {
			st, ok := k.(*Struct)
			if !ok {
				continue
			}
			for _, key := range st.Order {
				if _, seen := out.Fields[key]; !seen {
					out.Order = append(out.Order, key)
				}
				out.Fields[key] = st.Fields[key]
			}
		}
		return out, nil
	}
	register("field_term_list", "field_term_list field_term", merge)
	register("field_term_list", "field_term", pass(1))
	register("line_term_list", "line_term_list line_term", merge)
	register("line_term_list", "line_term", pass(1))
	register("opt_field_term", "", constant("nil"))
	register("opt_field_term", "COLUMNS field_term_list", pass(2))
	register("opt_line_term", "", constant("nil"))
	register("opt_line_term", "LINES line_term_list", pass(2))

	// LOCK TABLES: table_lock is table_ident opt_table_alias lock_option, folded to a
	// node of the three; lock_option to the thr_lock_type the action picks
	register("lock_option", "READ_SYM", constant("TL_READ_NO_INSERT"))
	register("lock_option", "WRITE_SYM", constant("TL_WRITE_DEFAULT"))
	register("lock_option", "READ_SYM LOCAL_SYM", constant("TL_READ"))
	register("table_lock", "table_ident opt_table_alias lock_option", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "table_lock", Names: []string{"table_ident", "opt_table_alias", "lock_option"}, Args: []Value{kids[0], kids[1], kids[2]}, Start: n.Start, End: n.End}, nil
	})
	register("table_lock_list", "table_lock", listOf(1))
	register("table_lock_list", "table_lock_list ',' table_lock", appendTo(1, 3))
}

func init() {
	// LOCK TABLES: the `lock` rule's own action only sets sql_command on the LEX
	register("lock", "LOCK_SYM table_or_tables table_lock_list", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "lock_tables", Names: []string{"tables"}, Args: []Value{kids[2]}, Start: n.Start, End: n.End}, nil
	})
	// LOAD DATA's column list and SET clause: PT_load_table reads $22.set_var_list /
	// .set_expr_list / .set_expr_str_list off the by-value struct load_data_set_list builds
	register("fields_or_vars", "fields_or_vars ',' field_or_var", appendTo(1, 3))
	register("load_data_set_elem", "simple_ident_nospvar equal expr_or_default", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"set_var": kids[0], "set_expr": kids[2], "set_expr_str": Token{Text: n.Text(b.SQL), Start: n.Start, End: n.End}},
			Order: []string{"set_var", "set_expr", "set_expr_str"}}, nil
	})
	setList := func(elems ...Value) *Struct {
		st := &Struct{Fields: map[string]Value{"set_var_list": List{}, "set_expr_list": List{}, "set_expr_str_list": List{}},
			Order: []string{"set_var_list", "set_expr_list", "set_expr_str_list"}}
		for _, e := range elems {
			el, ok := e.(*Struct)
			if !ok {
				continue
			}
			for _, k := range []string{"set_var", "set_expr", "set_expr_str"} {
				st.Fields[k+"_list"] = append(st.Fields[k+"_list"].(List), el.Fields[k])
			}
		}
		return st
	}
	register("load_data_set_list", "load_data_set_elem", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return setList(kids[0]), nil
	})
	register("load_data_set_list", "load_data_set_list ',' load_data_set_elem", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, ok := kids[0].(*Struct)
		if !ok {
			return setList(kids[2]), nil
		}
		el, ok := kids[2].(*Struct)
		if !ok {
			return st, nil
		}
		for _, k := range []string{"set_var", "set_expr", "set_expr_str"} {
			st.Fields[k+"_list"] = append(st.Fields[k+"_list"].(List), el.Fields[k])
		}
		return st, nil
	})
	register("opt_load_data_set_spec", "", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return setList(), nil
	})
}

func init() {
	// select_stmt_with_into: two of its alternatives' PT_select_stmt constructors take the
	// (qe, into) pair by position, which parsegen reads under the wrong parameter names
	// (sql_command, qe); folded here to the names the other alternatives carry (the
	// locking variants below).
	register("select_stmt_with_into", "query_expression into_clause", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_select_stmt", Names: []string{"qe", "into"}, Args: []Value{kids[0], kids[1]}, Start: n.Start, End: n.End}, nil
	})
	// ALTER VIEW: the same view_tail as CREATE VIEW (Sql_cmd_create_view), under an
	// alternative whose action only sets create_view_mode = VIEW_ALTER on the LEX. Folded
	// to the CREATE node's shape under its own class, so the loader replaces the view the
	// way OR REPLACE would, after checking it exists.
	alterView := func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		view, ok := kids[len(kids)-1].(*Node)
		if !ok || view.Class != "Sql_cmd_create_view" {
			return kids[len(kids)-1], nil
		}
		var algorithm Value
		if len(kids) == 4 { // ALTER view_algorithm definer_opt view_tail
			algorithm = kids[1]
		}
		out := &Node{Class: "Sql_cmd_alter_view", Names: append(append([]string(nil), view.Names...), "replace", "algorithm", "definer"),
			Args: append(append([]Value(nil), view.Args...), Const("VIEW_ALTER"), algorithm, kids[len(kids)-2]), Start: n.Start, End: n.End}
		return out, nil
	}
	register("alter_view_stmt", "ALTER view_algorithm definer_opt view_tail", alterView)
	register("alter_view_stmt", "ALTER definer_opt view_tail", alterView)
}

func init() {
	// A trailing locking clause (FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE): the
	// constructor wraps the query in a PT_locking built inline, which parsegen reads as
	// unevaluated text and the generic fold kept as a constant in the query's place (the
	// statement's query, and the tables it reads, were lost). Folded here to the plain
	// PT_select_stmt with the clauses beside it under "locking".
	locking := func(qe, lock, into int) Hook {
		return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			var intoV Value
			if into > 0 {
				intoV = kids[into-1]
			}
			return &Node{Class: "PT_select_stmt", Names: []string{"qe", "into", "locking", "has_trailing_locking_clauses"},
				Args: []Value{kids[qe-1], intoV, kids[lock-1], Const("true")}, Start: n.Start, End: n.End}, nil
		}
	}
	register("select_stmt", "query_expression locking_clause_list", locking(1, 2, 0))
	register("select_stmt_with_into", "query_expression into_clause locking_clause_list", locking(1, 3, 2))
	register("select_stmt_with_into", "query_expression locking_clause_list into_clause", locking(1, 2, 3))
}
