package analyze

// Adversarial round 3, lane "newcode": round 2 fixed one form of "the same name declared
// twice in one block" -- DECLARE ... of a variable (1331, TestAdv2DuplicateVariableRefusedByServer,
// body.go's declareVar). MySQL raises the identical shape of error for a DECLARE ...
// CONDITION FOR, a DECLARE ... CURSOR FOR, and a DECLARE ... HANDLER FOR whose own condition
// set overlaps another handler's in the same block -- three siblings of the fixed case, each
// with its own error number, and none of the three registration paths that grew alongside
// declareVar (declareCondition, declareCursor, walkBlock's own handler collection) got the
// same duplicate check. See scratchpad/adv3/brief-newcode.md and
// scratchpad/adv3/adv3-newcode-report.md.

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

const adv3DupDeclSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
`

// ---------------------------------------------------------------------------------------
// Finding: two DECLARE ... CONDITION FOR of the same name in one block are refused by the
// server at CREATE time (1332 "Duplicate condition: cond1", measured below) -- the
// CONDITION sibling of the DECLARE variable case round 2 already fixed (1331,
// TestAdv2DuplicateVariableRefusedByServer). declareCondition (body.go) writes straight into
// a.vars.conds[name] with no check for an existing entry (unlike declareVar's own dup check,
// added for 1331), so two DECLARE ... CONDITION FORs of the same name silently shadow each
// other in the checker's bookkeeping instead of being reported. Suspect:
// check/mysql/internal/analyze/body.go's declareCondition and walkDecl's "sp_decl_condition"
// case, which never compares against what a.vars.conds already holds for the current block.
func TestAdv3DuplicateConditionRefusedByServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const dupCondSchema = adv3DupDeclSchema + `
CREATE PROCEDURE p_dupcond()
BEGIN
  DECLARE cond1 CONDITION FOR SQLSTATE '45000';
  DECLARE cond1 CONDITION FOR SQLSTATE '42000';
END;
`
	// the server: two DECLARE ... CONDITION FORs of "cond1" in the same block are refused
	// outright (1332), measured against mysqld 8.4.11
	_, err := mysqltest.Start(ctx, dupCondSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1332 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1332, got %v", err)
	}

	s, err := schema.Load(dupCondSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a procedure the server refuses at CREATE time with 1332 (duplicate condition); want a problem naming it")
	}
	r := s.RoutineOf(schema.Procedure, "p_dupcond")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1332 {
		t.Errorf("AnalyzeRoutine(p_dupcond) = %v, want a 1332 Error (the server refuses two DECLARE ... CONDITION FORs of the same name in one block)", err)
	}
}

// ---------------------------------------------------------------------------------------
// Finding: two DECLARE ... CURSOR FOR of the same name in one block are refused by the
// server at CREATE time (1333 "Duplicate cursor: cur1", measured below) -- the CURSOR
// sibling of the same 1331 case. declareCursor (body.go) writes straight into
// a.vars.cursors[name] with no check for an existing entry either. Suspect:
// check/mysql/internal/analyze/body.go's declareCursor and walkDecl's "sp_decl_cursor" case.
//
// (Measured separately, not a finding: a variable and a cursor sharing the same name in the
// same block -- `DECLARE cur1 INT; DECLARE cur1 CURSOR FOR SELECT ...;` -- is accepted by the
// server without error: variables and cursors are different namespaces, and the checker
// already keeps them in separate maps (a.vars.vars vs a.vars.cursors), so it already agrees.)
func TestAdv3DuplicateCursorRefusedByServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const dupCursSchema = adv3DupDeclSchema + `
CREATE PROCEDURE p_dupcurs()
BEGIN
  DECLARE cur1 CURSOR FOR SELECT id FROM t;
  DECLARE cur1 CURSOR FOR SELECT id FROM t;
END;
`
	// the server: two DECLARE ... CURSOR FORs of "cur1" in the same block are refused
	// outright (1333), measured against mysqld 8.4.11
	_, err := mysqltest.Start(ctx, dupCursSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1333 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1333, got %v", err)
	}

	s, err := schema.Load(dupCursSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a procedure the server refuses at CREATE time with 1333 (duplicate cursor); want a problem naming it")
	}
	r := s.RoutineOf(schema.Procedure, "p_dupcurs")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1333 {
		t.Errorf("AnalyzeRoutine(p_dupcurs) = %v, want a 1333 Error (the server refuses two DECLARE ... CURSOR FORs of the same name in one block)", err)
	}
}

// ---------------------------------------------------------------------------------------
// Finding: two DECLARE ... HANDLER FOR in the same block whose own condition sets overlap
// (here, both FOR SQLSTATE '45000') are refused by the server at CREATE time (1413
// "Duplicate handler declared in the same block", measured below) -- the HANDLER sibling of
// the same 1331 case, one block further out (the overlap is on what each handler catches,
// not a DECLAREd name). walkBlock's own handler pass (body.go, around line 783) collects
// every sp_decl_handler of the block into a plain `handlers []pendingHandler` slice with no
// comparison between one handler's own resolved condition set and another's in the same
// collection, so two handlers catching the very same condition never conflict in the
// checker's bookkeeping. Suspect: check/mysql/internal/analyze/body.go's walkBlock (the
// handlers collection loop, ~body.go:783-799), which never cross-checks pendingHandler.conds
// between the handlers gathered for one block.
func TestAdv3DuplicateHandlerRefusedByServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const dupHandlerSchema = adv3DupDeclSchema + `
CREATE PROCEDURE p_duphandler()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLSTATE '45000' BEGIN END;
  DECLARE CONTINUE HANDLER FOR SQLSTATE '45000' BEGIN END;
END;
`
	// the server: two HANDLERs for the same SQLSTATE '45000' in the same block are refused
	// outright (1413), measured against mysqld 8.4.11
	_, err := mysqltest.Start(ctx, dupHandlerSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1413 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1413, got %v", err)
	}

	s, err := schema.Load(dupHandlerSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a procedure the server refuses at CREATE time with 1413 (duplicate handler); want a problem naming it")
	}
	r := s.RoutineOf(schema.Procedure, "p_duphandler")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1413 {
		t.Errorf("AnalyzeRoutine(p_duphandler) = %v, want a 1413 Error (the server refuses two overlapping HANDLERs in the same block)", err)
	}
}
