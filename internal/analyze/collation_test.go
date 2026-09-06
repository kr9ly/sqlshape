package analyze

import (
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestCollationNotes covers the indeterminate-collation findings (collation.go): PG
// accepts these statements and fails at run time, except that sorting, grouping and
// DISTINCT on a conflict fail at parse time (42P21, assign_query_collations). Schema:
// users.alias is varchar COLLATE "C", users.name / users.nick have the default collation.
func TestCollationNotes(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	// p is a column with implicit collation "POSIX", reached through a subquery
	const posix = `(SELECT id, name COLLATE "POSIX" AS p FROM users) s`
	cases := []struct {
		sql   string
		notes []string // substrings, one per expected note, in order
		err   string   // expected SQLSTATE instead
	}{
		// a non-default collation beats the default: no conflict
		{sql: `SELECT id FROM users WHERE name = alias`},
		{sql: `SELECT id FROM users WHERE alias = 'x' AND alias LIKE $1`},
		{sql: `SELECT id FROM users ORDER BY name || alias`},
		{sql: `SELECT lower(alias || name) FROM users`},
		// an explicit collation settles it
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias = s.p COLLATE "C"`},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE (u.alias || s.p) COLLATE "C" < 'm'`},
		// operations that never compare are fine, the result type decides too
		{sql: `SELECT u.alias || s.p FROM users u, ` + posix},
		{sql: `SELECT length(u.alias || s.p) FROM users u, ` + posix},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE length(u.alias) = length(s.p)`},
		// two non-default implicit collations conflict where a comparison needs one
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias = s.p`, notes: []string{`string comparison: implicit collations "C" and "POSIX"`}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE s.p < u.alias`, notes: []string{`"POSIX" and "C"`}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias LIKE s.p`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE (u.alias || s.p) = 'x'`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE coalesce(u.alias, s.p) = 'x'`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias IN (s.p, 'x')`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias BETWEEN s.p AND 'z'`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE nullif(u.alias, s.p) IS NULL`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias = ANY(ARRAY[s.p])`, notes: []string{"string comparison"}},
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE CASE u.alias WHEN s.p THEN true ELSE false END`, notes: []string{"string comparison"}},
		// functions and aggregates that compare or case-fold
		{sql: `SELECT lower(u.alias || s.p) FROM users u, ` + posix, notes: []string{"lower()"}},
		{sql: `SELECT max(u.alias || s.p) FROM users u, ` + posix, notes: []string{"max()"}},
		{sql: `SELECT greatest(u.alias, s.p) FROM users u, ` + posix, notes: []string{"GREATEST"}},
		// sorting, grouping and DISTINCT compare too, and PG refuses them at parse time
		{sql: `SELECT u.alias || s.p AS x FROM users u, ` + posix + ` ORDER BY x`, err: "42P21"},
		{sql: `SELECT u.alias || s.p AS x FROM users u, ` + posix + ` ORDER BY 1`, err: "42P21"},
		{sql: `SELECT u.id FROM users u, ` + posix + ` ORDER BY u.alias || s.p`, err: "42P21"},
		{sql: `SELECT u.alias || s.p AS x FROM users u, ` + posix + ` GROUP BY 1`, err: "42P21"},
		{sql: `SELECT DISTINCT u.alias || s.p FROM users u, ` + posix, err: "42P21"},
		{sql: `SELECT DISTINCT ON (u.alias || s.p) u.id FROM users u, ` + posix, err: "42P21"},
		// UNION ALL never compares; the conflict travels with the column
		{sql: `SELECT x FROM (SELECT alias AS x FROM users UNION ALL SELECT p FROM ` + posix + `) t WHERE x = 'a'`, notes: []string{"string comparison"}},
		// the finding sits on the operator
		{sql: `SELECT u.id FROM users u, ` + posix + ` WHERE u.alias = s.p`, notes: []string{"(at 93)"}},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			r, err := Analyze(s, c.sql)
			if c.err != "" {
				if err == nil || !strings.HasPrefix(err.Error(), c.err) {
					t.Fatalf("want error %s, got %v", c.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			var got []string
			for _, n := range r.Notes {
				if n.Advisory() {
					continue
				}
				if n.Code != noteCollation {
					t.Errorf("unexpected code %q: %s", n.Code, n.Message)
				}
				got = append(got, n.Message+" (at "+itoa(n.Position)+")")
			}
			if len(got) != len(c.notes) {
				t.Fatalf("notes: want %d %q, got %q", len(c.notes), c.notes, got)
			}
			for i, want := range c.notes {
				if !strings.Contains(got[i], want) {
					t.Errorf("note %d: want %q in %q", i, want, got[i])
				}
			}
		})
	}
}
