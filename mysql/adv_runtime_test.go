package mysql_test

// Adversarial pass 1 (runtime lane): probes mysql/v2 against a real mysqld, looking for
// statements where the runtime's judgment (row-count reading of ExecOne / Find / Get,
// ConstraintError parsing, row mapping, `$n` -> `?` argument ordering) disagrees with what
// the server actually does. See scratchpad/adv-mysql-brief.md for the rules this pass
// follows. Findings are written as tests that state the CORRECT behaviour and currently
// fail against mysql/v2 as it stands today.

import (
	"context"
	"errors"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

const advUpsertSchema = `-- sqlshape: mysql 8.4
CREATE TABLE counters (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  v INT NOT NULL
);
`

var advUpsert = sqlshape.One[struct{}, struct {
	ID uint64
	V  int
}](`INSERT INTO counters (id, v) VALUES ({{.ID}}, {{.V}}) ON DUPLICATE KEY UPDATE v = {{.V}}`)

var advReplace = sqlshape.One[struct{}, struct {
	ID uint64
	V  int
}](`REPLACE INTO counters (id, v) VALUES ({{.ID}}, {{.V}})`)

func advStart(t *testing.T, schemaSQL string) (context.Context, mysql.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, schemaSQL)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db.Conn()
}

// A statement declared with One and run through ExecOne is, by construction, aimed at
// exactly one logical row — the checker only lets One through when it can prove that. For
// `INSERT ... ON DUPLICATE KEY UPDATE`, that logical row is the one the unique key names,
// whether the statement ends up inserting it or updating it in place: from the caller's
// point of view this is always a single-row upsert that succeeded.
//
// MySQL's RowsAffected does not agree: it reports 1 for a fresh insert, 2 for a row that
// existed and was changed, and 0 for a row that existed and was left unchanged (the
// values already matched). ExecOne reads RowsAffected literally (runtime.go: n==0 ->
// ErrNoRows, n>1 -> ErrManyRows), so an upsert that updates an existing row is
// misdiagnosed as "the checker's One proof did not hold" (ErrManyRows), and a upsert that
// changes nothing is misdiagnosed as "the key did not exist" (ErrNoRows) — both wrong for
// a statement that in fact touched the single row its key identifies every time.
func TestAdvExecOneUpsertIsAlwaysOneRow(t *testing.T) {
	ctx, db := advStart(t, advUpsertSchema)

	// fresh insert: RowsAffected == 1, ExecOne correctly reports success.
	if _, err := mysql.ExecOne(ctx, db, advUpsert, struct {
		ID uint64
		V  int
	}{1, 10}); err != nil {
		t.Fatalf("insert leg: %v", err)
	}

	// existing row, changed value: server reports RowsAffected == 2 for this single-row
	// upsert. ExecOne must not report ErrManyRows here — exactly one row (id=1) was
	// touched.
	if _, err := mysql.ExecOne(ctx, db, advUpsert, struct {
		ID uint64
		V  int
	}{1, 20}); err != nil {
		if errors.Is(err, mysql.ErrManyRows) {
			t.Errorf("update leg: ExecOne reported ErrManyRows for a single-row upsert (server RowsAffected=2 for a changed row): %v", err)
		} else {
			t.Fatalf("update leg: %v", err)
		}
	}

	// existing row, same value: server reports RowsAffected == 0 (nothing to change).
	// ExecOne must not report ErrNoRows here — the row (id=1) exists and was targeted;
	// the upsert simply had nothing to change.
	if _, err := mysql.ExecOne(ctx, db, advUpsert, struct {
		ID uint64
		V  int
	}{1, 20}); err != nil {
		if errors.Is(err, mysql.ErrNoRows) {
			t.Errorf("no-op leg: ExecOne reported ErrNoRows for a single-row upsert whose key exists (server RowsAffected=0 for an unchanged row): %v", err)
		} else {
			t.Fatalf("no-op leg: %v", err)
		}
	}
}

// Same misdiagnosis for REPLACE INTO: replacing an existing row deletes then reinserts
// it, so the server reports RowsAffected == 2 even though exactly one logical row (the
// one the primary/unique key names) was replaced.
func TestAdvExecOneReplaceIsAlwaysOneRow(t *testing.T) {
	ctx, db := advStart(t, advUpsertSchema)

	if _, err := mysql.ExecOne(ctx, db, advReplace, struct {
		ID uint64
		V  int
	}{1, 10}); err != nil {
		t.Fatalf("fresh replace: %v", err)
	}

	if _, err := mysql.ExecOne(ctx, db, advReplace, struct {
		ID uint64
		V  int
	}{1, 20}); err != nil {
		if errors.Is(err, mysql.ErrManyRows) {
			t.Errorf("replace of an existing row: ExecOne reported ErrManyRows (server RowsAffected=2 for delete+reinsert of one logical row): %v", err)
		} else {
			t.Fatalf("replace of an existing row: %v", err)
		}
	}
}
