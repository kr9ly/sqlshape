package analyze

import (
	"errors"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// The checker's side of TestFuncValServer: the same rules without a server.
func TestFuncVal(t *testing.T) {
	s, err := schema.Load("-- sqlshape: mysql 8.4\n" +
		"CREATE TABLE t (a INT, b VARBINARY(16), c VARCHAR(40));\n" +
		"CREATE TABLE ft (x INT, txt TEXT, FULLTEXT (txt));\n" +
		"CREATE TABLE ft2 (y INT, doc TEXT, FULLTEXT (doc));\n")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql  string
		want int
		msg  string
	}{
		{"INSERT INTO t (a) VALUES (INET_ATON('122.256'))", 1411, "Incorrect string value: ''122.256'' for function inet_aton"},
		{"INSERT INTO t (b) VALUES (INET6_ATON('1::2::3'))", 1411, "Incorrect string value: ''1::2::3'' for function inet6_aton"},
		{"INSERT INTO t (b) VALUES (UNHEX('GG'))", 1411, "Incorrect string value: ''GG'' for function unhex"},
		{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-02-30', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024-02-30' for function str_to_date"},
		{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01x', '%Y-%m-%d'))", 1292, "Truncated incorrect date value: '2024-01-01x'"},
		{"SELECT UUID_TO_BIN('zz')", 1411, "Incorrect string value: 'zz' for function uuid_to_bin"},
		{"SELECT BIN_TO_UUID(0x11)", 1411, "Incorrect string value: '\\x11' for function bin_to_uuid"},
		{"SELECT PERIOD_ADD(200013, 1)", 1210, "Incorrect arguments to period_add"},
		{"SELECT NAME_CONST('n', 1+1)", 1210, "Incorrect arguments to NAME_CONST"},
		{"SELECT NAME_CONST(NULL, 1)", 1382, "The 'NAME_CONST' syntax is reserved for purposes internal to the MySQL server"},
		{"SELECT 'a' LIKE 'b' ESCAPE 'xx'", 1210, "Incorrect arguments to ESCAPE"},
		{"SELECT NTILE(0) OVER () FROM t", 1210, "Incorrect arguments to ntile"},
		{"SELECT NTH_VALUE(a, 0) OVER () FROM t", 1210, "Incorrect arguments to nth_value"},
		{"SELECT * FROM ft, ft2 WHERE MATCH (txt, doc) AGAINST ('x')", 1210, "Incorrect arguments to MATCH"},
		{"SELECT * FROM ft WHERE MATCH (txt) AGAINST (ft.txt)", 1210, "Incorrect arguments to AGAINST"},
		{"SELECT INET_ATON('122.256')", 0, ""}, // not a strict write
		{"INSERT IGNORE INTO t (a) VALUES (INET_ATON('122.256'))", 0, ""},
		{"SELECT UUID_TO_BIN('zz') FROM t WHERE FALSE", 0, ""}, // per row, over what rows say
		{"SELECT PERIOD_ADD(200012, 1)", 0, ""},
		{"SELECT * FROM ft WHERE MATCH (txt) AGAINST ('x')", 0, ""},
	}
	for _, c := range cases {
		_, err := Analyze(s, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("%s: %v, want ok", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want || e.Message != c.msg {
			t.Errorf("%s: %v, want %d %q", c.sql, err, c.want, c.msg)
		}
	}
	// the per-row placement raises the violation instead
	r, err := Analyze(s, "UPDATE t SET a = INET_ATON('122.256')")
	if err != nil {
		t.Fatalf("per-row: %v", err)
	}
	found := false
	for _, v := range r.Violations {
		if v.Code == 1411 && v.Constraint == "1411" {
			found = true
		}
	}
	if !found {
		t.Errorf("per-row: violations %v, want 1411", r.Violations)
	}
}
