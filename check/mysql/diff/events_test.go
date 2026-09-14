package diff

import "testing"

// TestCompare_EventAddDropAlter: events are compared by schedule, bounds, options and body;
// a STARTS the target does not fix (omitted, or an expression) is not compared, a literal one
// is.
func TestCompare_EventAddDropAlter(t *testing.T) {
	a := load(t, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, v INT);
CREATE EVENT gone ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
CREATE EVENT changed ON SCHEDULE EVERY 1 DAY DO DELETE FROM t WHERE id < 0;
CREATE EVENT floating ON SCHEDULE EVERY 1 DAY STARTS '2026-01-01 00:00:00' DO DELETE FROM t;
CREATE EVENT fixed ON SCHEDULE EVERY 1 DAY STARTS '2026-01-01 00:00:00' DO DELETE FROM t;
CREATE EVENT same ON SCHEDULE EVERY 1 DAY STARTS '2030-01-01 00:00:00' ON COMPLETION PRESERVE DO DELETE FROM t;
`)
	b := load(t, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, v INT);
CREATE EVENT changed ON SCHEDULE EVERY 2 DAY DO DELETE FROM t WHERE id < 0;
CREATE EVENT added ON SCHEDULE AT '2030-01-01 00:00:00' DO DELETE FROM t;
CREATE EVENT floating ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
CREATE EVENT fixed ON SCHEDULE EVERY 1 DAY STARTS '2030-01-01 00:00:00' DO DELETE FROM t;
CREATE EVENT same ON SCHEDULE EVERY 1 DAY STARTS '2030-01-01 00:00:00' ON COMPLETION PRESERVE DO DELETE FROM t;
`)
	changes := Compare(a, b)
	if c := findChange(changes, "event", "gone"); c == nil || c.Op != Drop {
		t.Errorf("want event gone dropped, got %+v", changes)
	}
	if c := findChange(changes, "event", "added"); c == nil || c.Op != Add {
		t.Errorf("want event added added, got %+v", changes)
	}
	if c := findChange(changes, "event", "changed"); c == nil || c.Op != Alter || len(c.Fields) != 1 || c.Fields[0].Name != "schedule" || c.Fields[0].To != "EVERY 2 DAY" {
		t.Errorf("want event changed altered in its schedule, got %+v", c)
	}
	if c := findChange(changes, "event", "floating"); c != nil {
		t.Errorf("a STARTS the target leaves to the server is no difference, got %+v", c)
	}
	if c := findChange(changes, "event", "fixed"); c == nil || c.Op != Alter || len(c.Fields) != 1 || c.Fields[0].Name != "starts" {
		t.Errorf("a literal STARTS is compared, got %+v", c)
	}
	if c := findChange(changes, "event", "same"); c != nil {
		t.Errorf("identical events differ: %+v", c)
	}
	if !EventChanged(a.Event("fixed"), b.Event("fixed")) || EventChanged(a.Event("floating"), b.Event("floating")) || EventChanged(a.Event("same"), b.Event("same")) {
		t.Error("EventChanged disagrees with Compare")
	}
}
