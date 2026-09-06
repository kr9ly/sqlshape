package analyze

import (
	"os"
	"strings"
	"testing"
)

// TestRecordFields covers Column.Fields: the shape of row(...), whole-row and composite columns.
func TestRecordFields(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	var render func(cols []Column) string
	render = func(cols []Column) string {
		var parts []string
		for _, c := range cols {
			p := c.Name + ":" + s.Types.Format(c.Type)
			if c.Nullable {
				p += "?"
			}
			if len(c.Fields) > 0 {
				p += "{" + render(c.Fields) + "}"
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " ")
	}
	cases := []struct{ sql, want string }{
		{"SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id",
			"id:bigint orders:record[]?{f1:bigint f2:numeric(12,2)}"},
		{"SELECT array_agg(o) AS orders FROM orders o",
			"orders:orders[]?{id:bigint user_id:bigint status:order_status total:numeric(12,2) price:money_amount?{amount:numeric(12,2)? currency:character(3)?} note:text? meta:jsonb? uid:uuid? matrix:integer[]? created_at:timestamp with time zone}"},
		{"SELECT row(1, 'x', o.note) FROM orders o", "row:record{f1:integer f2:text f3:text?}"},
		{"SELECT price FROM orders", "price:money_amount?{amount:numeric(12,2)? currency:character(3)?}"},
		{"SELECT s.r FROM (SELECT row(id, email) AS r FROM users) s", "r:record{f1:bigint f2:email}"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if got := render(r.Columns); got != c.want {
			t.Errorf("%s:\n want %s\n got  %s", c.sql, c.want, got)
		}
	}
}
