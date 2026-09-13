package diff

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// load loads text, failing the test on an error or a Problem.
func load(t *testing.T, text string) *schema.Schema {
	t.Helper()
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func findChange(changes []Change, kind, name string) *Change {
	for i, c := range changes {
		if c.Kind == kind && c.Name == name {
			return &changes[i]
		}
	}
	return nil
}

// TestCompare_RoutineAddDropAlter covers differ.routines' own three branches (added,
// dropped, altered), for both a PROCEDURE and a FUNCTION.
func TestCompare_RoutineAddDropAlter(t *testing.T) {
	a := load(t, `-- sqlshape: mysql 8.4
CREATE PROCEDURE gone() BEGIN SELECT 1; END;
CREATE PROCEDURE changed(IN x INT) BEGIN SELECT x; END;
CREATE FUNCTION f_changed() RETURNS INT BEGIN RETURN 1; END;
`)
	b := load(t, `-- sqlshape: mysql 8.4
CREATE PROCEDURE changed(IN x INT) BEGIN SELECT x + 1; END;
CREATE PROCEDURE added() BEGIN SELECT 2; END;
CREATE FUNCTION f_changed() RETURNS INT BEGIN RETURN 2; END;
`)
	changes := Compare(a, b)

	if c := findChange(changes, "procedure", "gone"); c == nil || c.Op != Drop {
		t.Errorf("want procedure gone dropped, got %+v", changes)
	}
	if c := findChange(changes, "procedure", "added"); c == nil || c.Op != Add {
		t.Errorf("want procedure added added, got %+v", changes)
	}
	if c := findChange(changes, "procedure", "changed"); c == nil || c.Op != Alter {
		t.Errorf("want procedure changed altered, got %+v", changes)
	}
	if c := findChange(changes, "function", "f_changed"); c == nil || c.Op != Alter {
		t.Errorf("want function f_changed altered, got %+v", changes)
	}
}

// TestCompare_RoutineNamespacesIndependent: a PROCEDURE and a FUNCTION of the same name are
// compared independently (differ.routines' own kind filter), so one existing on both sides
// unchanged while the other kind is added does not confuse them.
func TestCompare_RoutineNamespacesIndependent(t *testing.T) {
	a := load(t, `-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
`)
	b := load(t, `-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
CREATE FUNCTION p() RETURNS INT BEGIN RETURN 1; END;
`)
	changes := Compare(a, b)
	if findChange(changes, "procedure", "p") != nil {
		t.Errorf("procedure p should be unchanged, got %+v", changes)
	}
	if c := findChange(changes, "function", "p"); c == nil || c.Op != Add {
		t.Errorf("want function p added, got %+v", changes)
	}
}

// TestCompare_TriggerAddDropAlter covers differ.triggers' own three branches.
func TestCompare_TriggerAddDropAlter(t *testing.T) {
	a := load(t, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, v INT);
CREATE TRIGGER gone BEFORE INSERT ON t FOR EACH ROW SET NEW.v = 1;
CREATE TRIGGER changed BEFORE INSERT ON t FOR EACH ROW SET NEW.v = 2;
`)
	b := load(t, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, v INT);
CREATE TRIGGER changed BEFORE INSERT ON t FOR EACH ROW SET NEW.v = 3;
CREATE TRIGGER added AFTER INSERT ON t FOR EACH ROW SET @x = 1;
`)
	changes := Compare(a, b)
	if c := findChange(changes, "trigger", "gone"); c == nil || c.Op != Drop {
		t.Errorf("want trigger gone dropped, got %+v", changes)
	}
	if c := findChange(changes, "trigger", "added"); c == nil || c.Op != Add {
		t.Errorf("want trigger added added, got %+v", changes)
	}
	if c := findChange(changes, "trigger", "changed"); c == nil || c.Op != Alter {
		t.Errorf("want trigger changed altered, got %+v", changes)
	}
}

// TestCompare_RoutineTriggerUnchanged: an identical routine/trigger on both sides produces
// no Change at all (props' own "no differing fields" path).
func TestCompare_RoutineTriggerUnchanged(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, v INT);
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET NEW.v = 1;
CREATE PROCEDURE p() BEGIN SELECT 1; END;
`
	a, b := load(t, text), load(t, text)
	changes := Compare(a, b)
	if findChange(changes, "trigger", "trg") != nil || findChange(changes, "procedure", "p") != nil {
		t.Errorf("want no changes for identical routine/trigger, got %+v", changes)
	}
}

// TestRoutineProps_DropsCreatePrefix and TestTriggerProps_DropsCreatePrefix cover
// RoutineProps/TriggerProps directly: the definition is trimmed and its "CREATE " prefix
// dropped.
func TestRoutineProps_DropsCreatePrefix(t *testing.T) {
	s := load(t, `-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
`)
	r := s.Routine("p")
	props := RoutineProps(r)
	if strings.HasPrefix(props["definition"], "CREATE ") {
		t.Errorf("got %q, want the CREATE prefix dropped", props["definition"])
	}
	if !strings.Contains(props["definition"], "PROCEDURE p") {
		t.Errorf("got %q", props["definition"])
	}
}

func TestTriggerProps_DropsCreatePrefix(t *testing.T) {
	s := load(t, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`)
	tg := s.Trigger("trg")
	props := TriggerProps(tg)
	if strings.HasPrefix(props["definition"], "CREATE ") {
		t.Errorf("got %q, want the CREATE prefix dropped", props["definition"])
	}
	if !strings.Contains(props["definition"], "TRIGGER trg") {
		t.Errorf("got %q", props["definition"])
	}
}
