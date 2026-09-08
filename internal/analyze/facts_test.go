package analyze

import (
	"strings"
	"testing"
)

const factsSchema = `
CREATE TABLE tenants (id bigint PRIMARY KEY);
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL REFERENCES tenants(id),
  status text NOT NULL,
  deleted_at timestamptz,
  UNIQUE (id, tenant_id)
);
CREATE TABLE order_items (
  id bigint PRIMARY KEY,
  order_id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  qty int NOT NULL,
  FOREIGN KEY (order_id, tenant_id) REFERENCES orders(id, tenant_id)
);
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY orders_tenant ON orders USING (tenant_id = current_setting('app.tenant', true)::bigint);
-- sqlshape: unfiltered orders
CREATE VIEW all_orders AS SELECT id, tenant_id, status FROM orders;
CREATE VIEW live_orders AS SELECT id, tenant_id, status FROM orders WHERE deleted_at IS NULL;
`

func TestFacts(t *testing.T) {
	s, err := Load(factsSchema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ sql, want string }{
		{`SELECT id FROM orders WHERE tenant_id = $1 AND deleted_at IS NULL AND status > 'a'`, `
select
  leaf 0 table orders @15
  pred 0.tenant_id = $1
  pred 0.deleted_at IS NULL
  pred opaque "status > 'a'" cols 0.status
  pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
  fixed 0.tenant_id
  notnull 0.status 0.tenant_id
`},
		{`SELECT i.qty FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.id = $1 AND o.deleted_at IS NULL`, `
select
  leaf 0 table orders as o @18
  leaf 1 table order_items as i @32
  pred 1.order_id = 0.id
  pred 0.id = $1
  pred 0.deleted_at IS NULL
  pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
  fixed 0.id 1.order_id
  edge 1.order_id -> 0.id
  edge 0.id -> 1.order_id
  notnull 0.id 1.order_id
`},
		{`SELECT o.id FROM orders o LEFT JOIN order_items i ON i.order_id = o.id AND i.qty = 1 WHERE o.tenant_id = $1`, `
select
  leaf 0 table orders as o @17
  leaf 1 table order_items as i @36
  pred 1.order_id = 0.id restricts [1]
  pred 1.qty = const i1 restricts [1]
  pred 0.tenant_id = $1
  pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
  fixed 0.tenant_id 1.qty
  edge 0.id -> 1.order_id
  notnull 0.tenant_id
`},
		{`-- sqlshape: unfiltered orders
SELECT id FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE qty > $1)`, `
select
  leaf 0 table orders waived unfiltered @46
  pred exists
      leaf 0 table order_items @87
      pred opaque "qty > $1" cols 0.qty
      pred 0.order_id = outer 0.id
      notnull 0.qty
  pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
`},
		{`SELECT v.id FROM live_orders v WHERE v.tenant_id = $1`, `
select
  leaf 0 view live_orders as v @17
      leaf 0 table orders @-1
      pred 0.deleted_at IS NULL
      pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
  pred 0.tenant_id = $1
  fixed 0.tenant_id
  notnull 0.tenant_id
`},
		{`SELECT id FROM all_orders`, `
select
  leaf 0 view all_orders @15
      leaf 0 table orders waived unfiltered @-1
      pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
`},
		{`UPDATE orders SET status = $1 WHERE id = $2 AND tenant_id = $3`, `
update
  leaf 0 table orders target @7
  pred 0.id = $2
  pred 0.tenant_id = $3
  pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
  fixed 0.id 0.tenant_id
  notnull 0.id 0.tenant_id
  write update orders set status
`},
		{`DELETE FROM order_items i USING orders o WHERE o.id = i.order_id AND o.tenant_id = $1`, `
delete
  leaf 0 table order_items as i target @12
  leaf 1 table orders as o @32
  pred 1.id = 0.order_id
  pred 1.tenant_id = $1
  pred 1.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [1] from policy
  fixed 1.tenant_id
  edge 1.id -> 0.order_id
  edge 0.order_id -> 1.id
  notnull 0.order_id 1.id 1.tenant_id
  write delete order_items
`},
		{`INSERT INTO order_items (id, order_id, tenant_id, qty) VALUES ($1, $2, $3, 1)`, `
insert
  leaf 0 table order_items target @12
  write insert order_items set id,order_id,tenant_id,qty
`},
		{`INSERT INTO order_items (id, order_id, tenant_id, qty) SELECT $1, id, tenant_id, 1 FROM orders WHERE id = $2 AND deleted_at IS NULL`, `
insert
  leaf 0 table order_items target @12
  scope
    leaf 0 table orders @88
    pred 0.id = $2
    pred 0.deleted_at IS NULL
    pred 0.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [0] from policy
    fixed 0.id
    notnull 0.id
  write insert order_items set id,order_id,tenant_id,qty
`},
		{`MERGE INTO order_items i USING orders o ON i.order_id = o.id AND o.tenant_id = $1 WHEN MATCHED THEN UPDATE SET qty = 0`, `
merge
  leaf 0 table order_items as i target @11
  leaf 1 table orders as o @31
  pred 0.order_id = 1.id
  pred 1.tenant_id = $1
  pred 1.tenant_id = known "current_setting('app.tenant', true)::bigint" restricts [1] from policy
  fixed 1.tenant_id
  edge 0.order_id -> 1.id
  edge 1.id -> 0.order_id
  notnull 0.order_id 1.id 1.tenant_id
  write merge order_items set qty
`},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		got := r.Facts.String()
		if got != strings.TrimPrefix(c.want, "\n") {
			t.Errorf("%s\n--- got ---\n%s--- want ---\n%s", c.sql, got, strings.TrimPrefix(c.want, "\n"))
		}
	}
}
