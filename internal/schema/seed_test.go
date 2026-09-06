package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/schema"
)

func mustLoad(t *testing.T, sql string) *schema.Schema {
	t.Helper()
	s, err := analyze.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSeed(t *testing.T) {
	base := `
CREATE TABLE order_statuses (code text PRIMARY KEY, label text NOT NULL, sort_order int NOT NULL DEFAULT 0, note text);
CREATE TABLE tags (id serial PRIMARY KEY, name text NOT NULL UNIQUE, created timestamptz NOT NULL DEFAULT now());
CREATE TABLE loose (id serial PRIMARY KEY, name text UNIQUE);
CREATE FUNCTION vol() RETURNS int LANGUAGE sql VOLATILE RETURN 1;
CREATE FUNCTION imm() RETURNS int LANGUAGE sql IMMUTABLE RETURN 1;
`
	cases := []struct {
		name, sql, problem string
	}{
		{"pk rows", "INSERT INTO order_statuses (code, label, sort_order) VALUES ('pending', 'Pending', 10), ('paid', 'Paid', 20);", ""},
		{"unique key without serial", "INSERT INTO tags (name) VALUES ('a'), ('b');", ""},
		{"two statements", "INSERT INTO tags (name) VALUES ('a'); INSERT INTO tags (name) VALUES ('b');", ""},
		{"reordered columns", "INSERT INTO order_statuses (code, label) VALUES ('a', 'A'); INSERT INTO order_statuses (label, code) VALUES ('B', 'b');", ""},
		{"immutable function", "INSERT INTO tags (name) VALUES (upper('a') || imm()::text);", ""},
		{"additive", "-- sqlshape: seed\nINSERT INTO tags (name) VALUES ('a');", ""},
		{"no key given", "INSERT INTO loose (name) VALUES ('a');", "no key identifies the rows"},
		{"volatile", "INSERT INTO tags (name) VALUES (vol()::text);", "vol() is not IMMUTABLE"},
		{"stable", "INSERT INTO tags (name) VALUES (now()::text);", "now() is not IMMUTABLE"},
		{"subquery", "INSERT INTO tags (name) VALUES ((SELECT 'a'));", "contains a subquery"},
		{"default", "INSERT INTO order_statuses (code, label, sort_order) VALUES ('a', 'A', DEFAULT);", "leave the column out"},
		{"on conflict", "INSERT INTO tags (name) VALUES ('a') ON CONFLICT DO NOTHING;", "ON CONFLICT"},
		{"select", "INSERT INTO tags (name) SELECT 'a';", "must be a VALUES list"},
		{"duplicate key", "INSERT INTO tags (name) VALUES ('a'), ('a');", "given twice"},
		{"columns differ", "INSERT INTO order_statuses (code, label) VALUES ('a', 'A'); INSERT INTO order_statuses (code, label, sort_order) VALUES ('b', 'B', 1);", "same columns"},
		{"type error", "INSERT INTO order_statuses (code, label, sort_order) VALUES ('a', 'A', 'x');", "22P02"},
		{"not null", "INSERT INTO order_statuses (code) VALUES ('a');", "NOT NULL without a default"},
		{"view", "CREATE VIEW v AS SELECT * FROM tags; INSERT INTO v (name) VALUES ('a');", "only tables"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustLoad(t, base+c.sql)
			var msgs []string
			for _, p := range s.Problems {
				msgs = append(msgs, p.Message)
			}
			got := strings.Join(msgs, "\n")
			if c.problem == "" && got != "" {
				t.Fatalf("unexpected problems:\n%s", got)
			}
			if c.problem != "" && !strings.Contains(got, c.problem) {
				t.Fatalf("want problem containing %q, got:\n%s", c.problem, got)
			}
		})
	}
	s := mustLoad(t, base+"INSERT INTO order_statuses (code, label) VALUES ('a', 'A'); INSERT INTO order_statuses (label, code) VALUES ('B', 'b');")
	sd := s.Relation("", "order_statuses").Seed
	if sd == nil || strings.Join(sd.Columns, ",") != "code,label" || strings.Join(sd.Key, ",") != "code" || len(sd.Rows) != 2 || sd.RowText(sd.Rows[1]) != "'b', 'B'" {
		t.Fatalf("seed: %+v", sd)
	}
	if s.Relation("", "tags").Seed != nil {
		t.Fatal("tags is not seeded")
	}
}
