package analyze

// Adversarial tests for the "semantic" lane: One's uniqueness proof (card.go) and the
// write failure-mode declaration (violation.go). Each case is reproduced against a real
// embedded PostgreSQL 17 (internal/oracle) before being written down here as a failing
// regression test; the assertion encodes the *correct* behaviour, so the test currently
// fails and turns green once the underlying gap is fixed.
//
// Run with:
//   nix-shell -p postgresql_17 --run "go test ./internal/analyze -run TestAdv -count=1 -v"

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
)

// TestAdvDeferrableUniqueBreaksOne: a DEFERRABLE UNIQUE (or PRIMARY KEY) constraint lets
// two rows share a key value for the lifetime of a transaction whose constraint checks are
// deferred (INITIALLY DEFERRED, or SET CONSTRAINTS ... DEFERRED). sqlshape.One proves "at
// most one row" purely from the presence of a unique key fixed by equality; it never looks
// at whether that key is deferrable, so it certifies as single a lookup that a concurrent
// (or even the same, mid-transaction) session can observe returning two rows. This is a
// false negative: vet says OK, PostgreSQL returns two rows.
func TestAdvDeferrableUniqueBreaksOne(t *testing.T) {
	const schemaSQL = `
CREATE TABLE items (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code text NOT NULL,
    CONSTRAINT items_code_key UNIQUE (code) DEFERRABLE INITIALLY DEFERRED
);
`
	const query = `SELECT id FROM items WHERE code = $1`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(s, query)
	if err != nil {
		t.Fatal(err)
	}
	// Bug: the analyzer currently proves AtMostOne=true for a key that is DEFERRABLE.
	// The correct behaviour is to refuse the proof for a deferrable unique constraint
	// (or at least when the enclosing statement can run with checks deferred), the same
	// way FULL JOIN is refused. This assertion encodes the CORRECT behaviour and fails
	// until the analyzer accounts for Constraint.Deferrable on UNIQUE / PRIMARY KEY.
	if res.AtMostOne {
		t.Errorf("analyzer proved AtMostOne=true for a DEFERRABLE UNIQUE key; want a refusal, since two rows can share \"code\" for the lifetime of a transaction with deferred checks (confirmed against real PostgreSQL below)")
	}

	if os.Getenv("SQLSHAPE_SKIP_ORACLE") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn := o.Conn()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Both rows share code = 'dup'; the UNIQUE check is deferred to COMMIT, so this
	// succeeds within the transaction.
	if _, err := tx.Exec(ctx, `INSERT INTO items (code) VALUES ('dup')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items (code) VALUES ('dup')`); err != nil {
		t.Fatalf("second insert (deferred unique should not fire yet): %v", err)
	}

	rows, err := tx.Query(ctx, query, "dup")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected PostgreSQL to return 2 rows for the deferred-duplicate key (got %d, ids=%v) -- the premise of this test no longer holds, re-check it", len(ids), ids)
	}
	t.Logf("real PostgreSQL returned %d rows for a statement the analyzer certified as at-most-one: ids=%v", len(ids), ids)
}

// TestAdvOnDeleteSetNullHitsNotNull: an FK declared ON DELETE SET NULL against a column
// that is itself NOT NULL is legal DDL (PostgreSQL accepts it at CREATE TIME) but fails at
// run time the moment a referenced parent row is actually deleted: the cascade tries to
// write NULL into a NOT NULL column and PostgreSQL raises 23502. violation.go's
// referencingViolations comment says outright "CASCADE / SET NULL / SET DEFAULT never fail
// here; they fail as the cascaded change" -- but nothing traces that cascaded change to see
// whether *it* can fail, so the DELETE on the parent table never gets this failure mode
// listed and the template's `-- sqlshape: expect` is never asked to declare it.
func TestAdvOnDeleteSetNullHitsNotNull(t *testing.T) {
	const schemaSQL = `
CREATE TABLE parents (id bigint PRIMARY KEY);
CREATE TABLE children (
    id        bigint PRIMARY KEY,
    parent_id bigint NOT NULL REFERENCES parents(id) ON DELETE SET NULL
);
`
	const query = `DELETE FROM parents WHERE id = $1`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(s, query)
	if err != nil {
		t.Fatal(err)
	}
	// Bug: no 23502 violation on children.parent_id is listed for this DELETE, even
	// though the ON DELETE SET NULL action is guaranteed to hit the NOT NULL constraint
	// for any parent that still has children. Correct behaviour: list it (the same way a
	// plain UPDATE ... SET parent_id = NULL would), so `-- sqlshape: expect` can require
	// it to be declared.
	found := false
	for _, v := range res.Violations {
		if v.Code == "23502" && v.Table == "children" && len(v.Columns) == 1 && v.Columns[0] == "parent_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("analyzer lists no NOT NULL violation for children.parent_id on %q, though ON DELETE SET NULL can hit it; violations=%+v", query, res.Violations)
	}

	if os.Getenv("SQLSHAPE_SKIP_ORACLE") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, `INSERT INTO parents VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO children VALUES (10, 1)`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, query, int64(1))
	if err == nil {
		t.Fatal("expected real PostgreSQL to reject the DELETE with a NOT NULL violation; it succeeded -- the premise of this test no longer holds, re-check it")
	}
	t.Logf("real PostgreSQL rejected the undeclared-by-the-analyzer DELETE: %v", err)
}

// TestAdvCascadeChainMissesDownstreamRestrict: ON DELETE CASCADE removes the referencing
// row, but if a further table references *that* row with the default NO ACTION, deleting
// the grandchild also fails -- a genuine consequence of the original DELETE, several hops
// away, that the analyzer never follows. referencingViolations only looks at foreign keys
// that reference the table named in the DELETE directly; it does not recurse into the rows
// a CASCADE removes to see what those removals, in turn, can violate.
func TestAdvCascadeChainMissesDownstreamRestrict(t *testing.T) {
	const schemaSQL = `
CREATE TABLE parents (id bigint PRIMARY KEY);
CREATE TABLE children (
    id        bigint PRIMARY KEY,
    parent_id bigint NOT NULL REFERENCES parents(id) ON DELETE CASCADE
);
CREATE TABLE grandchildren (
    id       bigint PRIMARY KEY,
    child_id bigint NOT NULL REFERENCES children(id)
);
`
	const query = `DELETE FROM parents WHERE id = $1`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(s, query)
	if err != nil {
		t.Fatal(err)
	}
	// Bug: no 23503 violation on grandchildren_child_id_fkey is listed for this DELETE on
	// parents, even though deleting a parent with children that have grandchildren always
	// fails through the cascade. Correct behaviour: the failure modes of a CASCADE delete
	// on children should be folded into the failure modes of the DELETE on parents.
	found := false
	for _, v := range res.Violations {
		if v.Code == "23503" && v.Table == "grandchildren" {
			found = true
		}
	}
	if !found {
		t.Errorf("analyzer lists no FK violation on grandchildren for %q, though the ON DELETE CASCADE to children always reaches it; violations=%+v", query, res.Violations)
	}

	if os.Getenv("SQLSHAPE_SKIP_ORACLE") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, `INSERT INTO parents VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO children VALUES (10, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO grandchildren VALUES (100, 10)`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, query, int64(1))
	if err == nil {
		t.Fatal("expected real PostgreSQL to reject the DELETE via the cascade chain; it succeeded -- the premise of this test no longer holds, re-check it")
	}
	t.Logf("real PostgreSQL rejected the undeclared-by-the-analyzer DELETE: %v", err)
}

// TestAdvExcludeConstraintUnmodeled: EXCLUDE constraints (used for "no two rows overlap"
// invariants: booking ranges, exclusive locks, etc.) are parsed and stored on the relation
// but internal/schema's addTableConstraint explicitly drops them with a "no typing
// consequence" comment, and violation.go has no notion of ConstraintKind for exclusion at
// all. So an INSERT/UPDATE into a table carrying an EXCLUDE constraint never gets that
// failure mode listed, `-- sqlshape: expect` can never be asked to declare 23P01, and
// sqlshape.Violates has nothing to key on for it either.
func TestAdvExcludeConstraintUnmodeled(t *testing.T) {
	const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE reservations (
    id     bigint PRIMARY KEY,
    room   integer NOT NULL,
    during tsrange NOT NULL,
    EXCLUDE USING gist (room WITH =, during WITH &&)
);
`
	const query = `INSERT INTO reservations (id, room, during) VALUES ($1, $2, $3)`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(s, query)
	if err != nil {
		t.Fatal(err)
	}
	// Bug: no 23P01 violation is ever listed for a table with an EXCLUDE constraint.
	found := false
	for _, v := range res.Violations {
		if v.Code == "23P01" {
			found = true
		}
	}
	if !found {
		t.Errorf("analyzer lists no exclusion-constraint violation (23P01) for an INSERT into a table with EXCLUDE USING gist; violations=%+v", res.Violations)
	}

	if os.Getenv("SQLSHAPE_SKIP_ORACLE") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, `INSERT INTO reservations (id, room, during) VALUES (1, 101, tsrange('2026-01-01 10:00','2026-01-01 11:00'))`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO reservations (id, room, during) VALUES (2, 101, tsrange('2026-01-01 10:30','2026-01-01 11:30'))`)
	if err == nil {
		t.Fatal("expected real PostgreSQL to reject the overlapping reservation with an exclusion violation; it succeeded -- the premise of this test no longer holds, re-check it")
	}
	t.Logf("real PostgreSQL rejected the undeclared-by-the-analyzer INSERT: %v", err)
}

// TestAdvExcludeAbsorbedByOnConflict: ON CONFLICT ON CONSTRAINT names the exclusion
// constraint as its arbiter, so the 23P01 it would raise is absorbed like a unique key's.
func TestAdvExcludeAbsorbedByOnConflict(t *testing.T) {
	const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE reservations (
    id     bigint PRIMARY KEY,
    room   integer NOT NULL,
    during tsrange NOT NULL,
    EXCLUDE USING gist (room WITH =, during WITH &&)
);
`
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(s, `INSERT INTO reservations (id, room, during) VALUES ($1, $2, $3)
ON CONFLICT ON CONSTRAINT reservations_room_during_excl DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range res.Violations {
		if v.Code == "23P01" {
			t.Errorf("23P01 listed although ON CONFLICT names the exclusion constraint: %+v", res.Violations)
		}
	}
}

// TestAdvCheckOptionViewCycle: CREATE OR REPLACE can make two views name each other in
// their FROM (PG would refuse the second, the loader does not); the CHECK OPTION walk must
// terminate rather than recurse until the stack overflows.
func TestAdvCheckOptionViewCycle(t *testing.T) {
	const schemaSQL = `
CREATE TABLE base (id int PRIMARY KEY, v int NOT NULL);
CREATE VIEW a AS SELECT id, v FROM base WHERE v > 0 WITH CASCADED CHECK OPTION;
CREATE VIEW b AS SELECT id, v FROM a WHERE v > 1 WITH CASCADED CHECK OPTION;
CREATE OR REPLACE VIEW a AS SELECT id, v FROM b WHERE v > 0 WITH CASCADED CHECK OPTION;
`
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	// the loader refuses the redefinition as PG does (42P17) and keeps the earlier one
	found := false
	for _, p := range s.Problems {
		if strings.Contains(p.String(), "infinite recursion") {
			found = true
		}
	}
	if !found {
		t.Errorf("no problem reported for the cyclic redefinition; problems=%v", s.Problems)
	}
	if _, err := Analyze(s, `INSERT INTO a (id, v) VALUES ($1, $2)`); err != nil {
		t.Errorf("analyze: %v", err)
	}
}
