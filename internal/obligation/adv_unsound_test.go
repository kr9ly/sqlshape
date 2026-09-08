package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/obligation"
)

// A statement that touches a table with an obligation is always diagnosed when it does
// not discharge that obligation -- these tests cover three ways a statement can touch
// the table indirectly (through a view, through TRUNCATE, through a bare assignment)
// and still owes a judgment.

const advUnsoundSchema = `
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, amount int NOT NULL);

-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL);

CREATE VIEW ledger_v AS SELECT id, amount FROM ledger;
CREATE VIEW orders_v AS SELECT id, tenant_id, status FROM orders;
`

// `never` says no UPDATE or DELETE may exist against ledger, "inside a WITH as much as
// on its own" (docs/checks.md). Writing through an ordinary auto-updatable view
// rewrites to the same DML on the base table -- the database still runs the DELETE
// against ledger's rows -- so DELETE FROM ledger_v is diagnosed exactly like DELETE
// FROM ledger.
func TestAdvWriteThroughUpdatableViewIsJudgedByNever(t *testing.T) {
	s, err := analyze.Load(advUnsoundSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	for _, sql := range []string{
		`DELETE FROM ledger_v WHERE id = $1`,
		`UPDATE ledger_v SET amount = $1 WHERE id = $2`,
	} {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		ds := obligation.Check(s, decls, r.Facts, lowerer{s})
		var failed bool
		for _, d := range ds {
			if d.Failed() {
				failed = true
			}
		}
		if !failed {
			t.Errorf("%s: want a failing `never` discharge against ledger (this statement really does UPDATE/DELETE ledger's rows through the view); got %d discharge(s), none failing: %+v", sql, len(ds), ds)
		}
	}
}

// Same as above, over `pinned`: UPDATE orders_v ... WHERE id = $1 (no tenant_id
// anywhere) is refused, just like the same statement against `orders` directly.
func TestAdvWriteThroughUpdatableViewIsJudgedByPinned(t *testing.T) {
	s, err := analyze.Load(advUnsoundSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `UPDATE orders_v SET status = 'closed' WHERE id = $1`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	ds := obligation.Check(s, decls, r.Facts, lowerer{s})
	var failed bool
	for _, d := range ds {
		if d.Failed() {
			failed = true
		}
	}
	if !failed {
		t.Errorf("%s: want a failing `pinned(tenant_id)` discharge against orders; got %d discharge(s), none failing: %+v", sql, len(ds), ds)
	}
}

// `never` is documented to reject "an UPDATE or DELETE on ledger ... inside a WITH as
// much as on its own", and TRUNCATE has the same effect on the rows (every one of them
// gone, no undo) without being an UPDATE or DELETE statement. A statement that empties
// ledger is diagnosed -- either as a `never` failure or as "does not analyze", the two
// categories the docs promise are always caught.
func TestAdvTruncateIsDiagnosedByNever(t *testing.T) {
	s, err := analyze.Load(advUnsoundSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `TRUNCATE ledger`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		// analyzing the statement failed outright: that is an acceptable diagnosis too.
		return
	}
	if r.Facts == nil {
		t.Fatalf("%s: analyze.Analyze reported no error but also no facts (r.Facts == nil): TRUNCATE is silently unjudged, not diagnosed", sql)
	}
	ds := obligation.Check(s, decls, r.Facts, lowerer{s})
	var failed bool
	for _, d := range ds {
		if d.Failed() {
			failed = true
		}
	}
	if !failed {
		t.Errorf("%s: want a failing `never` discharge against ledger; got %d discharge(s): %+v", sql, len(ds), ds)
	}
}

// docs/checks.md says pinned(tenant_id) is discharged "when reading, updating or
// deleting" by fixing the column to a known value, and "assign it when inserting" for
// INSERT only: `UPDATE orders SET tenant_id = $2 WHERE id = $1` fails pinned(tenant_id)
// because nothing fixes the row's current tenant_id -- an UPDATE that merely assigns
// tenant_id in SET, without fixing its old value in WHERE, does not discharge pinned.
func TestAdvPinnedRejectsBareAssignment(t *testing.T) {
	s, err := analyze.Load(advUnsoundSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	cases := []string{
		`UPDATE orders SET tenant_id = $2 WHERE id = $1`,
		`INSERT INTO orders (id, tenant_id, status) VALUES ($1, $2, 'x') ON CONFLICT (id) DO UPDATE SET tenant_id = excluded.tenant_id`,
	}
	for _, sql := range cases {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		ds := obligation.Check(s, decls, r.Facts, lowerer{s})
		var failed bool
		var lines []string
		for _, d := range ds {
			lines = append(lines, d.Obligation.Body.Spec()+" failed="+boolStr(d.Failed()))
			if d.Failed() {
				failed = true
			}
		}
		if !failed {
			t.Errorf("%s: want pinned(tenant_id) to fail (nothing fixes the row's old tenant_id); got: %s", sql, strings.Join(lines, "; "))
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
