package analyze

import (
	"os"
	"strings"
	"testing"
)

// TestCardinality covers the at-most-one-row proof (card.go). Schema: users (PK id,
// UNIQUE email), orders (PK id, UNIQUE (user_id, note), partial UNIQUE (uid) WHERE
// status <> 'cancelled', FK user_id → users), order_items (PK (order_id, line_no)),
// view order_summary (orders JOIN users), matview order_stats (GROUP BY user_id).
func TestCardinality(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql string
		one bool
		why string // substring of ManyRowsWhy when !one
	}{
		// keys fixed by equality to known values
		{sql: "SELECT id FROM users WHERE id = $1", one: true},
		{sql: "SELECT id FROM users WHERE email = 'a@x'", one: true},
		{sql: "SELECT id FROM users u WHERE u.id = $1 AND name = $2", one: true},
		{sql: "SELECT id FROM users WHERE $1 = id", one: true},
		{sql: "SELECT id FROM users WHERE id IN ($1)", one: true},
		{sql: "SELECT id FROM users WHERE id = $1::bigint + 1", one: true},
		{sql: "SELECT id FROM users WHERE id = (SELECT max(user_id) FROM orders)", one: true},
		{sql: "SELECT id FROM users WHERE id = coalesce($1, 0)", one: true},
		{sql: "SELECT sku FROM order_items WHERE order_id = $1 AND line_no = $2", one: true},
		{sql: "SELECT id FROM orders WHERE user_id = $1 AND note = $2", one: true},
		// partial unique index needs its predicate repeated
		{sql: "SELECT id FROM orders WHERE uid = $1 AND status <> 'cancelled'", one: true},
		{sql: "SELECT id FROM orders o WHERE o.uid = $1 AND o.status <> 'cancelled'", one: true},
		{sql: "SELECT id FROM orders WHERE uid = $1", why: "orders: no unique key"},
		{sql: "SELECT id FROM orders WHERE uid = $1 AND status <> 'paid'", why: "orders: no unique key"},
		// not fixed
		{sql: "SELECT id FROM users", why: "users: no unique key is fixed by equality (keys: (id), (email))"},
		{sql: "SELECT id FROM users WHERE name = $1", why: "users: no unique key"},
		{sql: "SELECT id FROM users WHERE id = $1 OR email = $2", why: "users"},
		{sql: "SELECT id FROM users WHERE id <> $1", why: "users"},
		{sql: "SELECT id FROM users WHERE id = ANY($1)", why: "users"},
		{sql: "SELECT id FROM users WHERE id IN ($1, $2)", why: "users"},
		{sql: "SELECT sku FROM order_items WHERE order_id = $1", why: "order_items: no unique key is fixed by equality (keys: (order_id, line_no))"},
		{sql: "SELECT id FROM users WHERE id = random()::bigint", why: "users"},
		// other single shapes
		{sql: "SELECT count(*) FROM orders", one: true},
		{sql: "SELECT max(total) FROM orders WHERE user_id = $1", one: true},
		{sql: "SELECT count(*) FROM orders GROUP BY user_id", why: "GROUP BY yields one row per group (SELECT user_id is not pinned)"},
		{sql: "SELECT user_id, count(*) FROM orders WHERE user_id = $1 GROUP BY user_id", one: true},
		{sql: "SELECT o.user_id, u.email, count(*) FROM orders o JOIN users u ON u.id = o.user_id WHERE u.id = $1 GROUP BY o.user_id, u.email", one: true},
		{sql: "SELECT status, count(*) FROM orders WHERE user_id = $1 GROUP BY status", why: "GROUP BY"},
		// stable functions of known values are known; volatile ones are not
		{sql: "SELECT id FROM users WHERE email = lower($1)", one: true},
		{sql: "SELECT id FROM users WHERE id = abs($1::bigint)", one: true},
		{sql: "SELECT id FROM users WHERE id = nick_of($1)::bigint", one: true},
		{sql: "SELECT id FROM users WHERE id = floor(random() * 10)::bigint", why: "users"},
		{sql: "SELECT id FROM users WHERE id = length(name)", why: "users"},
		{sql: "SELECT id FROM orders LIMIT 1", one: true},
		{sql: "SELECT id FROM orders LIMIT 2", why: "orders"},
		{sql: "SELECT id FROM orders LIMIT $1", why: "orders"},
		{sql: "SELECT $1::int", one: true},
		{sql: "SELECT now()", one: true},
		{sql: "SELECT id FROM users WHERE id = 1 UNION SELECT id FROM users WHERE id = 2", why: "UNION"},
		{sql: "SELECT * FROM generate_series(1, 3)", why: "function generate_series may return many rows"},
		{sql: "SELECT * FROM now()", one: true},
		// joins: dependencies flow through equalities
		{sql: "SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id WHERE o.id = $1", one: true},
		{sql: "SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id WHERE u.id = $1", why: "orders o: no unique key"},
		{sql: "SELECT o.id FROM users u JOIN orders o ON o.user_id = u.id AND o.note = $2 WHERE u.email = $1", one: true},
		{sql: "SELECT o.id FROM orders o, users u WHERE o.id = $1 AND u.id = o.user_id", one: true},
		{sql: "SELECT i.sku FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.id = $1 AND i.line_no = $2", one: true},
		{sql: "SELECT i.sku FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.uid = $1 AND o.status <> 'cancelled' AND i.line_no = 1", one: true},
		{sql: "SELECT id FROM users u JOIN orders o USING (id) WHERE u.id = $1", one: true},
		{sql: "SELECT id FROM users u JOIN orders o USING (id) WHERE o.note = $1", why: "users u"},
		// outer joins: ON restricts only the nullable side
		{sql: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.note = $2 WHERE u.id = $1", one: true},
		{sql: "SELECT u.id FROM users u LEFT JOIN orders o ON o.id = $2 WHERE u.id = $1", one: true},
		{sql: "SELECT u.id FROM users u LEFT JOIN orders o ON u.id = $1", why: "users u"},
		{sql: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.id = $1", why: "orders o"},
		{sql: "SELECT u.id FROM users u FULL JOIN orders o ON o.id = $2 WHERE u.id = $1", why: "FULL JOIN"},
		// views, subqueries, CTEs are proved through their definitions
		{sql: "SELECT id FROM order_summary WHERE id = $1", one: true},
		{sql: "SELECT id FROM order_summary WHERE email = $1", why: "view order_summary: orders o: no unique key"},
		{sql: "SELECT n FROM order_stats WHERE user_id = $1", one: true},
		{sql: "SELECT s.id FROM (SELECT id, user_id FROM orders) s WHERE s.id = $1", one: true},
		{sql: "SELECT s.id FROM (SELECT id, user_id FROM orders) s WHERE s.user_id = $1", why: "subquery s: orders: no unique key"},
		{sql: "SELECT s.id FROM (SELECT * FROM orders) s WHERE s.id = $1", one: true},
		{sql: "SELECT s.id FROM (SELECT o.*, u.email FROM orders o JOIN users u ON u.id = o.user_id) s WHERE s.id = $1", one: true},
		{sql: "SELECT s.n FROM (SELECT count(*) AS n FROM orders) s", one: true},
		{sql: "WITH c AS (SELECT id, user_id FROM orders) SELECT id FROM c WHERE id = $1", one: true},
		{sql: "WITH c AS (SELECT id, user_id FROM orders) SELECT id FROM c WHERE user_id = $1", why: "CTE c: orders: no unique key"},
		// correlated scalar subqueries are not constants
		{sql: "SELECT u.id FROM users u WHERE u.id = (SELECT o.user_id FROM orders o WHERE o.id = u.id LIMIT 1)", why: "users u"},
		{sql: "SELECT u.id FROM users u WHERE u.id = (SELECT max(id) FROM users)", one: true},
		{sql: "SELECT u.id FROM users u WHERE u.id = (SELECT max(o.user_id) FROM orders o)", one: true},
		{sql: "SELECT u.id FROM users u WHERE u.id = (SELECT max(user_id) FROM orders WHERE note = name)", why: "users u"},
		{sql: "SELECT u.id FROM users u WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id) AND u.id = $1", one: true},
		// DML
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, $2) RETURNING id", one: true},
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, $2), ($3, $4) RETURNING id", why: "VALUES has 2 rows"},
		{sql: "INSERT INTO orders (user_id, total) SELECT id, 0 FROM users WHERE id = $1 RETURNING id", one: true},
		{sql: "INSERT INTO orders (user_id, total) SELECT id, 0 FROM users RETURNING id", why: "users"},
		{sql: "UPDATE orders SET status = 'paid' WHERE id = $1 RETURNING id", one: true},
		{sql: "UPDATE orders SET status = 'paid' WHERE user_id = $1", why: "orders"},
		{sql: "UPDATE orders o SET status = 'paid' FROM users u WHERE u.id = o.user_id AND u.email = $1 AND o.note = $2 RETURNING o.id", one: true},
		{sql: "DELETE FROM orders WHERE id = $1", one: true},
		{sql: "DELETE FROM orders o USING users u WHERE u.id = o.user_id AND u.email = $1", why: "orders o"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			r, err := Analyze(s, c.sql)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			if r.AtMostOne != c.one {
				t.Fatalf("AtMostOne: want %v, got %v (%s)", c.one, r.AtMostOne, r.ManyRowsWhy)
			}
			if !c.one && !strings.Contains(r.ManyRowsWhy, c.why) {
				t.Errorf("why: want %q in %q", c.why, r.ManyRowsWhy)
			}
			if c.one && r.ManyRowsWhy != "" {
				t.Errorf("why should be empty: %q", r.ManyRowsWhy)
			}
		})
	}
}
