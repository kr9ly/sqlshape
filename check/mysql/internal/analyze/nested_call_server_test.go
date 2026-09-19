package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// TestNestedCallsServer measures nestedCallSchema against mysqld: the error each
// statement raises (1442 for the overlap shapes, 1305 for a missing routine, nothing for
// the rest) is the one the checker predicts, an unqualified abs() is the native function
// and db.abs() the stored one, and an AFTER INSERT trigger sees NEW.col of a NOT NULL
// column non-NULL while the server's own NOT NULL check runs before it.
func TestNestedCallsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, nestedCallSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(nestedCallSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	var dbname string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"INSERT INTO t VALUES (1, 1)", "INSERT INTO other VALUES (1, 1)"} {
		// the second one fires other_ai, whose function writes other: 1442, the row is not stored
		_, _ = conn.ExecContext(ctx, sql)
	}

	serverCode := func(err error) int {
		if err == nil {
			return 0
		}
		var me *driver.MySQLError
		if errors.As(err, &me) {
			return int(me.Number)
		}
		return -1
	}
	for _, sql := range []string{
		"CALL p_same_stmt()", "CALL p_same_stmt_nested()", "CALL p_two_stmts()", "CALL p_two_stmts_call()",
		"CALL p_update_then_call()", "CALL p_other()", "CALL p_missing()",
		"SELECT fcall(1) FROM t", "SELECT fnest(1) FROM t", "SELECT fnest(1) FROM other",
		"INSERT INTO other VALUES (2, 1)", "INSERT INTO t VALUES (2, 1)", "INSERT INTO t SELECT id + 10, v FROM other",
		"SELECT " + dbname + ".nope(1)",
	} {
		_, serr := conn.ExecContext(ctx, sql)
		_, cerr := Analyze(s, sql)
		got, want := errCode(cerr), serverCode(serr)
		if sql == "INSERT INTO other VALUES (2, 1)" {
			// the trigger's own 1442 is reported on the trigger's definition (AnalyzeTrigger),
			// not on the statement that fires it, the way a direct own-table write is
			_, terr := AnalyzeTrigger(s, s.Trigger("other_ai"))
			got = errCode(terr)
		}
		if got != want {
			t.Errorf("%s: server %v, checker %v", sql, serr, cerr)
		}
	}

	var native, stored int
	if err := conn.QueryRowContext(ctx, "SELECT abs(-1), "+dbname+".abs(-1)").Scan(&native, &stored); err != nil {
		t.Fatal(err)
	}
	if native != 1 || stored != 42 {
		t.Errorf("abs(-1) = %d, %s.abs(-1) = %d: want the native 1 and the stored 42", native, dbname, stored)
	}

	// AFTER INSERT: NEW.id (AUTO_INCREMENT), NEW.v (NOT NULL DEFAULT) and NEW.w (NOT NULL,
	// given) are all non-NULL; an explicit NULL into v is 1048 before the AFTER trigger runs
	if _, err := conn.ExecContext(ctx, "INSERT INTO nn (w) VALUES (1)"); err != nil {
		t.Fatalf("INSERT INTO nn (w): %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM log2").Scan(&n); err != nil || n != 2 {
		t.Errorf("log2 rows after one INSERT: %d %v, want 2 (BEFORE and AFTER each stored NEW.w)", n, err)
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO nn (v, w) VALUES (NULL, 2)")
	if serverCode(err) != 1048 {
		t.Errorf("INSERT NULL into NOT NULL v: got %v, want 1048", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM log2").Scan(&n); err != nil || n != 2 {
		t.Errorf("log2 rows after the refused INSERT: %d %v, want still 2 (no AFTER trigger ran)", n, err)
	}
}
