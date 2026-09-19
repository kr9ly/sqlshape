package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// The second adversarial round left six shapes for a decision; these pin the decisions.
// `paired` does not compare values and `sensitive` counts RETURNING as a read (kept, in
// docs); `col = NULL` fixes nothing; SET col = DEFAULT is the column's literal default; an
// opt-out above a CREATE TABLE is told where it belongs; a bare and a qualified name under
// search_path are one relation.

func discharges(t *testing.T, schemaSQL, sql string) []obligation.Discharge {
	t.Helper()
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return obligation.Check(s.Contract(), decls, r.Facts, lowerer{s})
}

func failing(ds []obligation.Discharge) []string {
	var out []string
	for _, d := range ds {
		if d.Failed() {
			out = append(out, d.Message)
		}
	}
	return out
}

func TestNullEqualityPinsNothing(t *testing.T) {
	const schemaSQL = `
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
`
	for _, sql := range []string{
		`SELECT id FROM orders WHERE tenant_id = NULL`,
		`DELETE FROM orders WHERE tenant_id = NULL::bigint`,
		`SELECT id FROM orders WHERE tenant_id IN (NULL)`,
	} {
		if f := failing(discharges(t, schemaSQL, sql)); len(f) != 1 || !strings.Contains(f[0], "tenant_id is not pinned") {
			t.Errorf("%s: %v, want the pinned failure", sql, f)
		}
	}
	if f := failing(discharges(t, schemaSQL, `SELECT id FROM orders WHERE tenant_id = 1`)); len(f) != 0 {
		t.Errorf("= 1: %v", f)
	}
}

func TestSetDefaultIsTheColumnsLiteralDefault(t *testing.T) {
	const schemaSQL = `
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | draft
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL DEFAULT 'draft', note text DEFAULT now()::text);
-- sqlshape: transitions state: a -> b
CREATE TABLE jobs (id bigint PRIMARY KEY, state text NOT NULL DEFAULT lower('A'));
`
	// DEFAULT is 'draft': a transition to draft, which submitted may take
	if f := failing(discharges(t, schemaSQL, `UPDATE orders SET status = DEFAULT WHERE id = $1 AND status = 'submitted'`)); len(f) != 0 {
		t.Errorf("submitted -> DEFAULT (draft): %v", f)
	}
	f := failing(discharges(t, schemaSQL, `UPDATE orders SET status = DEFAULT WHERE id = $1`))
	if len(f) != 1 || !strings.Contains(f[0], "SET status = 'draft' must fix the current state in WHERE (status = 'submitted')") {
		t.Errorf("DEFAULT without the current state: %v", f)
	}
	// a default that is not a literal is not a declared state
	f = failing(discharges(t, schemaSQL, `UPDATE jobs SET state = DEFAULT WHERE id = $1`))
	if len(f) != 1 || !strings.Contains(f[0], "DEFAULT is not one here, the column's default is not a literal") {
		t.Errorf("non-literal DEFAULT: %v", f)
	}
}

func TestOptOutAboveCreateTableIsExplained(t *testing.T) {
	s, err := analyze.Load("-- sqlshape: unfiltered orders\nCREATE TABLE orders (id bigint PRIMARY KEY);\n-- sqlshape: waive orders pinned(x)\nCREATE TABLE t2 (id bigint PRIMARY KEY);")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 2 {
		t.Fatalf("problems: %v", s.Problems)
	}
	for _, p := range s.Problems {
		if !strings.Contains(p.Message, "is a view's or a statement's opt-out: write it above the CREATE VIEW, or above the statement, whose reads it waives") {
			t.Errorf("%s", p)
		}
	}
}

func TestBareAndQualifiedNamesUnderSearchPathAreOneRelation(t *testing.T) {
	const schemaSQL = `
CREATE SCHEMA app;
SET search_path TO app, public;
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
`
	for _, sql := range []string{`SELECT id FROM orders WHERE id = $1`, `SELECT id FROM app.orders WHERE id = $1`} {
		if f := failing(discharges(t, schemaSQL, sql)); len(f) != 1 || !strings.Contains(f[0], "tenant_id is not pinned") {
			t.Errorf("%s: %v", sql, f)
		}
	}
	// mixed in one statement: each occurrence is judged on its own
	f := failing(discharges(t, schemaSQL, `SELECT o.id FROM app.orders o JOIN orders p ON p.id = o.id WHERE o.tenant_id = $1`))
	if len(f) != 1 || !strings.Contains(f[0], "(the occurrence p)") {
		t.Errorf("mixed: %v", f)
	}
	if f := failing(discharges(t, schemaSQL, `SELECT o.id FROM app.orders o JOIN orders p ON p.id = o.id WHERE o.tenant_id = $1 AND p.tenant_id = $1`)); len(f) != 0 {
		t.Errorf("both pinned: %v", f)
	}
}
