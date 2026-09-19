package analyze

// Adversarial round 3, lane "newcode": probes the new code from round 2 (2026-09-15,
// commit 20a55af) -- body.go, violations.go, facts.go, call.go, expr.go, fullgroup.go,
// schema.go -- against a running mysqld 8.4, one rung further out from what adv2_*_test.go
// already pinned. See scratchpad/adv3/brief-newcode.md and adv3-newcode-report.md.

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// ---------------------------------------------------------------------------------------
// Finding: WITH CHECK OPTION, a LOCAL view built on top of a CASCADED one -- MySQL's own
// docs (CREATE VIEW, "LOCAL and CASCADED Check Options"): "If the LOCAL keyword is used,
// checking of underlying views only takes place if the underlying view itself specifies a
// check option." v_inner DOES specify one (CASCADED), so writing through v_outer (LOCAL)
// must still enforce v_inner's own WHERE (tenant_id = 1), even though v_outer's own WHERE
// (v > 0) says nothing about tenant_id. Measured below: UPDATE v_outer SET tenant_id = 2
// through a row that started at tenant_id = 1 is refused every time with 1369 naming
// v_outer, on mysqld 8.4.11.
//
// facts.go's checkOptionFacts computes the cascade flag it hands to x/facts.LiftThroughView
// from only the WRITTEN-THROUGH view's own CheckOption (`v.CheckOption == "CASCADED"`) --
// here v_outer's, "LOCAL", so cascade = false. x/facts/lift.go's liftPreds only descends
// into a nested view's own body when its caller's cascaded flag is true (the `if cascaded
// && len(body.Leaves) == 1 ...` branch), so it never even looks at v_inner's body, let alone
// v_inner's own CheckOption -- the single bool threaded down ignores that an inner view can
// independently force cascading regardless of what a view built on top of it asks for. The
// result: Analyze's facts.Top.Preds for `UPDATE v_outer SET tenant_id = ...` carries no
// FromView equality on tenant_id at all, so a `require pinned(tenant_id)` on t's writes
// (x/obligation, checked from check/mysql/dialect, not reproduced here) would wrongly see
// this UPDATE as not fixing tenant_id through the view chain, even though the server
// guarantees tenant_id stays 1 for every row that can be written through v_outer.
// Suspect: check/mysql/internal/analyze/facts.go's checkOptionFacts (the
// `v.CheckOption == "CASCADED"` argument to LiftThroughView) and x/facts/lift.go's liftPreds
// (the single `cascaded bool` it threads all the way down, instead of asking each nested
// view's own CheckOption before deciding whether to keep descending).
func TestAdv3CheckOptionCascadedUnderLocalViewMissesInnerEquality(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const nestedCheckOptionSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  tenant_id INT NOT NULL,
  v INT NOT NULL
);
CREATE VIEW v_inner AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1 WITH CASCADED CHECK OPTION;
CREATE VIEW v_outer AS SELECT id, tenant_id, v FROM v_inner WHERE v > 0 WITH LOCAL CHECK OPTION;
`
	db, err := mysqltest.Start(ctx, nestedCheckOptionSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, `INSERT INTO t (id, tenant_id, v) VALUES (1, 1, 5)`); err != nil {
		t.Fatal(err)
	}
	// the server: moving the row to a different tenant_id through v_outer is refused with
	// 1369, though v_outer's own WHERE (v > 0) says nothing about tenant_id -- because
	// v_inner is itself CASCADED, its own WHERE (tenant_id = 1) is still enforced through a
	// LOCAL view built on top of it (measured against mysqld 8.4.11)
	_, execErr := conn.ExecContext(ctx, `UPDATE v_outer SET tenant_id = 2 WHERE id = 1`)
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 1369 {
		t.Fatalf("test premise wrong: want the server to refuse this UPDATE with 1369, got %v", execErr)
	}

	s, err := schema.Load(nestedCheckOptionSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, aerr := Analyze(s, "UPDATE v_outer SET tenant_id = $1 WHERE id = $2")
	if aerr != nil {
		t.Fatal(aerr)
	}
	found := false
	for _, p := range r.Facts.Top.Preds {
		if p.Op == facts.Eq && p.Origin == facts.FromView && p.Term.Kind == facts.Const && p.Term.Const == "i1" { // constText's tagged spelling (facts.go)
			found = true
		}
	}
	if !found {
		t.Errorf("Analyze(UPDATE v_outer SET tenant_id = ...).Facts.Top.Preds = %+v, want a FromView equality fixing tenant_id = 1 among them (v_inner's own CASCADED check option is still enforced through the LOCAL v_outer, measured on mysqld) -- checkOptionFacts's cascade flag comes only from the written-through view's own CheckOption, so it never looks at v_inner's", r.Facts.Top.Preds)
	}
}
