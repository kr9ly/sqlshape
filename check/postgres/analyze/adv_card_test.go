package analyze

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
)

// readSchema reads the shared testdata/schema.sql (users / orders / order_items, with the
// partial unique index orders_uid_active) that card_test.go and this file both analyze.
func readSchema() (string, error) {
	b, err := os.ReadFile("testdata/schema.sql")
	return string(b), err
}

// Adversarial round 3 (PostgreSQL), lane "card": the soundness of x/cardinality.AtMostOne
// over the facts check/postgres/analyze produces (keyFacts, groupTerm, predFacts). Every
// test below is reproduced against a real, running PostgreSQL (embedded-postgres, via
// oracle.Start) and states the *correct* expected outcome, so it fails today.
//
// All findings here are the "conservative" direction the lane brief calls mid severity:
// sqlshape refuses to prove AtMostOne for a statement that a real PostgreSQL always
// answers with at most one row (it never wrongly claims AtMostOne for a statement that
// can return two). Each test pins that down two ways: the analyzer's own verdict (which
// is wrong) and a live query against real data (which shows the true, single-row answer).

// checkOneRow loads schemaSQL, analyzes sql, asserts the analyzer proves AtMostOne (the
// documented "correct" outcome), then runs inserts and the query itself (with args) against
// a real PostgreSQL and asserts it returns at most one row -- the independent, ground-truth
// half of the claim.
func checkOneRow(t *testing.T, schemaSQL string, inserts []string, sql string, args ...any) {
	t.Helper()
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	if !r.AtMostOne {
		t.Errorf("AtMostOne: want true, got false (%s)", r.ManyRowsWhy)
	} else if r.ManyRowsWhy != "" {
		t.Errorf("why should be empty when AtMostOne: %q", r.ManyRowsWhy)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatalf("oracle start: %v", err)
	}
	defer o.Close()
	conn := o.Conn()
	for _, ins := range inserts {
		if _, err := conn.Exec(ctx, ins); err != nil {
			t.Fatalf("insert %q: %v", ins, err)
		}
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("query returned no rows; cannot demonstrate the single-row answer")
	}
	if n > 1 {
		t.Errorf("real PostgreSQL returned %d rows, not at most one -- the finding above is not what we thought", n)
	}
}

// TestAdvCardGroupByCastOfPinnedColumn: a column fixed to a known value by WHERE equality
// still identifies a single group when GROUP BY names a cast of it (or an alias for that
// cast), not just the bare column. sqlshape's groupTerm resolves a GROUP BY item to the
// underlying column only for a bare ColumnRef (card.go prover.resolve); wrapped in a
// TypeCast it falls through to an opaque Expr term, which the proof never treats as
// pinned even though the column beneath it is. Reproduced: one order with status =
// 'paid'; grouping by status::text (equivalently, by its alias) still yields the single
// group PostgreSQL actually returns.
func TestAdvCardGroupByCastOfPinnedColumn(t *testing.T) {
	schemaSQL, err := readSchema()
	if err != nil {
		t.Fatal(err)
	}
	checkOneRow(t, schemaSQL, []string{
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')",
		"INSERT INTO orders (user_id, status, total) VALUES (1, 'paid', 10)",
	}, "SELECT status::text, count(*) FROM orders WHERE status = 'paid' GROUP BY status::text")
}

// TestAdvCardGroupByAliasOfCastOfPinnedColumn: the same gap, reached through a SELECT-list
// alias that names the cast expression rather than the cast written out again in GROUP BY.
func TestAdvCardGroupByAliasOfCastOfPinnedColumn(t *testing.T) {
	schemaSQL, err := readSchema()
	if err != nil {
		t.Fatal(err)
	}
	checkOneRow(t, schemaSQL, []string{
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')",
		"INSERT INTO orders (id, user_id, status, total) OVERRIDING SYSTEM VALUE VALUES (1, 1, 'paid', 10)",
	}, "SELECT id::int AS x, count(*) FROM orders WHERE id = $1 GROUP BY x", int64(1))
}

// TestAdvCardSingletonArrayAny: `col = ANY(ARRAY[$1])`, an array literal with exactly one
// (known) element, is exactly as pinning as `col = $1` -- PostgreSQL never returns more
// than one row for it against a unique key. sqlshape's alternatives()/predFacts only
// special-cases `IN (...)` and `x = a OR x = b OR ...`, not `= ANY(ARRAY[...])`, so it
// falls back to treating the whole comparison as unknown and refuses the proof, the same
// way it (correctly) refuses `id = ANY($1)` where the whole array is an opaque parameter.
func TestAdvCardSingletonArrayAny(t *testing.T) {
	schemaSQL, err := readSchema()
	if err != nil {
		t.Fatal(err)
	}
	checkOneRow(t, schemaSQL, []string{
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')",
	}, "SELECT id FROM users WHERE id = ANY(ARRAY[$1::bigint])", int64(1))
}

// TestAdvCardIsNotDistinctFromKnownValue: `col IS NOT DISTINCT FROM $1` pins col exactly
// like `col = $1` whenever the bound value is not NULL (the two predicates agree on every
// row for a non-NULL right-hand side). sqlshape's equalitySides only recognizes plain `=`
// (and a one-item IN), so a lookup written with IS NOT DISTINCT FROM -- a common pattern
// for a nullable filter column -- is never treated as pinning a unique key.
func TestAdvCardIsNotDistinctFromKnownValue(t *testing.T) {
	schemaSQL, err := readSchema()
	if err != nil {
		t.Fatal(err)
	}
	checkOneRow(t, schemaSQL, []string{
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')",
	}, "SELECT id FROM users WHERE email IS NOT DISTINCT FROM $1", "a@x")
}

// TestAdvCardPartialIndexPredicateExplicitCast: a partial unique index's predicate and the
// statement's own WHERE conjunct restrict exactly the same rows even when the statement
// spells its enum literal with an explicit cast the index's predicate text does not carry
// (`status <> 'cancelled'::order_status` vs. the index's `status <> 'cancelled'`).
// sqlshape's samePred (x/cardinality via check/postgres/analyze's Opaque predicate) compares
// the two predicates by their deparsed text, which differs with the cast present, so the
// partial index `orders_uid_active ON orders (uid) WHERE status <> 'cancelled'` is not
// recognized as repeated and the proof is refused even though PostgreSQL enforces -- and
// this statement restricts to -- exactly the same one row.
func TestAdvCardPartialIndexPredicateExplicitCast(t *testing.T) {
	schemaSQL, err := readSchema()
	if err != nil {
		t.Fatal(err)
	}
	checkOneRow(t, schemaSQL, []string{
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')",
		"INSERT INTO orders (id, user_id, status, total, uid) OVERRIDING SYSTEM VALUE " +
			"VALUES (1, 1, 'pending', 10, '00000000-0000-0000-0000-000000000001')",
	}, "SELECT id FROM orders WHERE uid = $1::uuid AND status <> 'cancelled'::order_status",
		"00000000-0000-0000-0000-000000000001")
}
