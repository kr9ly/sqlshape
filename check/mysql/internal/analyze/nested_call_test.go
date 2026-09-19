package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// nestedCallSchema: routines calling routines, a stored function named like a builtin, and
// triggers whose bodies call functions -- every shape measured on mysqld 8.4 in
// TestNestedCallsServer.
const nestedCallSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE TABLE other (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE TABLE log2 (w INT NOT NULL);
CREATE TABLE nn (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, v INT NOT NULL DEFAULT 5, w INT NOT NULL, x INT);
CREATE FUNCTION abs(x INT) RETURNS INT DETERMINISTIC RETURN 42;
CREATE FUNCTION writer(x INT) RETURNS INT MODIFIES SQL DATA BEGIN UPDATE t SET v = v + 1 WHERE id = -1; RETURN x; END;
CREATE FUNCTION writer_other(x INT) RETURNS INT MODIFIES SQL DATA BEGIN UPDATE other SET v = v + 1 WHERE id = -1; RETURN x; END;
CREATE PROCEDURE q() BEGIN UPDATE t SET v = v + 1 WHERE id = -1; END;
CREATE FUNCTION fcall(x INT) RETURNS INT MODIFIES SQL DATA BEGIN CALL q(); RETURN x; END;
CREATE FUNCTION fnest(x INT) RETURNS INT MODIFIES SQL DATA BEGIN RETURN writer(x); END;
-- the statement that calls the writer references its table: 1442 when CALLed
CREATE PROCEDURE p_same_stmt() BEGIN UPDATE t SET v = writer(v); END;
CREATE PROCEDURE p_same_stmt_nested() BEGIN UPDATE t SET v = fnest(v); END;
-- two statements: the read of t and the write of t through writer() do not collide
CREATE PROCEDURE p_two_stmts() BEGIN DECLARE n INT; SELECT COUNT(*) INTO n FROM t; SET @x = writer(1); END;
CREATE PROCEDURE p_two_stmts_call() BEGIN DECLARE n INT; SELECT COUNT(*) INTO n FROM t; CALL q(); END;
CREATE PROCEDURE p_update_then_call() BEGIN UPDATE t SET v = v + 1 WHERE id = -1 AND v = (SELECT MAX(v) FROM other); CALL q(); END;
-- the statement references other, the writer writes t: no collision
CREATE PROCEDURE p_other() BEGIN UPDATE other SET v = writer(v) WHERE id = -1; END;
CREATE PROCEDURE p_missing() BEGIN CALL no_such_procedure(); END;
CREATE PROCEDURE p_wrong_arity() BEGIN CALL q(1); END;
-- a trigger calling a function that writes the trigger's own table: 1442 always
CREATE TRIGGER other_ai AFTER INSERT ON other FOR EACH ROW BEGIN SET @y = writer_other(1); END;
-- a trigger calling a function that writes another table: no collision, even when the
-- firing statement reads that table
CREATE TRIGGER t_ai AFTER INSERT ON t FOR EACH ROW BEGIN SET @y = writer_other(1); END;
-- NEW.col of a NOT NULL column: nullable in BEFORE, as declared in AFTER
CREATE TRIGGER nn_bi BEFORE INSERT ON nn FOR EACH ROW BEGIN INSERT INTO log2 (w) VALUES (NEW.w); END;
CREATE TRIGGER nn_ai AFTER INSERT ON nn FOR EACH ROW BEGIN INSERT INTO log2 (w) VALUES (NEW.w); END;
`

func loadNestedCalls(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Load(nestedCallSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

// errCode is the analyze.Error code of err, 0 for nil, -1 for another error.
func errCode(err error) int {
	if err == nil {
		return 0
	}
	if ae, ok := err.(*Error); ok {
		return ae.Code
	}
	return -1
}

// TestQualifiedCallNamesTheStoredFunction: an unqualified abs() is the native function even
// when the schema declares a FUNCTION abs; db.abs() is the schema's own; db.nope() is 1305.
func TestQualifiedCallNamesTheStoredFunction(t *testing.T) {
	s := loadNestedCalls(t)
	r, err := Analyze(s, "SELECT abs(-1) AS a")
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Columns[0]; c.Type.Name != "bigint" || c.Nullable {
		t.Errorf("abs(-1) must be the native function (bigint not null), got %s nullable=%v", c.Type.Name, c.Nullable)
	}
	r, err = Analyze(s, "SELECT mydb.abs(-1) AS a")
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Columns[0]; c.Type.Name != "int" || !c.Nullable {
		t.Errorf("mydb.abs(-1) must be the stored function (int, nullable), got %s nullable=%v", c.Type.Name, c.Nullable)
	}
	if _, err := Analyze(s, "SELECT mydb.nope(1)"); errCode(err) != 1305 {
		t.Errorf("mydb.nope(1): got %v, want 1305", err)
	}
}

// TestNestedCallOverlap: the 1442 overlap rule applied per body statement, transitively
// through nested calls, and to a trigger's own table.
func TestNestedCallOverlap(t *testing.T) {
	s := loadNestedCalls(t)
	for _, c := range []struct {
		sql  string
		code int
	}{
		{"CALL p_same_stmt()", 1442},
		{"CALL p_same_stmt_nested()", 1442},
		{"CALL p_two_stmts()", 0},
		{"CALL p_two_stmts_call()", 0},
		{"CALL p_update_then_call()", 0},
		{"CALL p_other()", 0},
		{"CALL p_missing()", 1305},
		{"CALL p_wrong_arity()", 1318},
		{"SELECT fcall(1) FROM t", 1442},                  // f CALLs a procedure writing t
		{"SELECT fnest(1) FROM t", 1442},                  // f RETURNs a function writing t
		{"SELECT fnest(1) FROM other", 0},                 // t is not this statement's
		{"INSERT INTO t VALUES (1, 1)", 0},                // t_ai writes other, not t
		{"INSERT INTO t SELECT id + 10, v FROM other", 0}, // reading other does not collide with the trigger's function writing it
	} {
		_, err := Analyze(s, c.sql)
		if got := errCode(err); got != c.code {
			t.Errorf("%s: got %v, want code %d", c.sql, err, c.code)
		}
	}
	for name, want := range map[string][]string{"p_two_stmts": {"t"}, "p_two_stmts_call": {"t"}, "p_other": {"other", "t"}, "fcall": {"t"}, "fnest": {"t"}} {
		kind := schema.Procedure
		if name == "fcall" || name == "fnest" {
			kind = schema.Function
		}
		br, err := AnalyzeRoutine(s, s.RoutineOf(kind, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := tableSet(br.WriteTables); got != tableSet(want) {
			t.Errorf("%s WriteTables: got %s, want %s", name, got, tableSet(want))
		}
	}
	if _, err := AnalyzeTrigger(s, s.Trigger("other_ai")); errCode(err) != 1442 {
		t.Errorf("other_ai (its function writes the trigger's own table): got %v, want 1442", err)
	}
	if br, err := AnalyzeTrigger(s, s.Trigger("t_ai")); err != nil || tableSet(br.WriteTables) != "other" {
		t.Errorf("t_ai: got %v %v, want no error and WriteTables [other]", err, br)
	}
}

// TestAfterTriggerNewIsAsDeclared: NEW.w of a NOT NULL column may be NULL in a BEFORE INSERT
// trigger (its INSERT into a NOT NULL column carries 1048) and is as declared in an AFTER
// one (no 1048) -- measured in TestNestedCallsServer.
func TestAfterTriggerNewIsAsDeclared(t *testing.T) {
	s := loadNestedCalls(t)
	br, err := AnalyzeTrigger(s, s.Trigger("nn_bi"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "1048 log2.w" {
		t.Errorf("BEFORE: got %q, want 1048 log2.w", got)
	}
	br, err = AnalyzeTrigger(s, s.Trigger("nn_ai"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "" {
		t.Errorf("AFTER: got %q, want no violation", got)
	}
}

func tableSet(names []string) string {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	var out []string
	for _, n := range []string{"log2", "nn", "other", "t"} {
		if m[n] {
			out = append(out, n)
		}
	}
	s := ""
	for i, n := range out {
		if i > 0 {
			s += " "
		}
		s += n
	}
	return s
}
