package analyze

import (
	"os"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestNullRejection: predicates that reject NULL refine the nullability of result columns.
func TestNullRejection(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql      string
		nullable bool
	}{
		{"SELECT note FROM orders", true},
		{"SELECT note FROM orders WHERE note IS NOT NULL", false},
		{"SELECT note FROM orders WHERE note = $1", false},
		{"SELECT note FROM orders WHERE note LIKE 'a%'", false},
		{"SELECT note FROM orders WHERE note IN ('a', 'b')", false},
		{"SELECT note FROM orders WHERE note = ANY($1)", false},
		{"SELECT note FROM orders WHERE note IS NOT DISTINCT FROM $1", true},
		{"SELECT note FROM orders WHERE note = $1 OR id = 1", true},
		{"SELECT note FROM orders WHERE note IS NULL", true},
		{"SELECT o.id FROM users u LEFT JOIN orders o ON o.user_id = u.id", true},
		{"SELECT o.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE o.id IS NOT NULL", false},
		{"SELECT o.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE o.total > 0", false},
		{"SELECT o.note FROM users u JOIN orders o ON o.note = u.name", false},
		{"SELECT s.note FROM (SELECT note FROM orders WHERE note IS NOT NULL) s", false},
		// a searched CASE branch runs where its condition held
		{"SELECT CASE WHEN note IS NOT NULL THEN note ELSE '' END FROM orders", false},
		{"SELECT CASE WHEN note = 'x' THEN note ELSE 'y' END FROM orders", false},
		{"SELECT CASE WHEN note IS NOT NULL THEN note END FROM orders", true},
		{"SELECT CASE WHEN id > 1 THEN note ELSE 'y' END FROM orders", true},
		{"SELECT CASE WHEN note IS NOT NULL THEN upper(note) ELSE 'y' END FROM orders", false},
		{"SELECT CASE WHEN note IS NOT NULL THEN 1 END, note FROM orders WHERE true", true},
		// non-strict built-ins that never return NULL, and nullary ones
		{"SELECT concat(note, 'x') FROM orders", false},
		{"SELECT format('%s', note) FROM orders", false},
		{"SELECT upper(note) FROM orders", true},
		{"SELECT now()", false},
		{"SELECT gen_random_uuid()", false},
		{"SELECT inet_client_addr()", true},
		{"SELECT nullif(id, 1) FROM orders", true},
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
