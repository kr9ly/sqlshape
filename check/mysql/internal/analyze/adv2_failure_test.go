package analyze

// Adversarial pass 2 (failure lane): probes check/mysql/internal/analyze's failure-mode
// enumeration (violations.go), trigger-driven failure modes, and the runtime's SIGNAL /
// constraint key mapping (mysql/errors.go), against a running mysqld 8.4. See
// scratchpad/adv2/brief.md for the rules this pass follows: every finding is measured
// against a real server, not guessed. Findings are written as tests that state the CORRECT
// (server-measured) behaviour and currently FAIL against the checker as it stands today.

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

func adv2Keys(vs []Violation) string {
	var keys []string
	for _, v := range vs {
		keys = append(keys, fmt.Sprintf("%d %s", v.Code, v.Key()))
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// --- finding 1: a composite UNIQUE key is only recognized as "a NULL leaves it alone" when
// EVERY one of its columns is left to default to NULL (violations.go's leftNull) -----------

const adv2CompositeNullSchema = `-- sqlshape: mysql 8.4
CREATE TABLE widgets (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  a INT NOT NULL,
  b INT,
  UNIQUE KEY uq_a_b (a, b)
);
`

// TestAdv2CompositeUniqueKeyLeftPartiallyNull: docs/mysql.md's own constraint table says a
// UNIQUE key "a NULL leaves alone... cannot be violated" -- MySQL's multi-column UNIQUE key
// semantics are that a NULL in ANY of its columns takes the whole tuple out of duplicate
// checking, not only when every column is NULL. Measured: two rows sharing the same `a`
// with `b` left NULL both times insert without error (1062 never raised).
//
// violations.go's leftNull requires every column of the key to be always-NULL to skip the
// 1062 prediction (it returns false as soon as any one column, here `a`, is inserted), so
// insertViolations lists "1062 uq_a_b" for an INSERT that in fact can never violate it.
// Suspect: check/mysql/internal/analyze/violations.go leftNull (around line 256) and its use
// in insertViolations (the `continue` guard around line 211) only skip the whole key when
// ALL of cols pass the always-NULL test, instead of skipping when ANY column does.
func TestAdv2CompositeUniqueKeyLeftPartiallyNull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2CompositeNullSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2CompositeNullSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	r, err := Analyze(s, "INSERT INTO widgets (id, a) VALUES ($1, $2)")
	if err != nil {
		t.Fatal(err)
	}
	if got := adv2Keys(r.Violations); strings.Contains(got, "1062 uq_a_b") {
		t.Errorf("checker predicts 1062 uq_a_b for an INSERT that always leaves b NULL: %s (want no 1062 uq_a_b: a NULL in any column of a composite UNIQUE key exempts the row)", got)
	}

	conn := db.Conn()
	for i, a := range []int{1, 1, 1} {
		if _, err := conn.ExecContext(ctx, "INSERT INTO widgets (id, a) VALUES (?, ?)", i+1, a); err != nil {
			t.Fatalf("insert #%d (a=%d, b left NULL): server rejected a row the composite key should let through: %v", i+1, a, err)
		}
	}
}

// --- finding 2: an omitted NOT NULL column with no DEFAULT is not predicted at all ----------

const adv2NoDefaultSchema = `-- sqlshape: mysql 8.4
CREATE TABLE items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  name VARCHAR(50) NOT NULL,
  qty INT NOT NULL
);
`

// TestAdv2OmittedNotNullColumnNoDefault: an INSERT that simply leaves a NOT NULL column with
// no DEFAULT out of its column list is a common mistake -- measured on mysqld 8.4 (default
// strict mode) as error 1364 "Field 'qty' doesn't have a default value", not 1048 (which the
// checker only predicts for a column the statement actually stores a possibly-NULL
// expression into). notNullViolations (violations.go, iterating w.values) never sees an
// omitted column at all, so the checker predicts no failure mode whatsoever for a statement
// that mysqld always rejects.
func TestAdv2OmittedNotNullColumnNoDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2NoDefaultSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2NoDefaultSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	sql := "INSERT INTO items (id, name) VALUES ($1, $2)"
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatal(err)
	}
	got := adv2Keys(r.Violations)
	if !strings.Contains(got, "1364 items.qty") {
		t.Errorf("checker predicts %q for %s (omits NOT NULL qty with no default; qty is never mentioned), want a 1364 items.qty failure mode among them (measured on mysqld)", got, sql)
	}

	conn := db.Conn()
	_, execErr := conn.ExecContext(ctx, "INSERT INTO items (id, name) VALUES (?, ?)", 1, "widget")
	if execErr == nil {
		t.Fatal("server accepted an INSERT omitting a NOT NULL column with no default -- test schema/assumption is wrong")
	}
	if !strings.Contains(execErr.Error(), "1364") {
		t.Fatalf("expected 1364, server said: %v", execErr)
	}
}

// --- finding 3: a CHECK on a generated column is not predicted for a write to the source
// column the generated column is computed from -------------------------------------------

const adv2GeneratedCheckSchema = `-- sqlshape: mysql 8.4
CREATE TABLE prices (
  id INT NOT NULL PRIMARY KEY,
  cents INT NOT NULL,
  dollars INT AS (cents / 100) STORED,
  CONSTRAINT chk_dollars CHECK (dollars < 1000)
);
`

// TestAdv2GeneratedColumnCheckViaSourceColumn: `dollars` is a STORED generated column
// computed from `cents`; `chk_dollars` names `dollars`, not `cents`. Measured: INSERT INTO
// prices (id, cents) VALUES (1, 200000) raises 3819 on chk_dollars (the server recomputes
// the generated column and re-checks CHECK on every write that can change it, whether or
// not the statement names the generated column itself).
//
// violations.go's exprColumns (used by both insertViolations and updateViolations) walks
// only the literal column names chk_dollars's own expression spells (`dollars`), and
// anyIn(cols, w.inserted) then looks for `dollars` in the statement's own written columns
// (`id`, `cents`) -- never present, since a generated column is never itself assigned. The
// checker never expands a CHECK's column set through schema.Column.Generated to the source
// columns a generated column reads, so it predicts nothing at all for an INSERT/UPDATE that
// in fact always risks 3819 through the column it actually wrote.
func TestAdv2GeneratedColumnCheckViaSourceColumn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2GeneratedCheckSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2GeneratedCheckSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	sql := "INSERT INTO prices (id, cents) VALUES ($1, $2)"
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatal(err)
	}
	if got := adv2Keys(r.Violations); !strings.Contains(got, "3819 chk_dollars") {
		t.Errorf("checker predicts %s for %s, want 3819 chk_dollars among them (writing cents can trip the CHECK on the generated dollars column, measured)", got, sql)
	}

	conn := db.Conn()
	_, execErr := conn.ExecContext(ctx, "INSERT INTO prices (id, cents) VALUES (?, ?)", 1, 200000)
	if execErr == nil {
		t.Fatal("server accepted a cents value that should trip chk_dollars through the generated column -- test schema/assumption is wrong")
	}
	if !strings.Contains(execErr.Error(), "3819") {
		t.Fatalf("expected 3819, server said: %v", execErr)
	}
}

// --- finding 4: a trigger chain across two different tables that loops back into the
// original table is never predicted as 1442, unlike a trigger writing its own table -------

const adv2ChainedTableReuseSchema = `-- sqlshape: mysql 8.4
CREATE TABLE x (
  id INT NOT NULL PRIMARY KEY
);
CREATE TABLE y (
  id INT NOT NULL PRIMARY KEY
);
CREATE TRIGGER x_bi BEFORE INSERT ON x FOR EACH ROW
BEGIN
  INSERT INTO y (id) VALUES (NEW.id + 100);
END;
CREATE TRIGGER y_bi BEFORE INSERT ON y FOR EACH ROW
BEGIN
  INSERT INTO x (id) VALUES (NEW.id + 100);
END;
`

// TestAdv2ChainedTriggerTableReuseIs1442: MySQL's "table already in use" restriction (1442)
// is not limited to a trigger writing its OWN table directly -- it covers every table
// already in use anywhere up the invoking statement's chain. Measured: INSERT INTO x fires
// x_bi (BEFORE INSERT ON x), which inserts into y, which fires y_bi (BEFORE INSERT ON y),
// which inserts into x -- table x is already in use by the original INSERT INTO x, and
// mysqld always raises 1442 on x here, even though neither trigger's own table (x for
// x_bi, y for y_bi) is the one its own body writes.
//
// The checker's model only catches the direct case: walkDML's ownTableWrite (body.go,
// around line 1298) compares a write's table against a.trigTable, the CURRENT trigger
// being walked -- when AnalyzeTrigger(y_bi) walks "INSERT INTO x", a.trigTable is y, not x,
// so ownTableWrite says false and the write is treated as an ordinary embedded INSERT
// instead of a runtime-certain 1442. Since AnalyzeTrigger caches and analyzes each trigger's
// body independently (body.go's bodyCache), nothing threads "the set of tables already in
// use by the statement so far" through triggerViolations' cross-trigger recursion
// (violations.go's triggerViolations / triggerFailureModes), so Analyze predicts nothing
// for the INSERT INTO x that mysqld always refuses.
func TestAdv2ChainedTriggerTableReuseIs1442(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2ChainedTableReuseSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(adv2ChainedTableReuseSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}

	sql := "INSERT INTO x (id) VALUES ($1)"
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatal(err)
	}
	if got := adv2Keys(r.Violations); !strings.Contains(got, "1442") {
		t.Errorf("checker predicts %s for %s, want a 1442 failure mode among them (the x -> y -> x trigger chain always fails at run time on mysqld, measured)", got, sql)
	}

	conn := db.Conn()
	_, execErr := conn.ExecContext(ctx, "INSERT INTO x (id) VALUES (?)", 1)
	if execErr == nil {
		t.Fatal("server accepted an insert that should always fail through the chained trigger's table reuse -- test schema/assumption is wrong")
	}
	if !strings.Contains(execErr.Error(), "1442") {
		t.Fatalf("expected 1442, server said: %v", execErr)
	}
}
