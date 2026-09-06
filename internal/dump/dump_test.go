package dump

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/diff"
)

func TestNormalize(t *testing.T) {
	in := "--\n\\restrict abc\nSET a = 1;\nSELECT pg_catalog.set_config('search_path', '', false);\nCREATE TABLE t (id int);\n\\unrestrict abc\n"
	want := "--\nSET a = 1;\nSET search_path = '';\nCREATE TABLE t (id int);\n"
	if got := Normalize(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func requirePgDump(t *testing.T) {
	if _, err := exec.LookPath(Binary()); err != nil {
		t.Skipf("%s not found: %v", Binary(), err)
	}
}

// The examples' schema.sql each round-trip through PostgreSQL: the dump loads without
// problems, and a second round trip of the dump is a fixed point (no changes).
func TestCanonicalFixedPoint(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	srv, err := NewServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, ex := range []string{"1-tables", "2-database-api", "3-everything"} {
		t.Run(ex, func(t *testing.T) {
			sql, err := os.ReadFile(filepath.Join("../../examples", ex, "schema.sql"))
			if err != nil {
				t.Fatal(err)
			}
			s1, text, err := srv.Canonical(ctx, string(sql), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range s1.Problems {
				t.Errorf("problem loading dump: %s", p)
			}
			s2, _, err := srv.Canonical(ctx, text, nil)
			if err != nil {
				t.Fatalf("re-applying the dump: %v", err)
			}
			if changes := diff.Compare(s1, s2); len(changes) > 0 {
				var lines []string
				for _, c := range changes {
					lines = append(lines, c.String())
				}
				t.Errorf("dump is not a fixed point:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// Edits to a schema show up as changes between the two canonical forms, in PostgreSQL's
// spelling of the expressions.
func TestCanonicalChanges(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base, err := os.ReadFile("../../examples/1-tables/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	edit := string(base) + `
ALTER TABLE customers ADD COLUMN nickname text DEFAULT 'anon';
ALTER TABLE orders ALTER COLUMN total SET DEFAULT 1;
CREATE INDEX orders_status_idx ON orders (status) WHERE status <> 'paid';
ALTER TYPE order_status ADD VALUE 'refunded';
COMMENT ON TABLE orders IS 'orders placed';
`
	from, _, err := Canonical(ctx, string(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := Canonical(ctx, edit, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range diff.Compare(from, to) {
		lines = append(lines, c.String())
	}
	got := strings.Join(lines, "\n")
	want := strings.TrimSpace(`
~ enum order_status
    labels: pending, paid, shipped, cancelled -> pending, paid, shipped, cancelled, refunded
~ table customers
    column order: id, email, name, created_at -> id, email, name, created_at, nickname
+ column customers.nickname
~ column orders.total
    default: 0 -> 1
+ index orders.orders_status_idx
+ comment orders
`)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
