package analyze

// Adversarial pass 2 (calls lane): probes check/mysql/internal/analyze/call.go and its
// surroundings (expr.go's storedFuncCall, body.go's walkCall/AnalyzeRoutine, schema.go's
// column/DEFAULT/CHECK reading) against a running mysqld 8.4. See
// scratchpad/adv2/brief.md for the rules this pass follows: every finding is measured
// against a real server, not guessed. Findings are written as tests that state the CORRECT
// (server-measured) behaviour and currently FAIL against the checker as it stands today.

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

func adv2ServerCode(err error) int {
	if err == nil {
		return 0
	}
	var me *driver.MySQLError
	if errors.As(err, &me) {
		return int(me.Number)
	}
	return -1
}

// --- finding 1: a procedure that CALLs itself is certain to fail at every actual recursion,
// undetected -----------------------------------------------------------------------------

const adv2RecursiveCallSchema = `-- sqlshape: mysql 8.4
CREATE PROCEDURE p_self(IN n INT)
BEGIN
  IF n > 0 THEN
    CALL p_self(n - 1);
  END IF;
END;
`

// TestAdv2RecursiveCallAlwaysFails: mysqld's max_sp_recursion_depth defaults to 0, so ANY
// actual recursive invocation of a stored routine -- even a single level -- is refused at
// run time with 1456 ("Recursive limit 0 ... was exceeded"), measured: `CALL p_self(1)`
// (whose body takes the recursive branch exactly once) raises 1456 every time, unlike a
// non-recursive CALL.
//
// The checker has no notion of this at all: AnalyzeRoutine's own cycle-breaking cache
// (body.go around line 375-394, "a cycle ... is broken here") exists only to stop the
// *checker itself* from looping forever walking p_self's body; the in-progress placeholder
// it returns on the recursive Load (body.go line 376-383) is silently treated by
// checkCalledRoutineOverlap (call.go line 56, via the AnalyzeRoutine call at call.go line
// 83) and finishCalls (body.go line 495, via the AnalyzeRoutine call at body.go line 504) as
// "no writes, no error" rather than "this branch always fails" -- so `CALL p_self(1)`
// analyzes clean, with no Violation and no Error, even though the branch it is certain to
// take (n=1 > 0) is certain to raise 1456 on the real server.
func TestAdv2RecursiveCallAlwaysFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2RecursiveCallSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2RecursiveCallSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	conn := db.Conn()

	// server: measured 1456 for a CALL whose branch recurses exactly once
	_, execErr := conn.ExecContext(ctx, "CALL p_self(1)")
	if adv2ServerCode(execErr) != 1456 {
		t.Fatalf("CALL p_self(1) on the server: got %v, want 1456 (max_sp_recursion_depth=0)", execErr)
	}

	// checker: predicts nothing wrong (r.Facts is Kind Call, no Violations, err nil) -- it
	// should instead treat the recursive branch as certain to fail, the same way it already
	// treats a trigger writing its own table (1442) as certain rather than a "may"
	r, cerr := Analyze(s, "CALL p_self(1)")
	if cerr == nil && len(r.Violations) == 0 {
		t.Errorf("Analyze(CALL p_self(1)) = %+v, %v -- want a predicted failure (1456), since the server always raises it for this call", r, cerr)
	}
}

// --- finding 2: NEW.col is a valid OUT/INOUT argument in a BEFORE trigger, and the checker
// rejects it unconditionally -----------------------------------------------------------

const adv2NewOutArgSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT);

CREATE PROCEDURE p_out(OUT o INT)
BEGIN
  SET o = 1;
END;

CREATE TRIGGER t_bi BEFORE INSERT ON t FOR EACH ROW
BEGIN
  CALL p_out(NEW.v);
END;
`

// TestAdv2NewPseudoVariableAsOutArgInBeforeTrigger: mysqld accepts NEW.col as an OUT (or
// INOUT) argument of a CALL inside a BEFORE trigger -- the server's own 1414 message even
// names the exception ("... is not a variable or NEW pseudo-variable in BEFORE trigger"),
// and `INSERT INTO t VALUES (1, 5)` firing t_bi succeeds with no error (measured).
//
// The checker's isCallVariableTarget (call.go line 120) only recognizes a placeholder
// (isParam), a `@user` variable (PTI_user_variable) and a declared local
// variable/parameter (PTI_simple_ident_ident / PTI_simple_ident_nospvar_ident through
// a.lookupVar) as valid OUT/INOUT targets; a qualified NEW.v reference is none of those, so
// both walkCall (body.go line 1096, the check at line 1113) and callStmt's own copy (call.go
// line 150, the check inside its args loop) fall through and report 1414 "OUT or INOUT
// argument 1 for routine p_out is not a variable" for a CREATE TRIGGER body the server
// accepts outright: a valid schema is flagged as broken (AnalyzeTrigger returns a non-nil
// error), the false-positive direction the brief calls out as high severity.
func TestAdv2NewPseudoVariableAsOutArgInBeforeTrigger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2NewOutArgSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2NewOutArgSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	conn := db.Conn()

	// server: the INSERT (which fires t_bi, whose body passes NEW.v as p_out's OUT arg)
	// succeeds outright
	if _, execErr := conn.ExecContext(ctx, "INSERT INTO t VALUES (1, 5)"); execErr != nil {
		t.Fatalf("INSERT INTO t VALUES (1, 5) on the server: got %v, want no error (NEW.v is a valid OUT arg in a BEFORE trigger)", execErr)
	}

	// checker: AnalyzeTrigger on t_bi should report no error for a body the server accepts
	if _, aerr := AnalyzeTrigger(s, s.Trigger("t_bi")); aerr != nil {
		t.Errorf("AnalyzeTrigger(t_bi) = %v, want nil -- checker rejects NEW.v as an OUT argument even though the server accepts it in a BEFORE trigger", aerr)
	}
}

// --- finding 3: a stored FUNCTION named in a generated column, a column DEFAULT expression
// or a CHECK constraint is refused by the server at CREATE TABLE time, unconditionally (not
// only when non-deterministic) -- schema.Load lets it straight through with no Problem ----

const adv2FuncInGeneratedSchema = `-- sqlshape: mysql 8.4
CREATE FUNCTION f_det(x INT) RETURNS INT DETERMINISTIC
BEGIN
  RETURN x + 1;
END;

CREATE TABLE t_gen (
  id INT NOT NULL PRIMARY KEY,
  v INT,
  g INT GENERATED ALWAYS AS (f_det(v)) VIRTUAL
);
`

const adv2FuncInDefaultSchema = `-- sqlshape: mysql 8.4
CREATE FUNCTION f_det(x INT) RETURNS INT DETERMINISTIC
BEGIN
  RETURN x + 1;
END;

CREATE TABLE t_def (
  id INT NOT NULL PRIMARY KEY,
  v INT DEFAULT (f_det(1))
);
`

const adv2FuncInCheckSchema = `-- sqlshape: mysql 8.4
CREATE FUNCTION f_det(x INT) RETURNS INT DETERMINISTIC
BEGIN
  RETURN x + 1;
END;

CREATE TABLE t_chk (
  id INT NOT NULL PRIMARY KEY,
  v INT,
  CHECK (f_det(v) > 0)
);
`

// TestAdv2StoredFunctionDisallowedInGeneratedDefaultCheck: measured on mysqld 8.4, a
// GENERATED ALWAYS AS expression naming any stored FUNCTION -- DETERMINISTIC or not -- is
// refused at CREATE TABLE time with 3763 ("Expression of generated column 'g' contains a
// disallowed function: `f_det`"); a column DEFAULT (expr) naming one is refused with 3770
// ("Default value expression of column 'v' contains a disallowed function"); a CHECK
// constraint naming one is refused with 3814 ("An expression of a check constraint ...
// contains disallowed function"). All three are CREATE-time refusals docs/mysql.md already
// promises the checker reports as schema Problems ("What the server itself refuses at
// CREATE time is a problem the same way an unknown table is").
//
// schema.go's column() (around line 687) reads col.Generated (line 704), col.Default (lines
// 721-723) and a table's Check.Expr (line 741) as bare AST with no validation at all of what
// they reference; nothing in the loader walks those expressions looking for a stored
// FUNCTION call. All three schemas here load with zero Problems even though mysqld refuses
// every one of them at CREATE TABLE time.
func TestAdv2StoredFunctionDisallowedInGeneratedDefaultCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cases := []struct {
		name    string
		schema  string
		wantErr int
	}{
		{"generated column", adv2FuncInGeneratedSchema, 3763},
		{"column DEFAULT expression", adv2FuncInDefaultSchema, 3770},
		{"CHECK constraint", adv2FuncInCheckSchema, 3814},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, err := mysqltest.Start(ctx, c.schema)
			if errors.Is(err, mysqltest.ErrNoServer) {
				t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
			}
			// the server refuses the CREATE TABLE itself, so mysqltest.Start fails with the
			// server's own error text
			if err == nil {
				db.Close()
				t.Fatalf("mysqltest.Start(%s) succeeded, want the server to refuse CREATE TABLE with %d", c.name, c.wantErr)
			}
			var me *driver.MySQLError
			if !errors.As(err, &me) || int(me.Number) != c.wantErr {
				t.Fatalf("mysqltest.Start(%s): got %v, want MySQL error %d", c.name, err, c.wantErr)
			}

			s, lerr := schema.Load(c.schema)
			if lerr != nil {
				t.Fatalf("schema.Load(%s): unexpected error %v", c.name, lerr)
			}
			if len(s.Problems) == 0 {
				t.Errorf("schema.Load(%s): 0 Problems, want a Problem reporting the server's %d refusal of a stored function in this position", c.name, c.wantErr)
			}
		})
	}
}

// --- finding 4: a called routine's write cascades through another table's own trigger back
// onto a table the calling statement references, and the 1442 check misses it ------------

const adv2CascadingWriteSchema = `-- sqlshape: mysql 8.4
CREATE TABLE a_tbl (id INT NOT NULL PRIMARY KEY, v INT);
CREATE TABLE b_tbl (id INT NOT NULL PRIMARY KEY, v INT);

CREATE FUNCTION f_write_b(x INT) RETURNS INT
BEGIN
  UPDATE b_tbl SET v = x WHERE id = 1;
  RETURN x;
END;

CREATE TRIGGER b_au AFTER UPDATE ON b_tbl FOR EACH ROW
BEGIN
  UPDATE a_tbl SET v = NEW.v WHERE id = 1;
END;
`

// TestAdv2CalledRoutineWriteCascadesThroughTriggerTo1442: f_write_b's own body only writes
// b_tbl directly, but b_tbl's own AFTER UPDATE trigger (b_au) writes a_tbl in turn --
// measured: `SELECT f_write_b(v) FROM a_tbl WHERE id = 1` (a statement that only reads
// a_tbl and calls f_write_b) raises 1442 on the real server ("Can't update table 'a_tbl' ...
// because it is already used by statement which invoked this stored function/trigger"),
// the same collision docs/mysql.md documents for a routine that writes the referenced table
// directly -- it is exactly as certain here, since b_au always fires on b_tbl's UPDATE.
//
// checkCalledRoutineOverlap (call.go line 56) computes the collision from
// AnalyzeRoutine(...).WriteTables alone (call.go lines 82-90), and WriteTables (body.go
// line 30, populated at body.go lines 1264-1273 and folded transitively through nested CALLs
// at body.go lines 503-511) only ever grows from a body's own embedded DML and from routines
// it CALLs or RETURNs -- never from a trigger AnalyzeRoutine's own writes fire in turn. So
// f_write_b's BodyResult.WriteTables is ["b_tbl"] only, a_tbl never enters it, and the
// checker predicts no error at all for a statement the server always refuses.
func TestAdv2CalledRoutineWriteCascadesThroughTriggerTo1442(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2CascadingWriteSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2CascadingWriteSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO a_tbl VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO b_tbl VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}

	const sqlText = "SELECT f_write_b(v) FROM a_tbl WHERE id = 1"

	// server: measured 1442, through the cascade b_tbl -> b_au trigger -> a_tbl
	rows, execErr := conn.QueryContext(ctx, sqlText)
	if execErr == nil {
		for rows.Next() {
		}
		execErr = rows.Err()
	}
	if adv2ServerCode(execErr) != 1442 {
		t.Fatalf("%s on the server: got %v, want 1442 (f_write_b's write to b_tbl fires b_au, which writes a_tbl)", sqlText, execErr)
	}

	// checker: should predict the same certain failure
	if _, cerr := Analyze(s, sqlText); cerr == nil {
		t.Errorf("Analyze(%s) = nil error, want a 1442 Error: the checker misses the write that cascades from f_write_b's own UPDATE of b_tbl, through b_au, into a_tbl", sqlText)
	} else if ae, ok := cerr.(*Error); !ok || ae.Code != 1442 {
		t.Errorf("Analyze(%s) = %v, want a 1442 Error", sqlText, cerr)
	}
}

// --- finding 5: a CALL of a procedure that itself CALLs another procedure producing a
// result set loses that inner result set's columns ----------------------------------------

const adv2NestedCallResultSchema = `-- sqlshape: mysql 8.4
CREATE PROCEDURE q_inner(IN a INT)
BEGIN
  SELECT a AS x, a * 2 AS y;
END;

CREATE PROCEDURE p_outer(IN a INT)
BEGIN
  CALL q_inner(a);
END;
`

// TestAdv2NestedCallResultColumnsPropagate: mysqld propagates a nested CALL's own result set
// out through every enclosing CALL exactly as if the inner SELECT sat directly in the
// outer body -- measured: `CALL p_outer(5)` returns q_inner's two columns (x, y) to the
// client, the same as calling q_inner directly would.
//
// resultColumnsOf (call.go line 196) only ever looks at br.Statements (populated by
// appendStatement / appendStatementCols, body.go's walkSelect around lines 1200 and 1224):
// walkCall (body.go line 1096) types the nested CALL's arguments and folds its callee's
// writes and failure modes into the body through finishCalls (body.go line 1124), but never
// appends q_inner's own result columns as one of p_outer's br.Statements entries -- so
// callStmt's own resultColumnsOf(br) for p_outer sees zero Columns-bearing statements and
// reports "no result columns" (ok=true, cols=nil) for a CALL the server actually returns two
// columns from.
func TestAdv2NestedCallResultColumnsPropagate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2NestedCallResultSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2NestedCallResultSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	conn := db.Conn()

	// server: measured 2 result columns (x, y) from CALL p_outer(5), via the nested CALL
	rows, execErr := conn.QueryContext(ctx, "CALL p_outer(?)", 5)
	if execErr != nil {
		t.Fatalf("CALL p_outer(5) on the server: %v", execErr)
	}
	cts, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(cts) != 2 {
		t.Fatalf("server returned %d columns for CALL p_outer(5), want 2 (q_inner's x, y)", len(cts))
	}

	// checker: predicts no result columns at all for the same CALL
	r, cerr := Analyze(s, "CALL p_outer($1)")
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(r.Columns) != 2 {
		t.Errorf("Analyze(CALL p_outer($1)).Columns = %v (%d columns), want 2 -- the nested CALL to q_inner's own result set should propagate out", r.Columns, len(r.Columns))
	}
}
