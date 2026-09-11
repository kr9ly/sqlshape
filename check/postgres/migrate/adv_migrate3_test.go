package migrate

// Adversarial cases for lane "migrate", round 3: diff / plan / apply behavior around
// generated columns, probed against a running PostgreSQL (17 for the STORED-expression
// cases, 18 for the VIRTUAL-vs-STORED case, since VIRTUAL generated columns are new in
// PostgreSQL 18 and dumping a PG18 database needs pg_dump 18). Each test documents the
// *correct* behavior and therefore fails until fixed.

import (
	"context"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
)

// TestAdvMigrateGeneratedColumnRewriteRestoresDependentIndex: migrate.go's alterTable
// has no in-place ALTER for a changed generation expression (PostgreSQL has none
// either), so it emits ALTER TABLE ... DROP COLUMN b; ALTER TABLE ... ADD COLUMN b ...
// (migrate.go alterTable, the "generated" branch). PostgreSQL's DROP COLUMN silently
// takes any index defined solely on that column down with it (no CASCADE needed,
// verified below), but the plan does not know this happened and never re-emits the
// index's CREATE statement -- because the "adds" phase's index pass only creates an
// index when its definition differs from (or is missing from) the *modeled* from-schema,
// and the model still says the index exists (it was never told about the DROP COLUMN's
// side effect). The result: a schema.sql edit that only changes a generation expression
// silently deletes a unique index, with no error and no note, and the database ends up
// missing an index the target schema still declares.
//
// Repro (PG 17):
//
//	CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) STORED);
//	CREATE UNIQUE INDEX t_b_idx ON t (b);
//	-- edit: change the expression to (a * 3)
//
// sqlshape's plan: `ALTER TABLE "public"."t" DROP COLUMN "b"; ALTER TABLE "public"."t"
// ADD COLUMN "b" integer GENERATED ALWAYS AS (a * 3) STORED;` -- no CREATE INDEX, no note.
// PostgreSQL's answer, ground truth: after running that plan, t_b_idx no longer exists.
// migrate.Verify (comparing the resulting database to the target) confirms the plan does
// not reach the target: it reports the target's t_b_idx as missing.
func TestAdvMigrateGeneratedColumnRewriteRestoresDependentIndex(t *testing.T) {
	requirePgDump(t)
	from := mustCanonical(t, `
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) STORED);
CREATE UNIQUE INDEX t_b_idx ON t (b);
`)
	to := mustCanonical(t, `
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 3) STORED);
CREATE UNIQUE INDEX t_b_idx ON t (b);
`)
	plan, err := Plan(from.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v\nplan so far:\n%s", err, strings.Join(plan, "\n"))
	}
	ddl := strings.Join(plan, "\n")
	t.Logf("plan:\n%s", ddl)

	changes, notes, err := Verify(context.Background(), server, from.text, ddl, to.s)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected the plan to restore t_b_idx (rewriting a generated column's expression must not silently drop a dependent index), but Verify still finds a difference: %v (notes=%v)", changes, notes)
	}
}

// TestAdvMigrateGeneratedColumnRewriteWithForeignKeyDependent: the same DROP COLUMN /
// ADD COLUMN rewrite (see above) fails outright, instead of merely losing an object,
// when another table's foreign key references the generated column (through a UNIQUE
// constraint on it): PostgreSQL refuses to drop a column another table's constraint
// depends on. sqlshape's plan has no note, no error at Plan time, and no attempt to
// drop/recreate the dependent foreign key around the rewrite -- it just emits a DROP
// COLUMN that PostgreSQL is guaranteed to refuse.
//
// Repro (PG 17):
//
//	CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) STORED UNIQUE);
//	CREATE TABLE child (id bigint PRIMARY KEY, t_b integer REFERENCES t(b));
//	-- edit: change t's generation expression to (a * 3)
//
// sqlshape's plan: `ALTER TABLE "public"."t" DROP COLUMN "b"; ALTER TABLE "public"."t"
// ADD COLUMN "b" integer GENERATED ALWAYS AS (a * 3) STORED;` (Plan returns no error).
// PostgreSQL's answer, ground truth: running that DDL fails with "ERROR: cannot drop
// column b of table t because other objects depend on it" (SQLSTATE 2BP01) -- the plan
// can never be applied as generated, only hand-edited by an operator who already
// understands the dependency migrate.go itself computed and threw away.
func TestAdvMigrateGeneratedColumnRewriteWithForeignKeyDependent(t *testing.T) {
	requirePgDump(t)
	from := mustCanonical(t, `
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) STORED UNIQUE);
CREATE TABLE child (id bigint PRIMARY KEY, t_b integer REFERENCES t(b));
`)
	to := mustCanonical(t, `
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 3) STORED UNIQUE);
CREATE TABLE child (id bigint PRIMARY KEY, t_b integer REFERENCES t(b));
`)
	plan, err := Plan(from.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v\nplan so far:\n%s", err, strings.Join(plan, "\n"))
	}
	ddl := strings.Join(plan, "\n")
	t.Logf("plan:\n%s", ddl)

	_, _, err = Verify(context.Background(), server, from.text, ddl, to.s)
	if err != nil {
		t.Errorf("expected the plan to be runnable (drop and recreate the dependent foreign key around the column rewrite, or at least warn instead of emitting a DROP COLUMN PostgreSQL is certain to refuse), got: %v", err)
	}
}

// TestAdvMigrateGeneratedColumnKindChangeIsNotIgnored: a generated column's kind
// (STORED vs VIRTUAL, PostgreSQL 18) is tracked by diff.Compare (schema.Column.
// GeneratedVirtual feeds diff's "generated kind" property) but migrate.go's alterTable
// only compares fp["generated"] (the deparsed expression text) -- never "generated
// kind" -- before deciding a generated column needs no DROP COLUMN / ADD COLUMN
// rewrite. When only the kind changes and the expression stays byte-identical, alters()
// finds no property it knows to check different and Plan emits nothing at all: schema.sql
// changes from VIRTUAL to STORED (or back), Plan reports zero DDL, and the database is
// left exactly as it was -- silently out of step with the target schema forever, since
// nothing about this ever produces an error or a note for an operator to notice.
//
// Repro (PG 18, needs pg_dump 18 -- run this file's suite with
// `nix-shell -p postgresql_18 --run "go test ./check/postgres/migrate/..."`, since
// this package's shared `server` boots PostgreSQL 17):
//
//	-- sqlshape: postgres 18
//	CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) VIRTUAL);
//	-- edit: VIRTUAL -> STORED, same expression
//
// PostgreSQL's answer, ground truth: diff.Compare(from, to) reports `~ column t.b /
// generated kind: virtual -> ` -- the two schemas are genuinely different. sqlshape's
// answer: migrate.Plan(from, to, nil) returns an empty plan and no error, as if the two
// schemas already matched.
func TestAdvMigrateGeneratedColumnKindChangeIsNotIgnored(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	srv18, err := dump.NewServer(ctx, pgparse.PG18)
	if err != nil {
		t.Skipf("pg_dump 18 / postgres 18 not available: %v", err)
	}
	defer srv18.Close()

	canon := func(sql string) canonical {
		t.Helper()
		s, text, err := srv18.Canonical(ctx, sql, nil)
		if err != nil {
			if strings.Contains(err.Error(), "server version mismatch") {
				t.Skipf("pg_dump 18 not on PATH (nix-shell -p postgresql_18): %v", err)
			}
			t.Fatalf("canonical: %v", err)
		}
		in, err := ParseIntents(sql)
		if err != nil {
			t.Fatalf("intents: %v", err)
		}
		return canonical{s, text, in}
	}

	from := canon(`-- sqlshape: postgres 18
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) VIRTUAL);
`)
	to := canon(`-- sqlshape: postgres 18
CREATE TABLE t (id bigint PRIMARY KEY, a integer NOT NULL, b integer GENERATED ALWAYS AS (a * 2) STORED);
`)

	// Ground truth: the schemas genuinely differ.
	if changes := diff.Compare(from.s, to.s); len(changes) == 0 {
		t.Fatal("test setup: expected diff.Compare to see the VIRTUAL -> STORED change")
	}

	plan, err := Plan(from.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) == 0 {
		t.Fatalf("expected a non-empty plan for a VIRTUAL -> STORED generated column kind change; got an empty plan even though the schemas differ")
	}

	got, _, err := srv18.Canonical(ctx, from.text+"\nRESET search_path;\n"+strings.Join(plan, "\n"), to.s)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changes := diff.Compare(got, to.s); len(changes) != 0 {
		t.Errorf("plan did not reach the target: %v", changes)
	}
}
