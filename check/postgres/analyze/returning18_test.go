package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
)

const returning18Schema = `-- sqlshape: postgres 18
CREATE TABLE foo (f1 int PRIMARY KEY, f2 text, f3 int NOT NULL DEFAULT 0);
CREATE TABLE zerocol ();
CREATE TABLE src (f1 int, f2 text);
`

// RETURNING old / new (PostgreSQL 18): the row before and after the write, reached only
// by a qualified reference; old does not exist for an inserted row and new not for a
// deleted one, so those are nullable (the oracle reports the column's own NOT NULL there,
// which is what its Describe knows; the analyzer is right).
func TestReturningOldNew(t *testing.T) {
	s, err := Load(returning18Schema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql  string
		want string // the analyzer's rendering, or "error: <code>"
	}{
		{"INSERT INTO foo (f1) VALUES ($1) RETURNING old.f1, old.f3, new.f1, new.f3, f3",
			"params:\n  $1 integer\ncolumns:\n  f1 integer <- foo.f1 null\n  f3 integer <- foo.f3 null\n  f1 integer <- foo.f1 not null\n  f3 integer <- foo.f3 not null\n  f3 integer <- foo.f3 not null\n"},
		{"UPDATE foo SET f3 = f3 + 1 WHERE f1 = $1 RETURNING old.f3, new.f3, old, new",
			"params:\n  $1 integer\ncolumns:\n  f3 integer <- foo.f3 not null\n  f3 integer <- foo.f3 not null\n  old foo\n  new foo\n"},
		{"DELETE FROM foo WHERE f1 = $1 RETURNING old.f3, new.f3, old, new",
			"params:\n  $1 integer\ncolumns:\n  f3 integer <- foo.f3 not null\n  f3 integer <- foo.f3 null\n  old foo\n  new foo\n"},
		{"UPDATE foo SET f3 = 1 RETURNING WITH (OLD AS o, NEW AS n) o.f3, n.f3, o.ctid",
			"params:\ncolumns:\n  f3 integer <- foo.f3 not null\n  f3 integer <- foo.f3 not null\n  ctid tid <- foo.ctid not null\n"},
		// unqualified names never mean the row variables; * does not expand them
		{"UPDATE foo SET f3 = 1 RETURNING *",
			"params:\ncolumns:\n  f1 integer <- foo.f1 not null\n  f2 text <- foo.f2 null\n  f3 integer <- foo.f3 not null\n"},
		// a FROM item named old hides the default alias; a chosen alias may not clash with one
		{"UPDATE foo SET f3 = old.f1 FROM src AS old WHERE foo.f1 = old.f1 RETURNING old.f2",
			"params:\ncolumns:\n  f2 text <- src.f2 null\n"},
		{"UPDATE foo SET f3 = 1 FROM src AS o RETURNING WITH (OLD AS o) o.f3", "error: 42712"},
		{"UPDATE foo SET f3 = 1 RETURNING WITH (OLD AS x, NEW AS x) x.f3", "error: 42712"},
		{"UPDATE foo SET f3 = 1 RETURNING WITH (OLD AS o, OLD AS p) o.f3", "error: 42601"},
		{"INSERT INTO zerocol SELECT RETURNING old.*, new.*, *", "error: 42601"},
		{"MERGE INTO foo USING src ON foo.f1 = src.f1 WHEN MATCHED THEN UPDATE SET f2 = src.f2 RETURNING old.f3, new.f3",
			"params:\ncolumns:\n  f3 integer <- foo.f3 not null\n  f3 integer <- foo.f3 not null\n"},
		{"MERGE INTO foo USING src ON foo.f1 = src.f1 WHEN MATCHED THEN DELETE WHEN NOT MATCHED THEN INSERT (f1) VALUES (src.f1) RETURNING old.f3, new.f3",
			"params:\ncolumns:\n  f3 integer <- foo.f3 null\n  f3 integer <- foo.f3 null\n"},
	}
	for _, c := range cases {
		got := renderAnalyzer(s, c.sql)
		if strings.HasPrefix(c.want, "error:") {
			if !strings.HasPrefix(got, c.want) {
				t.Errorf("%s\n got %s want %s", c.sql, got, c.want)
			}
		} else if got != c.want {
			t.Errorf("%s\n got:\n%s want:\n%s", c.sql, got, c.want)
		}
	}
	// a 17 schema has no row variables
	s17, err := Load(strings.TrimPrefix(returning18Schema, "-- sqlshape: postgres 18\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderAnalyzer(s17, "UPDATE foo SET f3 = 1 RETURNING old.f3"); !strings.HasPrefix(got, "error: 42P01") {
		t.Errorf("17: %s", got)
	}

	// the oracle agrees on every error code and on every column's name and type
	if testing.Short() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.StartVersion(ctx, pgparse.PG18, returning18Schema)
	if err != nil {
		t.Skipf("PostgreSQL 18 oracle: %v", err)
	}
	defer o.Close()
	for _, c := range cases {
		want := renderOracle(ctx, o, c.sql)
		got := renderAnalyzer(s, c.sql)
		if strings.HasPrefix(want, "error:") || strings.HasPrefix(got, "error:") {
			if !match(want, got) {
				t.Errorf("%s\n oracle %s analyzer %s", c.sql, want, got)
			}
			continue
		}
		// nullability aside (see above), the two agree
		strip := func(s string) string { return strings.NewReplacer(" not null", "", " null", "").Replace(s) }
		if strip(want) != strip(got) {
			t.Errorf("%s\n oracle:\n%s analyzer:\n%s", c.sql, want, got)
		}
	}
}
