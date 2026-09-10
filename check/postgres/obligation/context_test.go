package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/obligation"
)

const contextSchema = `
-- sqlshape: visible where deleted_at IS NULL
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete
-- sqlshape: context analyst: require via view
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, deleted_at timestamptz);
-- sqlshape: require pinned(tenant_id)
CREATE VIEW live_orders AS SELECT id, tenant_id FROM orders WHERE deleted_at IS NULL;
`

func TestContext(t *testing.T) {
	s, err := analyze.Load(contextSchema)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sets := map[string]string{
		"":        "orders: visible where deleted_at IS NULL | orders: require pinned(tenant_id) | live_orders: require pinned(tenant_id)",
		"ops":     "orders: visible where deleted_at IS NULL | orders: context ops: waive pinned(tenant_id); require id = $1 on delete | live_orders: require pinned(tenant_id)",
		"analyst": "orders: visible where deleted_at IS NULL | orders: require pinned(tenant_id) | orders: context analyst: require via view | live_orders: require pinned(tenant_id)",
	}
	for name, want := range sets {
		var got []string
		for _, o := range obligation.InContext(all, name) {
			got = append(got, o.Subject+": "+o.Source)
		}
		if g := strings.Join(got, " | "); g != want {
			t.Errorf("context %q:\n got %s\nwant %s", name, g, want)
		}
	}
	run := func(ctx, sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s, obligation.InContext(all, ctx), r.Facts, lowerer{s}) {
			line := d.Leaf.Table + " " + d.Obligation.Body.Spec() + " " + pathName(d.Path)
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	// the base set pins; ops lifts the pin and demands a keyed DELETE; analyst forbids the table
	del := `DELETE FROM orders WHERE id = $1 AND deleted_at IS NULL`
	if got := run("", del); got != "orders deleted_at IS NULL statement\norders pinned(tenant_id) FAIL" {
		t.Errorf("base: %s", got)
	}
	if got := run("ops", del); got != "orders deleted_at IS NULL statement\norders id = $1 statement" {
		t.Errorf("ops: %s", got)
	}
	if got := run("ops", `DELETE FROM orders WHERE deleted_at IS NULL`); got != "orders deleted_at IS NULL statement\norders id = $1 FAIL" {
		t.Errorf("ops unkeyed: %s", got)
	}
	if got := run("analyst", `SELECT id FROM orders WHERE deleted_at IS NULL AND tenant_id = $1`); got != "orders deleted_at IS NULL statement\norders pinned(tenant_id) statement\norders via view FAIL" {
		t.Errorf("analyst: %s", got)
	}
	// an obligation declared on a view binds the view's readers; the view's own definition
	// answers for the tables inside it
	if got := run("", `SELECT id FROM live_orders`); got != "live_orders pinned(tenant_id) FAIL" {
		t.Errorf("view reader: %s", got)
	}
	if got := run("", `SELECT id FROM live_orders WHERE tenant_id = $1`); got != "live_orders pinned(tenant_id) statement" {
		t.Errorf("view reader pinned: %s", got)
	}

	// malformed contexts are declaration problems
	for _, bad := range []string{
		"-- sqlshape: context ops waive pinned(tenant_id)",
		"-- sqlshape: context ops: drop pinned(tenant_id)",
		"-- sqlshape: context ops: waive pinned()",
	} {
		s2, err := analyze.Load(bad + "\nCREATE TABLE t (id int, tenant_id int);")
		if err != nil {
			t.Fatal(err)
		}
		if _, p := obligation.Declarations(s2); len(p) != 1 {
			t.Errorf("%s: problems %+v", bad, p)
		}
	}
}
