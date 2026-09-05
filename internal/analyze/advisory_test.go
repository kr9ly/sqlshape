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
			if !n.Advisory() {
				t.Errorf("%s: non-advisory note %v", c.sql, n)
			}
			if n.Code == noteNoIndex || n.Code == noteViewPushdown {
				continue // covered by TestPlanAdvisories
			}
			codes = append(codes, n.Code)
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

// TestPlanAdvisories: structural stand-ins for EXPLAIN — no index leading with a predicate
// column, and predicates the planner cannot push into a view.
func TestPlanAdvisories(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ sql, want string }{
		{"SELECT id FROM orders WHERE id = $1", ""},
		{"SELECT id FROM orders WHERE user_id = $1", ""}, // orders_user_idx
		{"SELECT id FROM orders WHERE status = 'paid'", "no index on orders leads with any of (status)"},
		{"SELECT id FROM orders WHERE status = 'paid' AND id = $1", ""},
		{"SELECT o.id FROM orders o JOIN users u ON u.email = o.note", "no index on orders leads with any of (note)"},
		{"SELECT id FROM memos WHERE deleted_at IS NULL AND body = $1", "no index on memos leads with any of (deleted_at, body)"},
		{"SELECT n FROM order_stats WHERE user_id = $1", ""},
		{"SELECT n FROM order_stats WHERE n > 1", "predicate on view order_stats (n) is not pushed into the view: n is an aggregate, not a grouping column"},
		{"SELECT id FROM order_summary WHERE id = $1", ""},
		{"SELECT id FROM live_memos WHERE user_id = $1", ""},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []string
		for _, n := range r.Notes {
			if n.Code == noteNoIndex || n.Code == noteViewPushdown {
				got = append(got, n.Message)
			}
		}
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected %q", c.sql, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("%s: want %q, got %q", c.sql, c.want, got)
		}
	}
}
