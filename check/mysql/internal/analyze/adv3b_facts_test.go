package analyze

// Adversarial round 3, lane "newcode", second half, face 4 (the rest of facts.go): probes
// HAVING folding, single-element IN facts and the string-column/numeric-literal
// non-fixation rule against a running mysqld 8.4, one rung further out from what
// adv2_*_test.go pinned. See scratchpad/adv3/brief-followup-newcode.md and
// scratchpad/adv3/adv3b-newcode-report.md (which also lists the faces measured here that
// agreed with the server and got no test).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

const adv3bCoercionSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  c VARCHAR(8) NOT NULL UNIQUE,
  tag INT NOT NULL
);
`

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a string-typed column compared with the literal TRUE is treated by eqFacts as
// an ordinary, fixing equality (facts.Eq, and c lands in Facts.Top.Fixed) -- but TRUE is
// exactly the integer 1, and mysqld's own comparison rules (Type Conversion in Expression
// Evaluation) convert the *column's* stored value to a number for such a comparison, the
// very hazard stringNumberCoercion already guards against for a bare numeric literal
// (`c = 5` against a VARCHAR column, round 2). Measured against mysqld 8.4.11: on a
// VARCHAR(8) UNIQUE column holding '1' and '01', `UPDATE t SET tag = 9 WHERE c = TRUE`
// updates 2 rows -- two distinct column values satisfy the "equality", so it proves neither
// `require single` (x/cardinality's One) nor `require pinned` (Fixed / Eq read as "exactly
// one value here").
//
// Suspect: check/mysql/internal/analyze/facts.go's numericLiteralClass (facts.go:812: only
// Item_int / Item_uint / Item_decimal / Item_float -- Item_func_true and Item_func_false
// are missing, though the server compares them as the numbers 1 and 0), read by
// stringNumberCoercion (facts.go:849), which eqFacts consults before recording col = TRUE
// as a Fixed equality.
func TestAdv3StringColumnEqTrueCoercionMissed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := mysqltest.Start(ctx, adv3bCoercionSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (1,'1',0),(2,'01',0)"); err != nil {
		t.Fatal(err)
	}
	// the server: '1' and '01' both satisfy c = TRUE (the column is converted to a number,
	// TRUE is 1), measured against mysqld 8.4.11
	const sql = "UPDATE t SET tag = 9 WHERE c = TRUE"
	res, err := conn.ExecContext(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("test premise wrong: want the server to update 2 rows ('1' and '01' both coerce to TRUE = 1), got %d", n)
	}

	s, err := schema.Load(adv3bCoercionSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, aerr := Analyze(s, "UPDATE t SET tag = $1 WHERE c = TRUE")
	if aerr != nil {
		t.Fatal(aerr)
	}
	for _, c := range r.Facts.Top.Fixed {
		if c.Column == "c" {
			t.Errorf("Analyze(%s).Facts.Top.Fixed = %v: treats c as fixed to one value by c = TRUE, but the server updates 2 distinct rows ('1' and '01' both coerce to TRUE = 1) -- stringNumberCoercion does not count Item_func_true as a numeric literal class", sql, r.Facts.Top.Fixed)
		}
	}
}

// ---------------------------------------------------------------------------------------
// severity: high
//
// Finding: a string-typed column compared with CAST($n AS SIGNED) is treated as a fixing
// equality -- but the CAST's result is a number, so the server converts the *column* to a
// number for the comparison exactly as it does for a bare numeric literal. Measured against
// mysqld 8.4.11: `UPDATE t SET tag = 9 WHERE c = CAST(1 AS SIGNED)` on the same VARCHAR(8)
// UNIQUE column ('1', '01') updates 2 rows.
//
// Suspect: check/mysql/internal/analyze/facts.go's stringNumberCoercion (facts.go:849),
// which only asks numericLiteralClass of the other operand's node class: a computed
// expression such as CAST(... AS SIGNED) (mysqlparse's create_func_cast) is never a literal
// class, so the hazard detection is literal-shaped and blind to any non-literal operand
// that is nonetheless guaranteed numeric.
func TestAdv3StringColumnEqCastToSignedCoercionMissed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := mysqltest.Start(ctx, adv3bCoercionSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (1,'1',0),(2,'01',0)"); err != nil {
		t.Fatal(err)
	}
	// the server: '1' and '01' both satisfy c = CAST(1 AS SIGNED), measured against
	// mysqld 8.4.11
	const sql = "UPDATE t SET tag = 9 WHERE c = CAST(1 AS SIGNED)"
	res, err := conn.ExecContext(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("test premise wrong: want the server to update 2 rows ('1' and '01' both coerce to the number 1), got %d", n)
	}

	s, err := schema.Load(adv3bCoercionSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, aerr := Analyze(s, "UPDATE t SET tag = $1 WHERE c = CAST($2 AS SIGNED)")
	if aerr != nil {
		t.Fatal(aerr)
	}
	for _, c := range r.Facts.Top.Fixed {
		if c.Column == "c" {
			t.Errorf("Analyze(%s).Facts.Top.Fixed = %v: treats c as fixed to one value by c = CAST($2 AS SIGNED), but the server updates 2 distinct rows ('1' and '01' both coerce to the number 1) -- stringNumberCoercion only recognizes numericLiteralClass node classes, never a computed CAST", sql, r.Facts.Top.Fixed)
		}
	}
}

// ---------------------------------------------------------------------------------------
// severity: medium
//
// Finding: a two-column row-constructor IN with a single alternative, `(a, b) IN (($1,
// $2))`, is left entirely Opaque instead of being decomposed into `a = $1 AND b = $2` --
// unlike the scalar case (`v IN ($1)`, handled through the grammar's single-item IN rule,
// PTI_handle_sql2003_note184_exception) and unlike a direct row-constructor equality
// (`(a, b) = ($1, $2)`, unpacked by predFacts's comparison case via rowElements). Measured
// against mysqld 8.4.11: with rows (a=5,b=6), (a=5,b=7), (a=9,b=6), `UPDATE t SET tag = 1
// WHERE (a, b) IN ((5, 6))` updates exactly 1 row -- the form is the conjunction of the
// per-column equalities, the same as the already-unpacked `=` case.
//
// This under-proves rather than over-proves (the analyzer is more conservative, not less
// safe), so a `require pinned(a)` / `require single` on a write that fixes both columns
// through this form is wrongly refused -- the same false-positive shape as round 3's
// nested CHECK OPTION finding.
//
// Suspect: check/mysql/internal/analyze/facts.go's predFacts, case
// "PTI_handle_sql2003_note184_exception" (facts.go:399), which hands the two sides to
// eqFacts as single scalars; eqFacts never calls rowElements (facts.go:583) on its
// operands, so an Item_row on either side is opaque to it, unlike predFacts's own
// comparison case for a bare `=`.
func TestAdv3RowConstructorInNotDecomposedToEqualities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const sc = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  a INT NOT NULL,
  b INT NOT NULL,
  tag INT NOT NULL
);
`
	db, err := mysqltest.Start(ctx, sc)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (1,5,6,0),(2,5,7,0),(3,9,6,0)"); err != nil {
		t.Fatal(err)
	}
	// the server: (a, b) IN ((5, 6)) selects exactly the row with a = 5 AND b = 6,
	// measured against mysqld 8.4.11
	const sql = "UPDATE t SET tag = 1 WHERE (a, b) IN ((5, 6))"
	res, err := conn.ExecContext(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("test premise wrong: want the server to update exactly 1 row (only a=5 AND b=6), got %d", n)
	}

	s, err := schema.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, aerr := Analyze(s, "UPDATE t SET tag = $1 WHERE (a, b) IN (($2, $3))")
	if aerr != nil {
		t.Fatal(aerr)
	}
	gotA, gotB := false, false
	for _, p := range r.Facts.Top.Preds {
		if p.Op == facts.Eq && p.Col.Column == "a" {
			gotA = true
		}
		if p.Op == facts.Eq && p.Col.Column == "b" {
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Errorf("Analyze(%s).Facts.Top.Preds = %+v, want an Eq fact on both a and b ((a, b) IN ((5, 6)) is exactly a = 5 AND b = 6 on the server, measured: 1 row of 3) -- eqFacts never unpacks the Item_row operands the single-element IN grammar path hands it", sql, r.Facts.Top.Preds)
	}
}
