package analyze

// Adversarial round 3 (PostgreSQL), lane: violations.
//
// Each test below was reproduced against a real, running PostgreSQL (embedded-postgres,
// via check/postgres/v2/oracle) before being written down here as a "correct behaviour"
// expectation, so every one of them currently fails: sqlshape's failure-mode enumeration
// (check/postgres/analyze/violation.go, docs/checks.md "Preparing for a write to fail")
// disagrees with what the server actually does for the statement in question. See the
// PG transcript embedded in each test's comment for the exact server answer.
//
// Scope: Result.Violations only (the expect-line contract). Findings that are about a PG
// *error* sqlshape fails to raise at all (rather than about the violation set) belong to
// the semantic lane; the four cases below are kept here because they are all about
// violation.go's own bookkeeping (the ON CONFLICT "absorbed" set, and the missing
// TRUNCATE case in the top-level violations() switch).

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
)

// advViolationKeys runs sql through the analyzer and returns its declared violations as
// sorted "code key" strings, the same shape violation_test.go uses.
func advViolationKeys(t *testing.T, schemaSQL, sql string) []string {
	t.Helper()
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var got []string
	for _, v := range r.Violations {
		got = append(got, v.Code+" "+v.Key())
	}
	sort.Strings(got)
	return got
}

// advStartOracle starts a real PostgreSQL loaded with schemaSQL, skipping the test if the
// embedded server cannot be started (offline / no cached binary).
func advStartOracle(t *testing.T, ctx context.Context, schemaSQL string) *oracle.Oracle {
	t.Helper()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Skipf("oracle start: %v", err)
	}
	return o
}

// TestAdvViolationsTruncateBlockedByForeignKeyIsUnreported: TRUNCATE has no case at all in
// violations() (check/postgres/analyze/violation.go, the switch in (*analyzer).violations),
// so a bare `TRUNCATE p` is reported with an empty violation set -- as if it always
// succeeds. But real PostgreSQL refuses to truncate a table that is still referenced, by
// an enforced foreign key, from a table not named in the same TRUNCATE statement (and not
// reached by CASCADE): this is not a maybe -- with a referencing row-independent, purely
// structural check, it fails on every single execution, the same way `INSERT` into a
// NOT NULL column without a default always fails (violation.go already flags that one
// with a Note). Reproduced on a running PG 17:
//
//	CREATE TABLE p (id int PRIMARY KEY);
//	CREATE TABLE c (id int PRIMARY KEY, pid int REFERENCES p(id));
//	TRUNCATE p;
//	-- ERROR:  cannot truncate a table referenced in a foreign key constraint (SQLSTATE 0A000)
//	-- DETAIL:  Table "c" references "p".
//	TRUNCATE p, c;        -- succeeds: the referencing table is truncated together with it
//	TRUNCATE p CASCADE;   -- succeeds: CASCADE truncates every table that references p too
//
// sqlshape's answer for the plain `TRUNCATE p` today: zero violations, zero notes -- no
// signal at all that the statement is certain to fail.
func TestAdvViolationsTruncateBlockedByForeignKeyIsUnreported(t *testing.T) {
	const schemaSQL = `-- sqlshape: postgres 17
CREATE TABLE p (id int PRIMARY KEY);
CREATE TABLE c (id int PRIMARY KEY, pid int REFERENCES p(id));
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := advStartOracle(t, ctx, schemaSQL)
	defer o.Close()
	if _, err := o.Conn().Exec(ctx, "TRUNCATE p"); err == nil {
		t.Fatal("setup: real PG accepted TRUNCATE p while c still references it; test premise is stale")
	} else if !strings.Contains(err.Error(), "0A000") {
		t.Fatalf("setup: expected SQLSTATE 0A000, got %v", err)
	}

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, "TRUNCATE p")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(r.Violations) == 0 && len(r.Notes) == 0 {
		t.Fatalf("TRUNCATE p: sqlshape reports no violation and no note for a statement that always fails (SQLSTATE 0A000, table c references p)")
	}
}

// TestAdvViolationsOnConflictColumnArbiterIgnoresPartialIndexPredicate: violation.go's
// insertViolations absorbs a unique constraint into the ON CONFLICT arbiter by matching
// columns alone (`sameColumns(con.Columns, cols)`), never checking con.Predicate. But a
// partial unique index can only serve as an inference-specification arbiter when the
// ON CONFLICT target's own WHERE clause matches the index predicate; without it, real
// PostgreSQL does not pick the index up as a candidate at all and, finding no unique
// constraint on plain (email), rejects the statement outright. Reproduced on PG 17:
//
//	CREATE TABLE t (id int PRIMARY KEY, email text, active boolean NOT NULL DEFAULT true);
//	CREATE UNIQUE INDEX t_email_active_idx ON t (email) WHERE active;
//	INSERT INTO t (id, email, active) VALUES (1, 'a@x', true);
//	INSERT INTO t (id, email, active) VALUES (2, 'a@x', true) ON CONFLICT (email) DO NOTHING;
//	-- ERROR:  there is no unique or exclusion constraint matching the ON CONFLICT specification (SQLSTATE 42P10)
//	INSERT INTO t (id, email, active) VALUES (2, 'a@x', true) ON CONFLICT (email) WHERE active DO NOTHING;
//	-- succeeds (0 rows): the WHERE clause matches the index predicate, so it is a valid arbiter
//
// sqlshape's answer for the first (always-fails) form today: it silently absorbs
// t_email_active_idx by column match, so no 23505 is listed for it -- and since the
// statement is certain to fail (42P10) rather than certain to insert, that "no violation"
// answer is as wrong as it can be: the checker should at least keep listing the unique
// constraint as unabsorbed, since ON CONFLICT (email) does not actually guard it.
func TestAdvViolationsOnConflictColumnArbiterIgnoresPartialIndexPredicate(t *testing.T) {
	const schemaSQL = `-- sqlshape: postgres 17
CREATE TABLE t (id int PRIMARY KEY, email text, active boolean NOT NULL DEFAULT true);
CREATE UNIQUE INDEX t_email_active_idx ON t (email) WHERE active;
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := advStartOracle(t, ctx, schemaSQL)
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, email, active) VALUES (1, 'a@x', true)"); err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	_, err := conn.Exec(ctx, "INSERT INTO t (id, email, active) VALUES (2, 'a@x', true) ON CONFLICT (email) DO NOTHING")
	if err == nil {
		t.Fatal("setup: real PG accepted the column-only arbiter against a partial unique index; test premise is stale")
	} else if !strings.Contains(err.Error(), "42P10") {
		t.Fatalf("setup: expected SQLSTATE 42P10, got %v", err)
	}

	got := advViolationKeys(t, schemaSQL,
		"INSERT INTO t (id, email, active) VALUES ($1, $2, $3) ON CONFLICT (email) DO NOTHING")
	const want = "23505 t_email_active_idx"
	found := false
	for _, k := range got {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Errorf("ON CONFLICT (email) DO NOTHING with no WHERE against a partial unique index: sqlshape absorbed t_email_active_idx via column match alone (ignoring the predicate); got violations %v, want %q to remain listed (or the statement flagged as always-failing)", got, want)
	}
}

// TestAdvViolationsOnConflictColumnArbiterIgnoresDeferrable: the same absorbed-by-column
// path never checks con.Deferrable either. PostgreSQL refuses to use a DEFERRABLE unique
// (or exclusion) constraint as an ON CONFLICT arbiter at all, unconditionally --
// reproduced on PG 17:
//
//	CREATE TABLE t (id int PRIMARY KEY, email text,
//	  CONSTRAINT t_email_key UNIQUE (email) DEFERRABLE INITIALLY IMMEDIATE);
//	INSERT INTO t (id, email) VALUES (1, 'a@x');
//	INSERT INTO t (id, email) VALUES (2, 'a@x') ON CONFLICT (email) DO NOTHING;
//	-- ERROR:  ON CONFLICT does not support deferrable unique constraints/exclusion constraints as arbiters (SQLSTATE 55000)
//
// sqlshape's answer today: t_email_key is absorbed by column match, so the statement is
// reported with no 23505 for it at all, though the statement can never even reach the
// point of inserting or skipping a row.
func TestAdvViolationsOnConflictColumnArbiterIgnoresDeferrable(t *testing.T) {
	const schemaSQL = `-- sqlshape: postgres 17
CREATE TABLE t (id int PRIMARY KEY, email text,
  CONSTRAINT t_email_key UNIQUE (email) DEFERRABLE INITIALLY IMMEDIATE);
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := advStartOracle(t, ctx, schemaSQL)
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, email) VALUES (1, 'a@x')"); err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	_, err := conn.Exec(ctx, "INSERT INTO t (id, email) VALUES (2, 'a@x') ON CONFLICT (email) DO NOTHING")
	if err == nil {
		t.Fatal("setup: real PG accepted a deferrable unique constraint as an ON CONFLICT arbiter; test premise is stale")
	} else if !strings.Contains(err.Error(), "55000") {
		t.Fatalf("setup: expected SQLSTATE 55000, got %v", err)
	}

	got := advViolationKeys(t, schemaSQL,
		"INSERT INTO t (id, email) VALUES ($1, $2) ON CONFLICT (email) DO NOTHING")
	const want = "23505 t_email_key"
	found := false
	for _, k := range got {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Errorf("ON CONFLICT (email) DO NOTHING against a DEFERRABLE unique constraint: sqlshape absorbed t_email_key via column match alone (ignoring Deferrable); got violations %v, want %q to remain listed (or the statement flagged as always-failing)", got, want)
	}
}

// TestAdvViolationsOnConflictDoUpdateWithExcludeArbiterAlwaysFails: PostgreSQL never
// allows ON CONFLICT ... DO UPDATE when the arbiter is an EXCLUDE constraint (only
// DO NOTHING is supported there) -- unconditionally, regardless of any row's data.
// Reproduced on PG 17 (btree_gist for the exclusion constraint):
//
//	CREATE EXTENSION btree_gist;
//	CREATE TABLE t (id int PRIMARY KEY, room int NOT NULL, during int4range NOT NULL,
//	  CONSTRAINT t_room_excl EXCLUDE USING gist (room WITH =, during WITH &&));
//	INSERT INTO t VALUES (1, 1, int4range(1,5));
//	INSERT INTO t VALUES (2, 1, int4range(3,8))
//	  ON CONFLICT ON CONSTRAINT t_room_excl DO UPDATE SET during = excluded.during;
//	-- ERROR:  ON CONFLICT DO UPDATE not supported with exclusion constraints (SQLSTATE 42809)
//
// sqlshape's answer today: it treats the statement as an ordinary absorbed-arbiter
// ON CONFLICT DO UPDATE and happily reports the UPDATE's own violation set (NOT NULL /
// t_pkey) as if the write could succeed -- no note, no error, nothing marking the
// statement as certain to fail on every execution.
func TestAdvViolationsOnConflictDoUpdateWithExcludeArbiterAlwaysFails(t *testing.T) {
	const schemaSQL = `-- sqlshape: postgres 17
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE t (id int PRIMARY KEY, room int NOT NULL, during int4range NOT NULL,
  CONSTRAINT t_room_excl EXCLUDE USING gist (room WITH =, during WITH &&));
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := advStartOracle(t, ctx, schemaSQL)
	defer o.Close()
	conn := o.Conn()
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, room, during) VALUES (1, 1, int4range(1,5))"); err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	_, err := conn.Exec(ctx, "INSERT INTO t (id, room, during) VALUES (2, 1, int4range(3,8)) "+
		"ON CONFLICT ON CONSTRAINT t_room_excl DO UPDATE SET during = excluded.during")
	if err == nil {
		t.Fatal("setup: real PG accepted ON CONFLICT DO UPDATE with an exclusion-constraint arbiter; test premise is stale")
	} else if !strings.Contains(err.Error(), "42809") {
		t.Fatalf("setup: expected SQLSTATE 42809, got %v", err)
	}

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, "INSERT INTO t (id, room, during) VALUES ($1, $2, $3) "+
		"ON CONFLICT ON CONSTRAINT t_room_excl DO UPDATE SET during = excluded.during")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(r.Notes) == 0 {
		t.Errorf("ON CONFLICT ON CONSTRAINT <exclusion> DO UPDATE: sqlshape gives no signal that the statement always fails (SQLSTATE 42809); violations=%v notes=%v", r.Violations, r.Notes)
	}
}
