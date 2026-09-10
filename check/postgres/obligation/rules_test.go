package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/obligation"
)

const rulesSchema = `
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled, paid -> refunded
-- sqlshape: require paired(outbox) on insert
-- sqlshape: require single on delete
-- sqlshape: sensitive pii: email, phone
-- sqlshape: context billing: may read pii
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL, email text NOT NULL, phone text, total int NOT NULL);
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id), amount int NOT NULL);
CREATE TABLE outbox (id bigint PRIMARY KEY, payload text NOT NULL);
CREATE VIEW order_contacts AS SELECT id, email, left(phone, 3) || '***' AS phone_masked FROM orders;
`

func TestRules(t *testing.T) {
	s, err := analyze.Load(rulesSchema)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	run := func(ctx, sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s, obligation.InContext(all, ctx), r.Facts, lowerer{s}) {
			line := d.Leaf.Table + " " + d.Obligation.Body.Spec() + " " + pathName(d.Path)
			if d.Failed() {
				line += ": " + d.Message
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct{ ctx, sql, want string }{
		// transitions: compare-and-set on the state column
		{"", `UPDATE orders SET status = 'paid' WHERE id = $1 AND status = 'submitted'`, "orders transitions status statement"},
		{"", `UPDATE orders SET status = 'paid' WHERE id = $1`,
			"orders transitions status FAIL: orders.status: SET status = 'paid' must fix the current state in WHERE (status = 'submitted')"},
		{"", `UPDATE orders SET status = 'paid' WHERE id = $1 AND status = 'draft'`,
			"orders transitions status FAIL: orders.status: draft -> paid is not a declared transition (paid comes from submitted)"},
		{"", `UPDATE orders SET status = 'shipped' WHERE id = $1 AND status = 'paid'`,
			"orders transitions status FAIL: orders.status is a state machine: \"shipped\" is not a state anything transitions to (declared: submitted, paid, cancelled, refunded)"},
		{"", `UPDATE orders SET status = $1 WHERE id = $2`,
			"orders transitions status FAIL: orders.status is a state machine: SET it to a declared state (a literal), not to $1"},
		{"", `UPDATE orders SET total = $1 WHERE id = $2`, "orders transitions status statement"},
		{"", `UPDATE orders SET status = 'cancelled' WHERE id = $1 AND status = 'submitted' AND total > 0`, "orders transitions status statement"},
		// never: an append-only table
		{"", `INSERT INTO ledger (id, order_id, amount) VALUES ($1, $2, $3)`, ""},
		{"", `UPDATE ledger SET amount = 0 WHERE id = $1`,
			"ledger never FAIL: ledger is declared `require never on update, delete`: no statement may do this to it"},
		{"", `DELETE FROM ledger WHERE id = $1`,
			"ledger never FAIL: ledger is declared `require never on update, delete`: no statement may do this to it"},
		// paired: the outbox row travels in the same statement
		{"", `WITH o AS (INSERT INTO orders (id, status, email, total) VALUES ($1, 'draft', $2, 0) RETURNING id) INSERT INTO outbox (id, payload) SELECT id, 'created' FROM o`,
			"orders paired(outbox) statement"},
		{"", `INSERT INTO orders (id, status, email, total) VALUES ($1, 'draft', $2, 0)`,
			"orders paired(outbox) FAIL: a write to orders must also write outbox in the same statement (a data-modifying WITH): require paired(outbox) on insert"},
		// single: a DELETE the One proof accepts
		{"", `DELETE FROM orders WHERE id = $1`, "orders single statement"},
		{"", `DELETE FROM orders WHERE status = 'cancelled'`,
			"orders single FAIL: orders requires a single-row DELETE: fix a unique key by equality (the One proof)"},
		// sensitive: labelled columns need a context that may read them; a masked view column does not
		{"", `SELECT id, total FROM orders WHERE id = $1`, ""},
		{"", `SELECT email FROM orders WHERE id = $1`,
			"orders sensitive pii FAIL: orders.email is pii: this context may not read it (a context with `may read pii`, or a view that masks it)"},
		{"", `SELECT id FROM orders WHERE phone = $1`,
			"orders sensitive pii FAIL: orders.phone is pii: this context may not read it (a context with `may read pii`, or a view that masks it)"},
		{"billing", `SELECT email, phone FROM orders WHERE id = $1`, "orders sensitive pii statement\norders sensitive pii statement"},
		{"", `SELECT email FROM order_contacts WHERE id = $1`,
			"order_contacts sensitive pii FAIL: orders.email is pii (through order_contacts.email): this context may not read it (a context with `may read pii`, or a view that masks it)"},
		{"", `SELECT id, phone_masked FROM order_contacts WHERE id = $1`, ""},
	}
	for _, c := range cases {
		if got := run(c.ctx, c.sql); got != c.want {
			t.Errorf("[%s] %s\n--- got ---\n%s\n--- want ---\n%s", c.ctx, c.sql, got, c.want)
		}
	}
	// declaration problems
	for _, bad := range []string{
		"-- sqlshape: transitions status: draft => paid",
		"-- sqlshape: transitions nope: a -> b",
		"-- sqlshape: sensitive pii: nope",
		"-- sqlshape: require paired(nowhere) on insert",
		"-- sqlshape: context x: may read",
	} {
		s2, err := analyze.Load(bad + "\nCREATE TABLE t (id int PRIMARY KEY, status text);")
		if err != nil {
			t.Fatal(err)
		}
		if _, p := obligation.Declarations(s2); len(p) != 1 {
			t.Errorf("%s: problems %+v", bad, p)
		}
	}
}
