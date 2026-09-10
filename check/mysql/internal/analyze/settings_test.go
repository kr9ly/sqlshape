package analyze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/oracle"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

// settings_test.go: the analyzer follows the server settings the schema declares
// (`-- sqlshape: server ...`). The unit tests state the rules; TestSettingsServer starts a
// mysqld with the same declarations (mysqltest passes them through) and confirms every
// rule against it.

const settingsTables = `
CREATE TABLE a (id INT NOT NULL PRIMARY KEY, n INT NULL, u INT UNSIGNED NOT NULL, s VARCHAR(20) NOT NULL, r REAL NULL);
CREATE TABLE b (s VARCHAR(20) NULL);
`

var settingsSchemas = map[string]string{
	"default": "-- sqlshape: mysql 8.4\n" + settingsTables,
	"empty":   "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = ''\n" + settingsTables,
	"ansi":    "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI'\n" + settingsTables,
	"nounsub": "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'NO_UNSIGNED_SUBTRACTION,STRICT_ALL_TABLES'\n" + settingsTables,
}

// settingsCase is one statement judged under one declared mode: the result column's type
// and nullability (as typeKey / oracleKey reduce them) when it is accepted, or the error
// number when it is rejected.
type settingsCase struct {
	schema string
	sql    string
	typ    string // "" for an error case
	null   bool
	code   int
}

var settingsCases = []settingsCase{
	// ONLY_FULL_GROUP_BY: on by default and under ANSI, off under ''
	{"default", "SELECT s FROM a GROUP BY n", "", false, 1055},
	{"ansi", "SELECT s FROM a GROUP BY n", "", false, 1055},
	{"empty", "SELECT s FROM a GROUP BY n", "string", false, 0},
	{"empty", "SELECT DISTINCT s FROM a ORDER BY n", "string", false, 0},
	{"default", "SELECT DISTINCT s FROM a ORDER BY n", "", false, 3065},
	// the rules the server applies in every mode stay
	{"empty", "SELECT s FROM a HAVING n > 1", "", false, 1054},
	{"empty", "SELECT s FROM a ORDER BY COUNT(*)", "", false, 3029},
	// PIPES_AS_CONCAT: `||` is OR (a bigint(1)) without it, CONCAT under ANSI
	{"default", "SELECT 'a' || 'b' FROM a", "bigint", false, 0},
	{"ansi", "SELECT 'a' || 'b' FROM a", "string", false, 0},
	{"ansi", "SELECT s || s FROM a", "string", false, 0},
	// ANSI_QUOTES: "s" is a column under ANSI, a string literal otherwise
	{"ansi", `SELECT "s" FROM b`, "string", true, 0},
	{"default", `SELECT "s" FROM b`, "string", false, 0},
	{"ansi", `SELECT "nope" FROM b`, "", false, 1054},
	// strict mode: Item_str_func is nullable only in strict mode (the default; ANSI is not strict)
	{"default", "SELECT CONCAT(s, 'x') FROM a", "string", true, 0},
	{"empty", "SELECT CONCAT(s, 'x') FROM a", "string", false, 0},
	{"ansi", "SELECT CONCAT(s, 'x') FROM a", "string", false, 0},
	{"ansi", "SELECT CONCAT(s, 'x') FROM b", "string", true, 0},
	// REAL_AS_FLOAT: the column r is FLOAT under ANSI, DOUBLE otherwise
	{"default", "SELECT r FROM a", "double", true, 0},
	{"ansi", "SELECT r FROM a", "float", true, 0},
	// NO_UNSIGNED_SUBTRACTION: an unsigned difference is signed
	{"default", "SELECT u - 1 FROM a", "bigint unsigned", false, 0},
	{"nounsub", "SELECT u - 1 FROM a", "bigint", false, 0},
	{"nounsub", "SELECT u + 1 FROM a", "bigint unsigned", false, 0},
}

func loadSettings(t *testing.T, name string) *schema.Schema {
	t.Helper()
	s, err := schema.Load(settingsSchemas[name])
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("%s: schema problems: %v", name, s.Problems)
	}
	return s
}

func TestSettingsRules(t *testing.T) {
	for _, c := range settingsCases {
		s := loadSettings(t, c.schema)
		r, err := Analyze(s, c.sql)
		if c.code != 0 {
			var ae *Error
			if !errors.As(err, &ae) || ae.Code != c.code {
				t.Errorf("[%s] %s: got %v, want error %d", c.schema, c.sql, err, c.code)
			}
			continue
		}
		if err != nil {
			t.Errorf("[%s] %s: %v", c.schema, c.sql, err)
			continue
		}
		if len(r.Columns) != 1 || !r.Columns[0].Known {
			t.Errorf("[%s] %s: columns %+v", c.schema, c.sql, r.Columns)
			continue
		}
		if got := typeKey(r.Columns[0].Type); got != c.typ || r.Columns[0].Nullable != c.null {
			t.Errorf("[%s] %s: %s nullable=%v, want %s nullable=%v", c.schema, c.sql, got, r.Columns[0].Nullable, c.typ, c.null)
		}
	}
}

// Without strict mode a NULL for a NOT NULL column is an error (1048) only in a single-row
// INSERT / REPLACE, its ON DUPLICATE KEY UPDATE included; more rows, the query form and an
// UPDATE store the implicit default with a warning.
var notNullCases = []struct {
	sql         string
	strict, lax bool // whether a.s (NOT NULL) is listed as a possible violation
}{
	{"INSERT INTO a (id, u, s) VALUES (1, 1, $1)", true, true},
	{"REPLACE INTO a (id, u, s) VALUES (1, 1, $1)", true, true},
	{"INSERT INTO a (id, u, s) VALUES (1, 1, 'x') ON DUPLICATE KEY UPDATE s = $1", true, true},
	{"INSERT INTO a (id, u, s) VALUES (1, 1, $1), (2, 1, 'y')", true, false},
	{"INSERT INTO a (id, u, s) SELECT 3, 1, s FROM b", true, false},
	{"UPDATE a SET s = $1 WHERE id = 1", true, false},
}

func TestSettingsNotNull(t *testing.T) {
	for _, c := range notNullCases {
		for name, want := range map[string]bool{"default": c.strict, "empty": c.lax} {
			r, err := Analyze(loadSettings(t, name), c.sql)
			if err != nil {
				t.Fatalf("[%s] %s: %v", name, c.sql, err)
			}
			got := false
			for _, v := range r.Violations {
				if v.Code == 1048 && v.Key() == "a.s" {
					got = true
				}
			}
			if got != want {
				t.Errorf("[%s] %s: NOT NULL violation listed = %v, want %v", name, c.sql, got, want)
			}
		}
	}
}

// lower_case_table_names: 0 distinguishes, 1 lower-cases, 2 compares without case.
func TestSettingsLowerCaseTableNames(t *testing.T) {
	const tables = "CREATE TABLE Users (id INT PRIMARY KEY);\nCREATE VIEW ActiveUsers AS SELECT id FROM Users;\n"
	for lctn, want := range map[string]string{"0": "Users", "1": "users", "2": "Users"} {
		s, err := schema.Load("-- sqlshape: mysql 8.4\n-- sqlshape: server lower_case_table_names = " + lctn + "\n" + tables)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Problems) > 0 {
			t.Fatalf("lctn=%s: %v", lctn, s.Problems)
		}
		if s.Tables[0].Name != want || s.Table(want) == nil {
			t.Errorf("lctn=%s: table named %q, want %q", lctn, s.Tables[0].Name, want)
		}
		for _, sql := range []string{"SELECT id FROM USERS", "SELECT id FROM activeusers"} {
			r, err := Analyze(s, sql)
			if lctn == "0" {
				var ae *Error
				if !errors.As(err, &ae) || ae.Code != 1146 {
					t.Errorf("lctn=0: %s: got %v, want 1146", sql, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("lctn=%s: %s: %v", lctn, sql, err)
				continue
			}
			if got := r.Facts.Top.Leaves[0].Table; got != strings.ToLower(want) && got != want && got != "ActiveUsers" && got != "activeusers" {
				t.Errorf("lctn=%s: %s: leaf table %q", lctn, sql, got)
			}
		}
		if lctn == "1" {
			if r, err := Analyze(s, "SELECT id FROM ACTIVEUSERS"); err != nil || r.Facts.Top.Leaves[0].Table != "activeusers" {
				t.Errorf("lctn=1: the view is named lower-cased: %v %+v", err, r)
			}
		}
	}
}

func TestSettingsProblems(t *testing.T) {
	cases := []struct{ line, want string }{
		{"-- sqlshape: server sql_mode = 'STRICT'", `"STRICT" is not a mode of MySQL 8.4`},
		{"-- sqlshape: server lower_case_table_names = 3", "want 0, 1 or 2"},
		{"-- sqlshape: server max_allowed_packet = 64M", "not a variable sqlshape reads for MySQL (sql_mode, lower_case_table_names)"},
	}
	for _, c := range cases {
		s, err := schema.Load("-- sqlshape: mysql 8.4\n" + c.line + "\nCREATE TABLE t (a INT);")
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Problems) != 1 || !strings.Contains(s.Problems[0].Message, c.want) || s.Problems[0].Position != 23 {
			t.Errorf("%s: problems %v, want one at 23 saying %q", c.line, s.Problems, c.want)
		}
		if len(s.Tables) != 1 || len(s.Tables[0].Directives) != 0 {
			t.Errorf("%s: the setting is not the table's directive: %+v", c.line, s.Tables)
		}
	}
	if _, err := schema.Load("-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode\n"); err == nil || !strings.Contains(err.Error(), "want `<variable> = <value>`") {
		t.Errorf("malformed: %v", err)
	}
	s, _ := schema.Load(settingsSchemas["ansi"])
	if !s.Settings.SQLMode.Has(sqlmode.ANSI|sqlmode.ANSIQuotes|sqlmode.OnlyFullGroupBy) || s.Settings.Strict() {
		t.Errorf("ansi: %v", s.Settings.SQLMode)
	}
	if s, _ := schema.Load(settingsSchemas["default"]); s.Settings.SQLMode != sqlmode.Default || !s.Settings.Strict() {
		t.Errorf("default: %v", s.Settings.SQLMode)
	}
}

// TestSettingsServer confirms every case above against a mysqld started with the same
// declarations. Skipped without a mysqld on PATH (nix-shell -p mysql84).
func TestSettingsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	oracles := map[string]*oracle.Oracle{}
	for name, text := range settingsSchemas {
		o, err := oracle.Start(ctx, text)
		if errors.Is(err, oracle.ErrNoServer) {
			t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer o.Close()
		oracles[name] = o
	}
	for _, c := range settingsCases {
		d, err := oracles[c.schema].Describe(ctx, c.sql)
		if c.code != 0 {
			var oe *oracle.Error
			if !errors.As(err, &oe) || oe.Number != c.code {
				t.Errorf("[%s] %s: the server says %v, the analyzer %d", c.schema, c.sql, err, c.code)
			}
			continue
		}
		if err != nil {
			t.Errorf("[%s] %s: the server rejects it: %v", c.schema, c.sql, err)
			continue
		}
		if len(d.Columns) != 1 || oracleKey(d.Columns[0]) != c.typ || d.Columns[0].Nullable != c.null {
			t.Errorf("[%s] %s: the server says %+v, the analyzer %s nullable=%v", c.schema, c.sql, d.Columns, c.typ, c.null)
		}
	}
	// the NOT NULL matrix needs execution: a fresh server without strict mode
	db, err := mysqltest.Start(ctx, settingsSchemas["empty"])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO b VALUES (NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range notNullCases {
		sql := strings.ReplaceAll(c.sql, "$1", "NULL")
		if c.sql == "INSERT INTO a (id, u, s) VALUES (1, 1, 'x') ON DUPLICATE KEY UPDATE s = $1" {
			conn.ExecContext(ctx, "INSERT INTO a (id, u, s) VALUES (1, 1, 'x')") // the row the update hits
		}
		_, err := conn.ExecContext(ctx, sql)
		rejected := err != nil && strings.Contains(err.Error(), "Error 1048")
		if err != nil && !rejected {
			t.Errorf("%s: %v", sql, err)
		}
		if rejected != c.lax {
			t.Errorf("%s: the server without strict mode rejects it = %v, the analyzer lists NOT NULL = %v", sql, rejected, c.lax)
		}
		conn.ExecContext(ctx, "DELETE FROM a")
	}
	t.Log(fmt.Sprintf("server %s agrees on %d statements and %d writes", db.Version, len(settingsCases), len(notNullCases)))
}

// A directive names a table as the server would resolve it: under lower_case_table_names 1
// or 2 any spelling reaches the declared table, and the waiver meets the facts' leaf.
func TestSettingsDirectiveNames(t *testing.T) {
	for _, lctn := range []string{"1", "2"} {
		s, err := schema.Load("-- sqlshape: mysql 8.4\n-- sqlshape: server lower_case_table_names = " + lctn + `
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE Users (id INT PRIMARY KEY, deleted_at DATETIME NULL);
-- sqlshape: unfiltered USERS
CREATE VIEW AllUsers AS SELECT id FROM users;
`)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Problems) > 0 {
			t.Fatalf("lctn=%s: %v", lctn, s.Problems)
		}
		want := s.Tables[0].Name
		if !s.Views[0].Unfiltered[want] {
			t.Errorf("lctn=%s: the view's unfiltered keys are %v, want %q", lctn, s.Views[0].Unfiltered, want)
		}
		r, err := Analyze(s, "-- sqlshape: unfiltered uSeRs\nSELECT id FROM Users")
		if err != nil {
			t.Fatal(err)
		}
		if lf := r.Facts.Top.Leaves[0]; lf.Table != want || len(lf.Waived) != 1 || lf.Waived[0] != "unfiltered" {
			t.Errorf("lctn=%s: leaf %+v, want table %q waived [unfiltered]", lctn, lf, want)
		}
	}
	// at 0 the spelling must match exactly: a differently spelled waiver waives nothing
	s, _ := schema.Load("-- sqlshape: mysql 8.4\nCREATE TABLE Users (id INT PRIMARY KEY);\n")
	r, err := Analyze(s, "-- sqlshape: unfiltered users\nSELECT id FROM Users")
	if err != nil || len(r.Facts.Top.Leaves[0].Waived) != 0 {
		t.Errorf("lctn=0: %v %+v", err, r.Facts.Top.Leaves[0])
	}
}
