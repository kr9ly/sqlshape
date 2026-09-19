package analyze

// Adversarial tests for the "semantic" lane, round 3 (2.0 layers): PG errors sqlshape
// fails to raise (or does not raise in the same way) -- name resolution across ORDER
// BY / GROUP BY aliases, ambiguous-reference diagnosis, message wording, and Position
// accuracy. Each case below was reproduced against a real embedded PostgreSQL 17
// (check/postgres/oracle) before being written down as a failing regression test: the
// assertion encodes the *correct* behaviour, so the test currently fails and turns
// green once the underlying gap is fixed.
//
// Run with:
//   go test ./check/postgres/analyze -run TestAdvSemantic3 -count=1 -v

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
)

// TestAdvSemantic3OrderByAmbiguousAliasAccepted: two output columns can carry the same
// alias (PG only rejects a *duplicate alias* when something must resolve it against
// that name later). ORDER BY by that name is then genuinely ambiguous -- PG raises
// 42702 ("ORDER BY \"id\" is ambiguous") -- but the analyzer's ORDER BY name resolution
// (resolve.go) picks the first matching output column and returns no error at all.
// This is a false acceptance: vet says the statement is fine, PostgreSQL refuses it
// outright (it never even reaches execution).
func TestAdvSemantic3OrderByAmbiguousAliasAccepted(t *testing.T) {
	const schemaSQL = `
CREATE TABLE t1 (id int PRIMARY KEY, v int);
`
	const query = `SELECT v AS id, id AS id FROM t1 ORDER BY id`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	res, aerr := Analyze(s, query)
	// Bug: the analyzer accepts this statement (res != nil, aerr == nil). Correct
	// behaviour: refuse it the way PG does, with 42702 "ORDER BY ... is ambiguous".
	if aerr == nil {
		t.Fatalf("analyzer accepted %q as unambiguous (res=%+v); PostgreSQL rejects it: ORDER BY \"id\" is ambiguous, since two output columns are named \"id\"", query, res)
	}
	if pe, ok := aerr.(*Error); !ok || pe.Code != "42702" {
		t.Errorf("expected 42702 (ambiguous), got %v", aerr)
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
	_, derr := o.Describe(ctx, query)
	if derr == nil {
		t.Fatal("expected real PostgreSQL to reject the ambiguous ORDER BY reference; it accepted the statement -- the premise of this test no longer holds, re-check it")
	}
	if pe, ok := derr.(*oracle.PgError); !ok || pe.Code != "42702" || !strings.Contains(pe.Message, "ambiguous") {
		t.Fatalf("expected PostgreSQL to raise 42702 \"... is ambiguous\", got %v", derr)
	}
	t.Logf("real PostgreSQL rejected the query sqlshape accepts: %v", derr)
}

// TestAdvSemantic3HavingMessageOmitsTableQualifier: a HAVING clause without GROUP BY
// that references a plain table column is rejected by both sides with 42803, but PG
// qualifies the offending column with its table in the message ("column \"t1.v\" must
// appear in the GROUP BY clause..."), while the analyzer's message drops the
// qualifier ("column \"v\" must ..."). Anyone matching sqlshape's diagnostic text
// against PG's own wording (docs, tooling, tests copied from a real error) sees a
// different string for the same failure.
func TestAdvSemantic3HavingMessageOmitsTableQualifier(t *testing.T) {
	const schemaSQL = `
CREATE TABLE t1 (id int PRIMARY KEY, v int);
`
	const query = `SELECT count(*) FROM t1 HAVING v > 0`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(s, query)
	pe, ok := aerr.(*Error)
	if !ok || pe.Code != "42803" {
		t.Fatalf("expected 42803, got %v", aerr)
	}
	// Bug: message says `column "v" must appear ...` instead of `column "t1.v" must
	// appear ...` (PG always qualifies the column with its table in this message).
	if !strings.Contains(pe.Message, `"t1.v"`) {
		t.Errorf("analyzer's 42803 message does not qualify the column with its table: %q; PostgreSQL says column \"t1.v\" must appear in the GROUP BY clause or be used in an aggregate function", pe.Message)
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
	_, derr := o.Describe(ctx, query)
	dpe, ok := derr.(*oracle.PgError)
	if !ok || dpe.Code != "42803" {
		t.Fatalf("expected real PostgreSQL to reject with 42803, got %v", derr)
	}
	if !strings.Contains(dpe.Message, `"t1.v"`) {
		t.Fatalf("expected PostgreSQL's message to name \"t1.v\"; got %q -- the premise of this test no longer holds, re-check it", dpe.Message)
	}
	t.Logf("real PostgreSQL's message: %q (analyzer's: %q)", dpe.Message, pe.Message)
}

// TestAdvSemantic3GroupByDuplicateAliasWrongDiagnosis: two output columns sharing one
// alias (`v AS q, id AS q`) make `GROUP BY q` genuinely ambiguous -- PostgreSQL raises
// 42702 ("GROUP BY \"q\" is ambiguous") naming the GROUP BY item itself. The analyzer's
// GROUP BY name resolution instead silently binds `q` to the first matching output
// column (`v AS q`) and then reports an unrelated 42803 ("column \"id\" must appear in
// the GROUP BY clause...") about the *other* column carrying that alias -- a different
// SQLSTATE, a different message, and a diagnosis that points at the wrong column
// entirely.
func TestAdvSemantic3GroupByDuplicateAliasWrongDiagnosis(t *testing.T) {
	const schemaSQL = `
CREATE TABLE t1 (id int PRIMARY KEY, v int);
`
	const query = `SELECT v AS q, id AS q FROM t1 GROUP BY q`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(s, query)
	// Bug: the analyzer raises 42803 about column "id", not 42702 about the ambiguous
	// GROUP BY reference "q".
	pe, ok := aerr.(*Error)
	if !ok {
		t.Fatalf("expected an *Error, got %v (%T)", aerr, aerr)
	}
	if pe.Code != "42702" || !strings.Contains(pe.Message, "ambiguous") {
		t.Errorf("analyzer misdiagnoses the duplicate-alias GROUP BY as %s %q; PostgreSQL raises 42702 GROUP BY \"q\" is ambiguous", pe.Code, pe.Message)
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
	_, derr := o.Describe(ctx, query)
	dpe, ok := derr.(*oracle.PgError)
	if !ok || dpe.Code != "42702" || !strings.Contains(dpe.Message, "ambiguous") {
		t.Fatalf("expected real PostgreSQL to raise 42702 \"... is ambiguous\"; got %v -- the premise of this test no longer holds, re-check it", derr)
	}
	t.Logf("real PostgreSQL: %v (analyzer instead: %v)", derr, aerr)
}

// TestAdvSemantic3UndefinedCteRefPositionPicksLaterOccurrence: a CTE name that never
// appears in the enclosing query's FROM is not visible there (PostgreSQL scopes a WITH
// name to statements that actually put it in their FROM / JOIN; referencing it as a
// bare column-qualifier elsewhere is just an undefined table). When the same invalid
// qualifier is used twice, PostgreSQL's parse analysis walks the select list before the
// WHERE clause and reports the *first* occurrence's position. The analyzer instead
// reports the position of the *second* occurrence (in WHERE) -- a real, reproducible
// Position mismatch that would point an editor's error squiggle at the wrong token.
func TestAdvSemantic3UndefinedCteRefPositionPicksLaterOccurrence(t *testing.T) {
	const schemaSQL = `
CREATE TABLE t1 (id int PRIMARY KEY);
`
	const query = `WITH x AS (SELECT id FROM t1) SELECT x.id FROM t1 WHERE id = x.id`

	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(s, query)
	pe, ok := aerr.(*Error)
	if !ok || pe.Code != "42P01" {
		t.Fatalf("expected 42P01 (missing FROM-clause entry), got %v", aerr)
	}
	// Bug: analyzer reports position 62 (the second "x.id", in WHERE); PostgreSQL
	// reports position 38 (the first "x.id", in the SELECT list).
	const wantPos = int32(38)
	if pe.Position != wantPos {
		t.Errorf("analyzer reports Position=%d (the second \"x.id\", in WHERE); PostgreSQL reports the first occurrence at %d", pe.Position, wantPos)
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
	_, derr := o.Describe(ctx, query)
	dpe, ok := derr.(*oracle.PgError)
	if !ok || dpe.Code != "42P01" {
		t.Fatalf("expected real PostgreSQL to reject with 42P01, got %v", derr)
	}
	if dpe.Position != wantPos {
		t.Fatalf("expected PostgreSQL's Position to be %d; got %d -- the premise of this test no longer holds, re-check it", wantPos, dpe.Position)
	}
	t.Logf("real PostgreSQL points at position %d (analyzer instead: %d)", dpe.Position, pe.Position)
}
