package schema

import (
	"strings"
	"testing"
)

// ALTER VIEW replaces the view's definition whole (measured on 8.4: the column list, the
// algorithm and the check option are the new statement's), and the server's own refusals
// are problems: no such view (1146), a table of that name (1347), CHECK OPTION on a view it
// would not merge (1368).
func TestAlterView(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT NOT NULL, b INT);
CREATE VIEW v AS SELECT id, a FROM t;
ALTER ALGORITHM=TEMPTABLE VIEW v (x, y, z) AS SELECT id, a, b FROM t;
ALTER VIEW nope AS SELECT id FROM t;
ALTER VIEW t AS SELECT id FROM t;
ALTER VIEW v AS SELECT a, COUNT(*) n FROM t GROUP BY a WITH CHECK OPTION;
`)
	if err != nil {
		t.Fatal(err)
	}
	v := s.View("v")
	if v == nil {
		t.Fatal("view v not loaded")
	}
	if got := strings.Join(v.Columns, ","); got != "x,y,z" || v.Algorithm != "TEMPTABLE" || !strings.HasPrefix(v.Definition, "ALTER ALGORITHM=TEMPTABLE") {
		t.Errorf("after ALTER VIEW: columns %q, algorithm %q, definition %q", got, v.Algorithm, v.Definition)
	}
	var got []string
	for _, p := range s.Problems {
		got = append(got, p.Message)
	}
	want := []string{
		"ALTER VIEW nope: Table 'nope' doesn't exist (1146)",
		"ALTER VIEW t: 't' is not VIEW (1347)",
		"ALTER VIEW v: CHECK OPTION on non-updatable view (1368)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(v.Columns) != 3 {
		t.Errorf("the refused ALTER VIEW replaced the view: %v", v.Columns)
	}
}

// LOCK TABLES, UNLOCK TABLES, LOAD DATA and ALTER VIEW inside any body are refused by the
// server's grammar at CREATE time (1314 "<X> is not allowed in stored procedures", measured
// for a PROCEDURE, a FUNCTION and a TRIGGER alike): a problem at load, the body still
// loaded for analyze to raise the same error.
func TestBodyRefusesLockLoadAlterView(t *testing.T) {
	s, err := Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT NOT NULL);
CREATE TABLE u (id INT PRIMARY KEY);
CREATE VIEW v AS SELECT id FROM t;
CREATE PROCEDURE p1() BEGIN LOCK TABLES t WRITE; END;
CREATE FUNCTION f1() RETURNS INT DETERMINISTIC BEGIN UNLOCK TABLES; RETURN 1; END;
CREATE TRIGGER tr1 BEFORE INSERT ON u FOR EACH ROW BEGIN LOAD DATA INFILE '/tmp/x' INTO TABLE t; END;
CREATE PROCEDURE p2() BEGIN IF 1 THEN ALTER VIEW v AS SELECT a FROM t; END IF; END;
CREATE PROCEDURE p3() BEGIN SELECT a INTO OUTFILE '/tmp/o' FROM t; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range s.Problems {
		got = append(got, p.Message)
	}
	want := []string{
		"CREATE PROCEDURE p1: LOCK is not allowed in stored procedures",
		"CREATE FUNCTION f1: UNLOCK is not allowed in stored procedures",
		"CREATE TRIGGER tr1: LOAD DATA is not allowed in stored procedures",
		"CREATE PROCEDURE p2: ALTER VIEW is not allowed in stored procedures",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, name := range []string{"p1", "p2", "p3"} {
		if s.RoutineOf(Procedure, name) == nil {
			t.Errorf("procedure %s not loaded", name)
		}
	}
}
