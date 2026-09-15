package obligation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// TestCheckOptionPinsCascaded: a write through a CASCADED (or plain, since CASCADED is
// PostgreSQL's default) WITH CHECK OPTION view is pinned by whatever the view chain's own
// WHERE fixes, down to the base table -- the server refuses any row that would not satisfy
// it, so `require pinned(tenant_id)` is discharged (ByView) even though the writing
// statement's own WHERE never mentions tenant_id.
//
// Measured against a real, running PostgreSQL: an UPDATE through the CASCADED view that
// tries to move a row's tenant_id away from 1 (the underlying view v1's WHERE) is rejected
// with SQLSTATE 44000, so the pin genuinely holds for every write PostgreSQL accepts.
func TestCheckOptionPinsCascaded(t *testing.T) {
	schema := `
-- sqlshape: require pinned(tenant_id)
CREATE TABLE t (id bigint PRIMARY KEY, tenant_id int NOT NULL, v int NOT NULL);
CREATE VIEW v1 AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1;
CREATE VIEW v2 AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH CASCADED CHECK OPTION;`
	s, err := analyze.Load(schema)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	// the statement itself never mentions tenant_id: only the view chain pins it
	sql := `UPDATE v2 SET v = $1 WHERE id = $2`
	r, aerr := analyze.Analyze(s, sql)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	var got string
	for _, d := range obligation.Check(s.Contract(), all, r.Facts, lowerer{s}) {
		if d.Obligation.Body.Pinned == "tenant_id" {
			got = pathName(d.Path)
			if d.Failed() {
				got += ": " + d.Message
			}
		}
	}
	if got != "view" {
		t.Errorf("pinned(tenant_id) discharge = %q, want \"view\" (through v2's CASCADED chain down to v1's WHERE tenant_id = 1)", got)
	}

	// ground truth: a statement that does move the row's tenant_id is rejected by the
	// server (44000), so the pin the analyzer reports is not a false promise
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schema)
	if err != nil {
		t.Fatalf("oracle start: %v", err)
	}
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, `INSERT INTO t (id, tenant_id, v) VALUES (1, 1, 5)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = conn.Exec(ctx, `UPDATE v2 SET tenant_id = 2 WHERE id = 1`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "44000" {
		t.Fatalf("UPDATE v2 SET tenant_id = 2: want SQLSTATE 44000 (new row violates check option), got %v", err)
	}
}

// TestCheckOptionLocalDoesNotPinUnderlyingView: WITH LOCAL CHECK OPTION only re-checks the
// view's own WHERE after a write, not an underlying view's -- so a write through it is not
// pinned by the underlying view's WHERE. The analyzer must not discharge
// `require pinned(tenant_id)` here; PostgreSQL genuinely lets the write move the row's
// tenant_id away from the value v1's WHERE names, confirming the pin would be a false
// promise if the checker granted it.
func TestCheckOptionLocalDoesNotPinUnderlyingView(t *testing.T) {
	schema := `
-- sqlshape: require pinned(tenant_id)
CREATE TABLE t (id bigint PRIMARY KEY, tenant_id int NOT NULL, v int NOT NULL);
CREATE VIEW v1 AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1;
CREATE VIEW v2 AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH LOCAL CHECK OPTION;`
	s, err := analyze.Load(schema)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `UPDATE v2 SET v = $1 WHERE id = $2`
	r, aerr := analyze.Analyze(s, sql)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	var got string
	for _, d := range obligation.Check(s.Contract(), all, r.Facts, lowerer{s}) {
		if d.Obligation.Body.Pinned == "tenant_id" {
			got = pathName(d.Path)
			if d.Failed() {
				got += ": " + d.Message
			}
		}
	}
	if got == "view" {
		t.Errorf("pinned(tenant_id) discharge = %q through v2's LOCAL check option, but LOCAL never re-checks v1's WHERE: this would be a false promise", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schema)
	if err != nil {
		t.Fatalf("oracle start: %v", err)
	}
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, `INSERT INTO t (id, tenant_id, v) VALUES (1, 1, 5)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE v2 SET tenant_id = 2 WHERE id = 1`); err != nil {
		t.Fatalf("UPDATE v2 SET tenant_id = 2: WITH LOCAL CHECK OPTION should let this through (only v2's own v > 0 is re-checked), got %v", err)
	}
	var tenantID int
	if err := conn.QueryRow(ctx, `SELECT tenant_id FROM t WHERE id = 1`).Scan(&tenantID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if tenantID != 2 {
		t.Fatalf("tenant_id = %d, want 2 (LOCAL CHECK OPTION let the write move it away from v1's WHERE)", tenantID)
	}
}

// TestCheckOptionSkipsUnprojectedColumn: a WITH CHECK OPTION view's WHERE may name a
// column the view's own SELECT list never projects (tenant_id here); LiftThroughView has
// no output to translate it through, so it must be left out rather than guessed at. The
// obligation is not discharged -- a conservative, not a false, answer: nothing about the
// write statement's own leaf names tenant_id at all, so `require pinned(tenant_id)` simply
// does not apply to it in a way the checker can state.
func TestCheckOptionSkipsUnprojectedColumn(t *testing.T) {
	schema := `
-- sqlshape: require pinned(tenant_id)
CREATE TABLE t (id bigint PRIMARY KEY, tenant_id int NOT NULL, v int NOT NULL);
CREATE VIEW v1 AS SELECT id, v FROM t WHERE tenant_id = 1 WITH CASCADED CHECK OPTION;`
	s, err := analyze.Load(schema)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `UPDATE v1 SET v = $1 WHERE id = $2`
	r, aerr := analyze.Analyze(s, sql)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	var got string
	for _, d := range obligation.Check(s.Contract(), all, r.Facts, lowerer{s}) {
		if d.Obligation.Body.Pinned == "tenant_id" {
			got = pathName(d.Path)
			if d.Failed() {
				got += ": " + d.Message
			}
		}
	}
	if got == "view" {
		t.Errorf("pinned(tenant_id) discharge = %q, but v1 never projects tenant_id: there is nothing on this write's own leaf to call tenant_id", got)
	}
}
