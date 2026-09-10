package analyze

// Adversarial probes for the plpgsql lane: PL/pgSQL failure modes that the checker's
// promise ("PostgreSQL's full feature set, safely from Go") says join the caller's
// expect list, but that internal/analyze/plpgsql.go does not model. Each case here was
// confirmed against embedded PostgreSQL 17 (see the report at
// scratchpad/adv/plpgsql.md for the exact runs); the test encodes what PostgreSQL
// actually raises and currently fails because the analyzer computes something looser.
//
// Do not "fix" these by editing plpgsql.go from this file — that is the maintainer's
// call. This file only pins the gap down as a reproducible, executable case.

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// findFn is a tiny lookup helper so each case below can grab the function it just
// declared without repeating the schema.Function-hunting loop plpgsql_test.go uses.
func findFn(s *schema.Schema, name string) *schema.Function {
	for _, f := range s.Functions {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// A1 (high): RAISE ... USING ERRCODE = <non-literal expression> defaults to P0001,
// even though PostgreSQL raises whatever SQLSTATE the expression evaluates to at run
// time. sqlshape.go 1018-1069 (raise()) only recognizes a string *literal* via
// plStringLiteral; a variable or any other expression silently falls through to the
// default P0001. This is a real idiom (re-raising the caught SQLSTATE via
// `RAISE USING ERRCODE = SQLSTATE`, or centralizing error codes in a lookup table), so a
// caller can add `-- sqlshape: expect P0001` and vet is satisfied, while PostgreSQL
// actually raises a different SQLSTATE (confirmed: 55000) that no expect line covers.
//
// Confirmed against PostgreSQL 17 via pgtest:
//
//	CREATE FUNCTION padv1(x integer) RETURNS void LANGUAGE plpgsql AS $$
//	DECLARE code text := '55000';
//	BEGIN
//	  RAISE EXCEPTION 'boom' USING ERRCODE = code;
//	END $$;
//	SELECT padv1(1);
//	-- ERROR: boom (SQLSTATE 55000)
//
// The analyzer reports P0001 for this function's raises, not 55000.
func TestAdvPLpgSQL_RaiseErrcodeVariable(t *testing.T) {
	def := `
CREATE FUNCTION padv1(x integer) RETURNS void LANGUAGE plpgsql AS $$
DECLARE code text := '55000';
BEGIN
  RAISE EXCEPTION 'boom' USING ERRCODE = code;
END $$;`
	s, err := Load(def)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fn := findFn(s, "padv1")
	if fn == nil {
		t.Fatal("function not found")
	}
	var codes []string
	for _, re := range raisedErrors(s, fn) {
		codes = append(codes, re.Code)
	}
	want := "55000"
	got := ""
	if len(codes) > 0 {
		got = codes[0]
	}
	if got != want {
		t.Errorf("raisedErrors = %v, want a raise carrying SQLSTATE %q (PostgreSQL: ERROR: boom (SQLSTATE 55000)); got %q instead — the analyzer defaults a non-literal ERRCODE to P0001, so vet only demands `expect P0001` while PostgreSQL can raise any code", codes, want, got)
	}
}

// A2 (high): SELECT ... INTO STRICT is not modeled at all. PostgreSQL raises P0002
// (no_data_found) when the query returns no row and P0003 (too_many_rows) when it
// returns more than one; neither joins the function's raises, so a caller of a STRICT
// lookup gets no expect requirement for either, and the runtime SQLSTATE cannot be in
// any expect line the checker asked for.
//
// Confirmed against PostgreSQL 17 via pgtest:
//
//	CREATE TABLE t (id bigint primary key, v integer not null);
//	CREATE FUNCTION padv3(p_id bigint) RETURNS integer LANGUAGE plpgsql AS $$
//	DECLARE n integer;
//	BEGIN
//	  SELECT v INTO STRICT n FROM t WHERE id = p_id;
//	  RETURN n;
//	END $$;
//	SELECT padv3(1); -- no such row
//	-- ERROR: query returned no rows (SQLSTATE P0002)
//
// (and, with two rows matching a filter-less STRICT query, SQLSTATE P0003).
func TestAdvPLpgSQL_IntoStrictUnmodeled(t *testing.T) {
	base := `CREATE TABLE t (id bigint primary key, v integer not null);`
	def := `
CREATE FUNCTION padv3(p_id bigint) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n integer;
BEGIN
  SELECT v INTO STRICT n FROM t WHERE id = p_id;
  RETURN n;
END $$;`
	s, err := Load(base + "\n" + def)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fn := findFn(s, "padv3")
	if fn == nil {
		t.Fatal("function not found")
	}
	var codes []string
	for _, re := range raisedErrors(s, fn) {
		codes = append(codes, re.Code)
	}
	hasP0002, hasP0003 := false, false
	for _, c := range codes {
		if c == "P0002" {
			hasP0002 = true
		}
		if c == "P0003" {
			hasP0003 = true
		}
	}
	if !hasP0002 || !hasP0003 {
		t.Errorf("raisedErrors = %v, want both P0002 (no_data_found) and P0003 (too_many_rows) for a `SELECT ... INTO STRICT` — PostgreSQL raises P0002 when the query returns no row and P0003 when it returns more than one, but the analyzer never adds either to the function's failure modes", codes)
	}
}

// A3 (high): a CASE statement (PLpgSQL_stmt_case, not the CASE *expression*) without an
// ELSE and without a WHEN matching the input raises PostgreSQL's CASE_NOT_FOUND
// (SQLSTATE 20000, "case not found"). plpgsql.go's "PLpgSQL_stmt_case" branch (around
// line 555) type-checks every WHEN/THEN and any t_expr, and walks else_stmts, but never
// records 20000 as a possible raise when else_stmts is empty.
//
// Confirmed against PostgreSQL 17 via pgtest:
//
//	CREATE FUNCTION padv6(x integer) RETURNS integer LANGUAGE plpgsql AS $$
//	BEGIN
//	  CASE x
//	    WHEN 1 THEN RETURN 10;
//	    WHEN 2 THEN RETURN 20;
//	  END CASE;
//	END $$;
//	SELECT padv6(3);
//	-- ERROR: case not found (SQLSTATE 20000)
func TestAdvPLpgSQL_CaseNotFoundUnmodeled(t *testing.T) {
	def := `
CREATE FUNCTION padv6(x integer) RETURNS integer LANGUAGE plpgsql AS $$
BEGIN
  CASE x
    WHEN 1 THEN RETURN 10;
    WHEN 2 THEN RETURN 20;
  END CASE;
END $$;`
	s, err := Load(def)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fn := findFn(s, "padv6")
	if fn == nil {
		t.Fatal("function not found")
	}
	var codes []string
	for _, re := range raisedErrors(s, fn) {
		codes = append(codes, re.Code)
	}
	found := false
	for _, c := range codes {
		if c == "20000" {
			found = true
		}
	}
	if !found {
		t.Errorf("raisedErrors = %v, want 20000 (case_not_found) among them — a CASE statement with no ELSE and no matching WHEN raises PostgreSQL's CASE_NOT_FOUND at run time, and the analyzer does not model it", codes)
	}
}

// A4 (high): the ASSERT statement's own failure (SQLSTATE P0004, assert_failure — the
// same code `RAISE assert_failure` maps to and the plpgsql_test.go suite already checks
// for that spelling) is never added to the function's raises. plpgsql.go's
// "PLpgSQL_stmt_assert" branch (around line 636) only type-checks the condition
// expression as boolean; it never calls anything that appends P0004 to b.raises, unlike
// the RAISE path.
//
// Confirmed against PostgreSQL 17 via pgtest (plpgsql.check_asserts is on by default):
//
//	CREATE FUNCTION padv8(x integer) RETURNS void LANGUAGE plpgsql AS $$
//	BEGIN
//	  ASSERT x > 0, 'x must be positive';
//	END $$;
//	SELECT padv8(-1);
//	-- ERROR: x must be positive (SQLSTATE P0004)
func TestAdvPLpgSQL_AssertUnmodeled(t *testing.T) {
	def := `
CREATE FUNCTION padv8(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  ASSERT x > 0, 'x must be positive';
END $$;`
	s, err := Load(def)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fn := findFn(s, "padv8")
	if fn == nil {
		t.Fatal("function not found")
	}
	var codes []string
	for _, re := range raisedErrors(s, fn) {
		codes = append(codes, re.Code)
	}
	found := false
	for _, c := range codes {
		if c == "P0004" {
			found = true
		}
	}
	if !found {
		t.Errorf("raisedErrors = %v, want P0004 (assert_failure) among them — an ASSERT statement can fail at run time exactly like `RAISE assert_failure`, but the analyzer only models the latter", codes)
	}
}

// A5 (high): writes through an updatable view declared WITH CHECK OPTION are not
// checked against the view's own predicate at all: no violation for SQLSTATE 44000
// ("new row violates check option for view") is ever produced, for LOCAL or CASCADED,
// even though this is one of the most common ways an application-level "safe subset"
// view is used to prevent writes leaving the row invisible to its own view. Nothing in
// internal/schema or internal/analyze/viewdml.go even parses or records the WITH CHECK
// OPTION clause (grep for CheckOption/check_option/"CHECK OPTION" in those packages
// turns up nothing), so this is not a narrow gap in an existing check — the feature is
// entirely absent from the model.
//
// Confirmed against PostgreSQL 17 via pgtest:
//
//	CREATE TABLE t (id bigint primary key, v integer not null);
//	CREATE VIEW v1 AS SELECT id, v FROM t WHERE v > 0 WITH CHECK OPTION;
//	INSERT INTO v1 (id, v) VALUES (1, -1);
//	-- ERROR: new row violates check option for view "v1" (SQLSTATE 44000)
func TestAdvPLpgSQL_ViewCheckOptionUnmodeled(t *testing.T) {
	base := `
CREATE TABLE t (id bigint primary key, v integer not null);
CREATE VIEW v1 AS SELECT id, v FROM t WHERE v > 0 WITH CHECK OPTION;`
	s, err := Load(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r, err := Analyze(s, `INSERT INTO v1 (id, v) VALUES (1, -1)`)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	found := false
	for _, v := range r.Violations {
		if v.Code == "44000" {
			found = true
		}
	}
	if !found {
		t.Errorf("violations = %+v, want a 44000 violation (check option for view %q) — an INSERT that can produce a row outside the view's own WHERE predicate must be flagged when the view carries WITH CHECK OPTION", r.Violations, "v1")
	}
}

// B1 (mid, false positive / over-declaration): a write's constraint violation that is
// actually caught by the function's own nested BEGIN ... EXCEPTION WHEN ... END block
// still joins the function's declared violations and propagates to the caller's expect
// list, even though PostgreSQL never lets it escape the function (the block rolls back
// to its internal savepoint and the handler runs instead). plpgsql.go's
// "PLpgSQL_stmt_block" case (around line 467) walks the exception handlers' own bodies
// but never removes the SQLSTATEs the WHEN clauses name from what the protected body
// contributed. This is safe-direction (an unnecessary `-- sqlshape: expect
// t_pkey` is required) rather than a silent failure, but it means the "expect" line no
// longer describes what can actually reach the caller, which the docs (checks.md,
// "The expect line is kept as the exact list of reasons the statement can fail") treat
// as an invariant worth maintaining.
//
// Confirmed against PostgreSQL 17: the INSERT inside the block below never raises past
// padv2() (the duplicate key is caught and silently ignored), so a caller of padv2()
// can never observe SQLSTATE 23505 — yet the analyzer still reports it as one of
// padv2()'s violations.
func TestAdvPLpgSQL_ExceptionBlockDoesNotSuppressCaughtViolation(t *testing.T) {
	base := `CREATE TABLE t (id bigint primary key, v integer not null);`
	def := `
CREATE FUNCTION padv2() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  BEGIN
    INSERT INTO t (id, v) VALUES (1, 1);
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;
END $$;`
	s, err := Load(base + "\n" + def)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fn := findFn(s, "padv2")
	if fn == nil {
		t.Fatal("function not found")
	}
	r, err := AnalyzeFunction(s, fn)
	if err != nil {
		t.Fatalf("analyze function: %v", err)
	}
	for _, v := range r.Violations {
		if v.Code == "23505" {
			t.Errorf("violations = %+v, want no 23505 violation — the INSERT that could raise it is inside a BEGIN ... EXCEPTION WHEN unique_violation THEN block that catches exactly that SQLSTATE, so it can never reach padv2()'s caller", r.Violations)
		}
	}
}
