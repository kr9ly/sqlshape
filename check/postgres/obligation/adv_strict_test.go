package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/obligation"
)

// A `transitions` target with two or more declared predecessors (`shipped -> returned,
// delivered -> returned`) fixes the current state in its WHERE to one of that state's
// predecessors -- checks.md's wording for the rule, which only makes sense as a set
// membership test when there is more than one predecessor. The natural SQL for that,
// `status IN (...)`, discharges the transition when the row's current status is
// genuinely one of the declared predecessors.
func TestAdvStrictTransitionsMultiPredecessorIN(t *testing.T) {
	const schema = `
-- sqlshape: transitions status: shipped -> returned, delivered -> returned
CREATE TABLE parcels (id bigint PRIMARY KEY, status text NOT NULL);
`
	s, err := analyze.Load(schema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `UPDATE parcels SET status = 'returned' WHERE id = $1 AND status IN ('shipped', 'delivered')`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var lines []string
	for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
		line := d.Leaf.Table + " " + pathName(d.Path)
		if d.Message != "" {
			line += ": " + d.Message
		}
		lines = append(lines, line)
	}
	got := strings.Join(lines, "\n")
	// expected: the transition is discharged (statement), because the row's status is
	// provably one of the declared predecessors of "returned"
	want := "parcels statement"
	if got != want {
		t.Errorf("TestAdvStrictTransitionsMultiPredecessorIN\nSQL: %s\nDeclaration: -- sqlshape: transitions status: shipped -> returned, delivered -> returned\nexpected: %s\nactual:   %s", sql, want, got)
	}
}

// The OR-of-equalities form of the same statement discharges the transition the same
// way.
func TestAdvStrictTransitionsMultiPredecessorOR(t *testing.T) {
	const schema = `
-- sqlshape: transitions status: shipped -> returned, delivered -> returned
CREATE TABLE parcels (id bigint PRIMARY KEY, status text NOT NULL);
`
	s, err := analyze.Load(schema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `UPDATE parcels SET status = 'returned' WHERE id = $1 AND (status = 'shipped' OR status = 'delivered')`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var lines []string
	for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
		line := d.Leaf.Table + " " + pathName(d.Path)
		if d.Message != "" {
			line += ": " + d.Message
		}
		lines = append(lines, line)
	}
	got := strings.Join(lines, "\n")
	want := "parcels statement"
	if got != want {
		t.Errorf("TestAdvStrictTransitionsMultiPredecessorOR\nSQL: %s\nexpected: %s\nactual:   %s", sql, want, got)
	}
}

// --- Edge cases of `pinned` and `paired` that are correctly handled ---

const strictHolesSchema = `
CREATE SCHEMA app;
-- sqlshape: require pinned(tenant_id)
CREATE TABLE app.orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, version int NOT NULL DEFAULT 1);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE app.order_items (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES app.orders(id), tenant_id bigint NOT NULL);
-- sqlshape: require paired(app.outbox) on insert
CREATE TABLE app.staged_orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL);
CREATE TABLE app.outbox (id bigint PRIMARY KEY, payload text);
`

func TestAdvStrictHolesNotFound(t *testing.T) {
	s, err := analyze.Load(strictHolesSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			line := d.Leaf.Table + " " + pathName(d.Path)
			if d.Message != "" {
				line += ": " + d.Message
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct {
		name, sql, want string
	}{
		{"reversed equality ($1 on the left)",
			`SELECT id FROM app.orders WHERE $1 = tenant_id`,
			"app.orders statement"},
		{"cast on the pinned side",
			`SELECT id FROM app.orders WHERE tenant_id = $1::bigint`,
			"app.orders statement"},
		{"parenthesized equality",
			`SELECT id FROM app.orders WHERE (tenant_id = $1)`,
			"app.orders statement"},
		{"AND nested under an unrelated OR",
			`SELECT id FROM app.orders WHERE tenant_id = $1 AND (status = 'a' OR status = 'b')`,
			"app.orders statement"},
		{"USING join propagates the pin across a shared column name",
			`SELECT i.id FROM app.orders o JOIN app.order_items i USING (tenant_id) WHERE o.tenant_id = $1`,
			"app.orders statement\napp.order_items statement"},
		{"NATURAL JOIN propagates the pin either direction",
			`SELECT o.id FROM app.orders o NATURAL JOIN app.order_items i WHERE i.tenant_id = $1`,
			"app.orders statement\napp.order_items statement"},
		{"alias collision between outer and inner query (both named t)",
			`SELECT t.id FROM app.order_items t WHERE t.tenant_id = $1 AND t.id IN (SELECT t.id FROM app.order_items t WHERE t.tenant_id = $1)`,
			"app.order_items statement\napp.order_items statement"},
		{"paired discharged by the canonical WITH-CTE form",
			`WITH o AS (INSERT INTO app.staged_orders (id, tenant_id, status) VALUES ($1, $2, 'new') RETURNING id) INSERT INTO app.outbox (id, payload) SELECT id, 'created' FROM o`,
			"app.staged_orders statement"},
		{"paired correctly fails without the outbox write",
			`INSERT INTO app.staged_orders (id, tenant_id, status) VALUES ($1, $2, 'new')`,
			"app.staged_orders FAIL: a write to staged_orders must also write app.outbox in the same statement (a data-modifying WITH): require paired(app.outbox) on insert"},
		{"pinned via a multi-item IN (2+ literals) is correctly rejected: not a single value",
			`SELECT id FROM app.orders WHERE tenant_id IN ($1, $2)`,
			"app.orders FAIL: orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)"},
		{"pinned via a single-item IN is accepted (it is an equality in disguise)",
			`SELECT id FROM app.orders WHERE tenant_id IN ($1)`,
			"app.orders statement"},
	}
	for _, c := range cases {
		if got := run(c.sql); got != c.want {
			t.Errorf("%s\nSQL: %s\n--- got ---\n%s\n--- want ---\n%s", c.name, c.sql, got, c.want)
		}
	}
}
