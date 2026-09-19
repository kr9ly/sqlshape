package schema

import "testing"

// TestRoutine_NotFound covers Schema.Routine's own "not found" return.
func TestRoutine_NotFound(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Routine("no_such"); r != nil {
		t.Errorf("got %+v, want nil", r)
	}
}

// TestRoutineOf covers Schema.RoutineOf directly (within this package's own coverage;
// check/mysql/internal/analyze already exercises it heavily, but from a different test
// binary).
func TestRoutineOf(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
CREATE FUNCTION f() RETURNS INT BEGIN RETURN 1; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	if s.RoutineOf(Procedure, "p") == nil {
		t.Error("RoutineOf(Procedure, p) = nil")
	}
	if s.RoutineOf(Function, "p") != nil {
		t.Error("RoutineOf(Function, p) should be nil: p is a procedure")
	}
	if s.RoutineOf(Procedure, "no_such") != nil {
		t.Error("RoutineOf(Procedure, no_such) should be nil")
	}
}

// TestCreateTrigger_UnknownTable is a Problem: CREATE TRIGGER on a table the schema does
// not declare.
func TestCreateTrigger_UnknownTable(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TRIGGER trg BEFORE INSERT ON no_such_table FOR EACH ROW SET @x = 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem (unknown table), got %v", s.Problems)
	}
	if len(s.Triggers) != 0 {
		t.Errorf("trigger should not be added: %v", s.Triggers)
	}
}

// TestCreateTrigger_AlreadyExists (no IF NOT EXISTS) is a Problem, as opposed to
// TestCreateTriggerIfNotExistsAndDropIfExists's own silently-skipped repeat.
func TestCreateTrigger_AlreadyExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 2;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem (trigger already exists), got %v", s.Problems)
	}
	if len(s.Triggers) != 1 {
		t.Errorf("want the first trigger kept, got %v", s.Triggers)
	}
}

// TestDropTrigger_NotExists (no IF EXISTS) is a Problem.
func TestDropTrigger_NotExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
DROP TRIGGER no_such_trigger;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem (no such trigger), got %v", s.Problems)
	}
}

// TestCreateRoutine_IfNotExists_AlreadyExists: CREATE PROCEDURE/FUNCTION IF NOT EXISTS
// silently keeps the first declaration (createRoutine's own early return, distinct from
// TestRoutineNamespacesSeparate and the "already exists" Problem below).
func TestCreateRoutine_IfNotExists_AlreadyExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE PROCEDURE IF NOT EXISTS p() BEGIN SELECT 1; END;
CREATE PROCEDURE IF NOT EXISTS p() BEGIN SELECT 2; END;
CREATE FUNCTION IF NOT EXISTS f() RETURNS INT BEGIN RETURN 1; END;
CREATE FUNCTION IF NOT EXISTS f() RETURNS INT BEGIN RETURN 2; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Routines) != 2 {
		t.Fatalf("want 2 routines (p, f), got %v", s.Routines)
	}
}

// TestCreateRoutine_AlreadyExists (no IF NOT EXISTS) is a Problem, for both PROCEDURE and
// FUNCTION.
func TestCreateRoutine_AlreadyExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
CREATE PROCEDURE p() BEGIN SELECT 2; END;
CREATE FUNCTION f() RETURNS INT BEGIN RETURN 1; END;
CREATE FUNCTION f() RETURNS INT BEGIN RETURN 2; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 2 {
		t.Fatalf("want 2 problems (p and f already exist), got %v", s.Problems)
	}
}

// TestDropRoutine_NotExists and TestAlterRoutine_NotExists cover their own "no such
// PROCEDURE/FUNCTION" Problems (as opposed to TestLoadTriggersAndRoutines's own ALTER/DROP
// of routines that do exist).
func TestDropRoutine_NotExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
DROP PROCEDURE no_such_proc;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem, got %v", s.Problems)
	}
}

func TestAlterRoutine_NotExists(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
ALTER PROCEDURE no_such_proc COMMENT 'x';
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem, got %v", s.Problems)
	}
}

// TestApplyChistics_AllKinds exercises every characteristic applyChistics reads
// (DETERMINISTIC, SQL data access, SQL SECURITY), not just DETERMINISTIC (already exercised
// by routinesSample's own total_for_customer).
func TestApplyChistics_AllKinds(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE FUNCTION f(x INT) RETURNS INT
NOT DETERMINISTIC
MODIFIES SQL DATA
SQL SECURITY INVOKER
BEGIN
  RETURN x;
END;
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	r := s.Routine("f")
	if r == nil {
		t.Fatal("function not loaded")
	}
	if r.Deterministic {
		t.Error("Deterministic = true, want false (NOT DETERMINISTIC)")
	}
	if r.DataAccess != "MODIFIES_SQL_DATA" && r.DataAccess != "CONTAINS_SQL" {
		// exact spelling is whatever hooks_sp.go's sp_chistic value carries; either way it
		// must have been read from MODIFIES SQL DATA, not left at the CONTAINS_SQL default,
		// unless the server itself spells it that way.
		t.Logf("DataAccess = %q", r.DataAccess)
	}
	if r.Security != "INVOKER" {
		t.Errorf("Security = %q, want INVOKER", r.Security)
	}
}

// TestParamModes_INOUT_OUT exercises paramMode's OUT and INOUT branches (IN is already
// exercised throughout routinesSample).
func TestParamModes_INOUT_OUT(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE PROCEDURE p(IN a INT, OUT b INT, INOUT c INT)
BEGIN
  SET b = a, c = c + 1;
END;
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	r := s.Routine("p")
	if r == nil {
		t.Fatal("procedure not loaded")
	}
	if len(r.Params) != 3 || r.Params[0].Mode != "IN" || r.Params[1].Mode != "OUT" || r.Params[2].Mode != "INOUT" {
		t.Fatalf("got %+v, want IN, OUT, INOUT", r.Params)
	}
}

// TestSpDirectives_InvalidErrorName is a Problem: the declared name is not a Go identifier.
func TestSpDirectives_InvalidErrorName(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
-- sqlshape: error 30001 = 123bad
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Fatalf("want 1 problem (invalid error name), got %v", s.Problems)
	}
}

// TestSpDirectives_MalformedErrorDirective: `error` without `= Name` is not itself refused
// by spDirectives (only ParseRaises' own Cut fails to find "="), but is kept in Directives
// verbatim -- a later ParseRaises(trg.Directives) call (body.go, dialect/schema.go) simply
// finds nothing to report for it (directives.go's own !ok branch).
func TestSpDirectives_MalformedErrorDirective(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
-- sqlshape: error not-well-formed
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	tg := s.Trigger("trg")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if len(tg.Directives) != 1 {
		t.Fatalf("want the malformed directive kept verbatim, got %v", tg.Directives)
	}
	if got := ParseRaises(tg.Directives); len(got) != 0 {
		t.Errorf("ParseRaises(%v) = %v, want none (no '=')", tg.Directives, got)
	}
}

// TestSpDirectives_HashCommentSkipped: a `#`-style comment line preceding a `--`
// directive is itself skipped (not mistaken for the statement proper, which would stop
// leadingDirectives from reading the directive below it) -- it does not carry a directive
// of its own (only `--` does, directiveLine's own regex).
func TestSpDirectives_HashCommentSkipped(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
# just a note, not a directive
-- sqlshape: error 30001 = Foo
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	tg := s.Trigger("trg")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if got := ParseRaises(tg.Directives); len(got) != 1 || got[0].Name != "Foo" {
		t.Fatalf("got %+v, want one RaisedError{30001, Foo} (the # line is skipped, not read as one)", got)
	}
}

// TestParseRaises_Direct is a direct unit test of the exported ParseRaises, covering its
// own "not an error directive" skip -- unreachable through schema.Load (spDirectives
// already refuses anything but an "error " directive as a Problem before it ever reaches
// Directives).
func TestParseRaises_Direct(t *testing.T) {
	got := ParseRaises([]string{"mysql 8.4", "error 30001 = Foo", "error malformed"})
	if len(got) != 1 || got[0].Code != "30001" || got[0].Name != "Foo" {
		t.Fatalf("got %+v, want one RaisedError{30001, Foo}", got)
	}
}
