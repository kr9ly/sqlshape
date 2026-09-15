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

// Adversarial round 2, lane "body" (trigger / stored routine / event body analysis:
// check/mysql/internal/analyze/body.go). Every test below reproduces its finding against a
// real mysqld 8.4.11 (nix-shell -p mysql84) before asserting what the checker does; each one
// currently FAILs, showing a gap between the two. See
// scratchpad/adv2/report-body.md for the write-up.

// ---------------------------------------------------------------------------------------
// Finding 1: dynamic SQL (PREPARE / EXECUTE / DEALLOCATE PREPARE) inside a trigger or a
// stored FUNCTION is refused by the server at CREATE time (1336 "Dynamic SQL is not allowed
// in stored function or trigger", measured below); a PROCEDURE is exempt (measured
// separately, not reproduced here since it is not a finding). body.go's walkNode has no
// hook for PT_prepare / PT_execute / PT_deallocate -- they fall through to its own default
// "a body statement this milestone does not walk ... skipped, not an error" (body.go:706),
// so schema.Load accepts such a trigger with zero Problems and AnalyzeTrigger returns no
// error: the checker calls the schema fine although the real CREATE TRIGGER for it fails
// every time. Suspect: check/mysql/internal/analyze/body.go's walkNode (no PT_prepare /
// PT_execute / PT_deallocate case, and no CREATE-time check anywhere in this file), and
// body.go:706 in particular, which currently treats the construct as silently unwalked.
func TestAdv2DynamicSQLInTriggerRefusedByServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const dynamicTriggerSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE TRIGGER trg_dyn BEFORE INSERT ON t FOR EACH ROW
BEGIN
  SET @s = 'SELECT 1';
  PREPARE stmt FROM @s;
  EXECUTE stmt;
  DEALLOCATE PREPARE stmt;
END;
`
	// the server: CREATE TRIGGER with dynamic SQL inside it is refused outright (1336),
	// measured against mysqld 8.4.11
	_, err := mysqltest.Start(ctx, dynamicTriggerSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1336 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1336, got %v", err)
	}

	// the checker: schema.Load accepts the same trigger with no problems, and AnalyzeTrigger
	// finds nothing wrong with its body -- disagreeing with the server that just refused it
	s, err := schema.Load(dynamicTriggerSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a trigger the server refuses at CREATE time with 1336 (dynamic SQL); want a problem naming it")
	}
	tg := s.Trigger("trg_dyn")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if _, err := AnalyzeTrigger(s, tg); errCode(err) != 1336 {
		t.Errorf("AnalyzeTrigger(trg_dyn) = %v, want a 1336 Error (the server refuses CREATE TRIGGER for a body using PREPARE/EXECUTE)", err)
	}
}

// ---------------------------------------------------------------------------------------
// Finding 2: DECLAREing two variables of the same name in the same block is refused by the
// server at CREATE time (1331 "Duplicate variable: x", measured below); a parameter
// re-declared once as a local variable is fine (measured separately: only two DECLAREs of
// the same name in the very same block collide), and a nested block re-declaring an outer
// variable is ordinary shadowing, not a duplicate (measured separately too). body.go's
// declareVar (body.go:100-110) writes straight into a.vars.vars[name] with no check for an
// existing entry, so two DECLAREs of the same name in one block silently shadow each other
// in the checker's own bookkeeping instead of being reported. Suspect:
// check/mysql/internal/analyze/body.go's declareVar and walkDecl's "sp_decl_var" case
// (body.go:100-110, 809-823), which never compares against what a.vars.vars already holds
// for the current (innermost) block before overwriting it.
func TestAdv2DuplicateVariableRefusedByServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const dupVarSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE p_dup()
BEGIN
  DECLARE x INT;
  DECLARE x INT;
END;
`
	// the server: two DECLAREs of "x" in the same block are refused outright (1331),
	// measured against mysqld 8.4.11
	_, err := mysqltest.Start(ctx, dupVarSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1331 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1331, got %v", err)
	}

	// the checker: schema.Load accepts the same procedure with no problems, and
	// AnalyzeRoutine finds nothing wrong with its body
	s, err := schema.Load(dupVarSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a procedure the server refuses at CREATE time with 1331 (duplicate variable); want a problem naming it")
	}
	r := s.RoutineOf(schema.Procedure, "p_dup")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1331 {
		t.Errorf("AnalyzeRoutine(p_dup) = %v, want a 1331 Error (the server refuses two DECLAREs of the same variable name in one block)", err)
	}
}

// ---------------------------------------------------------------------------------------
// Finding 3: a bare RESIGNAL (no named condition) that does carry a `SET MYSQL_ERRNO = n`
// information item changes the number the caller sees to n, not the number the caught
// SIGNAL originally carried (measured below: a handler catching a SIGNAL SQLSTATE '45000'
// SET MYSQL_ERRNO = 30001, then re-raising with RESIGNAL SET MYSQL_ERRNO = 30002, surfaces
// 30002 to CALL's caller -- not 30001). walkSignal's own RESIGNAL-without-condition branch
// (body.go:954-960) ignores the "info" items entirely and re-raises a.handlerRaise
// unchanged, so BodyResult.Violations keeps the original SIGNAL's key (30001) instead of
// the overridden one (30002): a caller matching on the checker's predicted key would watch
// for the wrong number. (A bare RESIGNAL with no SET at all is unaffected and already
// covered by TestResignalReraises; a RESIGNAL SET MESSAGE_TEXT = ... with no MYSQL_ERRNO
// leaves the number as the original SIGNAL's, which the checker already gets right --
// verified against the server, not a finding.) Suspect:
// check/mysql/internal/analyze/body.go's walkSignal (body.go:943-992), specifically the
// `if cond == nil { if n.Class == "sp_resignal" { ... } }` branch at body.go:955-959, which
// never reads a RESIGNAL's own "info" items the way the named-condition branch below it
// reads a SIGNAL's own MYSQL_ERRNO (signalErrno, body.go:994-1010).
func TestAdv2ResignalSetErrnoOverridesNumber(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const resignalOverrideSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE p1()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLSTATE '45000'
  BEGIN
    RESIGNAL SET MYSQL_ERRNO = 30002;
  END;
  SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO = 30001;
END;
`
	db, err := mysqltest.Start(ctx, resignalOverrideSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// the server: CALL p1() raises 30002 (45000), the RESIGNAL's own overridden number, not
	// the original SIGNAL's 30001 -- measured against mysqld 8.4.11
	_, execErr := db.Conn().ExecContext(ctx, "CALL p1()")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 30002 {
		t.Fatalf("test premise wrong: want the server to raise 30002 from CALL p1(), got %v", execErr)
	}

	s, err := schema.Load(resignalOverrideSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "p1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "30002 30002" {
		t.Errorf("AnalyzeRoutine(p1).Violations = %s, want \"30002 30002\" (the server's own RESIGNAL SET MYSQL_ERRNO overrides the caught SIGNAL's number)", got)
	}
}

// ---------------------------------------------------------------------------------------
// Finding 4: a stored FUNCTION (or a TRIGGER) whose body CALLs a PROCEDURE whose own body
// returns a result set (an INTO-less top-level SELECT) is accepted at CREATE time -- the
// server only catches this at execution, every single time (1415 "Not allowed to return a
// result set from a function"/"... trigger", measured below for both a function calling
// such a procedure and a trigger doing the same). This is the same shape AnalyzeEvent
// already handles for an event naming an unknown table (schema.go's own doc: "the server
// checks nothing of it at CREATE time ... fails at every run ... the checker reports it as
// a schema problem"), but walkCall (body.go:1096-1125) does not look at the called
// PROCEDURE's own result-set-producing statements at all when the caller is itself a
// FUNCTION or a TRIGGER, so AnalyzeRoutine/AnalyzeTrigger return no error for it: the
// checker calls the body fine although every execution of it is certain to fail. Suspect:
// check/mysql/internal/analyze/body.go's walkCall (body.go:1096-1125), which never checks
// resultColumnsOf(cbr) (call.go's own helper, already used by callStmt for a top-level CALL)
// against whether the *caller* is a FUNCTION/TRIGGER body -- and by extension AnalyzeRoutine
// / AnalyzeTrigger, which never re-raise 1415 for a callee found this way.
func TestAdv2CallResultSetProcedureFromFunctionIs1415(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const callResultSetSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE p_rs()
BEGIN
  SELECT 1;
END;
CREATE FUNCTION f_call_rs() RETURNS INT DETERMINISTIC
BEGIN
  CALL p_rs();
  RETURN 1;
END;
`
	db, err := mysqltest.Start(ctx, callResultSetSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// the server: CREATE FUNCTION succeeds (p_rs's own result set is not caught at CREATE
	// time), but every call to f_call_rs() fails with 1415 -- measured against mysqld 8.4.11
	_, execErr := db.Conn().ExecContext(ctx, "SELECT f_call_rs()")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1415 {
		t.Fatalf("test premise wrong: want SELECT f_call_rs() to fail with 1415, got %v", execErr)
	}

	s, err := schema.Load(callResultSetSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.RoutineOf(schema.Function, "f_call_rs")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1415 {
		t.Errorf("AnalyzeRoutine(f_call_rs) = %v, want a 1415 Error (every call to it fails: it CALLs a procedure that returns a result set)", err)
	}
}

// ---------------------------------------------------------------------------------------
// Finding 5: a bare RESIGNAL (no condition, no SET) written outside any active HANDLER --
// directly in a routine's own top-level block, not inside a HANDLER's body -- creates fine
// (the grammar allows RESIGNAL anywhere a statement is allowed) but fails every single
// execution with 1645 ("RESIGNAL when handler not active", measured below). walkSignal's
// RESIGNAL branch (body.go:955-959) reads a.handlerRaise (empty outside a handler body, set
// only by absorb, body.go:791) and re-raises whatever it holds -- nothing, here -- instead
// of recognizing that a RESIGNAL reached outside any handler is itself certain to raise
// 1645. Suspect: check/mysql/internal/analyze/body.go's walkSignal (body.go:943-992), which
// has no notion of "no handler is currently active" to report 1645 by, and AnalyzeRoutine /
// AnalyzeTrigger, which consequently see no error and no Violation for this body at all.
func TestAdv2BareResignalOutsideHandlerIs1645(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const bareResignalSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE p_bare_resignal()
BEGIN
  RESIGNAL;
END;
`
	db, err := mysqltest.Start(ctx, bareResignalSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// the server: CREATE PROCEDURE succeeds, but CALL p_bare_resignal() fails every time
	// with 1645 -- measured against mysqld 8.4.11
	_, execErr := db.Conn().ExecContext(ctx, "CALL p_bare_resignal()")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1645 {
		t.Fatalf("test premise wrong: want CALL p_bare_resignal() to fail with 1645, got %v", execErr)
	}

	s, err := schema.Load(bareResignalSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "p_bare_resignal"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "1645 1645" {
		t.Errorf("AnalyzeRoutine(p_bare_resignal).Violations = %q, want \"1645 1645\" (a RESIGNAL outside any HANDLER fails every call)", got)
	}
}
