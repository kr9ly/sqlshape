package analyze

// Adversarial round 3, lane "newcode", follow-up half: face 3 ("本体のエラーの残り"), the items
// brief-followup-newcode.md carries over from brief-newcode.md's own face 3 list that the
// front half did not reach (1331's CONDITION/CURSOR/HANDLER siblings, in adv3_body_test.go,
// were the only face-3 item the front half touched). Every finding below reproduces its
// shape against a real mysqld 8.4.11 (nix-shell -p mysql84) before asserting what the checker
// (check/mysql/internal/analyze) does. See scratchpad/adv3/brief-newcode.md,
// brief-followup-newcode.md and scratchpad/adv3/adv3b-newcode-report.md (the write-up this
// file's findings back).

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

const adv3bBaseSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
`

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: FLUSH (here FLUSH TABLES) inside a stored FUNCTION or a TRIGGER is refused by the
// server at CREATE time every time (1336 "FLUSH is not allowed in stored function or
// trigger", measured below for both shapes) -- the same 1336 family PREPARE/EXECUTE/
// DEALLOCATE PREPARE already carry (round 2's own TestAdv2DynamicSQLInTriggerRefusedByServer),
// and the same "a PROCEDURE is exempt" shape too (measured separately: FLUSH TABLES inside a
// PROCEDURE body is accepted by the server with no error at all, so this is not a finding for
// PROCEDURE). commitCheck (body.go:632-648) is reached for FLUSH the same way it is for
// PREPARE/EXECUTE/DEALLOCATE and COMMIT/DDL (FLUSH folds to a generic {sql_command:
// SQLCOM_FLUSH} Struct, walkOne's own generic case at body.go:594-597 calls commitCheck(class,
// 0) for any such Struct unconditionally), but commitCheck's own class comparisons only
// recognize SQLCOM_PREPARE/SQLCOM_DEALLOCATE_PREPARE/SQLCOM_EXECUTE (its own 1336 branch) and
// a DDL/transaction-control class prefix (its own 1422 branch) -- SQLCOM_FLUSH matches
// neither, so commitCheck's final `return nil` is reached and AnalyzeRoutine/AnalyzeTrigger
// report no error for a body the server refuses to CREATE every time. Suspect:
// check/mysql/internal/analyze/body.go's commitCheck (body.go:632-648), whose own dynamic-SQL
// class list at body.go:636 does not include FLUSH's own SQLCOM_FLUSH.
func TestAdv3BodyFlushInFunctionOrTriggerIs1336(t *testing.T) {
	t.Run("function", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		const flushFuncSchema = adv3bBaseSchema + `
CREATE FUNCTION f_flush() RETURNS INT DETERMINISTIC
BEGIN
  FLUSH TABLES;
  RETURN 1;
END;
`
		_, err := mysqltest.Start(ctx, flushFuncSchema)
		if errors.Is(err, mysqltest.ErrNoServer) {
			t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
		}
		var me *driver.MySQLError
		if !errors.As(err, &me) || me.Number != 1336 {
			t.Fatalf("test premise wrong: want the server to refuse this schema with 1336, got %v", err)
		}

		s, err := schema.Load(flushFuncSchema)
		if err != nil {
			t.Fatalf("schema.Load: %v", err)
		}
		if len(s.Problems) == 0 {
			t.Errorf("schema.Load reports no problems for a function the server refuses at CREATE time with 1336 (FLUSH); want a problem naming it")
		}
		r := s.RoutineOf(schema.Function, "f_flush")
		if r == nil {
			t.Fatal("routine not loaded")
		}
		if _, err := AnalyzeRoutine(s, r); errCode(err) != 1336 {
			t.Errorf("AnalyzeRoutine(f_flush) = %v, want a 1336 Error (the server refuses CREATE FUNCTION for a body using FLUSH)", err)
		}
	})

	t.Run("trigger", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		const flushTrigSchema = adv3bBaseSchema + `
CREATE TRIGGER trg_flush BEFORE INSERT ON t FOR EACH ROW
BEGIN
  FLUSH TABLES;
END;
`
		_, err := mysqltest.Start(ctx, flushTrigSchema)
		if errors.Is(err, mysqltest.ErrNoServer) {
			t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
		}
		var me *driver.MySQLError
		if !errors.As(err, &me) || me.Number != 1336 {
			t.Fatalf("test premise wrong: want the server to refuse this schema with 1336, got %v", err)
		}

		s, err := schema.Load(flushTrigSchema)
		if err != nil {
			t.Fatalf("schema.Load: %v", err)
		}
		if len(s.Problems) == 0 {
			t.Errorf("schema.Load reports no problems for a trigger the server refuses at CREATE time with 1336 (FLUSH); want a problem naming it")
		}
		tg := s.Trigger("trg_flush")
		if tg == nil {
			t.Fatal("trigger not loaded")
		}
		if _, err := AnalyzeTrigger(s, tg); errCode(err) != 1336 {
			t.Errorf("AnalyzeTrigger(trg_flush) = %v, want a 1336 Error (the server refuses CREATE TRIGGER for a body using FLUSH)", err)
		}
	})
}

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a SIGNAL/RESIGNAL whose own SQLSTATE literal is not exactly 5 characters (measured
// below with a 4-digit one, '4500') is refused by the server at CREATE time every time (1407
// "Bad SQLSTATE: '4500'"); a well-formed 5-character SQLSTATE that happens to be
// non-numeric ('ABCDE') is accepted, measured separately, not a finding). walkSignal
// (body.go:1035-1084) reads a SIGNAL's condition through resolveCondValue and only branches on
// whether it names a bare number (1646) or inspects its own two-character class prefix
// (ref.sqlstate[:2], for "01"/"02"); it never validates the literal's own length against the
// 5-character SQLSTATE shape the grammar itself does not enforce, so schema.Load accepts the
// CREATE PROCEDURE outright (zero Problems) and AnalyzeRoutine finds nothing wrong with the
// body, although the real CREATE PROCEDURE for it always fails. Suspect:
// check/mysql/internal/analyze/body.go's walkSignal (body.go:1035-1084) and whatever resolves
// a SIGNAL's own literal SQLSTATE (resolveCondValue / condRef, referenced there), none of
// which checks len(ref.sqlstate) == 5.
func TestAdv3BodyMalformedSQLStateAcceptedAtCreateTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const badStateSchema = adv3bBaseSchema + `
CREATE PROCEDURE p_badstate()
BEGIN
  SIGNAL SQLSTATE '4500';
END;
`
	_, err := mysqltest.Start(ctx, badStateSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1407 {
		t.Fatalf("test premise wrong: want the server to refuse this schema with 1407, got %v", err)
	}

	s, err := schema.Load(badStateSchema)
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("schema.Load reports no problems for a procedure the server refuses at CREATE time with 1407 (malformed SQLSTATE); want a problem naming it")
	}
	r := s.RoutineOf(schema.Procedure, "p_badstate")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	if _, err := AnalyzeRoutine(s, r); errCode(err) != 1407 {
		t.Errorf("AnalyzeRoutine(p_badstate) = %v, want a 1407 Error (the server refuses CREATE PROCEDURE for a SIGNAL naming a malformed SQLSTATE)", err)
	}
}

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a stored FUNCTION whose body RETURNs the result of calling itself is accepted at
// CREATE time by the server (measured: CREATE FUNCTION succeeds outright, unlike a
// PROCEDURE's own direct recursion, which is a run-time-only failure too but through a
// different, allowed-up-to-a-limit mechanism, 1456), yet every single execution fails with
// 1424 "Recursive stored functions and triggers are not allowed." -- measured below calling
// it just one level deep (n=3 calling itself with n=2, whose own base case returns
// immediately). Functions (and triggers) can never recurse at all, regardless of
// max_sp_recursion_depth, unlike a PROCEDURE's own direct or indirect recursion.
//
// walkCall's own recursion check (body.go:1253-1262, "a.routine != nil && r == a.routine" ->
// 1456) exists only for a `CALL` statement, which a FUNCTION never uses to invoke itself --
// expr.go's storedFuncCall (expr.go:521 on) is what types a FUNCTION-call expression
// (including one naming the very FUNCTION whose own body is being walked, the RETURN
// f_rec(n-1) shape here), and it has no analogous check at all: no comparison between the
// callee (r) and a.routine, and even if it did, the right number for that shape is 1424, not
// the CALL-statement branch's own 1456. AnalyzeRoutine therefore returns no error and no
// Violation for a function every execution of which is certain to fail. Suspect:
// check/mysql/internal/analyze/expr.go's storedFuncCall (expr.go:521 on, and its caller
// `call`, expr.go:462-506), which never compares r against a.routine the way walkCall
// (body.go:1253) does for a PROCEDURE's own CALL.
func TestAdv3BodyFunctionDirectRecursionMissing1424(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const funcRecSchema = adv3bBaseSchema + `
CREATE FUNCTION f_rec(n INT) RETURNS INT DETERMINISTIC
BEGIN
  IF n <= 0 THEN
    RETURN 0;
  END IF;
  RETURN f_rec(n - 1);
END;
`
	db, err := mysqltest.Start(ctx, funcRecSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// the server: CREATE FUNCTION succeeds (recursion is not caught at CREATE time), but every
	// call fails with 1424 -- measured against mysqld 8.4.11, one level of recursion is enough
	_, execErr := db.Conn().ExecContext(ctx, "SELECT f_rec(3)")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1424 {
		t.Fatalf("test premise wrong: want SELECT f_rec(3) to fail with 1424, got %v", execErr)
	}

	s, err := schema.Load(funcRecSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.RoutineOf(schema.Function, "f_rec")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	br, aerr := AnalyzeRoutine(s, r)
	if aerr != nil {
		t.Fatalf("AnalyzeRoutine: unexpected CREATE-time error %v", aerr)
	}
	if got := adv2Keys(br.Violations); got != "1424 1424" {
		t.Errorf("AnalyzeRoutine(f_rec).Violations = %q, want \"1424 1424\" (every call to a directly recursive FUNCTION fails, measured)", got)
	}
}

// ---------------------------------------------------------------------------------------
// severity: medium
//
// Finding: a PROCEDURE's own direct recursion is not certain to fail once the schema raises
// max_sp_recursion_depth above its default of 0 -- measured below with `-- sqlshape: server
// max_sp_recursion_depth = 200` (mysqltest.Start passes it straight through to mysqld as
// --max-sp-recursion-depth=200, dialect.Settings/mysqltest.go's own mechanism for any `--
// sqlshape: server <var> = <value>` line) and a procedure recursing 5 levels deep: CALL
// succeeds with no error at all. walkCall's own recursion check (body.go:1253-1262) predicts
// 1456 unconditionally on any direct self-CALL, with no notion of the schema's own declared
// max_sp_recursion_depth (the comment there already says as much: "max_sp_recursion_depth
// defaults to 0 (not a setting sqlshape itself tracks): any actual recursive invocation --
// even this first one -- is refused ... every time"), so AnalyzeRoutine still predicts a
// certain 1456 for a call chain that in fact always succeeds under the raised limit.
// (schema.Load itself also reports a Problem for the setting name, "not a variable sqlshape
// reads for MySQL" -- SettingNames, schema.go:59, lists only sql_mode and
// lower_case_table_names -- a related but separate gap: the setting reaches mysqld fine
// through mysqltest's own dialect.Settings pass-through, which does not go through
// SettingNames at all, but schema.Load's own reading of it does.) Suspect:
// check/mysql/internal/analyze/body.go's walkCall (body.go:1253-1262), which raises 1456
// unconditionally rather than checking any recorded max_sp_recursion_depth (and
// internal/schema/schema.go's SettingNames, schema.go:59, which does not recognize the
// variable at all).
func TestAdv3BodyRecursionDepthSettingIgnoredBy1456(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const recDepthSchema = `-- sqlshape: mysql 8.4
-- sqlshape: server max_sp_recursion_depth = 200
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE p_rec(IN n INT)
BEGIN
  IF n > 0 THEN
    CALL p_rec(n - 1);
  END IF;
END;
`
	db, err := mysqltest.Start(ctx, recDepthSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatalf("Start (server option max_sp_recursion_depth=200): %v", err)
	}
	defer db.Close()

	// the server: 5 levels of direct recursion under max_sp_recursion_depth=200 succeeds --
	// measured against mysqld 8.4.11
	if _, execErr := db.Conn().ExecContext(ctx, "CALL p_rec(5)"); execErr != nil {
		t.Fatalf("test premise wrong: want CALL p_rec(5) to succeed under max_sp_recursion_depth=200, got %v", execErr)
	}

	s, err := schema.Load(recDepthSchema)
	if err != nil {
		t.Fatal(err)
	}
	r := s.RoutineOf(schema.Procedure, "p_rec")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	br, aerr := AnalyzeRoutine(s, r)
	if aerr != nil {
		t.Fatalf("AnalyzeRoutine: unexpected CREATE-time error %v", aerr)
	}
	for _, v := range br.Violations {
		if v.Code == 1456 {
			t.Errorf("AnalyzeRoutine(p_rec).Violations includes a 1456, but the server's own CALL p_rec(5) succeeds under this schema's max_sp_recursion_depth=200 (measured); want no 1456 here")
		}
	}
}

// ---------------------------------------------------------------------------------------
// severity: low
//
// Finding: a SIGNAL naming an explicit `SET MYSQL_ERRNO = 0` is not, in fact, certain to
// raise the SQLSTATE's own default number (1644 for an arbitrary SQLSTATE without its own
// number, the checker's own prediction here) -- measured below: the server instead raises
// 1231 "Variable 'MYSQL_ERRNO' can't be set to the value of '0'" every time (MYSQL_ERRNO must
// be in 1..65535; CREATE PROCEDURE itself is unaffected, only every CALL fails, and always
// with 1231, not the SIGNAL's own declared SQLSTATE/number at all).
//
// signalErrno (body.go:1127-1142) returns (0, true) for an explicit `SET MYSQL_ERRNO = 0` --
// indistinguishable from (0, false), which walkSignal's own caller only reads back through
// `errno, _ := signalErrno(items)` (body.go:1067), discarding the second return value
// entirely. `if code == 0 { ... default to 1643/1644 ... }` (body.go:1069-1074) then cannot
// tell "no MYSQL_ERRNO given" (for which 1644 is the server's own default, correct) from "
// MYSQL_ERRNO explicitly given as 0" (for which the server never even reaches the SQLSTATE's
// own number, raising 1231 instead, unconditionally). Suspect:
// check/mysql/internal/analyze/body.go's walkSignal (body.go:1067-1077), whose `errno, _ :=
// signalErrno(items)` throws away the very bit (signalErrno's own second return value) that
// would let it single out this case.
func TestAdv3BodySignalMysqlErrnoZeroIs1231(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const errnoZeroSchema = adv3bBaseSchema + `
CREATE PROCEDURE p_errno0()
BEGIN
  SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO = 0;
END;
`
	db, err := mysqltest.Start(ctx, errnoZeroSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// the server: CREATE PROCEDURE succeeds, but CALL always fails with 1231, not the
	// SIGNAL's own SQLSTATE/number -- measured against mysqld 8.4.11
	_, execErr := db.Conn().ExecContext(ctx, "CALL p_errno0()")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1231 {
		t.Fatalf("test premise wrong: want CALL p_errno0() to fail with 1231, got %v", execErr)
	}

	s, err := schema.Load(errnoZeroSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.RoutineOf(schema.Procedure, "p_errno0")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	br, aerr := AnalyzeRoutine(s, r)
	if aerr != nil {
		t.Fatalf("AnalyzeRoutine: unexpected CREATE-time error %v", aerr)
	}
	if got := adv2Keys(br.Violations); got != "1231 1231" {
		t.Errorf("AnalyzeRoutine(p_errno0).Violations = %q, want \"1231 1231\" (a SIGNAL with SET MYSQL_ERRNO = 0 always fails 1231, never the SQLSTATE's own default number, measured)", got)
	}
}
