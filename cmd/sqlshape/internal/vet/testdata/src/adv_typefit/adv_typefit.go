package adv_typefit

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

// Adversarial-testing lane "typefit": each case below is reproduced against a real
// PostgreSQL in pgtest/runtime_adv_typefit_test.go, so this file only records the
// diagnostic the checker should have given and currently does not; it fails until
// gofit.go / types.go grow the check.
//
// This file is checked in the default (non-strict) mode: the array-null-element note is
// standing advice (Advice / Lossy), not a rejection -- []int32 / []Item are still
// accepted. testdata/src/adv_typefit_strict holds the same three cases checked with
// -strict, where the note also becomes an explicit rejection (per the ruling in
// scratchpad/adv-pg3-fix-brief.md, decision 1). "field ID carries key" is a plain-type
// key-binding advisory (binding.go), unrelated to this lane, and is strict-only -- it
// does not appear here.

// PostgreSQL never guarantees an array's elements are themselves not null: a column's
// own NOT NULL only forbids the array value as a whole from being NULL --
// `ARRAY[1,NULL,3]` is a perfectly legal value for an ordinary `integer[]` column, NOT
// NULL or not. `[]int32` for `integer[]` is still accepted here (default mode), but now
// carries a standing note that pgx errors scanning a real NULL element into *int32
// (TestAdvTypefit_ScalarArrayNullElement in pgtest/runtime_adv_typefit_test.go).
type ScalarArrayRow struct {
	ID   int64
	Tags []int32
}

var scalarArray = sqlshape.Query[ScalarArrayRow, struct{}](`SELECT id, tags FROM t`) // want `field Tags: integer\[\] may contain a NULL element even though the column is not NULL; \[\]int32 cannot receive one \(use \[\]\*int32\)`

// The same gap for an array of a composite type is worse: a NULL item element is not
// merely a decode error, pgx silently turns it into a zero-valued Item
// (Item{Sku:"", Qty:0}), indistinguishable from a real all-empty-fields row
// (TestAdvTypefit_CompositeArrayNullElement in pgtest/runtime_adv_typefit_test.go).
type Item struct {
	Sku string
	Qty int32
}

type CompositeArrayRow struct {
	ID    int64
	Items []Item
}

var compositeArray = sqlshape.Query[CompositeArrayRow, struct{}](`SELECT id, items FROM t`) // want `field Items\.Sku is string but column "sku" may be NULL` `field Items\.Qty is int32 but column "qty" may be NULL` `field Items: item\[\] may contain a NULL element even though the column is not NULL; \[\]adv_typefit\.Item silently receives it as a zero-valued Item with no error \(use \[\]\*adv_typefit\.Item\)`

// interval keeps months, days and microseconds as separate components and PostgreSQL
// applies them to a calendar date one at a time; pgx's Duration codec instead flattens
// interval to a fixed months*30days + days*24h + microseconds, the same as any other
// non-faithful GoFit (numeric->float64, ...): a standing Lossy note, in both modes
// (TestAdvTypefit_IntervalCalendar in pgtest/runtime_adv_typefit_test.go: adding
// `interval '1 month'` to 2024-02-01, a leap February, lands one day apart between
// PostgreSQL and time.Time.Add(span)).
type IntervalRow struct {
	ID   int64
	Span time.Duration
}

var intervalRow = sqlshape.Query[IntervalRow, struct{}](`SELECT id, span FROM t`) // want `field Span is time\.Duration but column "span" may be NULL` `field Span: interval into time\.Duration approximates months as 30 days; PostgreSQL's own calendar arithmetic on the same interval can land on a different day`
