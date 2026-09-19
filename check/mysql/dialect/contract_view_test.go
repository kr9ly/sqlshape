package dialect

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// An obligation declared above CREATE VIEW binds the statements that touch the view, on
// MySQL as on PostgreSQL (checks.md, "How a declaration works"): the view is a relation
// of the contract, its columns resolve, and the discharge rules are the table's own.
func TestViewObligation(t *testing.T) {
	m := adv3Load(t, `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  total INT NOT NULL
);
-- sqlshape: require pinned(tenant_id)
CREATE VIEW v_orders AS SELECT id, tenant_id, total FROM orders;
`)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if len(decls) != 1 || decls[0].Subject != "v_orders" || decls[0].Source != "require pinned(tenant_id)" {
		t.Fatalf("decls: %+v", decls)
	}
	// check returns the pinned obligation's message for sql, and whether it was judged
	// at all: "" with true when discharged.
	check := func(sql string) (string, bool) {
		t.Helper()
		r, err := m.Analyze(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
			if d.Obligation.Source == "require pinned(tenant_id)" {
				return d.Message, true
			}
		}
		return "", false
	}
	if msg, ok := check("SELECT id FROM v_orders WHERE tenant_id = $1"); !ok || msg != "" {
		t.Errorf("pinned read: judged %v, %q", ok, msg)
	}
	if msg, ok := check("SELECT id FROM v_orders"); !ok || !strings.Contains(msg, "tenant_id is not pinned") {
		t.Errorf("unpinned read: judged %v, %q", ok, msg)
	}
	// a write through the view is a write to the base table (checks.md), so the view's own
	// obligation binds its readers only: the INSERT below is not judged against it. The
	// same on PostgreSQL (measured); an obligation that must bind the writes too belongs
	// on the base table.
	if msg, ok := check("INSERT INTO v_orders (id, total) VALUES ($1, $2)"); ok {
		t.Errorf("insert through the view was judged against the view's obligation: %q", msg)
	}
}

// A declaration naming a column the view does not expose is a problem carrying the view's
// name, same as a table's.
func TestViewObligationUnknownColumn(t *testing.T) {
	m := adv3Load(t, `-- sqlshape: mysql 8.4
CREATE TABLE orders (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL);
-- sqlshape: require pinned(nope)
CREATE VIEW v_orders AS SELECT id FROM orders;
`)
	_, problems := obligation.Declarations(m.Contract())
	found := false
	for _, p := range problems {
		if p.Subject == "v_orders" && strings.Contains(p.Message, "no column nope") {
			found = true
		}
	}
	if !found {
		t.Errorf("no v_orders problem about column nope: %+v", problems)
	}
}
