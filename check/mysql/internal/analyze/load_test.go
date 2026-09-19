package analyze

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// The statements this file covers were the third adversarial round's blind spot: LOCK
// TABLES, LOAD DATA and a FIELDS / LINES clause did not fold (mysqlast reported them
// unsupported, so schema.Load refused a whole CREATE PROCEDURE holding one), ALTER VIEW was
// not read, and a top-level SELECT ... FOR UPDATE was "query expression not understood".
const loadSchema = `-- sqlshape: mysql 8.4
CREATE TABLE p (id INT PRIMARY KEY);
CREATE TABLE t (id INT PRIMARY KEY, a INT NOT NULL, b VARCHAR(10), c INT, pid INT, u INT UNIQUE,
  CONSTRAINT fk FOREIGN KEY (pid) REFERENCES p (id), CONSTRAINT chk CHECK (c > 0));
CREATE VIEW v AS SELECT id, a FROM t;
`

func loadLoadSchema(t *testing.T, more string) *schema.Schema {
	t.Helper()
	s, err := schema.Load(loadSchema + more)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// codeKeys spells a result's violations as code:key, sorted.
func codeKeys(r *Result) string {
	var out []string
	for _, v := range r.Violations {
		out = append(out, fmt.Sprintf("%d:%s", v.Code, v.Key()))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// leafTables spells the top scope's leaves as table/alias.
func leafTables(r *Result) string {
	var out []string
	if r.Facts != nil && r.Facts.Top != nil {
		for _, l := range r.Facts.Top.Leaves {
			out = append(out, l.Table+"/"+l.Alias)
		}
	}
	return strings.Join(out, " ")
}

func TestLockTables(t *testing.T) {
	s := loadLoadSchema(t, "")
	r, err := Analyze(s, "LOCK TABLES t WRITE, p AS x READ LOCAL, v READ")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 0 || len(r.Params) != 0 || len(r.Violations) != 0 {
		t.Errorf("LOCK TABLES: %d columns, %d params, %d violations", len(r.Columns), len(r.Params), len(r.Violations))
	}
	if got := leafTables(r); got != "t/t p/x v/v" {
		t.Errorf("relations %q", got)
	}
	if r, err := Analyze(s, "UNLOCK TABLES"); err != nil || len(r.Columns) != 0 {
		t.Errorf("UNLOCK TABLES: %v", err)
	}
	for sql, code := range map[string]int{
		"LOCK TABLES nosuch WRITE":    1146,
		"LOCK TABLES t WRITE, t READ": 1066,
	} {
		if _, err := Analyze(s, sql); errCode(err) != code {
			t.Errorf("%s: %v, want %d", sql, err, code)
		}
	}
}

func TestLoadData(t *testing.T) {
	s := loadLoadSchema(t, "")
	for _, c := range []struct {
		sql, viol, params string
		writes            int
	}{
		// every column takes a field: the keys, the foreign key, the check, and a NULL for
		// a NOT NULL column (1263, not the INSERT's 1048)
		{"LOAD DATA INFILE '/tmp/f' INTO TABLE t", "1062:PRIMARY 1062:t.u 1263:t.a 1263:t.id 1452:fk 3819:chk", "", 1},
		// a column list: only those columns; a @var takes its field and assigns nothing; SET
		// assigns typed values (a placeholder takes the column's type)
		{"LOAD DATA INFILE '/tmp/f' INTO TABLE t (id, a, @x) SET b = @x, c = $1", "1062:PRIMARY 1263:t.a 1263:t.id 3819:chk", "int", 1},
		// SET NULL into a NOT NULL column is the INSERT's own 1048
		{"LOAD DATA INFILE '/tmp/f' INTO TABLE t (id) SET a = NULL", "1048:t.a 1062:PRIMARY 1263:t.id", "", 1},
		// a NOT NULL column with no default left out entirely takes the type's implicit
		// default (measured): not the INSERT's 1364
		{"LOAD DATA INFILE '/tmp/f' INTO TABLE t (id)", "1062:PRIMARY 1263:t.id", "", 1},
		{"LOAD DATA INFILE '/tmp/f' IGNORE INTO TABLE t", "", "", 1},
		// REPLACE deletes the colliding row: a second write, of Kind Delete
		{"LOAD DATA LOCAL INFILE '/tmp/f' REPLACE INTO TABLE t FIELDS TERMINATED BY ',' LINES TERMINATED BY '\\n' IGNORE 1 LINES", "1263:t.a 1263:t.id 1452:fk 3819:chk", "", 2},
	} {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if len(r.Columns) != 0 {
			t.Errorf("%s: %d columns, want none", c.sql, len(r.Columns))
		}
		if got := codeKeys(r); got != c.viol {
			t.Errorf("%s: violations %q, want %q", c.sql, got, c.viol)
		}
		var ps []string
		for _, p := range r.Params {
			ps = append(ps, param(p))
		}
		if got := strings.Join(ps, ","); got != c.params {
			t.Errorf("%s: params %q, want %q", c.sql, got, c.params)
		}
		if r.Facts == nil || len(r.Facts.Writes) != c.writes || r.Facts.Top.Many == "" || leafTables(r) != "t/t" {
			t.Errorf("%s: facts %+v", c.sql, r.Facts)
		}
	}
	for sql, code := range map[string]int{
		"LOAD DATA INFILE '/tmp/f' INTO TABLE nosuch":           1146,
		"LOAD DATA INFILE '/tmp/f' INTO TABLE t (id, nosuch)":   1054,
		"LOAD DATA INFILE '/tmp/f' INTO TABLE v":                1288,
		"LOAD DATA INFILE '/tmp/f' INTO TABLE t SET nosuch = 1": 1054,
		"LOAD DATA INFILE '/tmp/f' INTO TABLE t SET a = nosuch": 1054,
	} {
		if _, err := Analyze(s, sql); errCode(err) != code {
			t.Errorf("%s: %v, want %d", sql, err, code)
		}
	}
}

// SELECT ... INTO OUTFILE / DUMPFILE returns no result set (the rows go to a file on the
// server); the query is typed all the same, in either INTO position and under a locking
// clause. A top-level SELECT with a trailing FOR UPDATE / FOR SHARE keeps its columns.
func TestSelectIntoFileAndLocking(t *testing.T) {
	s := loadLoadSchema(t, "")
	for _, c := range []struct {
		sql     string
		columns int
		params  string
	}{
		{"SELECT a, b INTO OUTFILE '/tmp/o' FIELDS TERMINATED BY ',' FROM t WHERE id = $1", 0, "int"},
		{"SELECT a FROM t WHERE id = $1 INTO OUTFILE '/tmp/o'", 0, "int"},
		{"SELECT a INTO DUMPFILE '/tmp/o' FROM t LIMIT 1", 0, ""},
		{"SELECT a FROM t FOR UPDATE INTO OUTFILE '/tmp/o'", 0, ""},
		{"SELECT a INTO OUTFILE '/tmp/o' FROM t FOR SHARE", 0, ""},
		{"(SELECT a FROM t) UNION (SELECT id FROM p) INTO OUTFILE '/tmp/o'", 0, ""},
		{"SELECT id, a FROM t WHERE id = $1 FOR UPDATE", 2, "int"},
		{"SELECT id FROM t FOR SHARE NOWAIT", 1, ""},
		{"SELECT id FROM t LOCK IN SHARE MODE", 1, ""},
	} {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var ps []string
		for _, p := range r.Params {
			ps = append(ps, param(p))
		}
		if len(r.Columns) != c.columns || strings.Join(ps, ",") != c.params {
			t.Errorf("%s: %d columns, params %v; want %d, %q", c.sql, len(r.Columns), ps, c.columns, c.params)
		}
	}
	if _, err := Analyze(s, "SELECT nosuch INTO OUTFILE '/tmp/o' FROM t"); errCode(err) != 1054 {
		t.Errorf("INTO OUTFILE of an unknown column: %v, want 1054", err)
	}
}

// Inside a body: LOCK / UNLOCK TABLES, LOAD DATA and ALTER VIEW are 1314 for every routine
// kind; SELECT ... INTO OUTFILE is allowed even in a trigger or function (no result set).
func TestBodyLockLoadAlterViewOutfile(t *testing.T) {
	s := loadLoadSchema(t, `
CREATE TABLE u (id INT PRIMARY KEY);
CREATE PROCEDURE p1() BEGIN LOCK TABLES t WRITE; END;
CREATE FUNCTION f1() RETURNS INT DETERMINISTIC BEGIN UNLOCK TABLES; RETURN 1; END;
CREATE TRIGGER tr1 BEFORE INSERT ON u FOR EACH ROW BEGIN LOAD DATA INFILE '/tmp/x' INTO TABLE t; END;
CREATE PROCEDURE p2() BEGIN ALTER VIEW v AS SELECT a FROM t; END;
CREATE FUNCTION f2() RETURNS INT DETERMINISTIC BEGIN SELECT a INTO OUTFILE '/tmp/o' FROM t; RETURN 1; END;
CREATE TRIGGER tr2 BEFORE INSERT ON u FOR EACH ROW BEGIN SELECT a FROM t FOR UPDATE INTO OUTFILE '/tmp/o'; END;
`)
	if _, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "p1")); errCode(err) != 1314 || !strings.HasPrefix(err.Error(), "LOCK is not allowed") {
		t.Errorf("p1: %v", err)
	}
	if _, err := AnalyzeRoutine(s, s.RoutineOf(schema.Function, "f1")); errCode(err) != 1314 || !strings.HasPrefix(err.Error(), "UNLOCK is not allowed") {
		t.Errorf("f1: %v", err)
	}
	if _, err := AnalyzeTrigger(s, s.Trigger("tr1")); errCode(err) != 1314 || !strings.HasPrefix(err.Error(), "LOAD DATA is not allowed") {
		t.Errorf("tr1: %v", err)
	}
	if _, err := AnalyzeRoutine(s, s.RoutineOf(schema.Procedure, "p2")); errCode(err) != 1314 || !strings.HasPrefix(err.Error(), "ALTER VIEW is not allowed") {
		t.Errorf("p2: %v", err)
	}
	if br, err := AnalyzeRoutine(s, s.RoutineOf(schema.Function, "f2")); err != nil || len(br.Statements) != 1 {
		t.Errorf("f2: %v", err)
	}
	if br, err := AnalyzeTrigger(s, s.Trigger("tr2")); err != nil || len(br.Statements) != 1 || len(br.Statements[0].LockedReads) != 1 {
		t.Errorf("tr2: %v %+v", err, br)
	}
}

// The measurements the tests above rest on, against mysqld 8.4: the CREATE-time refusals
// (1314 for every routine kind, OUTFILE accepted), the resolution errors (1146 / 1066 /
// 1288 / 1054 / 1347), and LOAD DATA's failure modes on a real file (1263 for a NULL field
// in a NOT NULL column, 1048 for SET NULL, no 1364 for an omitted NOT NULL column, 1062 /
// 1452 / 3819, IGNORE swallowing, REPLACE affecting two rows).
func TestLockLoadOutfileAlterViewServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, loadSchema+"INSERT INTO p VALUES (1);\nINSERT INTO t VALUES (1, 1, 'x', 1, 1, 1);\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	code := func(err error) int {
		var me *driver.MySQLError
		if errors.As(err, &me) {
			return int(me.Number)
		}
		return 0
	}
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s := loadLoadSchema(t, "")
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"CREATE PROCEDURE p1() BEGIN LOCK TABLES t WRITE; END", 1314},
		{"CREATE FUNCTION f1() RETURNS INT DETERMINISTIC BEGIN UNLOCK TABLES; RETURN 1; END", 1314},
		{"CREATE TRIGGER tr1 BEFORE INSERT ON p FOR EACH ROW BEGIN LOAD DATA INFILE '/tmp/x' INTO TABLE t; END", 1314},
		{"CREATE PROCEDURE p2() BEGIN ALTER VIEW v AS SELECT a FROM t; END", 1314},
		{"CREATE FUNCTION f2() RETURNS INT DETERMINISTIC BEGIN SELECT a INTO OUTFILE '" + filepath.Join(dir, "f2.out") + "' FROM t; RETURN 1; END", 0},
		{"CREATE TRIGGER tr2 BEFORE INSERT ON p FOR EACH ROW BEGIN SELECT a FROM t FOR UPDATE INTO OUTFILE '" + filepath.Join(dir, "tr2.out") + "'; END", 0},
		{"ALTER VIEW nope AS SELECT id FROM t", 1146},
		{"ALTER VIEW t AS SELECT id FROM t", 1347},
		{"ALTER VIEW v AS SELECT a, COUNT(*) n FROM t GROUP BY a WITH CHECK OPTION", 1368},
		{"ALTER ALGORITHM=TEMPTABLE VIEW v (x, y, z) AS SELECT id, a, b FROM t", 0},
		{"LOCK TABLES nosuch WRITE", 1146},
		{"LOCK TABLES t WRITE, t READ", 1066},
		{"LOCK TABLES t WRITE, p AS x READ LOCAL, v READ", 0},
		{"UNLOCK TABLES", 0},
		{"LOAD DATA INFILE '" + file("null.csv", "2\t\\N\tx\t1\t1\t2\n") + "' INTO TABLE t", 1263},
		{"LOAD DATA INFILE '" + file("dup.csv", "1\t1\tx\t1\t1\t3\n") + "' INTO TABLE t", 1062},
		{"LOAD DATA INFILE '" + file("uniq.csv", "3\t1\tx\t1\t1\t1\n") + "' INTO TABLE t", 1062},
		{"LOAD DATA INFILE '" + file("fk.csv", "4\t1\tx\t1\t9\t4\n") + "' INTO TABLE t", 1452},
		{"LOAD DATA INFILE '" + file("chk.csv", "5\t1\tx\t-1\t1\t5\n") + "' INTO TABLE t", 3819},
		{"LOAD DATA INFILE '" + file("id.csv", "8\n") + "' INTO TABLE t (id) SET a = NULL", 1048},
		{"LOAD DATA INFILE '" + filepath.Join(dir, "id.csv") + "' INTO TABLE t (id)", 0}, // a omitted: no 1364, the implicit default
		{"LOAD DATA INFILE '" + filepath.Join(dir, "dup.csv") + "' IGNORE INTO TABLE t", 0},
		{"LOAD DATA INFILE '" + filepath.Join(dir, "null.csv") + "' IGNORE INTO TABLE t", 0},
		{"LOAD DATA INFILE '" + file("ok.csv", "7\t1\tx\t1\t1\t7\n") + "' INTO TABLE t", 0},
		{"LOAD DATA INFILE '" + filepath.Join(dir, "ok.csv") + "' INTO TABLE t (id, a, b, c, pid, u, nosuch)", 1054},
		{"LOAD DATA INFILE '" + filepath.Join(dir, "ok.csv") + "' INTO TABLE v", 1288},
		{"LOAD DATA INFILE '" + filepath.Join(dir, "ok.csv") + "' INTO TABLE nosuch", 1146},
		{"SELECT a INTO OUTFILE '" + filepath.Join(dir, "sel.out") + "' FROM t WHERE id = 1", 0},
		{"SELECT nosuch INTO OUTFILE '" + filepath.Join(dir, "sel2.out") + "' FROM t", 1054},
	} {
		_, err := db.Conn().ExecContext(ctx, c.sql)
		if got := code(err); got != c.want {
			t.Errorf("server: %s => %v, want %d", c.sql, err, c.want)
		}
	}
	// REPLACE: the colliding row is deleted and the new one inserted, two rows affected
	res, err := db.Conn().ExecContext(ctx, "LOAD DATA INFILE '"+filepath.Join(dir, "dup.csv")+"' REPLACE INTO TABLE t")
	if err != nil {
		t.Errorf("REPLACE: %v", err)
	} else if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("REPLACE affected %d rows, want 2", n)
	}
	// the analyzer's own verdicts on the same statements: the errors above where the
	// server's is a resolution error, the violation set where it is a constraint
	for sql, want := range map[string]int{
		"LOCK TABLES nosuch WRITE":                                         1146,
		"LOCK TABLES t WRITE, t READ":                                      1066,
		"LOAD DATA INFILE '/f' INTO TABLE t (id, a, b, c, pid, u, nosuch)": 1054,
		"LOAD DATA INFILE '/f' INTO TABLE v":                               1288,
		"LOAD DATA INFILE '/f' INTO TABLE nosuch":                          1146,
		"SELECT nosuch INTO OUTFILE '/o' FROM t":                           1054,
	} {
		if _, err := Analyze(s, sql); errCode(err) != want {
			t.Errorf("analyzer: %s => %v, want %d", sql, err, want)
		}
	}
	r, err := Analyze(s, "LOAD DATA INFILE '/f' INTO TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{1263, 1062, 1452, 3819} {
		found := false
		for _, v := range r.Violations {
			found = found || v.Code == code
		}
		if !found {
			t.Errorf("analyzer: LOAD DATA into t lacks the %d the server raised", code)
		}
	}
}
