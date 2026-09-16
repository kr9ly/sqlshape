package analyze

// Adversarial round 3, lane "newcode", second half, face 2 (the 1442 chain): round 2 pinned
// "a trigger chain as a whole" and "a called routine's write reaching a table through the
// trigger it fires" (adv2_calls_test.go, adv2_failure_test.go). This file probes the rungs
// around those against a running mysqld 8.4: a CALL inside a trigger whose callee's write
// cascades through another trigger back into the caller's own table, a locking read
// (SELECT ... FOR UPDATE) of a table already in use, and a recursive CALL sitting inside a
// trigger. See scratchpad/adv3/brief-followup-newcode.md and
// scratchpad/adv3/adv3b-newcode-report.md (which also lists the faces measured here that
// agreed with the server and got no test).

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a trigger's own CALL to a procedure, whose write cascades through another
// table's trigger back into the CALLing trigger's own table, is a certain 1442 on the real
// server (measured below: INSERT INTO ca_a, with ca_b seeded, is refused every time with
// 1442 naming ca_a) -- yet AnalyzeTrigger(ca_a_bi), called directly, ALREADY computes this
// exact 1442 (checkCalledRoutineOverlap sees ca_write_b's WriteTables, folded transitively
// through ca_b_au's AFTER UPDATE, include ca_a, the calling trigger's own table). It is
// triggerViolations (violations.go), walking ca_a's INSERT-event triggers for the top-level
// `INSERT INTO ca_a`, that throws this correctly computed answer away: `br, err :=
// cachedAnalyzeTrigger(s, tg); if err != nil || br == nil { continue }` (violations.go:214)
// treats ANY error from analyzing a chained trigger's body -- a certain 1442 collision
// included -- as "this trigger contributes nothing", instead of folding the trigger's own
// certain failure into the firing statement's predicted violations. So Analyze("INSERT
// INTO ca_a ...") reports no 1442 at all for a statement mysqld always refuses.
//
// Suspect: check/mysql/internal/analyze/violations.go's triggerViolations, the `if err !=
// nil || br == nil { continue }` at line 214 (the same pattern recurs at
// calledRoutineViolations, line 120, and at body.go's appendTriggerWriteTables, ~1497-1499
// -- none of the three surfaces a chained trigger's own certain failure to its caller).
func TestAdv3ChainCallCascadeThroughAnotherTriggerSwallowed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const callCascadeSchema = `-- sqlshape: mysql 8.4
CREATE TABLE ca_a (id INT NOT NULL PRIMARY KEY, v INT);
CREATE TABLE ca_b (id INT NOT NULL PRIMARY KEY, v INT);
CREATE PROCEDURE ca_write_b(IN x INT)
BEGIN
  UPDATE ca_b SET v = x WHERE id = 1;
END;
CREATE TRIGGER ca_a_bi BEFORE INSERT ON ca_a FOR EACH ROW
BEGIN
  CALL ca_write_b(NEW.v);
END;
CREATE TRIGGER ca_b_au AFTER UPDATE ON ca_b FOR EACH ROW
BEGIN
  UPDATE ca_a SET v = NEW.v WHERE id = 1;
END;
`
	db, err := mysqltest.Start(ctx, callCascadeSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO ca_b (id, v) VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}
	// the server: firing ca_a_bi, which CALLs ca_write_b, whose UPDATE of ca_b fires
	// ca_b_au, which writes back into ca_a -- the table the very first INSERT is already
	// using -- is refused every time with 1442 naming ca_a (measured on mysqld 8.4.11)
	_, execErr := conn.ExecContext(ctx, "INSERT INTO ca_a (id, v) VALUES (1, 5)")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1442 {
		t.Fatalf("test premise wrong: want the server to refuse this INSERT with 1442, got %v", execErr)
	}

	s, err := schema.Load(callCascadeSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	// the checker, asked about the trigger directly: already gets this right
	if _, terr := AnalyzeTrigger(s, s.Trigger("ca_a_bi")); errCode(terr) != 1442 {
		t.Fatalf("AnalyzeTrigger(ca_a_bi) = %v, want a 1442 Error -- if this no longer reproduces internally, the finding below needs re-measuring", terr)
	}

	// the checker, asked about the firing statement: predicts nothing at all
	r, cerr := Analyze(s, "INSERT INTO ca_a (id, v) VALUES ($1, $2)")
	if cerr != nil {
		t.Fatalf("Analyze(INSERT INTO ca_a ...) returned an unexpected error: %v", cerr)
	}
	for _, v := range r.Violations {
		if v.Code == 1442 {
			return
		}
	}
	t.Errorf("Analyze(INSERT INTO ca_a (id, v) VALUES ($1, $2)).Violations = %+v, want a 1442 violation among them: firing ca_a_bi always fails on the real server (CALL ca_write_b cascades through ca_b_au back into ca_a), and AnalyzeTrigger(ca_a_bi) already computes that very 1442 on its own -- triggerViolations discards it instead of folding it into the firing statement's predicted violations", r.Violations)
}

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a trigger elsewhere in the chain that only READS an already-in-use table with a
// locking SELECT ... FOR UPDATE is a certain 1442 on the real server. A plain SELECT (no
// lock) or a subquery read of the same table does NOT collide (measured separately, not a
// finding: see the read-only and subquery-read rows of adv3b-newcode-report.md), but
// SELECT ... FOR UPDATE does, every time (measured below: INSERT INTO rf_x, whose BEFORE
// INSERT trigger inserts into rf_y, whose BEFORE INSERT trigger runs `SELECT id INTO c FROM
// rf_x LIMIT 1 FOR UPDATE` -- rf_x already in use by the very first INSERT -- is refused
// with 1442 naming rf_x, on mysqld 8.4.11). triggerViolations only ever walks a trigger's
// br.Statements' Facts.Writes (INSERT / UPDATE / DELETE) to decide whether a further hop
// collides with the tables already in use; a SELECT (FOR UPDATE or not) records no Write at
// all (facts.Writes is appended to only by the embedded INSERT / UPDATE / DELETE paths of
// analyze.go), so the checker has no notion of a locking read counting toward the "already
// in use" set: it predicts nothing wrong for a statement mysqld always refuses.
//
// Suspect: check/mysql/internal/analyze/violations.go's triggerViolations (the
// `br.Statements` / `st.Facts.Writes` walk, ~violations.go:216-228), which never asks
// whether a body statement was a SELECT ... FOR UPDATE (or LOCK IN SHARE MODE) reading a
// table already in the chain's inUse set.
func TestAdv3ChainSelectForUpdateInsideTriggerIs1442(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const selectForUpdateSchema = `-- sqlshape: mysql 8.4
CREATE TABLE rf_x (id INT NOT NULL PRIMARY KEY);
CREATE TABLE rf_y (id INT NOT NULL PRIMARY KEY);
CREATE TRIGGER rf_x_bi BEFORE INSERT ON rf_x FOR EACH ROW
BEGIN
  INSERT INTO rf_y (id) VALUES (NEW.id);
END;
CREATE TRIGGER rf_y_bi BEFORE INSERT ON rf_y FOR EACH ROW
BEGIN
  DECLARE c INT;
  SELECT id INTO c FROM rf_x LIMIT 1 FOR UPDATE;
END;
`
	db, err := mysqltest.Start(ctx, selectForUpdateSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	// the server: rf_x_bi inserts into rf_y, which fires rf_y_bi, which reads rf_x with FOR
	// UPDATE -- rf_x already in use by the very statement inserting into it -- refused
	// every time with 1442 naming rf_x (measured on mysqld 8.4.11)
	_, execErr := conn.ExecContext(ctx, "INSERT INTO rf_x (id) VALUES (1)")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1442 {
		t.Fatalf("test premise wrong: want the server to refuse this INSERT with 1442, got %v", execErr)
	}

	s, err := schema.Load(selectForUpdateSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, cerr := Analyze(s, "INSERT INTO rf_x (id) VALUES ($1)")
	if cerr != nil {
		t.Fatalf("Analyze(INSERT INTO rf_x ...) returned an unexpected error: %v", cerr)
	}
	for _, v := range r.Violations {
		if v.Code == 1442 {
			return
		}
	}
	t.Errorf("Analyze(INSERT INTO rf_x (id) VALUES ($1)).Violations = %+v, want a 1442 violation among them: rf_y_bi's SELECT ... FOR UPDATE reads rf_x, already in use by the firing INSERT, and mysqld always refuses this -- triggerViolations only tracks a chained trigger's INSERT / UPDATE / DELETE writes, never a locking read", r.Violations)
}

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a trigger's CALL to a procedure that both recurses (a certain 1456, since
// max_sp_recursion_depth defaults to 0) and writes the calling trigger's own table (a
// second, independent certain-1442 shape, call.go's own rule): the real server reaches
// only the first, since the recursive CALL is the first statement rc_p's body runs
// (measured below: INSERT INTO rc_t is refused every time with 1456). AnalyzeRoutine(rc_p),
// asked directly, already gets the recursion right (its Violations carry a 1456 entry, from
// walkCall's own self-call check, body.go ~1253-1261), and AnalyzeTrigger(rc_t_bi) computes
// an internal 1442 Error (checkCalledRoutineOverlap: rc_p's WriteTables include rc_t). But
// Analyze("INSERT INTO rc_t ...") predicts NEITHER: triggerViolations' `if err != nil ||
// br == nil { continue }` (violations.go:214) discards rc_t_bi's analysis outright once
// cachedAnalyzeTrigger returns that internal 1442, so even the 1456 AnalyzeRoutine(rc_p)
// already worked out never reaches the firing statement's Violations -- the same swallow
// as TestAdv3ChainCallCascadeThroughAnotherTriggerSwallowed, landing here on a different
// failure mode to show it discards whatever the chained trigger's certain failure is.
//
// Suspect: check/mysql/internal/analyze/violations.go's triggerViolations, the `if err !=
// nil || br == nil { continue }` at line 214.
func TestAdv3ChainCallRecursionAnd1442BothSwallowed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const recursionSchema = `-- sqlshape: mysql 8.4
CREATE TABLE rc_t (id INT NOT NULL PRIMARY KEY, v INT);
CREATE PROCEDURE rc_p(IN n INT)
BEGIN
  IF n > 0 THEN
    CALL rc_p(n - 1);
  END IF;
  UPDATE rc_t SET v = n WHERE id = 1;
END;
CREATE TRIGGER rc_t_bi BEFORE INSERT ON rc_t FOR EACH ROW
BEGIN
  CALL rc_p(1);
END;
`
	db, err := mysqltest.Start(ctx, recursionSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	// the server: firing rc_t_bi CALLs rc_p(1), which immediately CALLs rc_p(0) -- an
	// actual recursive invocation, refused every time with 1456 (max_sp_recursion_depth
	// defaults to 0), before rc_p ever reaches its UPDATE of rc_t (measured on mysqld
	// 8.4.11)
	_, execErr := conn.ExecContext(ctx, "INSERT INTO rc_t (id, v) VALUES (1, 0)")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1456 {
		t.Fatalf("test premise wrong: want the server to refuse this INSERT with 1456, got %v", execErr)
	}

	s, err := schema.Load(recursionSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	// the checker, asked about the procedure directly: already gets the recursion right
	pbr, perr := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "rc_p"))
	if perr != nil || pbr == nil {
		t.Fatalf("AnalyzeRoutine(rc_p) = %v, %v, want a result with no error -- if this no longer reproduces internally, the finding below needs re-measuring", pbr, perr)
	}
	found1456 := false
	for _, v := range pbr.Violations {
		if v.Code == 1456 {
			found1456 = true
		}
	}
	if !found1456 {
		t.Fatalf("AnalyzeRoutine(rc_p).Violations = %+v, want a 1456 entry -- if this no longer reproduces internally, the finding below needs re-measuring", pbr.Violations)
	}

	// the checker, asked about the firing statement: predicts nothing at all
	r, cerr := Analyze(s, "INSERT INTO rc_t (id, v) VALUES ($1, $2)")
	if cerr != nil {
		t.Fatalf("Analyze(INSERT INTO rc_t ...) returned an unexpected error: %v", cerr)
	}
	for _, v := range r.Violations {
		if v.Code == 1456 {
			return
		}
	}
	t.Errorf("Analyze(INSERT INTO rc_t (id, v) VALUES ($1, $2)).Violations = %+v, want a 1456 violation among them: firing rc_t_bi always fails on the real server (its CALL rc_p(1) always recurses once), and AnalyzeRoutine(rc_p) already computes that 1456 on its own -- triggerViolations discards rc_t_bi's whole analysis instead of folding in what it already knows", r.Violations)
}
