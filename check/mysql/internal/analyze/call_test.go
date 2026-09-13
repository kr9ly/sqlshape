package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// callSchema exercises m6's own additions: a stored FUNCTION's call site (resolved,
// argument-checked, its own failure modes and the table it writes folded into the caller),
// and CALL of a PROCEDURE (argument-checked, OUT/INOUT validated, its own result columns
// and failure modes).
const callSchema = `-- sqlshape: mysql 8.4
CREATE TABLE widgets (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL
);

-- sqlshape: error 30001 = TooBig
CREATE FUNCTION next_total(x DECIMAL(10,2)) RETURNS DECIMAL(10,2) DETERMINISTIC
BEGIN
  IF x > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too big', MYSQL_ERRNO = 30001;
  END IF;
  RETURN x + 1;
END;

CREATE FUNCTION bump_widget(x INT) RETURNS INT
BEGIN
  UPDATE widgets SET v = x WHERE id = 1;
  RETURN x;
END;

-- sqlshape: error 30002 = TooBig2
CREATE PROCEDURE do_signal(IN a INT, OUT b INT)
BEGIN
  SET b = a + 1;
  IF a > 100 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too big', MYSQL_ERRNO = 30002;
  END IF;
END;

CREATE PROCEDURE report(IN a INT)
BEGIN
  SELECT a AS x, a * 2 AS y;
END;

CREATE PROCEDURE writes_widgets(IN a INT)
BEGIN
  UPDATE widgets SET v = a WHERE id = 1;
END;

-- two branches, the same shape (one nullable, one not): the CALL's own result column is
-- nullable (the OR across the branches, the way a schema constraint's own equality does not
-- have to match between them either)
CREATE PROCEDURE report_branch(IN a INT)
BEGIN
  IF a > 0 THEN
    SELECT id AS x FROM widgets WHERE id = a;
  ELSE
    SELECT a AS x;
  END IF;
END;

-- two branches of a different shape: not decidable statically (mysqld only refuses this at
-- runtime, with no error number of its own)
CREATE PROCEDURE report_mismatched(IN a INT)
BEGIN
  IF a > 0 THEN
    SELECT a AS x;
  ELSE
    SELECT a AS x, a * 2 AS y;
  END IF;
END;

-- two branches of the same column count but a different name in one position: still not
-- decidable statically, resultColumnsOf's own per-column disagreement (as opposed to
-- report_mismatched's disagreement in count).
CREATE PROCEDURE report_mismatched_name(IN a INT)
BEGIN
  IF a > 0 THEN
    SELECT a AS x;
  ELSE
    SELECT a AS y;
  END IF;
END;
`

func loadCallSchema(t *testing.T) *schema.Schema {
	s, err := schema.Load(callSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

// TestStoredFunctionCallResolves: a SELECT calling a schema FUNCTION types its result from
// RETURNS (always nullable) and folds the function's own SIGNAL into the statement's
// Violations, tagged with the function's name.
func TestStoredFunctionCallResolves(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "SELECT next_total($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 1 || !r.Columns[0].Known || r.Columns[0].Type.Name != "decimal" || !r.Columns[0].Nullable {
		t.Errorf("got columns %+v, want one known nullable decimal", r.Columns)
	}
	if len(r.Params) != 1 || !r.Params[0].Known || r.Params[0].Type.Name != "decimal" {
		t.Errorf("got params %+v, want $1 typed decimal from next_total's own parameter", r.Params)
	}
	if got := violationKeys(r.Violations); got != "30001 30001=TooBig" {
		t.Errorf("got %s, want the function's own SIGNAL (30001=TooBig)", got)
	}
	if len(r.Violations) != 1 || r.Violations[0].Function != "next_total" {
		t.Errorf("got %+v, want Function=next_total", r.Violations)
	}
}

// TestStoredFunctionQualified: `db.f(...)` resolves the same stored function as an
// unqualified call (the db qualifier is not validated against any particular schema name:
// m6's own scope, matching how a table's db qualifier is already ignored in target()).
func TestStoredFunctionQualified(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "SELECT anydb.next_total($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 1 || r.Columns[0].Type.Name != "decimal" {
		t.Errorf("got columns %+v, want a decimal (qualified call resolves the same function)", r.Columns)
	}
}

// TestStoredFunctionArgCountMismatch is 1318, the message shape measured on mysqld 8.4.
func TestStoredFunctionArgCountMismatch(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "SELECT next_total($1, $2)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1318 {
		t.Fatalf("got %v, want a 1318 Error", err)
	}
}

// TestStoredFunctionUnknown is 1305, the same code an unknown native function already gets.
func TestStoredFunctionUnknown(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "SELECT no_such_function($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1305 {
		t.Fatalf("got %v, want a 1305 Error", err)
	}
}

// TestFunctionWritingReferencedTableIs1442: a function whose body writes a table the
// invoking statement itself references (reading it is enough, measured on mysqld 8.4) is
// 1442, an Error (the collision is certain, not a "may").
func TestFunctionWritingReferencedTableIs1442(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "SELECT v, bump_widget(v) FROM widgets WHERE id = $1")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error", err)
	}
}

// TestFunctionWritingUnreferencedTableOK: the same function, called where the statement
// never references widgets at all, collides with nothing.
func TestFunctionWritingUnreferencedTableOK(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "SELECT bump_widget($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 1 {
		t.Errorf("got columns %+v", r.Columns)
	}
}

// TestCallStmtBasic: CALL types its arguments against the procedure's own parameters, its
// facts are Kind Call, and its own SIGNAL is a Violation of the CALL statement.
func TestCallStmtBasic(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "CALL do_signal($1, @out)")
	if err != nil {
		t.Fatal(err)
	}
	if r.Facts == nil || r.Facts.Kind != facts.Call {
		t.Fatalf("got Facts %+v, want Kind Call", r.Facts)
	}
	if !r.Facts.AtMostOne {
		t.Errorf("want AtMostOne for a CALL")
	}
	if len(r.Params) != 1 || !r.Params[0].Known || r.Params[0].Type.Name != "int" {
		t.Errorf("got params %+v, want $1 typed int from do_signal's IN parameter", r.Params)
	}
	if got := violationKeys(r.Violations); got != "30002 30002=TooBig2" {
		t.Errorf("got %s, want the procedure's own SIGNAL (30002=TooBig2)", got)
	}
}

// TestCallStmtArgCountMismatch is 1318, the same shape a function call's mismatch is.
func TestCallStmtArgCountMismatch(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL do_signal($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1318 {
		t.Fatalf("got %v, want a 1318 Error", err)
	}
}

// TestCallStmtUnknownProcedure is 1305.
func TestCallStmtUnknownProcedure(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL no_such_proc($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1305 {
		t.Fatalf("got %v, want a 1305 Error", err)
	}
}

// TestCallStmtOutArgNotVariable is 1414 (measured on mysqld 8.4: a literal in an OUT/INOUT
// position, as opposed to a placeholder or a `@var`, which the server always accepts).
func TestCallStmtOutArgNotVariable(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL do_signal($1, 5)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1414 {
		t.Fatalf("got %v, want a 1414 Error", err)
	}
}

// TestCallStmtOutArgPlaceholderOK: a placeholder in the OUT position is always accepted
// (measured: it is a prepared statement's own bind slot, an anonymous variable).
func TestCallStmtOutArgPlaceholderOK(t *testing.T) {
	s := loadCallSchema(t)
	if _, err := Analyze(s, "CALL do_signal($1, $2)"); err != nil {
		t.Fatalf("got %v, want no error (a placeholder OUT argument is always fine)", err)
	}
}

// TestCallStmtResultColumns: a procedure whose body is a single INTO-less SELECT gives the
// CALL statement that SELECT's own columns.
func TestCallStmtResultColumns(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "CALL report($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 2 || r.Columns[0].Name != "x" || r.Columns[1].Name != "y" {
		t.Fatalf("got columns %+v, want x, y", r.Columns)
	}
}

// TestCallStmtNoResultColumns: a procedure whose body writes but never SELECTs has no
// result columns.
func TestCallStmtNoResultColumns(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "CALL writes_widgets($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 0 {
		t.Errorf("got columns %+v, want none", r.Columns)
	}
}

// TestCallStmtResultColumnsMergeNullability: two same-shaped INTO-less SELECT branches
// agree on one column list, with the nullability ORed across them.
func TestCallStmtResultColumnsMergeNullability(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "CALL report_branch($1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 1 || r.Columns[0].Name != "x" || !r.Columns[0].Nullable {
		t.Fatalf("got columns %+v, want one nullable column x (ORed across the branches)", r.Columns)
	}
}

// TestCallStmtResultColumnsMismatchedShapes: two INTO-less SELECT branches of a different
// shape are not decidable statically -- the checker's own error (Code 0), not a MySQL one
// (mysqld itself only refuses this at runtime, whichever branch actually runs).
func TestCallStmtResultColumnsMismatchedShapes(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL report_mismatched($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 0 {
		t.Fatalf("got %v, want the checker's own Error (Code 0)", err)
	}
}

// TestCallStmtAloneDoesNotCollide: a CALL whose own arguments reference no table never
// collides with what the procedure's body writes (measured on mysqld 8.4: `CALL p3(7)`
// where p3 writes t raised nothing -- the statement itself never mentions t).
func TestCallStmtAloneDoesNotCollide(t *testing.T) {
	s := loadCallSchema(t)
	if _, err := Analyze(s, "CALL writes_widgets($1)"); err != nil {
		t.Fatalf("got %v, want no error", err)
	}
}

// TestCallStmtOutArgNotVariable_Identifier is 1414 through isCallVariableTarget's other
// path: a bare identifier (not a `@var`, not a placeholder) resolves through lookupVar the
// same way a routine body's own local variable would, but there is none in scope at a
// top-level CALL (schema.Load bodies push their own vars only around a trigger's/routine's
// own analysis), so it is rejected the same as any other non-variable expression.
func TestCallStmtOutArgNotVariable_Identifier(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL do_signal($1, v)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1414 {
		t.Fatalf("got %v, want a 1414 Error", err)
	}
}

// TestNoteCalledRoutine_DedupesSameCallTwice: calling the same FUNCTION twice in one
// statement folds its own SIGNAL into Violations once, not twice (noteCalledRoutine's own
// doc: a.calledSeen).
func TestNoteCalledRoutine_DedupesSameCallTwice(t *testing.T) {
	s := loadCallSchema(t)
	r, err := Analyze(s, "SELECT next_total($1) + next_total($2)")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Violations) != 1 {
		t.Fatalf("got %d violations, want 1 (the same routine called twice folds once): %+v", len(r.Violations), r.Violations)
	}
}

// TestCheckCalledRoutineOverlap_UpdateWriteTarget: the invoking statement's own write
// target (not merely something it reads, a.uses) also collides -- an UPDATE whose SET
// value calls a function that writes the very table being updated is 1442, exercising
// checkCalledRoutineOverlap's a.write.table branch (TestFunctionWritingReferencedTableIs1442
// above only exercises the a.uses branch, through a SELECT).
func TestCheckCalledRoutineOverlap_UpdateWriteTarget(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "UPDATE widgets SET v = bump_widget(v) WHERE id = $1")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error", err)
	}
}

// TestCheckCalledRoutineOverlap_MultiTableWriteTarget: a multi-table UPDATE's second write
// target (w.more, not w.table -- the first one assigned) also collides, when a called
// function writes it.
func TestCheckCalledRoutineOverlap_MultiTableWriteTarget(t *testing.T) {
	s := loadCallSchema(t)
	text := callSchema + `
CREATE TABLE gadgets (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL
);
`
	s2, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Problems) > 0 {
		t.Fatalf("schema problems: %v", s2.Problems)
	}
	_, err = Analyze(s2, "UPDATE gadgets g, widgets w SET g.v = $1, w.v = bump_widget(w.v) WHERE g.id = w.id")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error (bump_widget writes widgets, gadgets' second write target)", err)
	}
	_ = s // loadCallSchema's own schema is unused here; s2 carries the extra table
}

// TestCallStmtArgExprError: an IN argument's own expression can fail to type (here an
// unknown FUNCTION call), and callStmt returns that error as-is (a.expr's own, not
// wrapped or replaced).
func TestCallStmtArgExprError(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL do_signal(no_such_function(), @out)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1305 {
		t.Fatalf("got %v, want the argument expression's own 1305 Error", err)
	}
}

// TestCallStmtBrokenBody: CALL of a procedure whose own body fails to analyze (here LEAVE
// with no enclosing label, 1308) surfaces that error directly -- unlike
// checkCalledRoutineOverlap's own best-effort skip (TestCheckCalledRoutineOverlap_BrokenBodySwallowsError),
// callStmt itself needs the body's result columns, so it cannot proceed without one. This
// procedure is not part of callSchema (loaded against a real mysqld by
// call_violations_server_test.go), since the server itself refuses LEAVE with no matching
// label at CREATE time -- schema.Load, which never runs a body through mysqld, tolerates it
// as a Problem-free load, deferring the check to AnalyzeRoutine.
func TestCallStmtBrokenBody(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE broken_proc(IN x INT)
BEGIN
  LEAVE nowhere;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	_, err = Analyze(s, "CALL broken_proc($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1308 {
		t.Fatalf("got %v, want the body's own 1308 Error", err)
	}
}

// TestCallStmtResultColumnsMismatchedNames: same column count in both branches, but a
// different name in one position -- resultColumnsOf's own per-column disagreement, distinct
// from TestCallStmtResultColumnsMismatchedShapes's count disagreement.
func TestCallStmtResultColumnsMismatchedNames(t *testing.T) {
	s := loadCallSchema(t)
	_, err := Analyze(s, "CALL report_mismatched_name($1)")
	ae, ok := err.(*Error)
	if !ok || ae.Code != 0 {
		t.Fatalf("got %v, want the checker's own Error (Code 0)", err)
	}
}

// TestCheckCalledRoutineOverlap_BrokenBodySwallowsError: a called function whose own body
// fails to analyze (a construct the server refuses at CREATE time, here an unterminated
// FUNCTION with no RETURN) cannot be asked whether it writes an overlapping table --
// checkCalledRoutineOverlap skips it (the body's own error is reported separately, through
// dialect.Schema.Definitions, not duplicated here).
func TestCheckCalledRoutineOverlap_BrokenBodySwallowsError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE widgets (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL
);
CREATE FUNCTION broken_fn(x INT) RETURNS INT
BEGIN
  DECLARE y INT DEFAULT 0;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	if _, err := Analyze(s, "SELECT v, broken_fn(v) FROM widgets WHERE id = $1"); err != nil {
		t.Fatalf("got %v, want no error (the broken body is skipped, not propagated as a 1442 false positive)", err)
	}
}
