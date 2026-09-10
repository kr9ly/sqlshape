package dialect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// TestAdvPinnedRejectsVolatileEquality: `require pinned(tenant_id)` exists so that a
// statement can be trusted to touch exactly one tenant's rows -- the value must be fixed
// before any row is examined (x/facts' Term doc, on a Known term: "a value fixed before
// the row is examined... a stable function of the session"). WHERE tenant_id = FLOOR(RAND()*3)
// is not such a value: the server evaluates RAND() per row (already demonstrated against a
// real mysqld in TestAdvGroupByVolatileFunctionIsNotSingle, internal/analyze), so within one
// execution the predicate can pass for rows of *different* tenants.
//
// The MySQL producer's termFacts must not record an expression bound to the execution
// (RAND()) as a Known term: the checker's pinned() would see tenant_id in sc.Fixed and
// discharge `require pinned(tenant_id)` as satisfied (ByStatement) even though the statement
// does not, in fact, pin the row to one tenant.
func TestAdvPinnedRejectsVolatileEquality(t *testing.T) {
	m := loadContract(t)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `SELECT id, tenant_id FROM orders WHERE tenant_id = FLOOR(RAND() * 3) AND deleted_at IS NULL`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	pinnedSatisfied := false
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require pinned(tenant_id)" && d.Message == "" {
			pinnedSatisfied = true
		}
	}
	if pinnedSatisfied {
		t.Errorf("%s: require pinned(tenant_id) is discharged by an equality to RAND(), which fixes nothing", sql)
	}

	// Confirm against a real server: with tenant_id fixed by a per-row-random equality,
	// one execution touches more than one tenant's rows.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, testSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	for _, tenant := range []int{1, 2, 3} {
		if _, err := conn.ExecContext(ctx, "INSERT INTO tenants (id) VALUES (?)", tenant); err != nil {
			t.Fatal(err)
		}
	}
	id := 1
	for _, tenant := range []int{1, 2, 3} {
		for k := 0; k < 400; k++ {
			if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id, status, amount) VALUES (?, ?, 'open', 1)", id, tenant); err != nil {
				t.Fatal(err)
			}
			id++
		}
	}

	rows, err := conn.QueryContext(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	seen := map[int64]bool{}
	for rows.Next() {
		var oid, tid int64
		if err := rows.Scan(&oid, &tid); err != nil {
			t.Fatal(err)
		}
		seen[tid] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// the server's side of the argument: one execution touches more than one tenant (RAND()
	// is evaluated per row), so a checker that discharged the obligation would be wrong
	if len(seen) <= 1 {
		t.Logf("RAND() happened to pass rows of %d tenant(s) this run", len(seen))
	}
}
