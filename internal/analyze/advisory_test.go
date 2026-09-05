package analyze

import (
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestAdvisoryNotes covers the advisory notes: LIMIT without ORDER BY, enum ordering.
func TestAdvisoryNotes(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ sql, want string }{
		{"SELECT id FROM orders LIMIT 10", "unordered-limit"},
		{"SELECT id FROM orders ORDER BY id LIMIT 10", ""},
		{"SELECT id FROM orders WHERE id = $1 LIMIT 1", ""},
		{"SELECT count(*) FROM orders LIMIT 5", ""},
		{"SELECT id FROM orders WHERE status > 'paid'", "enum-order"},
		{"SELECT id FROM orders WHERE status = 'paid'", ""},
		{"SELECT id FROM orders ORDER BY status", "enum-order"},
		{"SELECT status AS s FROM orders ORDER BY s", "enum-order"},
		{"SELECT id FROM orders o ORDER BY o.status DESC, o.id", "enum-order"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var codes []string
		for _, n := range r.Notes {
			codes = append(codes, n.Code)
			if !n.Advisory() {
				t.Errorf("%s: non-advisory note %v", c.sql, n)
			}
		}
		if got := strings.Join(codes, ","); got != c.want {
			t.Errorf("%s: want %q, got %q", c.sql, c.want, got)
		}
	}
}

// TestFunctionNullability: `-- sqlshape: not null` on a schema function and STRICT user functions.
func TestFunctionNullability(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	cases := []struct {
		sql      string
		nullable bool
	}{
		{"SELECT order_count(1)", false},
		{"SELECT nick_of(1)", false},
		{"SELECT nick_of(o.user_id) FROM orders o", false},
		{"SELECT nick_of(u.id) FROM users u LEFT JOIN orders o ON o.user_id = u.id", false},
		{"SELECT nick_of(o.id) FROM users u LEFT JOIN orders o ON o.user_id = u.id", true},
		{"SELECT list_totals(1)", true},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if r.Columns[0].Nullable != c.nullable {
			t.Errorf("%s: nullable = %v, want %v", c.sql, r.Columns[0].Nullable, c.nullable)
		}
	}
}
