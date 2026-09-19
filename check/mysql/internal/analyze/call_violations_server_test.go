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

// TestFunctionCallViolationsServer checks the checker's predicted failure modes for a
// statement calling a schema FUNCTION -- its own SIGNAL, and its own embedded write's
// schema violation -- against what mysqld actually raises, and that a function writing a
// table the statement itself references (1442) is refused the same way at execution.
func TestFunctionCallViolationsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, callSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(callSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO widgets VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}

	// the function's own SIGNAL
	r, err := Analyze(s, "SELECT next_total($1)")
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(r.Violations); got != "30001 30001=TooBig" {
		t.Fatalf("predicted %s, want the function's own SIGNAL", got)
	}
	var x float64
	execErr := conn.QueryRowContext(ctx, "SELECT next_total(?)", 2000000).Scan(&x)
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 30001 || string(me.SQLState[:]) != "45000" {
		t.Errorf("SELECT next_total(2000000): got %v, want 30001 (45000)", execErr)
	}
	if err := conn.QueryRowContext(ctx, "SELECT next_total(?)", 5).Scan(&x); err != nil {
		t.Errorf("SELECT next_total(5): got %v, want no error", err)
	}

	// a function writing the table the statement itself references: 1442, predicted as an
	// Error rather than a Violation (the collision is certain, not a "may")
	if _, err := Analyze(s, "SELECT v, bump_widget(v) FROM widgets WHERE id = $1"); err == nil {
		t.Fatal("want a 1442 Error, got none")
	} else if ae, ok := err.(*Error); !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error", err)
	}
	rows, execErr := conn.QueryContext(ctx, "SELECT v, bump_widget(v) FROM widgets WHERE id = ?", 1)
	if execErr == nil {
		for rows.Next() {
		}
		execErr = rows.Err()
	}
	if !errors.As(execErr, &me) || me.Number != 1442 {
		t.Errorf("SELECT v, bump_widget(v) FROM widgets: got %v, want 1442", execErr)
	}
}

// TestCallStmtViolationsServer checks a CALL's own predicted SIGNAL and result columns
// against what mysqld actually raises and returns.
func TestCallStmtViolationsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, callSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(callSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()

	r, err := Analyze(s, "CALL do_signal($1, @out)")
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(r.Violations); got != "30002 30002=TooBig2" {
		t.Fatalf("predicted %s, want the procedure's own SIGNAL", got)
	}
	_, execErr := conn.ExecContext(ctx, "CALL do_signal(?, @out)", 200)
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 30002 || string(me.SQLState[:]) != "45000" {
		t.Errorf("CALL do_signal(200, @out): got %v, want 30002 (45000)", execErr)
	}
	if _, err := conn.ExecContext(ctx, "CALL do_signal(?, @out)", 5); err != nil {
		t.Errorf("CALL do_signal(5, @out): got %v, want no error", err)
	}

	// the CALL's own predicted result columns match what the server actually returns
	r, err = Analyze(s, "CALL report($1)")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, "CALL report(?)", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	if len(cts) != len(r.Columns) {
		t.Fatalf("server returns %d columns, predicted %d", len(cts), len(r.Columns))
	}
	for i, ct := range cts {
		if ct.Name() != r.Columns[i].Name {
			t.Errorf("column %d: server name %q, predicted %q", i, ct.Name(), r.Columns[i].Name)
		}
	}
}
