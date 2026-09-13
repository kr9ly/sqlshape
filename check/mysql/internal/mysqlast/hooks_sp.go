package mysqlast

import (
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
)

// Hand-written alternatives for CREATE/DROP/ALTER TRIGGER, PROCEDURE and FUNCTION, and the
// body of a stored routine or trigger (the compound statement grammar: BEGIN ... END,
// DECLARE, IF/CASE/LOOP/WHILE/REPEAT, LEAVE/ITERATE, SIGNAL/RESIGNAL, handlers, cursors).
//
// trigger_tail and sf_tail/sp_tail themselves need no hook: they are ActDefault (the server
// writes no action of its own beyond building the LEX in the children), so the generic fold
// already turns them into an Implicit Node named after the rule, holding the children in
// grammar order once those children stop being *Unsupported. That is what this file fixes:
// the leaves and small building blocks the server's action either has no parsegen-readable
// shape for (ActUnknown) or whose auto-derived shape carries no real value (a bookkeeping
// counter, a raw action-text constant) rather than the data the AST needs.
func init() {
	// sp_name: a possibly-qualified routine/trigger name -> Node{db, name}; db is nil when
	// unqualified (the current schema at the time of the statement).
	register("sp_name", "ident '.' ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Names: []string{"db", "name"}, Args: []Value{field(kids[0], "str"), field(kids[2], "str")}, Start: n.Start, End: n.End}, nil
	})
	register("sp_name", "ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_name", Names: []string{"db", "name"}, Args: []Value{nil, field(kids[0], "str")}, Start: n.Start, End: n.End}, nil
	})

	// sp_fdparam / sp_pdparam: one routine parameter. A function parameter has no mode
	// keyword (IN is implied); a procedure parameter's mode is sp_opt_inout's constant.
	register("sp_fdparam", "ident type opt_collate", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_param", Names: []string{"mode", "name", "type"},
			Args: []Value{Const("sp_variable::MODE_IN"), field(kids[0], "str"), kids[1]}, Start: n.Start, End: n.End}, nil
	})
	register("sp_pdparam", "sp_opt_inout ident type opt_collate", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_param", Names: []string{"mode", "name", "type"},
			Args: []Value{kids[0], field(kids[1], "str"), kids[2]}, Start: n.Start, End: n.End}, nil
	})

	// sp_fdparams / sp_pdparams: the parameter list, left-recursive; folded into a List (the
	// server instead counts and registers each parameter in its own parse context).
	register("sp_fdparams", "sp_fdparam", listOf(1))
	register("sp_fdparams", "sp_fdparams ',' sp_fdparam", appendTo(1, 3))
	register("sp_pdparams", "sp_pdparam", listOf(1))
	register("sp_pdparams", "sp_pdparams ',' sp_pdparam", appendTo(1, 3))

	// sp_decls: the routine/trigger body's DECLAREs, left-recursive; the server's own shape
	// (ActStruct summing "vars"/"conds"/"hndlrs"/"curs" counts) does not carry the
	// declarations themselves, since those texts are arithmetic on the previous struct that
	// this package cannot evaluate. Folded into a List of the sp_decl values instead.
	register("sp_decls", "", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) { return List(nil), nil })
	register("sp_decls", "sp_decls sp_decl ';'", appendTo(1, 2))

	// sp_decl: the four DECLARE forms.
	register("sp_decl", "DECLARE_SYM sp_decl_idents type opt_collate sp_opt_default", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		var def Value
		if st, ok := kids[4].(*Struct); ok {
			def = st.Fields["expr"]
		}
		return &Node{Class: "sp_decl_var", Names: []string{"names", "type", "default"}, Args: []Value{kids[1], kids[2], def}, Start: n.Start, End: n.End}, nil
	})
	register("sp_decl", "DECLARE_SYM ident CONDITION_SYM FOR_SYM sp_cond", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_decl_condition", Names: []string{"name", "value"}, Args: []Value{field(kids[1], "str"), kids[4]}, Start: n.Start, End: n.End}, nil
	})
	register("sp_decl", "DECLARE_SYM sp_handler_type HANDLER_SYM FOR_SYM sp_hcond_list sp_proc_stmt", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_decl_handler", Names: []string{"type", "conditions", "body"}, Args: []Value{kids[1], kids[4], kids[5]}, Start: n.Start, End: n.End}, nil
	})
	register("sp_decl", "DECLARE_SYM ident CURSOR_SYM FOR_SYM select_stmt", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_decl_cursor", Names: []string{"name", "query"}, Args: []Value{field(kids[1], "str"), kids[4]}, Start: n.Start, End: n.End}, nil
	})

	// sp_decl_idents: the comma-separated names of one DECLARE (several variables can share
	// a type and default), folded into a List of their names.
	register("sp_decl_idents", "ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{field(kids[0], "str")}, nil
	})
	register("sp_decl_idents", "sp_decl_idents ',' ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[0].(List)
		return append(l, field(kids[2], "str")), nil
	})

	// sp_proc_stmts / sp_proc_stmts1: a sequence of body statements, left-recursive (1
	// requires at least one, used by THEN/ELSE/WHEN/LOOP/WHILE/REPEAT bodies where an empty
	// body is not allowed); folded into a List of the statements.
	register("sp_proc_stmts", "", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) { return List(nil), nil })
	register("sp_proc_stmts", "sp_proc_stmts sp_proc_stmt ';'", appendTo(1, 2))
	register("sp_proc_stmts1", "sp_proc_stmt ';'", listOf(1))
	register("sp_proc_stmts1", "sp_proc_stmts1 sp_proc_stmt ';'", appendTo(1, 2))

	// sp_proc_stmt_statement: a plain SQL statement in the body (INSERT/UPDATE/DELETE/SELECT
	// INTO/SET/CALL and the like): the mid-rule action only resets the parser's lex, so the
	// value is simple_statement's own.
	register("sp_proc_stmt_statement", "simple_statement", pass(1))
	// sp_proc_stmt_return: RETURN expr (stored functions only; a PROCEDURE's RETURN with no
	// expression is a different, plain grammar alternative already handled elsewhere).
	register("sp_proc_stmt_return", "RETURN_SYM expr", build("sp_return", 2))
	// LEAVE / ITERATE name the enclosing (or an outer) label.
	register("sp_proc_stmt_leave", "LEAVE_SYM label_ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_leave", Names: []string{"label"}, Args: []Value{field(kids[1], "str")}, Start: n.Start, End: n.End}, nil
	})
	register("sp_proc_stmt_iterate", "ITERATE_SYM label_ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_iterate", Names: []string{"label"}, Args: []Value{field(kids[1], "str")}, Start: n.Start, End: n.End}, nil
	})

	// sp_labeled_block / sp_labeled_control: `label: BEGIN ... END [label]` / `label: LOOP
	// ... END LOOP [label]` (the trailing label is a repetition the server checks matches;
	// the AST keeps only the label once).
	register("sp_labeled_block", "label_ident ':' sp_block_content sp_opt_label", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_labeled_block", Names: []string{"label", "body"}, Args: []Value{field(kids[0], "str"), kids[2]}, Start: n.Start, End: n.End}, nil
	})
	register("sp_labeled_control", "label_ident ':' sp_unlabeled_control sp_opt_label", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_labeled_control", Names: []string{"label", "body"}, Args: []Value{field(kids[0], "str"), kids[2]}, Start: n.Start, End: n.End}, nil
	})

	// stored_routine_body: AS 'routine_string' is a form this AST does not otherwise model
	// (an external routine body); kept as the literal text rather than left Unsupported.
	register("stored_routine_body", "AS routine_string", pass(2))

	// signal_value / sp_hcond: a bare identifier names a DECLARE CONDITION, resolved later
	// against the enclosing block's declarations (this package does no scope resolution).
	register("signal_value", "ident", conditionName)
	register("sp_hcond", "ident", conditionName)

	// sp_hcond_element: validates and registers the handler in the server's parse context;
	// the value is its condition (sp_hcond passed through unchanged).
	register("sp_hcond_element", "sp_hcond", pass(1))
	// sp_hcond_list: the comma-separated conditions of one HANDLER, folded into a List (the
	// server instead counts them).
	register("sp_hcond_list", "sp_hcond_element", listOf(1))
	register("sp_hcond_list", "sp_hcond_list ',' sp_hcond_element", appendTo(1, 3))

	// signal_stmt / resignal_stmt: SIGNAL / RESIGNAL, folded from their condition value and
	// SET information items (the server's own shape only records the SQLCOM_* code and an
	// unevaluated `NEW_PTNSql_cmd_signal($2,$3)` action-text constant).
	register("signal_stmt", "SIGNAL_SYM signal_value opt_set_signal_information", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_signal", Names: []string{"condition", "info"}, Args: []Value{kids[1], kids[2]}, Start: n.Start, End: n.End}, nil
	})
	register("resignal_stmt", "RESIGNAL_SYM opt_signal_value opt_set_signal_information", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_resignal", Names: []string{"condition", "info"}, Args: []Value{kids[1], kids[2]}, Start: n.Start, End: n.End}, nil
	})

	// signal_information_item_list: the SET item = value list of a SIGNAL/RESIGNAL, folded
	// into a List of {name, expr} nodes (the server's own Set_signal_information is a fixed
	// array indexed by item, mutated in place).
	register("signal_information_item_list", "signal_condition_information_item_name EQ signal_allowed_expr", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "sp_signal_item", Names: []string{"name", "expr"}, Args: []Value{kids[0], kids[2]}, Start: n.Start, End: n.End}}, nil
	})
	register("signal_information_item_list", "signal_information_item_list ',' signal_condition_information_item_name EQ signal_allowed_expr", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[0].(List)
		return append(l, &Node{Class: "sp_signal_item", Names: []string{"name", "expr"}, Args: []Value{kids[2], kids[4]}, Start: n.Start, End: n.End}), nil
	})

	// simple_target_specification: GET DIAGNOSTICS' target, a local variable or a `@user`
	// variable.
	register("simple_target_specification", "ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "sp_variable_ref", Names: []string{"name"}, Args: []Value{field(kids[0], "str")}, Start: n.Start, End: n.End}, nil
	})
	register("simple_target_specification", "'@' ident_or_text", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "user_var_ref", Names: []string{"name"}, Args: []Value{kids[1]}, Start: n.Start, End: n.End}, nil
	})

	// drop_function_stmt: DROP FUNCTION [IF EXISTS] [db.]name -- folded into the same
	// {if_exists, spname} shape drop_procedure_stmt/drop_trigger_stmt already build.
	register("drop_function_stmt", "DROP FUNCTION_SYM if_exists ident '.' ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		spname := &Node{Class: "sp_name", Names: []string{"db", "name"}, Args: []Value{field(kids[3], "str"), field(kids[5], "str")}}
		return &Node{Class: "drop_function_stmt", Names: []string{"if_exists", "spname"}, Args: []Value{kids[2], spname}, Start: n.Start, End: n.End}, nil
	})
	register("drop_function_stmt", "DROP FUNCTION_SYM if_exists ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		spname := &Node{Class: "sp_name", Names: []string{"db", "name"}, Args: []Value{nil, field(kids[3], "str")}}
		return &Node{Class: "drop_function_stmt", Names: []string{"if_exists", "spname"}, Args: []Value{kids[2], spname}, Start: n.Start, End: n.End}, nil
	})
}

// conditionName builds a reference to a DECLARE ... CONDITION FOR ... by name (in a SIGNAL
// value or a HANDLER FOR condition); resolving it against the enclosing block's
// declarations is left to the analyzer.
func conditionName(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
	return &Node{Class: "sp_condition_name", Names: []string{"name"}, Args: []Value{field(kids[0], "str")}, Start: n.Start, End: n.End}, nil
}
