package analyze

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// Adversarial findings, lane "violations" (see the brief for the ground rules): each test
// names the correct behavior and is expected to fail against the current implementation.
// Every case below was reproduced against a real mysqld (nix-shell -p mysql84).

// keysOf renders a Result's predicted violations the way TestViolations does, for a
// stable, sorted comparison.
func keysOf(vs []Violation) string {
	var ks []string
	for _, v := range vs {
		k := fmt.Sprintf("%d %s", v.Code, v.Key())
		if v.Param > 0 {
			k += fmt.Sprintf(":%d", v.Param)
		}
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// TestAdvReplaceNeverDuplicates covers REPLACE INTO: it resolves a colliding PRIMARY /
// UNIQUE key by deleting the old row and inserting the new one, so it can never raise
// 1062 (ER_DUP_ENTRY) the way a plain INSERT can. sqlshape's analyzer does not read
// PT_insert's is_replace flag at all (grep turns up nothing in internal/analyze), so
// insertViolations predicts the same PRIMARY / UNIQUE key violations for REPLACE as for
// INSERT.
//
// Reproduced: against violationSchema, with accounts holding (id=1,email='a@x',nick='a')
// and (id=2,email='b@x',nick='b'), `REPLACE INTO accounts (id, email, nick) VALUES (99,
// 'z@x', 'b')` collides on the UNIQUE key `nick` (a row with nick='b' already exists) and
// yet the server accepts it without error (it deletes the old row and inserts the new
// one). sqlshape's Analyze predicts "1062 nick" (among others) for this statement.
func TestAdvReplaceNeverDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, violationSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(violationSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO accounts (id, email, nick, balance) VALUES (1, 'a@x', 'a', 10), (2, 'b@x', 'b', 0)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	const sql = "REPLACE INTO accounts (id, email, nick) VALUES (99, 'z@x', 'b')"
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.ExecContext(ctx, sql)
	tx.Rollback()
	if execErr != nil {
		t.Fatalf("the server rejects the reproduction itself: %v", execErr)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for _, v := range r.Violations {
		if v.Code == codeDuplicateKey {
			t.Errorf("REPLACE INTO is predicted to violate a duplicate key (%s), but the server never raises 1062 for REPLACE: got %s", v.Key(), keysOf(r.Violations))
			return
		}
	}
}

// TestAdvReplaceCanViolateReferencingKey covers the other half of REPLACE's semantics:
// its implicit DELETE of a colliding row can itself be rejected by a foreign key that
// references the table with the default RESTRICT / NO ACTION action -- 1451, the same
// as an explicit DELETE. sqlshape's insertViolations (used for REPLACE too, see above)
// never calls referencingViolations, so this failure mode is missing entirely from the
// predicted set, even for a same-row REPLACE that changes nothing about the reference.
//
// Reproduced: against violationSchema, with accounts(id=1,...) referenced by
// payments(account_id=1), `REPLACE INTO accounts (id, email, nick) VALUES (1, 'a@x',
// 'a')` -- replacing the row with itself -- fails on the server with 1451
// fk_payments_account (the implicit DELETE of id=1 is rejected). sqlshape predicts no
// 1451 violation for this statement.
func TestAdvReplaceCanViolateReferencingKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, violationSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(violationSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO accounts (id, email, nick, balance) VALUES (1, 'a@x', 'a', 10)",
		"INSERT INTO payments (id, account_id, amount) VALUES (10, 1, 5)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	const sql = "REPLACE INTO accounts (id, email, nick) VALUES (1, 'a@x', 'a')"
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.ExecContext(ctx, sql)
	tx.Rollback()
	if execErr == nil || !strings.Contains(execErr.Error(), "1451") {
		t.Fatalf("the server does not raise 1451 for the reproduction: %v", execErr)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for _, v := range r.Violations {
		if v.Code == codeForeignKeyRef && v.Constraint == "fk_payments_account" {
			return
		}
	}
	t.Errorf("REPLACE INTO on a row referenced by another table's FK (RESTRICT) should predict 1451 fk_payments_account (its implicit DELETE can be rejected), got %s", keysOf(r.Violations))
}

// TestAdvMultiTableUpdateAttributesEachAssignmentToItsOwnTable covers a multi-table
// UPDATE (a JOIN over two tables, each assigned to). update() tracks only one *write*
// for the whole statement: w.table is set to whichever table's column is assigned
// *first*, and updateViolations(w.table, w.values, nil) then walks w.table's own keys /
// foreign keys / CHECKs against the set of *all* assigned column names, regardless of
// which table each assignment actually belongs to. Two failures follow from this:
//   - a NOT NULL violation is reported with the wrong table name (notNullViolations
//     labels it with the `t` it was called with, not as.col's own table), so it can name
//     a table.column pair that does not exist in the schema;
//   - a foreign key on the *other* table is never checked at all, so a real 1452/1451 is
//     silently dropped from the predicted set.
//
// Reproduced: against violationSchema, `UPDATE accounts a JOIN payments p ON
// p.account_id = a.id SET a.nick = $1, p.account_id = $2 WHERE p.id = $3` with $2 =
// 999 (no such account) fails on the server with 1452 fk_payments_account. sqlshape
// predicts no 1452 (or 1451) violation at all, and instead predicts a NOT NULL
// violation "1048 accounts.account_id:2" -- accounts has no column named account_id.
func TestAdvMultiTableUpdateAttributesEachAssignmentToItsOwnTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, violationSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(violationSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO accounts (id, email, nick, balance) VALUES (1, 'a@x', 'a', 10), (2, 'b@x', 'b', 0)",
		"INSERT INTO payments (id, account_id, amount) VALUES (10, 1, 5)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	const sql = "UPDATE accounts a JOIN payments p ON p.account_id = a.id SET a.nick = $1, p.account_id = $2 WHERE p.id = $3"
	const runSQL = "UPDATE accounts a JOIN payments p ON p.account_id = a.id SET a.nick = ?, p.account_id = ? WHERE p.id = ?"
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.ExecContext(ctx, runSQL, "z", 999, 10)
	tx.Rollback()
	if execErr == nil || !strings.Contains(execErr.Error(), "1452") {
		t.Fatalf("the server does not raise 1452 for the reproduction: %v", execErr)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var foundFK, bogusColumn bool
	for _, v := range r.Violations {
		if v.Code == codeForeignKeyRow && v.Constraint == "fk_payments_account" {
			foundFK = true
		}
		if v.Code == codeNotNull && v.Table == "accounts" && len(v.Columns) == 1 && v.Columns[0] == "account_id" {
			bogusColumn = true
		}
	}
	if !foundFK {
		t.Errorf("assigning payments.account_id in a multi-table UPDATE should predict 1452 fk_payments_account, got %s", keysOf(r.Violations))
	}
	if bogusColumn {
		t.Errorf("the NOT NULL violation for payments.account_id is mislabeled as accounts.account_id, which is not a real column: got %s", keysOf(r.Violations))
	}
}

// TestAdvUpdatableViewViolations covers UPDATE through a merged (updatable) view.
// update()'s write.table comes from the target column's relation (col.rel.table), and a
// view relation always has table == nil (views hold their columns in .cols, not
// .table) even when the view merges into a real base table and is genuinely writable
// (relation.updatable / .merged, set at analyze.go:729-733). violations() bails out as
// soon as w.table is nil ("if w == nil || w.table == nil || w.ignore { return nil }"),
// so an UPDATE through an updatable view predicts zero violations no matter what the
// underlying base table's constraints are.
//
// Reproduced: against violationSchema plus `CREATE VIEW v_accounts AS SELECT * FROM
// accounts`, with two accounts seeded, `UPDATE v_accounts SET email = $1 WHERE id =
// $2` binding email to an already-used address fails on the server with 1062
// accounts.accounts_email. sqlshape predicts no violations at all for this statement.
func TestAdvUpdatableViewViolations(t *testing.T) {
	schemaSQL := violationSchema + "\nCREATE VIEW v_accounts AS SELECT * FROM accounts;\n"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, schemaSQL)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO accounts (id, email, nick, balance) VALUES (1, 'a@x', 'a', 10), (2, 'b@x', 'b', 0)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	const sql = "UPDATE v_accounts SET email = $1 WHERE id = $2"
	const runSQL = "UPDATE v_accounts SET email = ? WHERE id = ?"
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.ExecContext(ctx, runSQL, "b@x", 1)
	tx.Rollback()
	if execErr == nil || !strings.Contains(execErr.Error(), "1062") {
		t.Fatalf("the server does not raise 1062 for the reproduction: %v", execErr)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for _, v := range r.Violations {
		if v.Code == codeDuplicateKey && v.Constraint == "accounts_email" {
			return
		}
	}
	t.Errorf("UPDATE through an updatable view should predict the base table's violations (1062 accounts_email), got %s", keysOf(r.Violations))
}
