package dialect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// Second adversarial round (2026-09, since commit a2875e5): the MySQL adapter's facts for
// the obligation checker (require pinned / immutable / never / single, visible where),
// through the surface that grew after round 1 -- CALL, triggers, stored routines, and the
// MySQL-only statement shapes (INSERT ... ON DUPLICATE KEY UPDATE, REPLACE) round 1 never
// exercised. Every finding here is a case where the fact the MySQL producer hands to
// x/obligation does not match what mysqld 8.4.11 actually does.

// adv2Load is loadContract's twin for a schema local to this file (the shared testSchema in
// dialect_test.go has no ON DUPLICATE KEY UPDATE target, no non-numeric primary key and no
// trigger, which the findings below all need).
func adv2Load(t *testing.T, schemaSQL string) *mysql {
	t.Helper()
	an, err := load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	return m
}

const adv2DupKeySchema = `-- sqlshape: mysql 8.4
CREATE TABLE tenants (id BIGINT UNSIGNED NOT NULL PRIMARY KEY);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(10) NOT NULL,
  CONSTRAINT fk_orders_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);
`

// TestAdv2OnDuplicateKeyUpdateBypassesPinned: `require pinned(tenant_id)` exists so that
// every statement on orders is trusted to touch exactly one tenant's rows. x/obligation
// already has a case built for exactly this shape -- an INSERT whose conflict-handling
// branch reassigns the pinned column needs the branch's own WHERE-less write checked on its
// own (x/obligation/check.go's writes(): `case o.Body.Pinned != "" && w.Kind == facts.Update
// && c.f.Kind != facts.Update && assigns(w, o.Body.Pinned)`, wired up for PostgreSQL's
// `INSERT ... ON CONFLICT DO UPDATE`, which the obligations.md 1.1.0 log calls out by name:
// "ON CONFLICT DO UPDATE / MERGEの枝がpinned列に代入するなら、文が同じ列を固定していることも
// 求める"). That case only fires when the producer emits a second facts.Write{Kind: Update}
// for the conflict branch.
//
// check/mysql/internal/analyze/analyze.go's insert() never does this for MySQL's own
// equivalent, ON DUPLICATE KEY UPDATE: the ON DUPLICATE columns are typed and kept as
// w.onDuplicate (analyze.go:596-611), which violations.go reads for the 1048/1451/1642
// failure modes (violations.go:144,207,232-233), but a.facts.Writes is set once, at
// analyze.go:592, to a single facts.Write{Kind: Insert, ...} built before the ON DUPLICATE
// clause is even parsed. The UPDATE branch's assignment to tenant_id never becomes a
// facts.Write at all, so x/obligation never even reaches the case above: it has no Update
// write to iterate over (x/obligation/check.go's writes(): `for _, w := range c.f.Writes`).
//
// The result: `INSERT ... ON DUPLICATE KEY UPDATE tenant_id = ?` is reported exactly like a
// plain INSERT (pinned is satisfied because INSERT assigns tenant_id -- x/obligation's
// pinned(), the `c.f.Kind == facts.Insert && leaf.Role == facts.Target` branch), with no
// hint that, on a duplicate key, the statement can silently move an existing row to a
// different tenant_id with no equality fixing the row it moves in the first place -- the
// exact hazard the Postgres-side case exists to catch.
func TestAdv2OnDuplicateKeyUpdateBypassesPinned(t *testing.T) {
	m := adv2Load(t, adv2DupKeySchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `INSERT INTO orders (id, tenant_id, status) VALUES ($1, $2, $3) ON DUPLICATE KEY UPDATE tenant_id = $4, status = $5`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	flagged := false
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require pinned(tenant_id)" && d.Message != "" {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("%s: sqlshape reports require pinned(tenant_id) as satisfied, but the ON DUPLICATE KEY UPDATE branch reassigns tenant_id with nothing fixing the row it moves -- the statement is not, in fact, pinned to one tenant on a duplicate key", sql)
	}

	// Confirm against a real server: a duplicate key silently moves an existing row's
	// tenant_id, exactly the hazard require pinned(tenant_id) exists to rule out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2DupKeySchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	for _, tenant := range []int{1, 2} {
		if _, err := conn.ExecContext(ctx, "INSERT INTO tenants (id) VALUES (?)", tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id, status) VALUES (1, 1, 'open')"); err != nil {
		t.Fatal(err)
	}
	// the same INSERT ... ON DUPLICATE KEY UPDATE sqlshape analyzed above, now executed with
	// id=1 colliding: the server takes the UPDATE branch and moves the row to tenant 2
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id, status) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE tenant_id = ?, status = ?", 1, 2, "closed", 2, "closed"); err != nil {
		t.Fatalf("server rejects the duplicate-key branch: %v", err)
	}
	var tenantID int64
	if err := conn.QueryRowContext(ctx, "SELECT tenant_id FROM orders WHERE id = 1").Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if tenantID != 2 {
		t.Fatalf("expected the ON DUPLICATE KEY UPDATE branch to move order 1 to tenant 2 (demonstrating the hazard), got tenant %d", tenantID)
	}
}

const adv2CoercedPKSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require single on delete
CREATE TABLE orders (
  id VARCHAR(10) NOT NULL PRIMARY KEY,
  amount INT NOT NULL
);
`

// TestAdv2StringNumberCoercionBreaksSingleRowProof: `require single on delete` promises the
// One proof (x/cardinality) has established that the statement provably touches at most one
// row. The proof's premise, over a primary key: an equality to a known value fixes exactly
// one row because the key is unique. That premise silently assumes the equality compares
// like with like; MySQL's own comparison rules do not.
//
// DELETE FROM orders WHERE id = 5 compares a VARCHAR primary key with the *unquoted* integer
// literal 5. check/mysql/internal/analyze's termFacts/literalClass records Item_int as a
// plain Const term (facts.go's literalClass, analyze/facts.go:642-649) with no regard for
// the column's declared type, and eqFacts (facts.go:393-427) fixes the column to it exactly
// as it would fix a matching-typed column -- so closeFixed marks id Fixed, the PRIMARY key
// is enforced (leafFacts, facts.go:150-165), and cardinality.AtMostOne (and so `require
// single on delete`) reports the DELETE as provably single-row.
//
// mysqld disagrees: comparing a non-numeric string column to a number converts the *string*
// to a number for the comparison (Type Conversion in Expression Evaluation, measured below).
// '5' and '05' are both distinct VARCHAR primary keys, and both convert to the number 5, so
// `id = 5` matches both rows -- one statement sqlshape calls single-row deletes two.
func TestAdv2StringNumberCoercionBreaksSingleRowProof(t *testing.T) {
	m := adv2Load(t, adv2CoercedPKSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `DELETE FROM orders WHERE id = 5`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require single on delete" && d.Message == "" {
			t.Errorf("%s: sqlshape reports require single on delete as proved by the One proof, but id = 5 compares a VARCHAR primary key to a bare number: mysqld converts the column, not the literal, so more than one distinct id value can match", sql)
		}
	}

	// Confirm against a real server: two distinct primary keys ('5' and '05') both satisfy
	// `id = 5` once MySQL's own type conversion runs, so the "single-row" DELETE removes both.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2CoercedPKSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	for _, id := range []string{"5", "05"} {
		if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, amount) VALUES (?, 1)", id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := conn.ExecContext(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected mysqld's numeric coercion to match both '5' and '05' (2 rows deleted, demonstrating the hazard), got %d", n)
	}
}

const adv2ReplaceSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require never on delete
CREATE TABLE ledger (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, amount INT NOT NULL);
CREATE TABLE delete_log (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, ledger_id BIGINT UNSIGNED NOT NULL);

CREATE TRIGGER ledger_ad AFTER DELETE ON ledger FOR EACH ROW
BEGIN
  INSERT INTO delete_log (ledger_id) VALUES (OLD.id);
END;
`

// TestAdv2ReplaceIntoBypassesNeverOnDelete: `require never on delete` declares ledger
// append-only -- no statement may delete a row of it. REPLACE INTO is exactly such a
// statement whenever the key collides: the MySQL manual is explicit that REPLACE "deletes
// the old row and inserts the new row" on a duplicate key, and this codebase already knows
// it (analyze.go:282-284's own comment: "REPLACE INTO -- a colliding row is deleted first,
// so no key is violated but the rows referring to the replaced one are (1451)"; the fired
// trigger is also handled: mysql.md documents "REPLACE fires the INSERT and DELETE triggers
// (measured)", commit 8771888). Measured below: an AFTER DELETE trigger on ledger fires when
// a REPLACE collides, proving the delete is real, not just a manual's turn of phrase.
//
// None of that reaches x/facts, though. check/mysql/internal/analyze/analyze.go's insert()
// records is_replace only for the violations that follow from it (w.replace, read by
// violations.go:207 to skip the unique-violation report and by the trigger-selection logic
// that fires DELETE triggers for a REPLACE) -- but a.facts.Writes (analyze.go:592) is always
// built with facts.Insert as the write's Kind, whatever is_replace says. x/obligation's
// writes() (check.go:52-64) turns a facts.Write's Kind into a Kinds bitmask and skips a
// declaration whose Kinds bit is not set; `require never on delete`'s Kinds is OnDelete, so
// it is never even considered for a write recorded as Insert. A REPLACE INTO ledger that
// deletes an existing row is reported exactly like a plain INSERT: no discharge, no failure,
// nothing in `sqlshape check`'s audit log -- for the one table the schema declared may never
// be deleted from.
func TestAdv2ReplaceIntoBypassesNeverOnDelete(t *testing.T) {
	m := adv2Load(t, adv2ReplaceSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `REPLACE INTO ledger (id, amount) VALUES ($1, $2)`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	neverFlagged := false
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Body.Never && d.Message != "" {
			neverFlagged = true
		}
	}
	if !neverFlagged {
		t.Errorf("%s: sqlshape reports no problem with require never on delete, but a REPLACE INTO on a colliding key deletes the existing row of ledger (measured against mysqld below)", sql)
	}

	// Confirm against a real server: an AFTER DELETE trigger on ledger fires when the
	// REPLACE collides, so the "never deleted" table is, in fact, deleted from.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2ReplaceSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO ledger (id, amount) VALUES (1, 10)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "REPLACE INTO ledger (id, amount) VALUES (?, ?)", 1, 20); err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM delete_log WHERE ledger_id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the AFTER DELETE trigger to have fired once for the colliding REPLACE (demonstrating the hazard), got %d firings", n)
	}
}

const adv2HavingSchema = `-- sqlshape: mysql 8.4
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  deleted_at DATETIME
);
`

// TestAdv2HavingOnlyFilterFalselyFailsVisibleWhere: `visible where deleted_at IS NULL` is
// discharged when the statement provably restricts the rows it sees to the predicate,
// wherever that restriction is written. A statement whose GROUP BY repeats every selected
// column (so it groups every row alone, changing nothing) and whose HAVING carries the
// predicate is exactly as restrictive as the same predicate in WHERE -- mysqld returns the
// identical rows either way (measured below) -- so a checker that only reads WHERE and JOIN
// ON for facts, and treats HAVING as untyped noise, reports a false violation on a
// statement that filters correctly.
//
// check/mysql/internal/analyze's querySpecification (query.go:205-229) types opt_having_
// clause through a.condition (analyze.go:1362-1371, the same function WHERE and JOIN ON also
// go through) purely for name/type resolution; unlike opt_where_clause, its result is
// discarded rather than folded into facts.Scope. block() (facts.go:19-75), which builds the
// scope the obligation checker reads, only ever looks at body.Arg("opt_where_clause") and
// the joins -- HAVING is not one of its inputs. The predicate is real to mysqld and invisible
// to sqlshape.
func TestAdv2HavingOnlyFilterFalselyFailsVisibleWhere(t *testing.T) {
	m := adv2Load(t, adv2HavingSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `SELECT id, tenant_id, deleted_at FROM orders GROUP BY id, tenant_id, deleted_at HAVING deleted_at IS NULL`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "visible where deleted_at IS NULL" && d.Message != "" {
			t.Errorf("%s: sqlshape reports %q, but the HAVING clause restricts the rows exactly as a WHERE clause would (GROUP BY repeats every selected column, so it groups nothing away): the statement does satisfy visible where deleted_at IS NULL, measured against mysqld below", sql, d.Message)
		}
	}

	// Confirm against a real server: the HAVING-only form returns only the undeleted row,
	// the same rows `... WHERE deleted_at IS NULL` would.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2HavingSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id, deleted_at) VALUES (1, 1, NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id, deleted_at) VALUES (2, 1, '2020-01-01 00:00:00')"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	var ids []int64
	for rows.Next() {
		var id, tenant int64
		var deletedAt *time.Time
		if err := rows.Scan(&id, &tenant, &deletedAt); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("expected only the non-deleted row (id 1) back, got %v", ids)
	}
}

// TestAdv2SingleElementInFoldsToEq: this one is not a mysqld measurement, since it needs
// none -- `col IN (x)` with exactly one alternative fixes col to x for exactly the reason
// `col = x` does; SQL gives IN a list of alternatives, and a list of one alternative is one
// value. x/cardinality's One proof and require pinned / require single all read Eq (or
// Fixed, closed from Eq) for "this column has exactly one value here", never In.
//
// check/mysql/internal/analyze's predFacts (facts.go's "Item_func_in" case) used to build
// an In predicate only when the parsed list carries at least one alternative *beyond* the
// column itself (`len(list) > 1`, list[0] being the column) -- so `id IN ($1)` (list length
// 2: the column and its one alternative) qualified for In, not for the len(list) > 1 branch
// being skipped; the real gap was that even when it built an In predicate, cardinality and
// pinned never read In as fixing anything (x/cardinality's keyFixed and x/obligation's
// pinned both walk Fixed, populated only from Eq / closeFixed). A single-row DELETE guarded
// by `id IN ($1)` was reported as failing require single on delete, though the statement is
// exactly as single-row as the same DELETE spelled with `id = $1`.
func TestAdv2SingleElementInFoldsToEq(t *testing.T) {
	m := loadContract(t)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	const sql = `DELETE FROM orders WHERE id IN ($1) AND tenant_id = $2 AND deleted_at IS NULL`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Body.Single && d.Message != "" {
			t.Errorf("%s: sqlshape reports %q, but `id IN ($1)` with a single alternative fixes id exactly as `id = $1` would -- the One proof should discharge require single on delete here", sql, d.Message)
		}
	}
}
