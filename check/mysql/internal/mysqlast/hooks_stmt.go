package mysqlast

import (
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
)

// Hand-written alternatives for statements whose action fills LEX by hand rather than
// building a parse-tree class: their generic fold is a by-value Struct carrying
// sql_command (the way DROP TRIGGER's already is), or the class the action names.
// They are reached mostly through dumped routine bodies (SHOW CREATE PROCEDURE), where
// the walker skips what it does not analyze; the fold only has to be faithful.
func init() {
	// a statement the action writes into LEX: {sql_command: SQLCOM_*} plus whatever the
	// alternative read, mirroring the fields the fold of DROP TRIGGER and friends carries
	command := func(cmd string, fields ...string) func(kids []Value, at ...int) *Struct {
		return func(kids []Value, at ...int) *Struct {
			st := &Struct{Fields: map[string]Value{"sql_command": Const(cmd)}, Order: []string{"sql_command"}}
			for i, name := range fields {
				st.Order = append(st.Order, name)
				st.Fields[name] = kids[at[i]-1]
			}
			return st
		}
	}

	// execute_var_ident: '@' ident_or_text -> the user variable's name, pushed onto
	// LEX::prepared_stmt_params (EXECUTE stmt USING @a, @b)
	register("execute_var_ident", "'@' ident_or_text", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return field(kids[1], "str"), nil
	})

	// get_diagnostics: GET [CURRENT|STACKED] DIAGNOSTICS ... -> SQLCOM_GET_DIAGNOSTICS
	// over Sql_cmd_get_diagnostics($4) with $2 as the area
	register("get_diagnostics", "GET_SYM which_area DIAGNOSTICS_SYM diagnostics_information", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_GET_DIAGNOSTICS", "which_area", "info")(kids, 2, 4), nil
	})

	// kill: KILL [CONNECTION|QUERY] expr -> SQLCOM_KILL with the id expression
	register("kill", "KILL_SYM kill_option expr", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_KILL", "kill_option", "expr")(kids, 2, 3), nil
	})

	// checksum: CHECKSUM TABLE t1, t2 [QUICK|EXTENDED] -> SQLCOM_CHECKSUM
	register("checksum", "CHECKSUM_SYM table_or_tables table_list opt_checksum_type", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_CHECKSUM", "tables", "flags")(kids, 3, 4), nil
	})

	// install_stmt: INSTALL PLUGIN name SONAME 'lib' -> SQLCOM_INSTALL_PLUGIN
	// (Sql_cmd_install_plugin; INSTALL COMPONENT has a shape of its own)
	register("install_stmt", "INSTALL_SYM PLUGIN_SYM ident SONAME_SYM TEXT_STRING_sys", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_INSTALL_PLUGIN", "name", "soname")(kids, 3, 5), nil
	})

	// analyze_table_stmt -> PT_analyze_table_stmt(no_write_to_binlog, tables, histogram)
	register("analyze_table_stmt", "ANALYZE_SYM opt_no_write_to_binlog table_or_tables table_list opt_histogram", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_analyze_table_stmt", Names: []string{"no_write_to_binlog", "tables", "histogram"},
			Args: []Value{kids[1], kids[3], kids[4]}, Start: n.Start, End: n.End}, nil
	})
	// opt_histogram_num_buckets: WITH n BUCKETS -> n
	register("opt_histogram_num_buckets", "WITH NUM BUCKETS_SYM", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return number(kids[1])
	})
	// opt_histogram: UPDATE HISTOGRAM ON cols [param] -> {command, columns, param}
	register("opt_histogram", "UPDATE_SYM HISTOGRAM_SYM ON_SYM ident_string_list opt_histogram_update_param", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Struct{Fields: map[string]Value{"command": Const("Histogram_command::UPDATE_HISTOGRAM"), "columns": kids[3], "param": kids[4]},
			Order: []string{"command", "columns", "param"}}, nil
	})

	// drop_tablespace_stmt -> the tablespace family's shared SQLCOM_ALTER_TABLESPACE
	register("drop_tablespace_stmt", "DROP TABLESPACE_SYM ident opt_drop_ts_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_ALTER_TABLESPACE")(kids), nil
	})

	// show_keys_stmt -> PT_show_keys(extended, table [in opt_db], where)
	register("show_keys_stmt", "SHOW opt_extended keys_or_index from_or_in table_ident opt_db opt_where_clause", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_show_keys", Names: []string{"extended", "table", "opt_db", "where"},
			Args: []Value{kids[1], kids[4], kids[5], kids[6]}, Start: n.Start, End: n.End}, nil
	})

	// show_columns_stmt -> PT_show_fields(cmd_type, table [in opt_db], wild, where)
	register("show_columns_stmt", "SHOW opt_show_cmd_type COLUMNS from_or_in table_ident opt_db opt_wild_or_where", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "PT_show_fields", Names: []string{"cmd_type", "table", "opt_db", "wild", "where"},
			Args: []Value{kids[1], kids[4], kids[5], field(kids[6], "wild"), field(kids[6], "where")}, Start: n.Start, End: n.End}, nil
	})

	// opt_explain_format: FORMAT = JSON|TRADITIONAL|TREE -> the format's name
	register("opt_explain_format", "FORMAT_SYM EQ ident_or_text", pass(3))

	// create: the alternatives that fill LEX by hand (the tablespace family shares
	// SQLCOM_ALTER_TABLESPACE the way the server's actions do)
	register("create", "CREATE DATABASE opt_if_not_exists ident opt_create_database_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st := command("SQLCOM_CREATE_DB", "name", "options")(kids, 4, 5)
		st.Order = append(st.Order, "if_not_exists")
		st.Fields["if_not_exists"] = kids[2]
		return st, nil
	})
	register("create", "CREATE USER opt_if_not_exists create_user_list default_role_clause require_clause connect_options opt_account_lock_password_expire_options opt_user_attribute", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_CREATE_USER", "users")(kids, 4), nil
	})
	register("create", "CREATE LOGFILE_SYM GROUP_SYM ident ADD lg_undofile opt_logfile_group_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_ALTER_TABLESPACE")(kids), nil
	})
	register("create", "CREATE TABLESPACE_SYM ident opt_ts_datafile_name opt_logfile_group_name opt_tablespace_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_ALTER_TABLESPACE")(kids), nil
	})
	register("create", "CREATE UNDO_SYM TABLESPACE_SYM ident ADD ts_datafile opt_undo_tablespace_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_ALTER_TABLESPACE")(kids), nil
	})
	register("create", "CREATE SERVER_SYM ident_or_text FOREIGN DATA_SYM WRAPPER_SYM ident_or_text OPTIONS_SYM '(' server_options_list ')'", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return command("SQLCOM_CREATE_SERVER")(kids), nil
	})
	// server_option: the CREATE / ALTER SERVER options fill Lex->server_options by hand;
	// nothing reads them, so each folds to its value
	for _, syms := range []string{"USER TEXT_STRING_sys", "HOST_SYM TEXT_STRING_sys", "DATABASE TEXT_STRING_sys",
		"OWNER_SYM TEXT_STRING_sys", "PASSWORD TEXT_STRING_sys", "SOCKET_SYM TEXT_STRING_sys", "PORT_SYM ulong_num"} {
		register("server_option", syms, pass(2))
	}

	// create_user: the user with its authentication attached; the fold keeps the user
	// (the name is what a reader of a CREATE USER list needs, the auth methods are not read)
	register("create_user", "user identification opt_create_user_with_mfa", pass(1))
	register("create_user", "user identified_with_plugin opt_initial_auth", pass(1))
	register("create_user", "user opt_create_user_with_mfa", pass(1))
	register("create_user_list", "create_user", listOf(1))
	register("create_user_list", "create_user_list ',' create_user", appendTo(1, 3))
	// the identification family builds a LEX_MFA (an authentication method); nothing reads
	// its fields, so the fold is a bare marker node
	mfa := build("LEX_MFA")
	register("identified_by_password", "IDENTIFIED_SYM BY TEXT_STRING_password", mfa)
	register("identified_by_random_password", "IDENTIFIED_SYM BY RANDOM_SYM PASSWORD", mfa)
	register("identified_with_plugin", "IDENTIFIED_SYM WITH ident_or_text", mfa)
	register("identified_with_plugin_as_auth", "IDENTIFIED_SYM WITH ident_or_text AS TEXT_STRING_hash", mfa)
	register("identified_with_plugin_by_password", "IDENTIFIED_SYM WITH ident_or_text BY TEXT_STRING_password", mfa)
	register("identified_with_plugin_by_random_password", "IDENTIFIED_SYM WITH ident_or_text BY RANDOM_SYM PASSWORD", mfa)
	register("opt_create_user_with_mfa", "AND_SYM identification", mfa)
	register("opt_create_user_with_mfa", "AND_SYM identification AND_SYM identification", mfa)
	register("opt_initial_auth", "INITIAL_SYM AUTHENTICATION_SYM identified_by_random_password", pass(3))
	register("opt_initial_auth", "INITIAL_SYM AUTHENTICATION_SYM identified_with_plugin_as_auth", pass(3))
	register("opt_initial_auth", "INITIAL_SYM AUTHENTICATION_SYM identified_by_password", pass(3))
}
