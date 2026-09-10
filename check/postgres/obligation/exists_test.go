package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

const existsSchema = `
-- sqlshape: aggregate orders (order_items, order_item_tags) lock version
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, version int NOT NULL DEFAULT 1);
CREATE TABLE order_items (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id), qty int NOT NULL);
CREATE TABLE order_item_tags (item_id bigint NOT NULL REFERENCES order_items(id), tag text NOT NULL, PRIMARY KEY (item_id, tag));
-- sqlshape: require EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)
CREATE TABLE shipments (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id), carrier text);
-- the witness table has row security: its policy is the database's, not part of the declaration
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY orders_tenant ON orders USING (tenant_id = current_setting('app.tenant', true)::bigint);
`

// A cross-table obligation is an EXISTS the statement must witness: by a join in the same
// level, or by an EXISTS / IN subquery of its own. A `$n` in the declaration is any value
// known before the row is examined.
func TestExistsObligations(t *testing.T) {
	s, err := analyze.Load(existsSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	var lockSpecs []string
	for _, o := range decls {
		if o.Body.Predicate != "" && strings.HasPrefix(o.Source, "aggregate") {
			lockSpecs = append(lockSpecs, o.Subject+": "+o.Body.Predicate)
		}
	}
	wantLock := "order_items: EXISTS (SELECT 1 FROM orders a0 WHERE a0.id = order_id AND a0.version = $1) | order_item_tags: EXISTS (SELECT 1 FROM order_items a0, orders a1 WHERE a0.id = item_id AND a1.id = a0.order_id AND a1.version = $1)"
	if got := strings.Join(lockSpecs, " | "); got != wantLock {
		t.Errorf("lock expansion:\n got %s\nwant %s", got, wantLock)
	}
	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
			lines = append(lines, d.Leaf.Table+" "+shortSpec(d.Obligation)+" "+pathName(d.Path))
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct{ sql, want string }{
		// shipments: the tenant check through orders, witnessed three ways
		{`SELECT s.carrier FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.tenant_id = $1`,
			"shipments exists statement\norders alone statement"},
		{`SELECT carrier FROM shipments s WHERE EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = $1)`,
			"shipments exists statement\norders alone statement"},
		{`SELECT carrier FROM shipments WHERE order_id IN (SELECT id FROM orders WHERE tenant_id = 42)`,
			"shipments exists statement\norders alone statement"},
		// ... and not witnessed: no tenant, an outer join, a negated subquery
		{`SELECT s.carrier FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.status = 'open'`,
			"shipments exists FAIL\norders alone statement"},
		{`SELECT s.carrier FROM shipments s LEFT JOIN orders o ON o.id = s.order_id AND o.tenant_id = $1`,
			"shipments exists FAIL\norders alone statement"},
		{`SELECT carrier FROM shipments s WHERE NOT EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = $1)`,
			"shipments exists FAIL\norders alone statement"},
		// the lock: a child's UPDATE names the root's version, through a join or a subquery
		{`UPDATE order_items i SET qty = $1 FROM orders o WHERE o.id = i.order_id AND o.version = $2 AND i.id = $3`,
			"order_items pinned(order_id) fk\norder_items alone statement\norder_items exists statement\norders alone statement"},
		{`UPDATE order_items i SET qty = $1 WHERE i.id = $2 AND i.order_id = $3 AND EXISTS (SELECT 1 FROM orders o WHERE o.id = i.order_id AND o.version = $4)`,
			"order_items pinned(order_id) statement\norder_items alone statement\norder_items exists statement\norders alone statement"},
		{`UPDATE order_items i SET qty = $1 WHERE i.id = $2 AND i.order_id = $3`,
			"order_items pinned(order_id) statement\norder_items alone statement\norder_items exists FAIL"},
		// a grandchild reaches the root through its parent
		{`DELETE FROM order_item_tags t USING order_items i, orders o WHERE t.item_id = i.id AND i.order_id = o.id AND o.id = $1 AND o.version = $2`,
			"order_item_tags pinned(item_id) fk\norder_item_tags alone statement\norder_item_tags exists statement\norder_items pinned(order_id) statement\norder_items alone statement\norders alone statement"},
		// the root itself pins its version on write; a SELECT owes nothing of the lock
		{`UPDATE orders SET status = 'paid' WHERE id = $1 AND version = $2`,
			"orders alone statement\norders pinned(version) statement"},
		{`SELECT status FROM orders WHERE id = $1`, "orders alone statement"},
	}
	for _, c := range cases {
		if got := run(c.sql); got != c.want {
			t.Errorf("%s\n--- got ---\n%s\n--- want ---\n%s", c.sql, got, c.want)
		}
	}
}

func shortSpec(o *obligation.Obligation) string {
	if o.Body.Predicate != "" && strings.HasPrefix(strings.ToUpper(o.Body.Predicate), "EXISTS") {
		return "exists"
	}
	return o.Body.Spec()
}
