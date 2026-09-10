package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/obligation"
)

// A context's `waive <body> on <kinds>` is scoped to those kinds: a waiver spelled `on
// select` removes the base obligation only for select, leaving it in force for every
// other kind.
func TestAdvContextWaiveRespectsKind(t *testing.T) {
	const sch = `
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id) on select
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
`
	s, err := analyze.Load(sch)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	decls := obligation.InContext(all, "ops")

	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			line := d.Leaf.Table + " " + d.Obligation.Body.Spec() + " " + pathName(d.Path)
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}

	// the context's waiver is `on select` only, so a DELETE that does not pin
	// tenant_id still fails the base obligation under context "ops".
	got := run(`DELETE FROM orders WHERE id = $1`)
	want := "orders pinned(tenant_id) FAIL"
	if got != want {
		t.Errorf("waive on select should not excuse DELETE from pinning; got %q want %q", got, want)
	}
}

// -require-columns (and -no-tables / -no-table-reads) are documented as "shorthands for
// obligations declared per table" (docs/flags.md), so a context's `waive` lifts a
// flag-derived obligation the same way it lifts the equivalent obligation declared in
// schema.sql.
func TestAdvContextWaivesFlagObligation(t *testing.T) {
	const sch = `
-- sqlshape: context ops: waive pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
`
	s, err := analyze.Load(sch)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	// Mirrors how vet/check combine context selection with flag-derived obligations.
	decls := obligation.InContext(append(all, obligation.FromFlags(s, "tenant_id", false, false)...), "ops")

	r, err := analyze.Analyze(s, `DELETE FROM orders WHERE id = $1`)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
		lines = append(lines, d.Leaf.Table+" "+d.Obligation.Body.Spec()+" "+pathName(d.Path))
	}
	got := strings.Join(lines, "\n")
	// context "ops" waives pinned(tenant_id), so the unpinned DELETE passes: the
	// context lifts the flag's obligation as it lifts a declared one -- no judgment.
	if got != "" {
		t.Errorf("context ops should waive the flag-derived pinned(tenant_id) obligation too; got %q", got)
	}
}
