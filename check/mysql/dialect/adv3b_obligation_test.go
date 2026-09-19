package dialect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// Third adversarial round, second half (2026-09, obligation lane, since commit dae6382):
// the remaining faces from brief-obligation.md not yet touched in the first half
// (adv3_obligation_test.go, adv3-obligation-report.md). See brief-followup-obligation.md
// for the checklist this file works through and adv3b-obligation-report.md for the full
// classification of every face (finding / matches / cannot be measured).

const adv3bJoinFilterSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require single on delete
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL
);
CREATE TABLE items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  order_id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL
);
`

// TestAdv3JoinFilterFalselyBreaksSingleProof: severity medium (a correct, safe statement is
// rejected).
//
// `require single on delete` exists so that a hand-run DELETE cannot silently remove more
// than one row; the proof is x/cardinality's AtMostOne (x/obligation/check.go's
// checker.single, check.go:12-15) called once per write in writes() (check.go:99-107),
// not once per table. AtMostOne (x/cardinality/cardinality.go's scopeSingle) proves the
// whole *level* is at most one row: every leaf of the level must be pinned by a unique key
// (cardinality.go's doc comment: "When every leaf is single the level is at most one row").
//
// A single-table DELETE that merely *joins* another table to filter its rows -- a common,
// defensive pattern ("delete this order, but only if it still has items") -- puts that
// other table's leaf into the same level. If that leaf is not itself pinned by a unique
// key (items here has no WHERE fixing any of its own columns, only an ON equality to
// orders), AtMostOne reports the whole level as not proved single, even though the
// statement deletes at most one row of orders (o.id = $1 is its own PRIMARY KEY) and does
// not delete from items at all -- items is only read, to decide whether the one orders row
// qualifies. The obligation is declared on orders, about orders's own DELETE, and orders's
// own row is provably singular; a leaf the statement never writes should not be able to
// break the proof for a leaf it does.
//
// The message names the wrong thing, too: "orders requires a single-row DELETE" is issued
// for the DELETE FROM ... o.id = $1 form, which is exactly right by itself.
func TestAdv3JoinFilterFalselyBreaksSingleProof(t *testing.T) {
	m := adv3Load(t, adv3bJoinFilterSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `DELETE o FROM orders o JOIN items i ON i.order_id = o.id WHERE o.id = $1`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require single on delete" && d.Message != "" {
			t.Errorf("%s: sqlshape reports %q, but o.id = $1 fixes orders's own PRIMARY KEY: at most one row of orders is deleted regardless of how many items rows the JOIN happens to match (items is not a delete target here, only a filter) -- measured against mysqld below", sql, d.Message)
		}
	}

	// Confirm against a real server: with one order and two of its items (so the join
	// fans out to two rows at the JOIN level, defeating a naive whole-level row count),
	// the DELETE removes exactly the one orders row and none of items's.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv3bJoinFilterSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id) VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO items (id, order_id, tenant_id) VALUES (1, 1, 1), (2, 1, 1)"); err != nil {
		t.Fatal(err)
	}
	res, err := conn.ExecContext(ctx, "DELETE o FROM orders o JOIN items i ON i.order_id = o.id WHERE o.id = ?", 1)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("test premise wrong: want exactly 1 row deleted (orders's one row; items is a filter, not a target), got %d", n)
	}
	var remaining int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM items").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("test premise wrong: want both items rows to survive (they were never a delete target), got %d remaining", remaining)
	}
}

const adv3bUsingJoinSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL
);
-- sqlshape: require pinned(tenant_id)
CREATE TABLE items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  order_id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL
);
`

// TestAdv3UnqualifiedUsingColumnNeverPinsEitherSide: severity medium (a correct, safe
// statement is rejected on both sides of the join).
//
// docs/mysql.md / docs.md's own claim: "A USING or NATURAL join coalesces its common
// columns (an unqualified name resolves to the left side, SELECT * lists it once)." So
// `items JOIN orders USING (tenant_id) ... WHERE tenant_id = $1` names one coalesced
// column that both items.tenant_id and orders.tenant_id must equal (that is what USING
// means): the statement is pinned to one tenant on both tables.
//
// check/mysql/internal/analyze/facts.go's colFact (~line 733) only recognizes a plain
// column reference through one of a fixed set of AST node classes
// (PTI_simple_ident_ident and its siblings); an unqualified name after a USING/NATURAL
// join resolves through mysqld's own coalescing construct, which colFact does not
// recognize as any leaf's column at all. eqFacts (facts.go's eqFacts, ~line 528) then
// finds neither side of `tenant_id = $1` a known column (okl, okr both false) and drops
// the conjunct as opaque -- no Fixed fact is ever recorded for *either* items.tenant_id or
// orders.tenant_id, even though a qualified equivalent (`items.tenant_id = $1`) is
// recognized immediately and, through the same USING equality, is also read back as
// pinning orders (measured separately, not a finding).
func TestAdv3UnqualifiedUsingColumnNeverPinsEitherSide(t *testing.T) {
	m := adv3Load(t, adv3bUsingJoinSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	const sql = `UPDATE items JOIN orders USING (tenant_id) SET items.order_id = $1 WHERE tenant_id = $2`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Obligation.Source == "require pinned(tenant_id)" && d.Message != "" {
			t.Errorf("%s: sqlshape reports %q at %s, but the unqualified `tenant_id` after USING (tenant_id) is the joined column both items.tenant_id and orders.tenant_id must equal -- the statement is pinned to one tenant on both sides, measured against mysqld below", sql, d.Message, d.Leaf.Table)
		}
	}

	// Confirm against a real server: with two tenants' worth of matching rows, the
	// statement only ever touches the named tenant's items row.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv3bUsingJoinSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, tenant_id) VALUES (1, 1), (2, 2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO items (id, order_id, tenant_id) VALUES (1, 1, 1), (2, 2, 2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE items JOIN orders USING (tenant_id) SET items.order_id = ? WHERE tenant_id = ?", 99, 1); err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	var untouchedOrderID int
	if err := conn.QueryRowContext(ctx, "SELECT order_id FROM items WHERE id = 2").Scan(&untouchedOrderID); err != nil {
		t.Fatal(err)
	}
	if untouchedOrderID != 2 {
		t.Fatalf("test premise wrong: want tenant 2's items row untouched (order_id still 2), got %d", untouchedOrderID)
	}
}

const adv3bTransitionsSchema = `-- sqlshape: mysql 8.4
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled
CREATE TABLE workflow (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  status VARCHAR(10) NOT NULL
);
`

// TestAdv3TransitionsNeverRecognizesAnyMySQLLiteral: severity medium in the strict sense
// of the severity rubric (a correct statement is rejected), but total in practice: this
// makes `transitions` entirely unusable on MySQL, for every declared transition.
//
// `transitions status: draft -> submitted, ...` exists to enforce compare-and-set: an
// UPDATE that sets status to a constant must first fix the column, in its own WHERE, to
// one of that target's declared predecessors. x/obligation/check.go's transition()
// (~line 561) reads the target literal via stateOf (~line 632), which strips a leading
// type-tag letter ('i', 'f', 's', 'b', 'x') off the constant's text -- "spaid" is its own
// example of the expected spelling, i.e. the tag then the bare value with no quotes.
//
// check/mysql/internal/analyze's own constant text (termFacts, facts.go's literalClass
// branch, and thus also the Write.Values a transition reads through eqFacts/w.Values) is
// built by textOf (facts.go ~line 923): the raw source text between the token's start and
// end offsets, unchanged -- for a string literal this is 'submitted' complete with its
// surrounding quotes and no type tag at all. stateOf's guard (`c[0]` one of "ifsbx") never
// matches a leading quote character, so it returns the constant untouched: "to" is
// literally "'submitted'", which cannot equal any of the bare state names tr.From's keys
// use ("submitted", "paid", "cancelled", parsed from the plain, unquoted declaration text).
// Every literal assignment to status -- valid transition or not -- is reported as "is not a
// state anything transitions to", because the comparison can never succeed for a
// string-typed state column on MySQL. The same mismatch reaches the WHERE side of the
// proof (satisfied's `current = []string{stateOf(p.Term.Const)}`, check.go ~line 599): a
// current-state literal in WHERE is quoted the same way, so even a `-> spelled out
// correctly on both sides` proof is unreachable.
func TestAdv3TransitionsNeverRecognizesAnyMySQLLiteral(t *testing.T) {
	m := adv3Load(t, adv3bTransitionsSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}

	// The textbook-correct compare-and-set: draft -> submitted is declared, and the
	// statement fixes the current state (draft) in its own WHERE by equality.
	const sql = `UPDATE workflow SET status = 'submitted' WHERE status = 'draft'`
	r, err := m.Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
		if d.Message != "" {
			t.Errorf("%s: sqlshape reports %q, but draft -> submitted is declared and the statement's own WHERE fixes the current state to draft by equality -- this is exactly the compare-and-set the obligation exists to accept, measured against mysqld below", sql, d.Message)
		}
	}

	// Confirm against a real server: the declared, well-formed transition succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv3bTransitionsSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO workflow (id, status) VALUES (1, 'draft')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE workflow SET status = 'submitted' WHERE status = 'draft'"); err != nil {
		t.Fatalf("test premise wrong: server rejects the declared transition: %v", err)
	}
	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM workflow WHERE id = 1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" {
		t.Fatalf("test premise wrong: want status = submitted after the transition, got %q", status)
	}
}
