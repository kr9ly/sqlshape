package dialect

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

const testSchema = `-- sqlshape: mysql 8.4
CREATE TABLE tenants (id BIGINT UNSIGNED NOT NULL PRIMARY KEY);
-- sqlshape: visible where deleted_at IS NULL
-- sqlshape: require pinned(tenant_id)
-- sqlshape: require immutable(tenant_id)
-- sqlshape: require single on delete
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  status ENUM('open', 'paid', 'closed') NOT NULL,
  amount INT NOT NULL,
  deleted_at DATETIME,
  CONSTRAINT fk_orders_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id),
  UNIQUE KEY orders_id_tenant (id, tenant_id)
);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE order_items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  order_id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  qty INT NOT NULL,
  CONSTRAINT fk_items_order FOREIGN KEY (order_id, tenant_id) REFERENCES orders (id, tenant_id)
);
-- sqlshape: require via view
CREATE TABLE secrets (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, body TEXT);
-- sqlshape: require status <> 'closed' AND amount > 0 on update
CREATE TABLE tickets (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, status VARCHAR(10) NOT NULL, amount INT NOT NULL);
-- sqlshape: require never on delete
CREATE TABLE ledger (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, amount INT NOT NULL);
CREATE VIEW secret_ids AS SELECT id FROM secrets;
-- sqlshape: unfiltered orders
CREATE VIEW all_orders AS SELECT id, tenant_id FROM orders;
`

func loadContract(t *testing.T) *mysql {
	an, err := load(testSchema)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	return m
}

// TestContract runs the schema's obligations over statements through the MySQL contract:
// the declarations parse, the predicates lower, and each judgment reads like PostgreSQL's.
func TestContract(t *testing.T) {
	m := loadContract(t)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	cases := []struct {
		sql  string
		want string // one line per judgment: "<source> <path|FAIL> [message]"
	}{
		{`SELECT id FROM orders WHERE tenant_id = $1 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement`},
		{`SELECT id FROM orders o WHERE o.tenant_id = $1`, `
visible where deleted_at IS NULL FAIL rows of orders are visible where deleted_at IS NULL: add that predicate for orders o, or opt out with ` + "`-- sqlshape: unfiltered orders`" + `
require pinned(tenant_id) statement`},
		{`SELECT id FROM orders WHERE id = $1 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)`},
		// the child's tenant travels to the parent through the composite foreign key
		{`SELECT i.qty FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.tenant_id = $1 AND o.deleted_at IS NULL`, `
require pinned(tenant_id) fk
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement`},
		{`UPDATE orders SET tenant_id = $1 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement
require immutable(tenant_id) FAIL orders.tenant_id is immutable: the statement must not assign it`},
		{`DELETE FROM orders WHERE tenant_id = $1 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement
require single on delete FAIL orders requires a single-row DELETE: fix a unique key by equality (the One proof)`},
		{`DELETE FROM orders WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, `
visible where deleted_at IS NULL statement
require pinned(tenant_id) statement
require single on delete statement`},
		{`SELECT body FROM secrets WHERE id = $1`, `
require via view FAIL table secrets is referenced directly: it is declared `+"`require via view`"+`, read it through a view`},
		{`SELECT id FROM secret_ids WHERE id = $1`, ``},
		{`UPDATE tickets SET amount = $1 WHERE id = $2 AND status <> 'closed' AND amount > 0`, `
require status <> 'closed' AND amount > 0 on update statement`},
		{`UPDATE tickets t SET amount = $1 WHERE t.id = $2 AND t.status <> 'closed'`, `
require status <> 'closed' AND amount > 0 on update FAIL tickets requires status <> 'closed' AND amount > 0 here: add that predicate for tickets t, or opt out with ` + "`-- sqlshape: unfiltered tickets`"},
		{`DELETE FROM ledger WHERE id = $1`, `
require never on delete FAIL ledger is declared `+"`require never on delete`"+`: no statement may do this to it`},
		{`INSERT INTO ledger (id, amount) VALUES ($1, $2)`, ``},
		// a view's body is judged as the schema's own definition (TestDefinitions), not
		// again by the statements that read it
		{`SELECT id FROM all_orders WHERE tenant_id = $1`, ``},
		{"-- sqlshape: unfiltered orders\nSELECT id FROM orders WHERE tenant_id = $1", `
visible where deleted_at IS NULL waived orders: ` + "`visible where deleted_at IS NULL`" + ` is waived by this statement
require pinned(tenant_id) statement`},
	}
	for _, c := range cases {
		r, err := m.Analyze(c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var lines []string
		for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
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

// TestDefinitions judges the views' bodies by the schema's obligations: a view's own
// directives opt its body out.
func TestDefinitions(t *testing.T) {
	m := loadContract(t)
	decls, _ := obligation.Declarations(m.Contract())
	want := map[string]string{
		"view secret_ids": "",
		"view all_orders": "visible where deleted_at IS NULL waived orders: `visible where deleted_at IS NULL` is waived by this statement\nrequire pinned(tenant_id) FAIL orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)",
	}
	for _, d := range m.Definitions() {
		var lines []string
		for _, j := range obligation.Check(m.Contract(), decls, d.Facts, m) {
			if j.Obligation.Body.ViaView {
				continue // a view's body is the sanctioned reader (vet's judge skips it the same way)
			}
			line := j.Obligation.Source + " " + pathName(j.Path)
			if j.Message != "" {
				line += " " + j.Message
			}
			lines = append(lines, line)
		}
		if got := strings.Join(lines, "\n"); got != want[d.What] {
			t.Errorf("%s\n--- got ---\n%s\n--- want ---\n%s", d.What, got, want[d.What])
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

// TestSchema covers the checker's view of the schema: relations, columns, sources,
// definitions.
func TestSchema(t *testing.T) {
	m := loadContract(t)
	var sd dialect.Schema = m.Schema()
	rel := sd.Relation("orders")
	if rel == nil || rel.Kind.String() != "table" || len(rel.Columns) != 5 || !rel.Unique {
		t.Fatalf("orders: %+v", rel)
	}
	if c := rel.Column("tenant_id"); c == nil || !c.NotNull || c.HasDefault {
		t.Errorf("orders.tenant_id: %+v", c)
	}
	if v := sd.Relation("all_orders"); v == nil || v.Kind.String() != "view" || len(v.Columns) != 2 || v.Columns[1].Name != "tenant_id" {
		t.Errorf("all_orders: %+v", v)
	}
	if sd.Relation("nope") != nil {
		t.Error("nope resolves")
	}
	src := sd.Source("order_items", "tenant_id")
	if src == nil || src.Identity != "tenants.id" {
		t.Errorf("order_items.tenant_id: %+v", src)
	}
	if src := sd.Source("orders", "status"); src == nil || src.ValuesFrom != "check" || strings.Join(src.Values, ",") != "open,paid,closed" {
		t.Errorf("orders.status: %+v", src)
	}
	var whats []string
	for _, d := range sd.Definitions() {
		whats = append(whats, d.What)
		if d.Err != "" || d.Facts == nil {
			t.Errorf("%s: err %q facts %v", d.What, d.Err, d.Facts != nil)
		}
	}
	if got := strings.Join(whats, ","); got != "view secret_ids,view all_orders" {
		t.Errorf("definitions: %s", got)
	}
	ct := m.Contract()
	if v := ct.Relation("secret_ids"); v == nil || !v.HasColumn("id") || v.HasColumn("body") {
		t.Errorf("secret_ids contract: %v", v)
	} else if tbl, col, ok := v.ViewSource("id"); !ok || tbl != "secrets" || col != "id" {
		t.Errorf("secret_ids.id source: %s.%s %v", tbl, col, ok)
	}
	fks := ct.Relation("order_items").ForeignKeys()
	if len(fks) != 1 || strings.Join(fks[0].RefColumns, ",") != "id,tenant_id" {
		t.Errorf("order_items foreign keys: %+v", fks)
	}
}

// TestViolationsSpelled covers the failure modes as the checker reports them.
func TestViolationsSpelled(t *testing.T) {
	m := loadContract(t)
	r, err := m.Analyze("INSERT INTO orders (id, tenant_id, status, amount) VALUES ($1, $2, $3, $4)")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range r.Violations {
		got = append(got, v.Key+": "+v.Detail)
	}
	want := []string{
		"PRIMARY: PRIMARY KEY (id) on orders, MySQL error 1062",
		"orders_id_tenant: UNIQUE orders_id_tenant (id, tenant_id) on orders, MySQL error 1062",
		"fk_orders_tenant: FOREIGN KEY fk_orders_tenant (tenant_id) on orders REFERENCES tenants, MySQL error 1452",
		"orders.id: NOT NULL on orders.id, MySQL error 1048",
		"orders.tenant_id: NOT NULL on orders.tenant_id, MySQL error 1048",
		"orders.status: NOT NULL on orders.status, MySQL error 1048",
		"orders.amount: NOT NULL on orders.amount, MySQL error 1048",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("--- got ---\n%s\n--- want ---\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(r.Relations) != 1 || r.Relations[0].Name != "orders" || !r.Relations[0].Target {
		t.Errorf("relations: %+v", r.Relations)
	}
}
