package schema

import (
	"strings"
	"testing"
)

// TestCreateEvent reads CREATE EVENT's two schedule forms with their options, tells a
// literal time from an evaluated one, and applies DROP EVENT and IF NOT EXISTS; ALTER EVENT
// and a duplicate are problems.
func TestCreateEvent(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE EVENT e1 ON SCHEDULE EVERY 1 DAY DO DELETE FROM t WHERE id < 0;
CREATE DEFINER=root@localhost EVENT e2 ON SCHEDULE EVERY 1 HOUR STARTS '2030-01-01 00:00:00' ENDS '2031-01-01 00:00:00' ON COMPLETION PRESERVE DISABLE COMMENT 'c'
DO BEGIN
  DELETE FROM t WHERE id < 0;
  UPDATE t SET v = v + 1 WHERE id = 1;
END;
CREATE EVENT e3 ON SCHEDULE AT '2030-01-01 00:00:00' DO INSERT INTO t VALUES (1, 1);
CREATE EVENT e4 ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 YEAR DO INSERT INTO t VALUES (2, 1);
CREATE EVENT e5 ON SCHEDULE EVERY 1 DAY STARTS CURRENT_TIMESTAMP DO DELETE FROM t WHERE id < 0;
CREATE EVENT IF NOT EXISTS e1 ON SCHEDULE EVERY 2 DAY DO DELETE FROM t;
CREATE EVENT gone ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
DROP EVENT gone;
DROP EVENT IF EXISTS gone;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if len(s.Events) != 5 {
		t.Fatalf("got %d events, want 5", len(s.Events))
	}
	e1 := s.Event("e1")
	if e1.Every != "1 DAY" || e1.Starts != "" || e1.StartsLiteral || e1.Completion != "NOT PRESERVE" || e1.Status != "ENABLE" || e1.Comment != "" || e1.BodyText != "DELETE FROM t WHERE id < 0" {
		t.Errorf("e1: %+v", e1)
	}
	e2 := s.Event("e2")
	if e2.Every != "1 HOUR" || e2.Starts != "'2030-01-01 00:00:00'" || !e2.StartsLiteral || e2.Ends != "'2031-01-01 00:00:00'" || !e2.EndsLiteral || e2.Completion != "PRESERVE" || e2.Status != "DISABLE" || e2.Comment != "c" || !strings.HasPrefix(e2.BodyText, "BEGIN") || !strings.HasSuffix(e2.BodyText, "END") {
		t.Errorf("e2: %+v", e2)
	}
	if e3 := s.Event("e3"); e3.At != "'2030-01-01 00:00:00'" || !e3.AtLiteral || e3.Every != "" {
		t.Errorf("e3: %+v", e3)
	}
	if e4 := s.Event("e4"); e4.At != "CURRENT_TIMESTAMP + INTERVAL 1 YEAR" || e4.AtLiteral {
		t.Errorf("e4: %+v", e4)
	}
	if e5 := s.Event("e5"); e5.Starts != "CURRENT_TIMESTAMP" || e5.StartsLiteral {
		t.Errorf("e5: %+v", e5)
	}
	if s.Event("gone") != nil {
		t.Error("DROP EVENT did not drop")
	}

	s, err = Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE EVENT e1 ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
CREATE EVENT e1 ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
CREATE EVENT e2 ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
ALTER EVENT e1 RENAME TO e2;
ALTER EVENT nope ON SCHEDULE EVERY 2 DAY;
DROP EVENT nope;
`)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, p := range s.Problems {
		msgs = append(msgs, p.Message)
	}
	got := strings.Join(msgs, "\n")
	for _, want := range []string{"CREATE EVENT e1: event already exists", "ALTER EVENT e1 RENAME TO e2: event already exists", "ALTER EVENT nope: no such event", "DROP EVENT nope: no such event"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
}

// TestAlterEvent: each ALTER EVENT clause replaces that part of the event and the rest
// stays; a new schedule replaces the whole schedule (STARTS included); RENAME TO renames.
func TestAlterEvent(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE EVENT e ON SCHEDULE EVERY 1 HOUR STARTS '2030-01-01 00:00:00' ON COMPLETION PRESERVE DISABLE COMMENT 'c' DO DELETE FROM t WHERE id < 0;
ALTER EVENT e ON SCHEDULE EVERY 2 DAY;
ALTER EVENT e ENABLE COMMENT 'd';
ALTER EVENT e DO UPDATE t SET v = v + 1 WHERE id = 1;
ALTER EVENT e ON COMPLETION NOT PRESERVE RENAME TO f;
CREATE EVENT once ON SCHEDULE EVERY 1 DAY DO DELETE FROM t;
ALTER EVENT once ON SCHEDULE AT '2030-01-01 00:00:00' ON COMPLETION PRESERVE;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Event("e") != nil {
		t.Error("RENAME TO left the old name")
	}
	f := s.Event("f")
	if f == nil {
		t.Fatal("renamed event not found")
	}
	if f.Every != "2 DAY" || f.Starts != "" || f.StartsLiteral || f.Completion != "NOT PRESERVE" || f.Status != "ENABLE" || f.Comment != "d" || f.BodyText != "UPDATE t SET v = v + 1 WHERE id = 1" {
		t.Errorf("f: %+v", f)
	}
	if f.Body == nil {
		t.Error("f: the new body was not kept")
	}
	if o := s.Event("once"); o.At != "'2030-01-01 00:00:00'" || !o.AtLiteral || o.Every != "" || o.Completion != "PRESERVE" {
		t.Errorf("once: %+v", o)
	}
}
