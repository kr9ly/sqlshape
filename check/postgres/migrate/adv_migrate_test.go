package migrate

// Adversarial cases for lane "migrate": diff / plan / apply destructive-behavior probes.
// See /tmp/claude-1000/-home-kr9ly-projects-sqlshape/c120b088-d858-42eb-b453-2c47d8ce2ea3/scratchpad/adv/migrate.md
// for the write-up. Most reproduced holes are encoded here as tests that document the
// *correct* behavior and therefore fail until fixed. The two seed-removal tests are the
// exception: "the rows are left in place" was reviewed and kept as the deliberate,
// safe-by-default behavior, so those two pin down the current behavior as correct.

import (
	"context"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/oracle"
)

// TestAdvSeedRemovalLeavesExistingRowsInPlace pins down a deliberate design choice (not a
// bug): a seed's rows are declared only by the presence of an INSERT ... VALUES statement
// in schema.sql, and when that INSERT is simply deleted (the table itself stays), the table
// drops out of every row comparison on both sides (internal/dump readSeeds only reads a
// table whose *target* spec still carries a Seed; internal/diff.RowChanges bails out
// immediately when to.Seed == nil). Neither Plan, nor Verify, nor (by the same mechanism)
// verify-schema look at that table's row content again, and existing rows are left exactly
// as they are -- sqlshape never deletes data nobody's schema.sql asked it to delete. This
// was considered as a possible gap (data quietly diverging from the target) and rejected:
// the safe-by-default behavior is to leave rows alone, not to guess that removing a seed
// declaration means "drop these rows". docs/migrations.md is expected to spell this out
// explicitly next to the seed round-trip guarantees it already documents.
func TestAdvSeedRemovalLeavesExistingRowsInPlace(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()

	fromSQL := `
CREATE TABLE order_statuses (
    code       text PRIMARY KEY,
    label      text NOT NULL,
    sort_order integer NOT NULL
);
INSERT INTO order_statuses (code, label, sort_order) VALUES
    ('pending',   'Awaiting payment', 10),
    ('paid',      'Paid',             20),
    ('shipped',   'Shipped',          30),
    ('cancelled', 'Cancelled',        90);
`
	// Same table, no more INSERT: the author stopped declaring the seed (e.g. reverted a
	// seed block by hand, or a merge conflict dropped it) without any @migrate directive.
	toSQL := `
CREATE TABLE order_statuses (
    code       text PRIMARY KEY,
    label      text NOT NULL,
    sort_order integer NOT NULL
);
`
	from := mustCanonical(t, fromSQL)
	to := mustCanonical(t, toSQL)

	plan, err := Plan(from.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v\nplan so far:\n%s", err, strings.Join(plan, "\n"))
	}
	ddl := strings.Join(plan, "\n")
	t.Logf("plan:\n%s", ddl)

	// A dropped seed declaration is not, by itself, a data-affecting change as far as the
	// plan is concerned: the safe-by-default choice is to say nothing and touch nothing.
	if strings.Contains(ddl, "order_statuses") {
		t.Errorf("expected the plan to stay silent about order_statuses (dropping a seed declaration must not synthesize a DELETE), got:\n%s", ddl)
	}

	// Apply the (empty, as far as order_statuses is concerned) plan to a real database
	// that has the four seed rows, and ask Verify whether the result matches the target.
	changes, notes, err := Verify(ctx, server, from.text, ddl, to.s)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Verify compares structure and declared-seed rows; a table with no declared seed on
	// the target side is (by design) not diffed row by row, so this reports a clean match.
	if len(changes) != 0 || len(notes) != 0 {
		t.Errorf("Verify should report order_statuses as matching target (no seed declared on either side of the row comparison), got changes=%v notes=%v", changes, notes)
	}

	// Ground truth, independent of sqlshape's own machinery: actually apply
	// currentSQL+ddl to a real database and count the rows by hand. from.text is a
	// pg_dump with search_path emptied (see Verify's own comment above), so it must be
	// reset before running the plan's unqualified statements.
	o, err := oracle.Start(ctx, from.text+"\nRESET search_path;\n"+ddl)
	if err != nil {
		t.Fatalf("oracle.Start (apply schema + plan): %v", err)
	}
	defer o.Close()
	var n int
	if err := o.Conn().QueryRow(ctx, "SELECT count(*) FROM order_statuses").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	// The four rows that were already in the database are left exactly as they were:
	// removing a seed declaration is not read as "delete these rows".
	if n != 4 {
		t.Errorf("order_statuses should still have its 4 original rows after applying a plan that never declared them gone (sqlshape does not delete data nobody asked it to delete), got %d", n)
	}
}

// TestAdvSeedRemovalStaysSilentOnRoundTripDiff is a smaller-surface companion to
// TestAdvSeedRemovalLeavesExistingRowsInPlace: a completely fresh diff between two schema
// *texts* (not a live database) says nothing about the row-losing edit once the "from" side
// is also expressed without the historical rows -- diff.Compare has no way to represent
// "this table used to promise these rows and no longer promises anything about its rows"
// as opposed to "this table never had a seed"; both states canonicalize identically. That
// is consistent with the design pinned down above: a missing seed declaration is simply
// "no opinion about this table's rows", not a signal to delete anything, so there is
// nothing here for the plan to warn about either.
func TestAdvSeedRemovalStaysSilentOnRoundTripDiff(t *testing.T) {
	requirePgDump(t)

	seededTwice := `
CREATE TABLE lookups (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO lookups (code, label) VALUES ('a', 'A'), ('b', 'B');
`
	neverSeeded := `
CREATE TABLE lookups (code text PRIMARY KEY, label text NOT NULL);
`
	seeded := mustCanonical(t, seededTwice)
	never := mustCanonical(t, neverSeeded)

	// A table that used to carry declared rows and a table that never did compare as
	// having no Seed at all on the "removed" side; nothing in the model records that a
	// seed once existed and its rows are now unmanaged.
	if seeded.s.Relation("", "lookups").Seed == nil {
		t.Fatal("test setup: expected the seeded schema to have a Seed")
	}
	if never.s.Relation("", "lookups").Seed != nil {
		t.Fatal("test setup: expected the never-seeded schema to have no Seed")
	}

	plan, err := Plan(seeded.s, never.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	// By design, losing a seed declaration for a table that still exists produces no DDL
	// and no comment: "no seed declared" carries no opinion about the table's rows.
	if strings.Contains(ddl, "lookups") {
		t.Errorf("plan should say nothing about the dropped seed declaration for lookups, got:\n%q", ddl)
	}
}

// TestAdvNumericNarrowingSilentlyRounds: docs/migrations.md documents exactly one caveat
// for ALTER COLUMN ... TYPE — that it "needs a USING" and the plan is "meant to be read,
// and edited when ... the form is wrong for the data at hand". That framing implies the
// tool expects a human to notice and add a USING wherever the change could lose data. A
// numeric column whose scale (or precision) shrinks does not need a USING at all --
// PostgreSQL accepts the bare ALTER COLUMN ... TYPE and *silently rounds* every existing
// value to fit, with no error. Unlike an added NOT NULL (requires an explicit @migrate
// backfill or the plan errors) or a shrinking enum (requires an explicit @migrate enum decl
// or the plan errors), this one is not gated -- by the migrate A2 design decision it is not
// gated *either*: the plan is a proposal the operator edits, not something the tool blocks,
// so this only requires a note next to the ALTER (matching how a domain/range base type
// change that "cannot be altered" gets a note). Running the plan unedited still rounds the
// existing values exactly as PostgreSQL would without sqlshape in the picture; the note is
// what lets an operator notice and add a USING (or fix the data) before doing that.
func TestAdvNumericNarrowingNotedButUnenforced(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()

	from := mustCanonical(t, `CREATE TABLE prices (v numeric(10,2));`)
	to := mustCanonical(t, `CREATE TABLE prices (v numeric(10,0));`)

	plan, err := Plan(from.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	t.Logf("plan:\n%s", ddl)

	// A column narrowing that can change stored values without erroring must be flagged
	// (a note, at minimum), the same way domain/range type changes that "cannot be
	// altered" get a note.
	if !strings.Contains(strings.ToLower(ddl), "round") && !strings.Contains(strings.ToLower(ddl), "truncat") {
		t.Errorf("plan gives no warning that narrowing numeric(10,2) to numeric(10,0) can silently round existing values:\n%s", ddl)
	}

	o, err := oracle.Start(ctx, from.text+"\nRESET search_path;\nINSERT INTO prices VALUES (1.239), (2.995), (-0.5);\n"+ddl)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer o.Close()
	rows, err := o.Conn().Query(ctx, "SELECT v::text FROM prices ORDER BY v")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, v)
	}
	// The plan is a proposal, not something sqlshape enforces (migrate A2 design
	// decision): it does not add a USING or block the ALTER on the operator's behalf, so
	// running it unedited still rounds the existing values exactly as PostgreSQL would
	// without sqlshape in the picture (1.239 -> 1, 2.995 -> 3, -0.5 -> -1). What changed
	// is that the note above now gives an operator something to notice and act on before
	// running the plan unedited.
	t.Logf("values after the noted-but-unedited plan ran: %v", got)
	if strings.Join(got, ",") != "-1,1,3" {
		t.Errorf("expected the unedited plan to still round the stored values to -1,1,3 (the note does not block it), got %v", got)
	}
}
