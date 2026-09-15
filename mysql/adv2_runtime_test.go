package mysql_test

// Adversarial pass 2 (runtime lane): probes the MySQL runtime's SIGNAL / constraint-key
// mapping (mysql/errors.go's wrapErr and ConstraintError.Key, mysql/runtime.go's ExecOne)
// against a running mysqld 8.4. See scratchpad/adv2/brief.md for the rules this pass
// follows. Findings are written as tests that state the CORRECT (server-measured) behaviour
// and currently FAIL against mysql/v2 as it stands today.

import (
	"context"
	"errors"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

// adv2ChainSchema: two BEFORE INSERT triggers on different tables that loop back into each
// other (x's trigger inserts into y, y's trigger inserts into x). check/mysql's own
// violations.go documents 1442 ("a trigger writing its own table") as one of the codes its
// Violation.Code carries (see the doc comment on the Violation type, and call.go's
// checkCalledRoutineOverlap, which raises a 1442 Error precisely for a called routine
// writing a table the calling statement already references) -- 1442 is squarely one of the
// runtime's own failure modes to map back to the schema, the same way 1062/1451/1452/1048/
// 3819/1172/a SIGNAL already are.
//
// Measured (adv2's failure lane, check/mysql/internal/analyze/adv2_failure_test.go,
// TestAdv2ChainedTriggerTableReuseIs1442): mysqld raises 1442 at run time for exactly this
// kind of cross-table trigger chain even when the checker's own static analysis does not
// catch it in advance, so an application can reach this error live, not only in theory.
const adv2ChainSchema = `-- sqlshape: mysql 8.4
CREATE TABLE zx (
  id INT NOT NULL PRIMARY KEY
);
CREATE TABLE zy (
  id INT NOT NULL PRIMARY KEY
);
CREATE TRIGGER zx_bi BEFORE INSERT ON zx FOR EACH ROW
BEGIN
  INSERT INTO zy (id) VALUES (NEW.id + 100);
END;
CREATE TRIGGER zy_bi BEFORE INSERT ON zy FOR EACH ROW
BEGIN
  INSERT INTO zx (id) VALUES (NEW.id + 100);
END;
`

var adv2InsertZX = sqlshape.Query[struct{}, struct{ ID int }](`INSERT INTO zx (id) VALUES ({{.ID}})`)

// TestAdv2WrapErrDoesNotMap1442ToConstraintError: mysql/errors.go's wrapErr switches on
// me.Number (1062/1586, 1451/1452/1216/1217, 1048/1364, 3819, 1644/1643) and falls back to
// the SQLSTATE-45-prefix case for a SIGNAL's own error number; 1442's SQLSTATE is "HY000",
// so a live 1442 falls through every case and wrapErr returns the raw *driver.MySQLError
// unwrapped (mysql/errors.go, the switch in wrapErr, no `case 1442` and the `default` arm's
// SQLSTATE-prefix check does not match "HY000"). Unlike every other code the checker's own
// Violation.Code documents, `errors.As(err, &ConstraintError{})` is false for 1442 and
// `mysql.Violates(err, "1442")` can never be true, so an application cannot use the
// documented API to recognize this failure mode even though the checker's own model
// (violations.go, call.go) treats 1442 as one of its own.
func TestAdv2WrapErrDoesNotMap1442ToConstraintError(t *testing.T) {
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, adv2ChainSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, execErr := mysql.Exec(ctx, db.Conn(), adv2InsertZX, struct{ ID int }{1})
	if execErr == nil {
		t.Fatal("server accepted an insert that should always fail through the chained trigger's table reuse (1442) -- test schema/assumption is wrong")
	}
	var ce *mysql.ConstraintError
	if !errors.As(execErr, &ce) {
		t.Errorf("mysql.Exec's error is not a *mysql.ConstraintError for a 1442 failure: %v (want it wrapped, Key \"1442\", like every other documented failure mode)", execErr)
	}
	if !mysql.Violates(execErr, "1442") {
		t.Errorf("mysql.Violates(err, \"1442\") = false for a real 1442 failure: %v", execErr)
	}
}
