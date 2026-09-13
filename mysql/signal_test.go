package mysql_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

// signalSchema has a trigger, a function and a procedure that each SIGNAL depending on the
// value they are given, so a single mysqld exercises every case the runtime's ConstraintError
// mapping has to distinguish: a bare SQLSTATE '45000' (ER_SIGNAL_EXCEPTION, Number 1644), a
// SIGNAL that sets its own MYSQL_ERRNO, a SQLSTATE with no MESSAGE_TEXT (ER_SIGNAL_NOT_FOUND,
// Number 1643 — measured; see the doc comment on wrapErr's 1644/1643 case), and a '01000'
// warning that a statement survives.
const signalSchema = `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  amount INT NOT NULL
);
CREATE TRIGGER orders_check BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.amount = 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trigger 45000';
  ELSEIF NEW.amount = 2 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trigger 30001', MYSQL_ERRNO = 30001;
  ELSEIF NEW.amount = 3 THEN
    SIGNAL SQLSTATE '02000';
  ELSEIF NEW.amount = 4 THEN
    SIGNAL SQLSTATE '01000' SET MESSAGE_TEXT = 'just a warning';
  END IF;
END;
CREATE FUNCTION raise_signal(kind INT) RETURNS INT DETERMINISTIC
BEGIN
  IF kind = 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'function 45000';
  ELSEIF kind = 2 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'function 30001', MYSQL_ERRNO = 30001;
  END IF;
  RETURN kind;
END;
CREATE PROCEDURE raise_signal_proc(IN kind INT)
BEGIN
  IF kind = 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'procedure 45000';
  ELSEIF kind = 2 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'procedure 30001', MYSQL_ERRNO = 30001;
  END IF;
END;
`

var (
	insertOrderAmount = sqlshape.Query[struct{}, struct{ Amount int }](`INSERT INTO orders (amount) VALUES ({{.Amount}})`)
	callFunction      = sqlshape.One[int, struct{ Kind int }](`SELECT raise_signal({{.Kind}})`)
	callProcedure     = sqlshape.Query[struct{}, struct{ Kind int }](`CALL raise_signal_proc({{.Kind}})`)
)

func startSignal(t *testing.T) (context.Context, *mysqltest.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, signalSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

// TestTriggerSignal drives a BEFORE INSERT trigger's SIGNAL through mysql.Exec and checks
// the ConstraintError the runtime maps it to.
func TestTriggerSignal(t *testing.T) {
	ctx, srv := startSignal(t)
	db := srv.Conn()

	// (a) SIGNAL SQLSTATE '45000' with no MYSQL_ERRNO: the server reports 1644
	// (ER_SIGNAL_EXCEPTION); Key is the SQLSTATE itself.
	_, err := mysql.Exec(ctx, db, insertOrderAmount, struct{ Amount int }{1})
	assertSignal(t, "trigger 45000", err, 1644, "45000")
	if !mysql.Violates(err, "45000") {
		t.Fatalf("trigger 45000: Violates(err, %q) = false, err=%v", "45000", err)
	}

	// (b) SIGNAL ... SET MYSQL_ERRNO = 30001: the server reports the given number, not 1644;
	// Key is the number's decimal form since the SQLSTATE stays '45000'.
	_, err = mysql.Exec(ctx, db, insertOrderAmount, struct{ Amount int }{2})
	assertSignal(t, "trigger 30001", err, 30001, "30001")
	if !mysql.Violates(err, "30001") {
		t.Fatalf("trigger 30001: Violates(err, %q) = false, err=%v", "30001", err)
	}

	// (c) SIGNAL SQLSTATE '02000' with no MESSAGE_TEXT: measured as 1643
	// (ER_SIGNAL_NOT_FOUND); Key is the SQLSTATE.
	_, err = mysql.Exec(ctx, db, insertOrderAmount, struct{ Amount int }{3})
	assertSignal(t, "trigger 02000", err, 1643, "02000")
	if !mysql.Violates(err, "02000") {
		t.Fatalf("trigger 02000: Violates(err, %q) = false, err=%v", "02000", err)
	}

	// (d) SIGNAL SQLSTATE '01000' is a warning: the INSERT succeeds.
	if _, err := mysql.Exec(ctx, db, insertOrderAmount, struct{ Amount int }{4}); err != nil {
		t.Fatalf("warning SIGNAL should not fail the statement: %v", err)
	}

	// a plain value raises no SIGNAL at all
	if _, err := mysql.Exec(ctx, db, insertOrderAmount, struct{ Amount int }{5}); err != nil {
		t.Fatalf("no SIGNAL: %v", err)
	}
}

// TestRoutineSignal drives the same three SIGNAL shapes through a stored function (mysql.Get)
// and a stored procedure (mysql.Exec / CALL), the other two entry points ExecOne / Exec cover.
func TestRoutineSignal(t *testing.T) {
	ctx, srv := startSignal(t)
	db := srv.Conn()

	_, err := mysql.Get(ctx, db, callFunction, struct{ Kind int }{1})
	assertSignal(t, "function 45000", err, 1644, "45000")
	if !mysql.Violates(err, "45000") {
		t.Fatalf("function 45000: Violates(err, %q) = false, err=%v", "45000", err)
	}

	_, err = mysql.Get(ctx, db, callFunction, struct{ Kind int }{2})
	assertSignal(t, "function 30001", err, 30001, "30001")
	if !mysql.Violates(err, "30001") {
		t.Fatalf("function 30001: Violates(err, %q) = false, err=%v", "30001", err)
	}

	if n, err := mysql.Get(ctx, db, callFunction, struct{ Kind int }{0}); err != nil || n != 0 {
		t.Fatalf("function no SIGNAL: %d %v", n, err)
	}

	_, err = mysql.Exec(ctx, db, callProcedure, struct{ Kind int }{1})
	assertSignal(t, "procedure 45000", err, 1644, "45000")
	if !mysql.Violates(err, "45000") {
		t.Fatalf("procedure 45000: Violates(err, %q) = false, err=%v", "45000", err)
	}

	_, err = mysql.Exec(ctx, db, callProcedure, struct{ Kind int }{2})
	assertSignal(t, "procedure 30001", err, 30001, "30001")
	if !mysql.Violates(err, "30001") {
		t.Fatalf("procedure 30001: Violates(err, %q) = false, err=%v", "30001", err)
	}

	if _, err := mysql.Exec(ctx, db, callProcedure, struct{ Kind int }{0}); err != nil {
		t.Fatalf("procedure no SIGNAL: %v", err)
	}
}

// assertSignal checks that err is a *mysql.ConstraintError with the given Number and Key.
func assertSignal(t *testing.T, label string, err error, wantNumber uint16, wantKey string) {
	t.Helper()
	var ce *mysql.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("%s: not a ConstraintError: %v", label, err)
	}
	if ce.Number != wantNumber {
		t.Errorf("%s: Number = %d, want %d", label, ce.Number, wantNumber)
	}
	if ce.Key != wantKey {
		t.Errorf("%s: Key = %q, want %q", label, ce.Key, wantKey)
	}
}
