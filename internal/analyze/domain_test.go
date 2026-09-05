package analyze

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestDomainNotes covers the stricter-than-PG domain rules (domain.go): what mixes,
// what adopts the unit, and which results keep the domain. Schema: users.balance is
// domain yen (bigint), users.email is domain email (text), orders.user_id is plain
// bigint, orders.total is plain numeric, order_items.qty is plain integer.
func TestDomainNotes(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql   string
		notes []string // substrings, one per expected note, in order
		typ   string   // type of the first result column when it matters ("" = don't care)
	}{
		// literals, constants and parameters adopt the unit
		{sql: "SELECT balance + 1 FROM users", typ: "yen"},
		{sql: "SELECT balance - $1 FROM users", typ: "yen"},
		{sql: "SELECT id FROM users WHERE balance > 100 AND balance <> $1"},
		{sql: "SELECT id FROM users WHERE balance > -1"},
		{sql: "SELECT id FROM users WHERE balance = ANY($1)"},
		{sql: "SELECT id FROM users WHERE balance IN (1, 2, $1)"},
		{sql: "SELECT id FROM users WHERE balance BETWEEN 1 AND $1"},
		{sql: "SELECT id FROM users WHERE balance > $1::bigint"},
		// same domain on both sides
		{sql: "SELECT u.balance + v.balance FROM users u JOIN users v ON v.id = u.id", typ: "yen"},
		{sql: "SELECT id FROM users u WHERE balance > (SELECT max(balance) FROM users)"},
		{sql: "SELECT u.balance / v.balance FROM users u JOIN users v ON v.id = u.id", typ: "bigint"},
		{sql: "SELECT u.balance % v.balance FROM users u JOIN users v ON v.id = u.id", typ: "yen"},
		// scaling keeps the unit
		{sql: "SELECT u.balance * i.qty FROM users u, order_items i", typ: "yen"},
		{sql: "SELECT i.qty * u.balance FROM users u, order_items i", typ: "yen"},
		{sql: "SELECT u.balance / i.qty FROM users u, order_items i", typ: "yen"},
		{sql: "SELECT -balance FROM users", typ: "yen"},
		// unit-preserving functions and select_common_type contexts
		{sql: "SELECT abs(balance) FROM users", typ: "yen"},
		{sql: "SELECT max(balance) FROM users", typ: "yen"},
		{sql: "SELECT coalesce(balance, 0) FROM users", typ: "yen"},
		{sql: "SELECT greatest(balance, 0) FROM users", typ: "yen"},
		{sql: "SELECT CASE WHEN id > 1 THEN balance ELSE 0 END FROM users", typ: "yen"},
		{sql: "SELECT nullif(balance, 0) FROM users", typ: "yen"},
		{sql: "SELECT sum(balance) FROM users", typ: "numeric"},
		{sql: "SELECT lower(email) FROM users", typ: "email"},
		{sql: "SELECT length(email) FROM users", typ: "integer"},
		// an explicit cast drops (or asserts) the unit
		{sql: "SELECT u.balance::bigint + o.user_id FROM users u, orders o", typ: "bigint"},
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance = o.user_id::yen"},
		{sql: "UPDATE users SET balance = $1::bigint::yen"},
		// mixing is reported
		{sql: "SELECT u.balance + o.user_id FROM users u, orders o", notes: []string{"yen + bigint: operands must share"}, typ: "bigint"},
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance = o.user_id", notes: []string{"yen = bigint"}},
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance < o.total", notes: []string{"yen < numeric"}},
		{sql: "SELECT u.id FROM users u, orders o WHERE o.total >= u.balance", notes: []string{"numeric(12,2) >= yen"}},
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance BETWEEN o.user_id AND 100", notes: []string{"yen >= bigint"}},
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance = ANY(ARRAY[o.user_id])", notes: []string{"yen = bigint"}},
		{sql: "SELECT u.balance * v.balance FROM users u JOIN users v ON v.id = u.id", notes: []string{"yen * yen: product of two domain values"}, typ: "bigint"},
		{sql: "SELECT i.qty / u.balance FROM users u, order_items i", notes: []string{"integer / yen"}},
		{sql: "SELECT u.balance + o.user_id + 1 FROM users u, orders o", notes: []string{"yen + bigint"}},
		{sql: "SELECT coalesce(u.balance, o.user_id) FROM users u, orders o", notes: []string{"COALESCE mixes yen with bigint"}, typ: "bigint"},
		{sql: "SELECT CASE WHEN true THEN u.balance ELSE o.user_id END FROM users u, orders o", notes: []string{"CASE mixes yen with bigint"}},
		{sql: "SELECT balance FROM users UNION SELECT user_id FROM orders", notes: []string{"UNION mixes yen with bigint"}},
		{sql: "SELECT id FROM users WHERE email = name", notes: []string{"email = character varying"}},
		{sql: "UPDATE users u SET balance = o.user_id FROM orders o WHERE o.user_id = u.id", notes: []string{"users.balance is yen but the value is bigint"}},
		{sql: "INSERT INTO users (balance) SELECT user_id FROM orders", notes: []string{"users.balance is yen but the value is bigint"}},
		// a plain column accepts anything: it declares no unit
		{sql: "UPDATE orders SET user_id = (SELECT balance FROM users LIMIT 1)"},
		// the finding is placed at the operator
		{sql: "SELECT u.id FROM users u, orders o WHERE u.balance = o.user_id", notes: []string{"(at 52)"}},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			r, err := Analyze(s, c.sql)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			var got []string
			for _, n := range r.Notes {
				if n.Code != noteDomainMismatch {
					t.Errorf("unexpected code %q", n.Code)
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
			if c.typ != "" {
				if len(r.Columns) == 0 {
					t.Fatal("no columns")
				}
				if tn := s.Types.Format(r.Columns[0].Type); tn != c.typ {
					t.Errorf("first column type: want %s, got %s", c.typ, tn)
				}
			}
		})
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }
