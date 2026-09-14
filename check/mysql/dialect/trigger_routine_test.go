package dialect

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// triggerRoutineSchema exercises the dialect's own trigger/routine surface: Errors() (named
// `-- sqlshape: error` declarations on both a trigger and a routine), Definitions()
// (bodyDefinitions' per-statement success path), and describeViolation's own trigger/
// routine cases (1172 SELECT INTO, a named SIGNAL, an unnamed one).
const triggerRoutineSchema = `-- sqlshape: mysql 8.4
CREATE TABLE widgets (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL
);
CREATE TABLE widget_audit (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  msg VARCHAR(50) NOT NULL
);

-- sqlshape: error 30001 = WidgetTooBig
CREATE TRIGGER widgets_bi BEFORE INSERT ON widgets FOR EACH ROW
BEGIN
  IF NEW.v > 1000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too big', MYSQL_ERRNO = 30001;
  END IF;
  IF NEW.v < 0 THEN
    SIGNAL SQLSTATE '02000';
  END IF;
  INSERT INTO widget_audit (msg) VALUES ('insert');
END;

-- sqlshape: error 30002 = ProcTooBig
CREATE PROCEDURE bump(IN cid INT)
BEGIN
  DECLARE t INT;
  SELECT id INTO t FROM widgets WHERE v = cid;
  IF t > 1000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too big', MYSQL_ERRNO = 30002;
  END IF;
END;

-- sqlshape: error 30003 = FuncTooBig
CREATE FUNCTION double_v(x INT) RETURNS INT DETERMINISTIC
BEGIN
  IF x > 1000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too big', MYSQL_ERRNO = 30003;
  END IF;
  RETURN x * 2;
END;
`

func loadTriggerRoutine(t *testing.T) *mysql {
	t.Helper()
	an, err := load(triggerRoutineSchema)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	return m
}

// TestErrors_TriggerAndRoutine covers Errors() reading both a trigger's and a routine's own
// `-- sqlshape: error` declarations.
func TestErrors_TriggerAndRoutine(t *testing.T) {
	m := loadTriggerRoutine(t)
	got := m.Errors()
	want := map[string]dialect.ErrorName{
		"30001": {Code: "30001", Name: "WidgetTooBig", Subject: "trigger widgets_bi"},
		"30002": {Code: "30002", Name: "ProcTooBig", Subject: "procedure bump"},
		"30003": {Code: "30003", Name: "FuncTooBig", Subject: "function double_v"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, e := range got {
		if want[e.Code] != e {
			t.Errorf("got %+v, want %+v", e, want[e.Code])
		}
	}
}

// TestDefinitions_TriggerAndRoutineBodies covers Definitions()'s own trigger/routine path
// (bodyDefinitions), naming each statement "<what>: line N".
func TestDefinitions_TriggerAndRoutineBodies(t *testing.T) {
	m := loadTriggerRoutine(t)
	var whats []string
	for _, d := range m.Definitions() {
		whats = append(whats, d.What)
		if strings.HasPrefix(d.What, "trigger widgets_bi") && d.Facts == nil {
			t.Errorf("%s: want Facts", d.What)
		}
	}
	joined := strings.Join(whats, "\n")
	if !strings.Contains(joined, "trigger widgets_bi: line") {
		t.Errorf("missing widgets_bi statement line:\n%s", joined)
	}
	if !strings.Contains(joined, "procedure bump: line") {
		t.Errorf("missing procedure bump statement lines:\n%s", joined)
	}
}

// TestDefinitions_BrokenTriggerBody covers bodyDefinitions' own Err path: a trigger the
// server would refuse at CREATE time (here, a trigger writing its own table, 1442 -- always
// certain, body.go's own doc) surfaces as one Definition carrying Err, not a panic or a
// silently dropped trigger.
func TestDefinitions_BrokenTriggerBody(t *testing.T) {
	an, err := load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE TRIGGER trg AFTER DELETE ON t FOR EACH ROW
BEGIN
  DELETE FROM t WHERE id = OLD.id;
END;
`)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	var found bool
	for _, d := range m.Definitions() {
		if d.What == "trigger trg" && strings.Contains(d.Err, "1442") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a Definition{What: %q, Err: ...1442...}, got %+v", "trigger trg", m.Definitions())
	}
}

// TestDescribeViolation_TriggerAndRoutine covers describeViolation's own trigger (1172, a
// named SIGNAL, an unnamed one) and routine (a named SIGNAL) cases, through a real Analyze
// call so each Violation carries what the analyzer actually sets.
func TestDescribeViolation_TriggerAndRoutine(t *testing.T) {
	m := loadTriggerRoutine(t)

	// a named SIGNAL (30001=WidgetTooBig) and an unnamed one (SQLSTATE '02000', no
	// declared name) both surface on an INSERT, through widgets_bi.
	r, err := m.Analyze("INSERT INTO widgets (id, v) VALUES ($1, $2)")
	if err != nil {
		t.Fatal(err)
	}
	var named, unnamed string
	for _, v := range r.Violations {
		if v.Key == "30001" {
			named = v.Detail
		}
		if v.Key == "02000" {
			unnamed = v.Detail
		}
	}
	if !strings.Contains(named, "widgets_bi") || !strings.Contains(named, "WidgetTooBig") {
		t.Errorf("named SIGNAL detail: %q", named)
	}
	if !strings.Contains(unnamed, "widgets_bi") || strings.Contains(unnamed, "as ") {
		t.Errorf("unnamed SIGNAL detail (want no 'as Name'): %q", unnamed)
	}

	// bump's own SELECT ... INTO is not provably single-row (1172), and its own named
	// SIGNAL (30002=ProcTooBig) both surface on CALL.
	r, err = m.Analyze("CALL bump($1)")
	if err != nil {
		t.Fatal(err)
	}
	var got1172, gotFn string
	for _, v := range r.Violations {
		if v.Code == "MySQL error 1172" {
			got1172 = v.Detail
		}
		if v.Key == "30002" {
			gotFn = v.Detail
		}
	}
	if !strings.Contains(got1172, "bump") {
		t.Errorf("1172 detail: %q", got1172)
	}
	if !strings.Contains(gotFn, "bump") || !strings.Contains(gotFn, "ProcTooBig") {
		t.Errorf("function SIGNAL detail: %q", gotFn)
	}
}

// TestErrorOf covers errorOf directly: an *analyze.Error becomes a *dialect.Error with the
// same message, code and position; any other error (here a parse error, which never
// reaches analyze.Error at all) passes through unchanged.
func TestErrorOf(t *testing.T) {
	m := loadTriggerRoutine(t)
	_, err := m.Analyze("CALL no_such_procedure($1)")
	if err == nil {
		t.Fatal("want an error")
	}
	de, ok := err.(*dialect.Error)
	if !ok {
		t.Fatalf("got %T, want *dialect.Error", err)
	}
	if de.Code != "MySQL error 1305" {
		t.Errorf("got Code=%q, want MySQL error 1305", de.Code)
	}

	_, err = m.Analyze("TRUNCATE TABLE widgets")
	if err == nil {
		t.Fatal("want an error (a top-level statement kind this checker does not analyze)")
	}
	if _, ok := err.(*dialect.Error); ok {
		t.Errorf("got a *dialect.Error, want the analyzer's own plain error passed through unchanged: %v", err)
	}
}

// TestDefinitions_EventBodies covers Definitions()'s event path: an event's body statements
// are judged like a routine's ("event <name>: line N"), and a body naming a table the schema
// does not have surfaces as one Definition carrying Err (the server accepts such an event at
// CREATE time and fails it at every run, measured).
func TestDefinitions_EventBodies(t *testing.T) {
	an, err := load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE EVENT sweep ON SCHEDULE EVERY 1 DAY DO BEGIN
  DELETE FROM t WHERE id < 0;
  UPDATE t SET v = v + 1 WHERE id = 1;
END;
CREATE EVENT broken ON SCHEDULE EVERY 1 DAY DO DELETE FROM nope WHERE id < 0;
`)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	var whats []string
	var brokenErr string
	for _, d := range m.Definitions() {
		whats = append(whats, d.What)
		if strings.HasPrefix(d.What, "event sweep") && d.Facts == nil {
			t.Errorf("%s: want Facts", d.What)
		}
		if d.What == "event broken" {
			brokenErr = d.Err
		}
	}
	joined := strings.Join(whats, "\n")
	if strings.Count(joined, "event sweep: line") != 2 {
		t.Errorf("want both statements of sweep:\n%s", joined)
	}
	if !strings.Contains(brokenErr, "nope") || !strings.Contains(brokenErr, "1146") {
		t.Errorf("event broken: want an Err naming the missing table (1146), got %q", brokenErr)
	}
}
