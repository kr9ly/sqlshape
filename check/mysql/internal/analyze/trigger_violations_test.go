package analyze

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// triggerViolationSchema exercises the failure modes a trigger's body contributes to the
// statement that fires it: a plain SIGNAL, a named CONDITION, a HANDLER absorbing what it
// catches, a RESIGNAL, a trigger writing its own table (1442), a SELECT INTO that cannot be
// proved single (1172), and REPLACE / ON DUPLICATE KEY UPDATE's own event pairs.
const triggerViolationSchema = `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note VARCHAR(100)
);
CREATE TABLE audit_log (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  msg VARCHAR(100) NOT NULL
);

-- sqlshape: error 30001 = OrderTooLarge
CREATE TRIGGER orders_bi BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.total > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO audit_log (msg) VALUES ('insert');
END;

CREATE TRIGGER orders_bu BEFORE UPDATE ON orders FOR EACH ROW
BEGIN
  DECLARE too_large CONDITION FOR SQLSTATE '45000';
  IF NEW.total > 1000000 THEN
    SIGNAL too_large SET MESSAGE_TEXT = 'too large';
  END IF;
END;

CREATE TRIGGER orders_own_write AFTER DELETE ON orders FOR EACH ROW
BEGIN
  DELETE FROM orders WHERE id = OLD.id;
END;

CREATE PROCEDURE find_total(IN cid BIGINT UNSIGNED)
BEGIN
  DECLARE t DECIMAL(10,2);
  SELECT total INTO t FROM orders WHERE customer_id = cid;
  SELECT t;
END;

CREATE PROCEDURE find_total_by_note(IN n VARCHAR(100))
BEGIN
  DECLARE t DECIMAL(10,2);
  SELECT total INTO t FROM orders WHERE note = n LIMIT 1;
  SELECT t;
END;

CREATE PROCEDURE insert_absorbed(IN v DECIMAL(10,2))
BEGIN
  DECLARE CONTINUE HANDLER FOR 1062
  BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'dup-mapped', MYSQL_ERRNO = 30003;
  END;
  INSERT INTO orders (id, total) VALUES (1, v);
  INSERT INTO orders (id, total) VALUES (1, v);
END;

CREATE PROCEDURE reraises()
BEGIN
  DECLARE EXIT HANDLER FOR SQLEXCEPTION
  BEGIN
    RESIGNAL;
  END;
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'orig', MYSQL_ERRNO = 30002;
END;
`

func loadTriggerViolations(t *testing.T) *schema.Schema {
	s, err := schema.Load(triggerViolationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func violationKeys(vs []Violation) string {
	var keys []string
	for _, v := range vs {
		k := fmt.Sprintf("%d %s", v.Code, v.Key())
		if v.Name != "" {
			k += "=" + v.Name
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// TestTriggerSignalPropagates: a BEFORE INSERT trigger's SIGNAL (with its own
// `-- sqlshape: error 30001 = OrderTooLarge` annotation) becomes a failure mode of the
// INSERT that fires it, alongside the schema's own constraint (the primary key).
func TestTriggerSignalPropagates(t *testing.T) {
	s := loadTriggerViolations(t)
	r, err := Analyze(s, "INSERT INTO orders (id, total) VALUES ($1, $2)")
	if err != nil {
		t.Fatal(err)
	}
	want := "1048 orders.id, 1048 orders.total, 1062 PRIMARY, 30001 30001=OrderTooLarge"
	if got := violationKeys(r.Violations); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestTriggerNamedConditionSignal: a BEFORE UPDATE trigger's SIGNAL through a named
// DECLARE ... CONDITION FOR SQLSTATE, with no MYSQL_ERRNO, keys by the SQLSTATE and reports
// as the unhandled generic number 1644 (measured).
func TestTriggerNamedConditionSignal(t *testing.T) {
	s := loadTriggerViolations(t)
	r, err := Analyze(s, "UPDATE orders SET total = $1 WHERE id = $2")
	if err != nil {
		t.Fatal(err)
	}
	want := "1048 orders.total, 1644 45000"
	if got := violationKeys(r.Violations); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestTriggerOwnTableWriteIs1442: a trigger whose own body writes its own table cannot be
// analyzed as a Definition (the server always refuses it, measured), so AnalyzeTrigger
// returns the 1442 Error rather than a BodyResult.
func TestTriggerOwnTableWriteIs1442(t *testing.T) {
	s := loadTriggerViolations(t)
	tg := s.Trigger("orders_own_write")
	if tg == nil {
		t.Fatal("no such trigger")
	}
	_, err := AnalyzeTrigger(s, tg)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error", err)
	}
}

// TestSelectIntoCardinality: a SELECT ... INTO not provably single-row is 1172; a LIMIT 1
// (or another x/cardinality proof) rules it out.
func TestSelectIntoCardinality(t *testing.T) {
	s := loadTriggerViolations(t)
	br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "find_total"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "1172 1172" {
		t.Errorf("find_total (no proof of at most one row): got %s, want 1172 1172", got)
	}
	br, err = AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "find_total_by_note"))
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(br.Violations); got != "" {
		t.Errorf("find_total_by_note (LIMIT 1): got %s, want no violations", got)
	}
}

// TestHandlerAbsorbsByNumber: a CONTINUE HANDLER FOR 1062 in a routine absorbs the
// duplicate-key violation its own embedded INSERT may raise, but its own SIGNAL (which
// runs when the handler fires) is not absorbed by anything and stays.
func TestHandlerAbsorbsByNumber(t *testing.T) {
	s := loadTriggerViolations(t)
	br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "insert_absorbed"))
	if err != nil {
		t.Fatal(err)
	}
	want := "1048 orders.total, 30001 30001=OrderTooLarge, 30003 30003"
	if got := violationKeys(br.Violations); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestResignalReraises: a bare RESIGNAL inside a HANDLER FOR SQLEXCEPTION re-raises the
// exact SIGNAL it caught (measured: the server reports the same number).
func TestResignalReraises(t *testing.T) {
	s := loadTriggerViolations(t)
	br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "reraises"))
	if err != nil {
		t.Fatal(err)
	}
	want := "30002 30002"
	if got := violationKeys(br.Violations); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
