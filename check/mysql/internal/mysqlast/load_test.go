package mysqlast

import (
	"strings"
	"testing"
)

// The statements the syntax gaps of the third adversarial round left unbuildable (LOCK
// TABLES, LOAD DATA and SELECT ... INTO OUTFILE with FIELDS / LINES clauses, ALTER VIEW), in
// the spellings mysql-test/t uses, fold to nodes the loader and the analyzer read.
func TestLoadLockOutfileAlterViewFold(t *testing.T) {
	for sql, wants := range map[string][]string{
		"LOCK TABLES t1 WRITE":                     {"lock_tables(tables=[table_lock(table_ident=Table_ident(table=t1), opt_table_alias=nil, lock_option=TL_WRITE_DEFAULT)])"},
		"lock table t1 read":                       {"lock_option=TL_READ_NO_INSERT"},
		"LOCK TABLES t3 WRITE, t2 AS x READ LOCAL": {"table=t3), opt_table_alias=nil, lock_option=TL_WRITE_DEFAULT)", "table=t2), opt_table_alias=x, lock_option=TL_READ)"},
		"UNLOCK TABLES":                            {"unlock("},
		"load data infile '../../std_data/loaddata1.dat' ignore into table t1 fields terminated by ',' LINES STARTING BY ',' (b,c,d)":               {"PT_load_table(", "on_duplicate=On_duplicate::IGNORE_DUP", `opt_field_separators={field_term: String(str=","`, `opt_line_separators={line_start: String(str=","`, "opt_fields_or_vars=[PTI_simple_ident_nospvar_ident(ident=b), PTI_simple_ident_nospvar_ident(ident=c), PTI_simple_ident_nospvar_ident(ident=d)]"},
		"LOAD DATA LOCAL INFILE 'f' REPLACE INTO TABLE t (a, @b) SET c = @b + 1, d = DEFAULT":                                                       {"is_local_file=true", "On_duplicate::REPLACE_DUP", "opt_fields_or_vars=[PTI_simple_ident_nospvar_ident(ident=a), Item_user_var_as_out_param(a=b)]", "opt_set_fields=[PTI_simple_ident_nospvar_ident(ident=c), PTI_simple_ident_nospvar_ident(ident=d)]", "opt_set_exprs=[Item_func_plus(", "Item_default_value()]", "opt_set_expr_strings=[c = @b + 1, d = DEFAULT]"},
		"SELECT a, b INTO OUTFILE '/tmp/x' FIELDS TERMINATED BY ',' OPTIONALLY ENCLOSED BY '\"' ESCAPED BY '\\\\' LINES TERMINATED BY '\\n' FROM t": {"opt_into1=PT_into_destination_outfile(file_name=TEXT_STRING=\"/tmp/x\", charset=nil, field_term={field_term: String(str=\",\"", "enclosed: String(str=\"\\\"\"", "opt_enclosed: true", "escaped: String(", "line_term={line_term: String(str=\"\\n\""},
		"SELECT a FROM t INTO OUTFILE '/tmp/x'":                     {"PT_select_stmt(qe=PT_query_expression(", "into=PT_into_destination_outfile(file_name=TEXT_STRING=\"/tmp/x\""},
		"SELECT a FROM t FOR UPDATE":                                {"PT_select_stmt(qe=PT_query_expression(", "locking=[PT_query_block_locking_clause(", "has_trailing_locking_clauses=true"},
		"SELECT a FROM t FOR SHARE NOWAIT INTO OUTFILE '/tmp/x'":    {"PT_select_stmt(qe=PT_query_expression(", "into=PT_into_destination_outfile(", "locking=[PT_query_block_locking_clause("},
		"SELECT a INTO DUMPFILE '/tmp/x' FROM t LOCK IN SHARE MODE": {"PT_select_stmt(qe=PT_query_expression(", "opt_into1=PT_into_destination_dumpfile(", "locking=[PT_query_block_locking_clause("},
		"ALTER VIEW v1 AS SELECT f2 FROM t1":                        {"Sql_cmd_alter_view(suid=nil, name=Table_ident(table=v1), column_list=[], query=PT_query_expression(", "check_option=VIEW_CHECK_NONE, replace=VIEW_ALTER, algorithm=nil"},
		"ALTER ALGORITHM=TEMPTABLE DEFINER=CURRENT_USER VIEW v1 (x) AS SELECT f2 FROM t1 WITH LOCAL CHECK OPTION": {"Sql_cmd_alter_view(", "column_list=[x]", "check_option=VIEW_CHECK_LOCAL, replace=VIEW_ALTER, algorithm=VIEW_ALGORITHM_TEMPTABLE"},
	} {
		got := Sprint(mustBuild(t, sql))
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in\n%s", sql, want, got)
			}
		}
	}
}
