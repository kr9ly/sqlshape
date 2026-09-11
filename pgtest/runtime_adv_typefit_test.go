package pgtest_test

// Adversarial-testing lane "typefit" (x/dialect.Type / GoFit / gofit.go / typebind.go /
// check/postgres/dialect/types.go: the Go types vet accepts against what pgx actually
// scans / encodes).
//
// cmd/sqlshape/internal/vet's TestAdvTypefit (internal/vet/adv_typefit_test.go, package
// adv_typefit) records the diagnostics the checker should give for the three cases below
// and currently does not; this file reproduces each one against a real running
// PostgreSQL to show the diagnostics are not merely stylistic -- they are cases where
// the type sqlshape accepts silently either errors or (worse) silently corrupts data at
// run time.

import (
	"context"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
)

const advTypefitSchema = `
-- sqlshape: postgres 17
CREATE TYPE item AS (
    sku text,
    qty integer
);

CREATE TABLE t (
    id    bigint PRIMARY KEY,
    tags  integer[],
    items item[],
    span  interval
);
`

// TestAdvTypefit_ScalarArrayNullElement: vet's adv_typefit.ScalarArrayRow (a []int32
// field for an integer[] column) vets clean under -strict -- gofit.go's "[]$elem" branch
// recurses into int32's own fit against integer (an exact match) and never asks whether
// the array itself can hold a NULL regardless of the column's own NOT NULL. PostgreSQL
// never forbids that: ARRAY[1,NULL,3] is a legal value for `integer[]` whether or not the
// column is NOT NULL, since NOT NULL constrains the array value as a whole, not its
// elements. pgx's array codec cannot scan a NULL element into a non-pointer *int32 and
// errors decoding the whole row.
//
// Correct behavior: sqlshape should warn that []int32 (or any non-pointer element slice)
// cannot receive an array with a NULL element, the way it already warns a non-pointer
// scalar field about a nullable column.
func TestAdvTypefit_ScalarArrayNullElement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, advTypefitSchema)
	if err != nil {
		t.Skipf("PostgreSQL oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO t (id, tags) VALUES (1, ARRAY[1, NULL, 3])`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type ScalarArrayRow struct {
		ID   int64
		Tags []int32
	}
	stmt := sqlshape.Query[ScalarArrayRow, struct{}](`SELECT id, tags FROM t WHERE id = 1`)
	rows, err := postgres.Collect(ctx, db, stmt, struct{}{})
	if err == nil {
		t.Fatalf("want a scan error for a NULL array element into []int32 (sqlshape.vet lets this Go type through unremarked), got rows=%+v", rows)
	}
	t.Logf("PostgreSQL/pgx correctly refuses at run time what vet accepted at check time: %v", err)
}

// TestAdvTypefit_CompositeArrayNullElement: vet's adv_typefit.CompositeArrayRow (a
// []Item field for an item[] column, item a composite type) has the identical static
// gap, but the runtime failure mode is worse: pgx does not error on a NULL composite
// array element, it silently decodes it as a zero-valued Item{} -- indistinguishable
// from a real all-empty-fields row ROW('', 0)::item. This is not a rejection sqlshape
// merely fails to warn about; it is silent data corruption sqlshape's type acceptance
// gives no signal for at all.
//
// Correct behavior: sqlshape should warn (at least as strongly as for the scalar case)
// that a NULL element of a composite array is received as a false zero value.
func TestAdvTypefit_CompositeArrayNullElement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, advTypefitSchema)
	if err != nil {
		t.Skipf("PostgreSQL oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO t (id, items) VALUES (1, ARRAY[ROW('a', 1)::item, NULL, ROW('b', 2)::item])`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type Item struct {
		Sku string
		Qty int32
	}
	type CompositeArrayRow struct {
		ID    int64
		Items []Item
	}
	stmt := sqlshape.Query[CompositeArrayRow, struct{}](`SELECT id, items FROM t WHERE id = 1`)
	rows, err := postgres.Collect(ctx, db, stmt, struct{}{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Items) != 3 {
		t.Fatalf("want 1 row with 3 items, got %+v", rows)
	}
	got := rows[0].Items[1]
	want := Item{} // the real value is SQL NULL, not ("", 0)
	if got != want {
		t.Fatalf("unexpected decode of the NULL element: %+v", got)
	}
	// The point of the test: this zero value is byte-for-byte indistinguishable from a
	// real ROW('', 0)::item, which item[] can just as legally hold -- []Item silently
	// loses the fact that the element was ever NULL, and vet's type acceptance for
	// []Item gives no warning that this can happen.
	t.Logf("NULL item element silently decoded as the zero value %+v: sqlshape.vet accepted []Item for item[] with no warning that a NULL element is indistinguishable from a real empty row", got)
}

// TestAdvTypefit_IntervalCalendar: vet's adv_typefit.IntervalRow (a time.Duration field
// for an interval column) vets clean under -strict with no Advice note, unlike every
// other zone- or precision-losing GoFit (timestamp without time zone, date,
// numeric->float64, ...). PostgreSQL's interval keeps months, days and microseconds as
// separate components and applies them to a calendar date one component at a time; pgx's
// Duration codec instead flattens interval to a fixed months*30days + days*24h +
// microseconds. This test shows the flattened value materially disagrees with
// PostgreSQL's own calendar arithmetic on the very same interval: adding `interval '1
// month'` to 2024-02-01 (a leap February, 29 days) lands one calendar day apart between
// PostgreSQL's own date arithmetic and time.Time.Add() on the value sqlshape hands the Go
// side as "the" interval.
func TestAdvTypefit_IntervalCalendar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, advTypefitSchema)
	if err != nil {
		t.Skipf("PostgreSQL oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO t (id, span) VALUES (1, interval '1 month')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type IntervalRow struct {
		ID   int64
		Span time.Duration
	}
	stmt := sqlshape.Query[IntervalRow, struct{}](`SELECT id, span FROM t WHERE id = 1`)
	rows, err := postgres.Collect(ctx, db, stmt, struct{}{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", rows)
	}
	span := rows[0].Span

	var pgResult time.Time
	if err := db.QueryRow(ctx, `SELECT timestamptz '2024-02-01 00:00:00+00' + span FROM t WHERE id = 1`).Scan(&pgResult); err != nil {
		t.Fatalf("PostgreSQL calendar arithmetic: %v", err)
	}
	goResult := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC).Add(span)

	if pgResult.Equal(goResult) {
		t.Fatalf("expected PostgreSQL's own calendar arithmetic to disagree with time.Time.Add(span) across a leap February, but both landed on %v (span=%v)", pgResult, span)
	}
	t.Logf("PostgreSQL: 2024-02-01 + interval '1 month' = %v; Go: 2024-02-01 + time.Duration(%v) (what sqlshape hands the application as the same interval) = %v -- sqlshape.vet gives no note that time.Duration cannot carry a calendar interval faithfully", pgResult, span, goResult)
}
