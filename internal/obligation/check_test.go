package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/obligation"
	"github.com/kr9ly/sqlshape/internal/schema"
)

const checkSchema = `
CREATE TABLE tenants (id bigint PRIMARY KEY);
-- sqlshape: visible where deleted_at IS NULL
-- sqlshape: require pinned(tenant_id)
-- sqlshape: require immutable(tenant_id)
-- sqlshape: require pinned(version) on update, delete
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL REFERENCES tenants(id),
  status text NOT NULL,
  version int NOT NULL DEFAULT 1,
  deleted_at timestamptz,
  UNIQUE (id, tenant_id)
);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE order_items (
  id bigint PRIMARY KEY,
  order_id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  qty int NOT NULL,
  FOREIGN KEY (order_id, tenant_id) REFERENCES orders(id, tenant_id)
);
-- sqlshape: require via view
CREATE TABLE secrets (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, body text);
-- sqlshape: require status <> 'closed' AND amount > 0 on update
CREATE TABLE tickets (id bigint PRIMARY KEY, status text NOT NULL, amount int NOT NULL);
CREATE TABLE notes (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, body text);
ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
CREATE POLICY notes_tenant ON notes USING (tenant_id = current_setting('app.tenant', true)::bigint);
CREATE VIEW secret_titles AS SELECT id FROM secrets;
`

type lowerer struct{ s *schema.Schema }

func (l lowerer) Lower(expr string, rel *schema.Relation) ([]facts.Pred, error) {
	return analyze.Lower(l.s, expr, rel)
}

func TestCheck(t *testing.T) {
	s, err := analyze.Load(checkSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	decls = append(decls, obligation.Obligation{Subject: "notes", Kinds: obligation.OnAll, Body: obligation.Body{Pinned: "tenant_id"}, Source: "-require-columns=tenant_id"})
	cases := []struct {
		sql  string
		want string // one line per judgment: "<source> <path|FAIL> [message]"
	}{
		{`SELECT id FROM orders WHERE tenant_id = $1 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement`},
		{`SELECT id FROM orders WHERE tenant_id = $1`, `
visible where deleted_at IS NULL FAIL rows of orders are visible where deleted_at IS NULL: add that predicate for orders, or opt out with ` + "`-- sqlshape: unfiltered orders`" + `
require pinned(tenant_id) statement`},
		{`SELECT o.id FROM orders o WHERE o.deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)`},
		{"-- sqlshape: unfiltered orders\nSELECT id FROM orders", `
visible where deleted_at IS NULL waived
require pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)`},
		// the pin of orders.tenant_id reaches order_items through the composite foreign key
		{`SELECT i.qty FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.tenant_id = $1 AND o.deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement
require pinned(tenant_id) fk`},
		// ... but not when only the id is joined to a table whose tenant is not fixed
		{`SELECT i.qty FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.id = $1 AND o.deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)
require pinned(tenant_id) FAIL order_items.tenant_id is not pinned: every statement on order_items must fix tenant_id by equality (or assign it)`},
		// the write side: INSERT assigns, UPDATE pins in WHERE and may not touch tenant_id; version is optimistic locking
		{`INSERT INTO orders (id, tenant_id, status) VALUES ($1, $2, 'new')`, `
require pinned(tenant_id) statement`},
		{`INSERT INTO orders (id, status) VALUES ($1, 'new')`, `
require pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)`},
		{`UPDATE orders SET status = $1 WHERE id = $2 AND tenant_id = $3 AND version = $4`, `
require pinned(tenant_id) statement
require immutable(tenant_id) statement
require pinned(version) on update, delete statement`},
		{`UPDATE orders SET status = $1, tenant_id = $2 WHERE id = $3`, `
require pinned(tenant_id) statement
require immutable(tenant_id) FAIL orders.tenant_id is immutable: the statement must not assign it
require pinned(version) on update, delete FAIL orders.version is not pinned: every statement on orders must fix version by equality (or assign it)`},
		{`DELETE FROM orders WHERE id = $1 AND tenant_id = $2 AND version = $3`, `
require pinned(tenant_id) statement
require pinned(version) on update, delete statement`},
		// a subquery is its own level: the outer pin does not reach the inner leaf
		{`SELECT id FROM orders WHERE tenant_id = $1 AND deleted_at IS NULL AND id IN (SELECT order_id FROM order_items WHERE qty > 1)`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement
require pinned(tenant_id) FAIL order_items.tenant_id is not pinned: every statement on order_items must fix tenant_id by equality (or assign it)`},
		// via view: reading the table is the violation, reading the view is not, writing the table is allowed
		{`SELECT body FROM secrets WHERE id = $1`, `
require via view FAIL table secrets is referenced directly: it is declared ` + "`require via view`" + `, read it through a view`},
		{`SELECT id FROM secret_titles`, ``},
		{`UPDATE secrets SET body = $1 WHERE id = $2`, ``},
		// a multi-conjunct predicate on update only: both conjuncts, syntactic fallback for the inequality
		{`UPDATE tickets SET amount = $1 WHERE id = $2 AND status <> 'closed' AND amount > 0`, `
require status <> 'closed' AND amount > 0 on update statement`},
		{`UPDATE tickets SET amount = $1 WHERE id = $2 AND status <> 'closed'`, `
require status <> 'closed' AND amount > 0 on update FAIL tickets requires status <> 'closed' AND amount > 0 here: add that predicate for tickets, or opt out with ` + "`-- sqlshape: unfiltered tickets`"},
		{`SELECT id FROM tickets`, ``},
		// row security pins the column for roles subject to it; the owner caveat rides on the discharge
		{`SELECT body FROM notes WHERE id = $1`, `
-require-columns=tenant_id policy notes.tenant_id is pinned by policy notes_tenant for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner`},
	}
	for _, c := range cases {
		r, err := analyze.Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var lines []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			line := d.Obligation.Source + " " + pathName(d.Path)
			if d.Message != "" {
				line += " " + d.Message
			}
			lines = append(lines, line)
		}
		got := strings.Join(lines, "\n")
		if want := strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("%s\n--- got ---\n%s\n--- want ---\n%s", c.sql, got, want)
		}
	}
}

func pathName(p obligation.Path) string {
	switch p {
	case obligation.ByStatement:
		return "statement"
	case obligation.ByView:
		return "view"
	case obligation.ByPolicy:
		return "policy"
	case obligation.ByForeignKey:
		return "fk"
	case obligation.Waived:
		return "waived"
	}
	return "FAIL"
}
