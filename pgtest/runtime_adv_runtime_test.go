package pgtest_test

// Adversarial tests for lane "runtime" (row mapper, error classification, user-type
// registration, execution paths). See scratchpad/adv/COMMON.md and lane-runtime.md.
//
// Each test below is written to FAIL while the bug it documents is present, and to
// pass once sqlshape is fixed (regression guard). Cases that did not reproduce a real
// bug were removed; see scratchpad/adv/runtime.md for the full write-up.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
)

// A1: docs/runtime.md promises that "a nullable field (pointer, slice, map, sql.Null*,
// pgtype.*) receives NULL as its zero value; a nullable field whose column is absent
// from this expansion's result stays zero (a column only some branches select)."
//
// optionalKind (runtime.go) only treats Pointer / Slice / Map / Interface as tolerant of
// a missing column. sql.NullString and pgtype.Text are structs, not pointers, so a
// struct R with such a field and a query that does not return that column is rejected
// with "has no result column", even though the docs list sql.Null* and pgtype.* as
// nullable kinds that should be allowed to be absent.
func TestAdvRuntime_A1_NullableStructFieldMissingColumn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	type rowWithPgtypeText struct {
		ID   int64
		Note pgtype.Text // docs: nullable kind, column absent from this expansion should be fine
	}
	stmt := sqlshape.Query[rowWithPgtypeText, struct{}](`SELECT 1::bigint AS id`)
	if rows, err := postgres.Collect(ctx, db, stmt, struct{}{}); err != nil {
		t.Errorf("pgtype.Text field with an absent column: docs promise this stays zero, got error: %v", err)
	} else if len(rows) != 1 || rows[0].Note.Valid {
		t.Errorf("pgtype.Text field with an absent column: want zero value, got %+v", rows)
	}

	type rowWithSQLNullString struct {
		ID   int64
		Note sql.NullString // docs: nullable kind (sql.Null*)
	}
	stmt2 := sqlshape.Query[rowWithSQLNullString, struct{}](`SELECT 1::bigint AS id`)
	if rows, err := postgres.Collect(ctx, db, stmt2, struct{}{}); err != nil {
		t.Errorf("sql.NullString field with an absent column: docs promise this stays zero, got error: %v", err)
	} else if len(rows) != 1 || rows[0].Note.Valid {
		t.Errorf("sql.NullString field with an absent column: want zero value, got %+v", rows)
	}
}

// AdvPriceTag is a declared sql.Scanner binding (like PriceTag in sqlshape_test.go,
// bound to the composite type money_amount there): it just keeps whatever text pgx
// hands it. When the column is requested in binary format instead (the stale-OID
// bug below), pgx hands the Scanner the raw wire bytes of the composite's binary
// representation, which do not parse back to the same text money_amount's own text
// codec would have produced.
type AdvPriceTag struct{ Text string }

func (p *AdvPriceTag) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		p.Text = ""
	case string:
		p.Text = v
	case []byte:
		p.Text = string(v)
	default:
		return fmt.Errorf("AdvPriceTag: cannot scan %T", src)
	}
	return nil
}

func (p AdvPriceTag) Value() (driver.Value, error) { return p.Text, nil }

// A2: runtime.go caches, per (R type, rendered SQL), the set of result-column OIDs that
// must be requested in TEXT format for a declared sql.Scanner field (formatCache, keyed
// by formatKey{typ, sql} — see Run's "columns a Scanner receives must come as text"
// branch). The cache key does not include the *type's OID*, only the Go type + SQL text.
//
// If the PostgreSQL type backing that column is dropped and recreated between two runs
// of the *same* Stmt value (its OID changes — schema migration, a domain redefined),
// the second run reuses the first run's stale OID -> text-format mapping. A plain query
// on the same table (no Scanner field, verified separately, no adv test needed) survives
// this fine: pgx's normal statement cache silently reprepares against the new type. But
// forcing an explicit QueryResultFormatsByOID (the formatCache path) makes pgx skip that
// transparent handling, and PostgreSQL's own prepared-plan guard rejects the reused plan
// outright: "ERROR: cached plan must not change result type (SQLSTATE 0A000)" leaks to
// the caller. This is a "vet OK, runtime breaks on a routine schema change that a plain
// pgx query of the same table survives" hole, caused specifically by sqlshape's
// per-(type,SQL) format cache never being invalidated or retried on this error.
func TestAdvRuntime_A2_StaleOIDFormatCacheAfterTypeRecreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	createType := func() {
		if _, err := db.Exec(ctx, `CREATE TYPE adv_money AS (amount integer)`); err != nil {
			t.Fatalf("create type: %v", err)
		}
		if _, err := db.Exec(ctx, `CREATE TABLE adv_amounts (v adv_money)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO adv_amounts (v) VALUES (ROW(42))`); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	createType()

	getAmt := sqlshape.Query[struct{ V AdvPriceTag }, struct{}](`SELECT v FROM adv_amounts`)

	// first run: establishes the text-format request for adv_money's current OID and
	// decodes correctly (the composite's text form, "(42)").
	rows, err := postgres.Collect(ctx, db, getAmt, struct{}{})
	if err != nil || len(rows) != 1 || rows[0].V.Text != "(42)" {
		t.Fatalf("first run (baseline): %v %+v", err, rows)
	}
	baseline := rows[0].V.Text

	// recreate the type: same name, same shape, new OID (as a schema migration would).
	if _, err := db.Exec(ctx, `DROP TABLE adv_amounts`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := db.Exec(ctx, `DROP TYPE adv_money`); err != nil {
		t.Fatalf("drop type: %v", err)
	}
	createType()

	// second run: same Stmt value, same rendered SQL text, but adv_money's OID changed.
	// Expected (per docs/runtime.md's promise that a declared binding "asks PostgreSQL
	// for the text format on those columns, so the Scanner receives the value's text
	// form"): this should still decode to "(42)". The stale format cache instead keeps
	// requesting text format for the OLD OID, which no longer names this column, so pgx
	// falls back to binary for the new OID and AdvPriceTag.Scan is handed the composite's
	// raw binary wire bytes instead of its text form.
	rows2, err := postgres.Collect(ctx, db, getAmt, struct{}{})
	if err != nil {
		t.Fatalf("second run (after type recreation): unexpected error, docs promise text format always: %v", err)
	}
	if len(rows2) != 1 || rows2[0].V.Text != baseline {
		t.Errorf("second run (after type recreation): stale format cache served binary to a text-only Scanner, want V.Text=%q, got %+v", baseline, rows2)
	}
}

// A3: docs/runtime.md documents that a custom SQLSTATE the template's `-- sqlshape:
// expect` line names (a trigger's RAISE ... USING ERRCODE, e.g. insertOrder's P0401 in
// sqlshape_test.go) is wrapped into *ConstraintError the same way a class-23 violation
// is, and Batch's own doc comment on Send repeats the promise: "constraint violations
// mapped to ConstraintError like Run does".
//
// This holds for a batched statement that returns rows (Query branch: rows.Err() is
// wrapped with the statement's own `expects`, verified separately, not reproduced as a
// bug). It does NOT hold for the Exec-branch of Queue (R = struct{}, no RETURNING):
// batch.go's Exec-branch callback (`qq.Exec(func(tag pgconn.CommandTag) error { ...
// return nil })`) never captures the tag or an error, and never runs at all on failure,
// so a failing Exec-branch statement's error reaches pgx's batch result purely through
// br.Close() in Batch.Send, which always maps with wrapPgErr(err, nil) (nil expects) —
// so an Exec-branch statement's expect-line SQLSTATE is never recognized in a batch,
// only class-23 violations are. Worse: since the callback never ran, q.done stays
// false, so the queued handle's Tag() reports ErrNotSent ("sqlshape: batch not sent")
// rather than the real error at all.
var advUpdateTotal = sqlshape.Query[struct{}, struct{ ID int64 }](`
-- sqlshape: expect P0401
UPDATE orders SET total = 2000000 WHERE id = {{.ID}}`)

func TestAdvRuntime_A3_BatchExecBranchDoesNotMapExpectLineSQLSTATE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO users (email, name) VALUES ('a@x', 'A')`); err != nil {
		t.Fatal(err)
	}
	id, err := postgres.First(ctx, db, insertOrder, NewOrder{UserID: 1, Total: "10.00"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// baseline, outside a batch: Violates(err, "P0401") holds via Exec's own wrapErr.
	_, err = postgres.Exec(ctx, db, advUpdateTotal, struct{ ID int64 }{id})
	if !postgres.Violates(err, "P0401") {
		t.Fatalf("baseline (non-batch) Exec P0401 mapping: want Violates to hold, got %v", err)
	}

	b := postgres.NewBatch()
	q := postgres.Queue(b, advUpdateTotal, struct{ ID int64 }{id})
	sendErr := b.Send(ctx, db)
	if sendErr == nil {
		t.Fatal("batch: want the trigger's error, got nil")
	}
	if !postgres.Violates(sendErr, "P0401") {
		t.Errorf("batch (Exec branch): Send's error is not mapped to ConstraintError(P0401) as docs promise: %v", sendErr)
	}
	if _, qErr := q.Tag(); !postgres.Violates(qErr, "P0401") {
		t.Errorf("batch (Exec branch): queued handle's error is not mapped to ConstraintError(P0401) as docs promise: %v", qErr)
	}
}
