package dialect

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// Third adversarial round (2026-09, obligation lane, since commit bcc79c4): whether the
// MySQL producer's facts (check/mysql/internal/analyze/facts.go, analyze.go's write side,
// check/mysql/dialect/dialect.go's Contract/Lower) and x/obligation's own judgment agree
// with mysqld 8.4. Every finding here is a case where the fact -- or the judgment over the
// fact -- does not match what mysqld actually does.

// adv3Load is loadContract's twin for a schema local to this file.
func adv3Load(t *testing.T, schemaSQL string) *mysql {
	t.Helper()
	an, err := load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	return m
}

const adv3NullableFKSchema = `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_orders_id_tenant (id, tenant_id)
);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  order_id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NULL,
  status VARCHAR(10) NOT NULL,
  CONSTRAINT fk_items_order FOREIGN KEY (order_id, tenant_id) REFERENCES orders (id, tenant_id)
);
`

// TestAdv3NullableForeignKeyColumnFalselyPinnedViaFK: severity high.
//
// `require pinned(tenant_id)` on items exists so that every statement on items is trusted
// to touch exactly one tenant's rows. x/obligation's viaForeignKey (check.go) carries a
// pin over a composite foreign key: if items(order_id, tenant_id) REFERENCES
// orders(id, tenant_id) (a UNIQUE key) and a statement joins items.order_id = orders.id
// while orders.tenant_id is itself fixed, viaForeignKey treats items.tenant_id as pinned
// too (ByForeignKey) -- reasoning that the FK guarantees the two columns agree.
//
// That guarantee is conditional on mysqld actually enforcing the constraint for the row,
// and mysqld only supports MATCH SIMPLE: when *any* column of a multi-column foreign key
// is NULL, the whole constraint is not checked for that row (measured below: items.tenant_id
// is declared NULL-able here, unlike order_items in dialect_test.go's testSchema, whose
// tenant_id is NOT NULL). viaForeignKey (check.go's viaForeignKey, ~line 430) never looks at
// whether the child's own FK columns can be NULL before using this propagation -- so a row
// of items whose tenant_id is NULL (never checked against orders at all) is still reported
// as pinned to whatever orders.tenant_id the statement's join fixed, though the row's actual
// tenant_id agrees with nothing.
func TestAdv3NullableForeignKeyColumnFalselyPinnedViaFK(t *testing.T) {
	m := adv3Load(t, adv3NullableFKSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `UPDATE items JOIN orders ON items.order_id = orders.id SET items.status = $1 WHERE orders.tenant_id = $2`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require pinned(tenant_id)" && d.Message == "" {
			t.Errorf("%s: sqlshape reports require pinned(tenant_id) on items as discharged (ByForeignKey, through order_id = orders.id and orders.tenant_id fixed), but items.tenant_id is NULL-able and mysqld does not enforce the composite FK when it is NULL -- a row with tenant_id NULL can still be touched by this statement, measured against mysqld below", sql)
		}
	}

	// Confirm against a real server: an items row with tenant_id NULL and order_id
	// pointing at a real order is accepted (the FK is not checked because one of its
	// columns is NULL), and the statement above still touches it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv3NullableFKSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id) VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO items (id, order_id, tenant_id, status) VALUES (1, 1, NULL, 'open')"); err != nil {
		t.Fatalf("server rejects an items row with a NULL half of the composite FK: %v", err)
	}
	res, err := conn.ExecContext(ctx, "UPDATE items JOIN orders ON items.order_id = orders.id SET items.status = ? WHERE orders.tenant_id = ?", "closed", 1)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the statement to touch the NULL-tenant_id row (demonstrating the hazard), got %d rows affected", n)
	}
	var tenantID *int64
	if err := conn.QueryRowContext(ctx, "SELECT tenant_id FROM items WHERE id = 1").Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if tenantID != nil {
		t.Fatalf("expected items.tenant_id to still be NULL after the update, got %v", *tenantID)
	}
}

const adv3CascadedJoinSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require pinned(tenant_id)
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  tenant_id INT NOT NULL,
  v INT NOT NULL
);
CREATE TABLE meta (
  t_id INT NOT NULL PRIMARY KEY,
  note VARCHAR(10) NOT NULL
);
CREATE VIEW v1 AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1;
CREATE VIEW v2 AS SELECT v1.id AS id, v1.tenant_id AS tenant_id, v1.v AS v, meta.note AS note
  FROM v1 JOIN meta ON meta.t_id = v1.id WHERE meta.note = 'ok' WITH CASCADED CHECK OPTION;
`

// TestAdv3CascadedCheckOptionSkipsNestedViewBehindJoin: severity medium.
//
// v2 is declared WITH CASCADED CHECK OPTION (docs/mysql.md: CASCADED is MySQL's own
// default, and docs/obligations.md says CASCADED "reaches down to view WHEREs"), built on
// top of v1 (WHERE tenant_id = 1) through a JOIN with meta. A single-row UPDATE through v2
// that does not touch tenant_id at all still requires the server to keep every row inside
// v1's own WHERE, because CASCADED does not stop at the first join it meets -- measured
// below: trying to move a row's tenant_id away from 1 through v2 is refused with 1369
// naming v2, exactly as it would be if v2 were declared directly on v1 without the join.
//
// x/facts's LiftThroughView (x/facts/lift.go's liftPreds) only recurses into a *nested*
// view's own body when `cascaded && len(body.Leaves) == 1` (lift.go:72) -- i.e. only when
// the current view's FROM is exactly the nested view alone, with no join. v2's own body has
// two leaves (v1 and meta), so the recursion never fires and v1's `tenant_id = 1` is never
// lifted into the statement's facts at all; only meta's own WHERE (note = 'ok', which says
// nothing about tenant_id) is. check/mysql/internal/analyze/facts.go's checkOptionFacts
// (~line 91) calls LiftThroughView unconditionally at the join, so it inherits the same gap.
//
// The result: `require pinned(tenant_id)` on t is reported as *not* discharged for a
// write through v2 (no ByView path -- the statement itself does not fix tenant_id either),
// even though the server does, in fact, keep every row of t inside tenant_id = 1 for
// exactly this write. A correct statement is reported as a violation.
func TestAdv3CascadedCheckOptionSkipsNestedViewBehindJoin(t *testing.T) {
	m := adv3Load(t, adv3CascadedJoinSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `UPDATE v2 SET v = $1 WHERE id = $2`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	flagged := false
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require pinned(tenant_id)" && d.Message != "" {
			flagged = true
			t.Errorf("%s: sqlshape reports %q, but v2 is WITH CASCADED CHECK OPTION over v1 (WHERE tenant_id = 1) through a JOIN with meta: CASCADED reaches v1's own WHERE regardless of the join (measured against mysqld below), so the statement is, in fact, pinned to tenant_id = 1", sql, d.Message)
		}
	}
	if !flagged {
		t.Skip("sqlshape already discharges pinned(tenant_id) through v2 -- gap not present (or already closed); not a finding")
	}

	// Confirm against a real server: WITH CASCADED CHECK OPTION on v2 keeps every row
	// inside v1's own WHERE (tenant_id = 1), even though v2's FROM is a join, not v1 alone.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv3CascadedJoinSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO t (id, tenant_id, v) VALUES (1, 1, 10)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO meta (t_id, note) VALUES (1, 'ok')"); err != nil {
		t.Fatal(err)
	}
	// The statement under analysis: it does not touch tenant_id and must succeed.
	if _, err := conn.ExecContext(ctx, "UPDATE v2 SET v = ? WHERE id = ?", 20, 1); err != nil {
		t.Fatalf("server rejects the statement sqlshape reports as a pinned(tenant_id) violation: %v", err)
	}
	// The hazard require pinned(tenant_id) exists to rule out: moving the row to another
	// tenant through v2. CASCADED must refuse it (1369) if it reaches through the join to
	// v1's own WHERE.
	_, err = conn.ExecContext(ctx, "UPDATE v2 SET tenant_id = ? WHERE id = ?", 2, 1)
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1369 {
		t.Fatalf("test premise wrong: want the server to refuse moving the row to another tenant through v2 with 1369 (ER_VIEW_CHECK_FAILED), got %v", err)
	}
}
