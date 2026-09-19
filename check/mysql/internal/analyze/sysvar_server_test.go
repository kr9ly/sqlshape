package analyze

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/catalog"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// Every entry of the generated catalog.SysVars, pinned against mysqld: a session read of
// a GLOBAL-only variable and a global read of a SESSION-only one are the statement's
// 1238, spelt as the server spells it; the matching scope reads run. A 1193 from the
// server would mean the generator listed a variable this build does not compile
// (sysVarDefined, parsegen/sysvars.go).
func TestSysVarScopeServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, testSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	run := func(sql string) (int, string) {
		_, err := conn.ExecContext(ctx, sql)
		if err == nil {
			return 0, ""
		}
		var me *driver.MySQLError
		if !errors.As(err, &me) {
			t.Errorf("%s: %v", sql, err)
			return -1, ""
		}
		return int(me.Number), me.Message
	}
	s := load(t)
	check := func(sql string, want int, wantMsg string) {
		got, msg := run(sql)
		if got < 0 {
			return
		}
		if got != want || (want != 0 && msg != wantMsg) {
			t.Errorf("%s: the server says %d %q, the checker %d %q", sql, got, msg, want, wantMsg)
			return
		}
		_, err := Analyze(s, sql)
		if want == 0 {
			if err != nil {
				t.Errorf("%s: analyzer %v, the server accepts", sql, err)
			}
			return
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != want || e.Message != wantMsg {
			t.Errorf("%s: analyzer %v, want %d %q", sql, err, want, wantMsg)
		}
	}
	for name, scope := range catalog.SysVars {
		session, global := 0, 0
		var sessionMsg, globalMsg string
		switch scope {
		case 'g':
			session, sessionMsg = 1238, fmt.Sprintf("Variable '%s' is a GLOBAL variable", name)
		case 's':
			global, globalMsg = 1238, fmt.Sprintf("Variable '%s' is a SESSION variable", name)
		}
		check("SELECT @@session."+name, session, sessionMsg)
		check("SELECT @@global."+name, global, globalMsg)
	}
}

// The read's own placement and spelling, pinned: the scope error fires wherever the
// read sits (a dead branch, a subquery), @@local means
// @@session, an unqualified read never scope-fails, and an unknown or component
// variable is not judged (the server's 1193 / 1238 there is a plugin's business).
func TestSysVarPlacementServer(t *testing.T) {
	cases := []struct {
		sql  string
		want int
	}{
		{"SELECT @@SESSION.Thread_Stack", 1238},
		{"SELECT @@local.thread_stack", 1238},
		{"SELECT COUNT(@@session.port)", 1238},
		{"SELECT IF(1, 1, @@session.port)", 1238},
		{"SELECT 1 FROM users WHERE FALSE AND @@session.port", 1238},
		{"SELECT (SELECT @@session.port)", 1238},
		{"SELECT @@thread_stack", 0},
		{"SELECT @@timestamp", 0},
		{"SELECT @@session.sql_mode", 0},
		{"SELECT @@global.sql_mode", 0},
		{"SELECT @@session.gtid_owned", 0},
		{"SELECT @@global.gtid_owned", 0},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, testSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	s := load(t)
	for _, c := range cases {
		_, serr := conn.ExecContext(ctx, c.sql)
		var me *driver.MySQLError
		scode := 0
		if errors.As(serr, &me) {
			scode = int(me.Number)
		} else if serr != nil {
			t.Errorf("%s: %v", c.sql, serr)
			continue
		}
		if scode != c.want {
			t.Errorf("%s: the server says %d, the checker %d", c.sql, scode, c.want)
			continue
		}
		_, aerr := Analyze(s, c.sql)
		var e *Error
		acode := 0
		if errors.As(aerr, &e) {
			acode = e.Code
		} else if aerr != nil {
			t.Errorf("%s: analyzer %v", c.sql, aerr)
			continue
		}
		if acode != c.want {
			t.Errorf("%s: analyzer says %d, want %d", c.sql, acode, c.want)
			continue
		}
		if c.want != 0 && me != nil && e.Message != me.Message {
			t.Errorf("%s: analyzer says %q, the server %q", c.sql, e.Message, me.Message)
		}
	}
}

// A function call argument carrying an alias (`f(x AS a)`, the loadable function
// syntax), pinned: a native function refuses it with 1583 (the name lowercased), after
// its own argument count (1582, the name as written); any other name -- a stored
// function, even one that does not exist -- is 1584 before the function is looked up.
// A data dictionary function (catalog.Function.Internal) is 3566 before either check.
func TestFunctionAliasServer(t *testing.T) {
	const ddl = "CREATE TABLE t (a INT, b INT);\nCREATE FUNCTION sf(x INT) RETURNS INT DETERMINISTIC RETURN x + 1;\n"
	cases := []struct {
		sql  string
		want int
		msg  string
	}{
		{"SELECT ABS(3 AS three)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT abs(3 three)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT instr(a 'p1', 'bar') FROM t", 1583, "Incorrect parameters in the call to native function 'instr'"},
		{"SELECT instr('foobar', 'bar' AS p2)", 1583, "Incorrect parameters in the call to native function 'instr'"},
		{"SELECT abs(nosuchcol AS x) FROM t", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT length(concat('a' AS x, 'b'))", 1583, "Incorrect parameters in the call to native function 'concat'"},
		{"SELECT upper(sf(1) AS x)", 1583, "Incorrect parameters in the call to native function 'upper'"},
		{"SELECT 1 UNION SELECT abs(1 AS x)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"INSERT INTO t VALUES (abs(1 AS x), 1)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT Abs(1 AS x, 2)", 1582, "Incorrect parameter count in the call to native function 'Abs'"},
		{"SELECT Abs()", 1582, "Incorrect parameter count in the call to native function 'Abs'"},
		{"SELECT conv(255 AS p1, 10)", 1582, "Incorrect parameter count in the call to native function 'conv'"},
		{"SELECT sf(1 AS x)", 1584, "Incorrect parameters in the call to stored function `sf`"},
		{"SELECT sf(1 x)", 1584, "Incorrect parameters in the call to stored function `sf`"},
		{"SELECT sf(1 AS x, 2)", 1584, "Incorrect parameters in the call to stored function `sf`"},
		{"SELECT nosuchfn(1 AS x)", 1584, "Incorrect parameters in the call to stored function `nosuchfn`"},
		{"SELECT INTERNAL_TABLE_ROWS(NULL, NULL, NULL, NULL)", 3566, "Access to native function 'INTERNAL_TABLE_ROWS' is rejected."},
		{"SELECT internal_table_rows()", 3566, "Access to native function 'internal_table_rows' is rejected."},
		{"SELECT get_dd_create_options(1 AS x)", 3566, "Access to native function 'get_dd_create_options' is rejected."},
		{"SELECT 1 FROM t WHERE FALSE AND INTERNAL_TABLE_ROWS(NULL, NULL, NULL, NULL)", 3566, "Access to native function 'INTERNAL_TABLE_ROWS' is rejected."},
		{"SELECT ICU_VERSION()", 0, ""},
		{"SELECT sf(1)", 0, ""},
		{"SELECT abs(1)", 0, ""},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n"+ddl)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	s, err := schema.Load("-- sqlshape: mysql 8.4\n" + ddl)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		_, serr := conn.ExecContext(ctx, c.sql)
		var me *driver.MySQLError
		scode, smsg := 0, ""
		if errors.As(serr, &me) {
			scode, smsg = int(me.Number), me.Message
		} else if serr != nil {
			t.Errorf("%s: %v", c.sql, serr)
			continue
		}
		if scode != c.want || smsg != c.msg {
			t.Errorf("%s: the server says %d %q, the checker %d %q", c.sql, scode, smsg, c.want, c.msg)
			continue
		}
		_, aerr := Analyze(s, c.sql)
		if c.want == 0 {
			if aerr != nil {
				t.Errorf("%s: analyzer %v, the server accepts", c.sql, aerr)
			}
			continue
		}
		var e *Error
		if !errors.As(aerr, &e) || e.Code != c.want || e.Message != c.msg {
			t.Errorf("%s: analyzer %v, want %d %q", c.sql, aerr, c.want, c.msg)
		}
	}
}
