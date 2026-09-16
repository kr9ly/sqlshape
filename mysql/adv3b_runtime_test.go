package mysql_test

// Adversarial pass 3, second half (runtime lane): follow-up on mysql/errors.go's wrapErr
// (the 1442/1172/1369 read-back the 2nd pass's schema work and the checker's own model rely
// on). See scratchpad/adv3/brief-followup-schema-runtime.md (face 6) for the questions this
// file answers. The finding below states the server-measured behavior and currently FAILS
// against mysql/v2 as it stands today.

import (
	"context"
	"errors"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

const adv3bSignalSchema = `-- sqlshape: mysql 8.4
CREATE TABLE sigtest (
  id INT NOT NULL PRIMARY KEY
);
-- sqlshape: error 1062 = duplicate_thing
CREATE TRIGGER sigtest_bi BEFORE INSERT ON sigtest FOR EACH ROW
BEGIN
  IF NEW.id = 300 THEN
    SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO = 1062, MESSAGE_TEXT = 'impersonated dup';
  END IF;
END;
`

var adv3bInsertSig300 = sqlshape.Query[struct{}, struct{}](`INSERT INTO sigtest (id) VALUES (300)`)

// TestAdv3bSignalImpersonatingBuiltinNumberLosesItsKey (severity: medium — a documented
// behavior contract silently breaks): docs/mysql.md ("the body's failure modes" section)
// says "A SIGNAL's key is its MYSQL_ERRNO as decimal text when it sets one, else its
// SQLSTATE (the same rule the runtime reads an error back by)" -- unconditionally, no
// exception carved out for a MYSQL_ERRNO that happens to collide with one of the numbers
// wrapErr's switch already claims for a builtin failure mode (1062/1586, 1451/1452/1216/
// 1217, 1048/1364, 3819, 1369, 1644/1643, 1442/1172).
//
// Measured: mysqld 8.4.11 raises exactly the declared SIGNAL (Error 1062 (23000):
// impersonated dup) for `INSERT INTO sigtest (id) VALUES (300)` -- SQLSTATE 23000, its own
// MYSQL_ERRNO 1062, the message text the SIGNAL itself set (not MySQL's own "Duplicate
// entry ..." wording, so this is genuinely the SIGNAL firing, not an actual duplicate key).
//
// wrapErr (mysql/errors.go)'s switch matches `case 1062, 1586:` before ever reaching the
// SIGNAL-shaped fallback in `default:` (the `strings.HasPrefix(SQLState, "45")` branch,
// whose own comment already anticipates "a builtin number impersonated this way" -- but
// only for a '45'-class SQLSTATE, never reached here since 1062 is caught earlier). Inside
// that case, reDuplicate ("for key '...'") does not match the SIGNAL's own message, so
// ce.Key is left "" instead of "1062" (the decimal MYSQL_ERRNO docs/mysql.md promises). The
// declared `-- sqlshape: error 1062 = duplicate_thing` annotation and `mysql.Violates(err,
// "1062")` -- or `sqlshape.Error("1062")` -- can never see this SIGNAL by its own declared
// key, even though the schema names it that way and the runtime is supposed to read it back
// the same way the schema declares it.
func TestAdv3bSignalImpersonatingBuiltinNumberLosesItsKey(t *testing.T) {
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, adv3bSignalSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, execErr := mysql.Exec(ctx, db.Conn(), adv3bInsertSig300, struct{}{})
	if execErr == nil {
		t.Fatal("server: test premise wrong -- the trigger's own SIGNAL must fail the insert")
	}
	var ce *mysql.ConstraintError
	if !errors.As(execErr, &ce) {
		t.Fatalf("mysql.Exec's error is not a *mysql.ConstraintError for a SIGNAL failure: %v", execErr)
	}
	if ce.Key != "1062" {
		t.Errorf("ConstraintError.Key = %q, want %q (the SIGNAL's own declared MYSQL_ERRNO as decimal text, "+
			"per docs/mysql.md: \"A SIGNAL's key is its MYSQL_ERRNO as decimal text when it sets one\")", ce.Key, "1062")
	}
	if !mysql.Violates(execErr, "1062") {
		t.Errorf("mysql.Violates(err, \"1062\") = false for a SIGNAL that declared MYSQL_ERRNO = 1062 " +
			"(and the schema names it `-- sqlshape: error 1062 = duplicate_thing`); want true")
	}
}
