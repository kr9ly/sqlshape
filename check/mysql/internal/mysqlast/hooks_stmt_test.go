package mysqlast

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
)

// The statement-level constructs whose server action fills LEX by hand fold to a Struct
// carrying sql_command (or the class the action names), so a dumped routine body that
// carries one of them folds instead of failing as *Unsupported.
func TestStatementFolds(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"KILL QUERY 5", []string{"sql_command: SQLCOM_KILL", "kill_option: QUERY"}},
		{"CHECKSUM TABLE t1, t2 EXTENDED", []string{"sql_command: SQLCOM_CHECKSUM", "Table_ident(table=t1)", "Table_ident(table=t2)"}},
		{"INSTALL PLUGIN foo SONAME 'foo.so'", []string{"sql_command: SQLCOM_INSTALL_PLUGIN"}},
		{"ANALYZE TABLE t UPDATE HISTOGRAM ON a, b WITH 8 BUCKETS", []string{"PT_analyze_table_stmt(", "command: Histogram_command::UPDATE_HISTOGRAM", "columns: [a, b]"}},
		{"SHOW FULL COLUMNS FROM t IN db LIKE 'x%'", []string{"PT_show_fields("}},
		{"CREATE DATABASE IF NOT EXISTS d DEFAULT CHARACTER SET utf8mb4", []string{"sql_command: SQLCOM_CREATE_DB", "name: d"}},
		{"CREATE USER u@'%' IDENTIFIED BY 'x'", []string{"sql_command: SQLCOM_CREATE_USER"}},
		{"CREATE USER v IDENTIFIED WITH caching_sha2_password", []string{"sql_command: SQLCOM_CREATE_USER"}},
		{"CREATE TABLESPACE ts ADD DATAFILE 'f.ibd'", []string{"sql_command: SQLCOM_ALTER_TABLESPACE"}},
		{"CREATE SERVER s FOREIGN DATA WRAPPER mysql OPTIONS (HOST 'h')", []string{"sql_command: SQLCOM_CREATE_SERVER"}},
		{"GET DIAGNOSTICS @n = NUMBER, @r = ROW_COUNT", []string{"sql_command: SQLCOM_GET_DIAGNOSTICS", "Statement_information("}},
		{"GET STACKED DIAGNOSTICS CONDITION 1 @t = MESSAGE_TEXT", []string{"which_area: Diagnostics_information::STACKED_AREA", "Condition_information("}},
		// EXECUTE ... USING pushes each @var's name onto LEX::prepared_stmt_params
		{"EXECUTE stmt USING @a, @b", []string{`"a"`, `"b"`}},
		// INSERT / REPLACE ... SET fold to the same PT_insert the VALUES form builds,
		// with the SET pairs as the single row
		{"REPLACE INTO t SET a = 1, b = 2", []string{"PT_insert(is_replace=true", "column_list=[PTI_simple_ident_nospvar_ident(ident=a), PTI_simple_ident_nospvar_ident(ident=b)]", "row_value_list=[[Item_int("}},
		{"INSERT INTO t SET a = 1 ON DUPLICATE KEY UPDATE b = 2", []string{"PT_insert(is_replace=false", "opt_on_duplicate_column_list=[PTI_simple_ident_nospvar_ident(ident=b)]"}},
		{"SELECT db.t.* FROM db.t", []string{`Item_asterisk(opt_schema_name="db", opt_table_name="t")`}},
		{"SELECT JSON_ARRAYAGG(a) FROM t", []string{"Item_sum_json_array(a=PTI_in_sum_expr(expr=PTI_simple_ident_ident(ident=a))"}},
		{"SELECT JSON_OBJECTAGG(k, v) FROM t", []string{"Item_sum_json_object(key=PTI_in_sum_expr(expr=PTI_simple_ident_ident(ident=k)), value=PTI_in_sum_expr(expr=PTI_simple_ident_ident(ident=v))"}},
		{"SELECT * FROM JSON_TABLE(j, '$[*]' COLUMNS (x INT PATH '$.x' DEFAULT '0' ON EMPTY)) AS jt",
			[]string{`PT_json_table_column_with_path(name="x"`}},
		// name_list: multi-column KEY partitioning and RANGE COLUMNS
		{"CREATE TABLE t (a INT, b INT) PARTITION BY KEY (a, b) PARTITIONS 2", []string{`opt_columns=["a", "b"]`}},
		{"CREATE TABLE t (a INT, b INT) PARTITION BY RANGE COLUMNS (a, b) (PARTITION p0 VALUES LESS THAN (1, 2))", []string{`columns=["a", "b"]`}},
		// ternary_option: 0 / 1 are OFF / ON
		{"CREATE TABLE t (a INT) PACK_KEYS=0 STATS_PERSISTENT=1", []string{"PT_create_pack_keys_option(value=Ternary_option::OFF)", "PT_create_stats_persistent_option(value=Ternary_option::ON)"}},
	}
	for _, c := range cases {
		got := Sprint(mustBuild(t, c.sql))
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s:\nmissing %q in\n%s", c.sql, want, got)
			}
		}
	}
}

// A body carrying these statements folds too (the dumped SHOW CREATE PROCEDURE case).
func TestStatementFoldsInBody(t *testing.T) {
	for _, sql := range []string{
		"CREATE PROCEDURE p() BEGIN DECLARE n INT; GET DIAGNOSTICS n = NUMBER; END",
		"CREATE PROCEDURE p() BEGIN PREPARE s FROM 'SELECT ?'; EXECUTE s USING @a; END",
		"CREATE PROCEDURE p() KILL CONNECTION 7",
		"CREATE PROCEDURE p() CHECKSUM TABLE t",
		"CREATE PROCEDURE p() ANALYZE TABLE t",
		"CREATE PROCEDURE p() REPLACE INTO t SET a = 1",
		"CREATE PROCEDURE p() CREATE DATABASE d",
	} {
		mustBuild(t, sql)
	}
}

// ternary_option refuses what the server's own action does: any value but 0 or 1 is its
// syntax error, kept explicit here as *Unsupported.
func TestTernaryOptionOutOfRange(t *testing.T) {
	sql := "CREATE TABLE t (a INT) PACK_KEYS=2"
	root, err := mysqlparse.Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(sql, root); err == nil {
		t.Errorf("want an error for PACK_KEYS=2, got none")
	}
}
