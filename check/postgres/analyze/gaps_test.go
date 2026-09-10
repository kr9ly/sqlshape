package analyze

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/schema"
)

// gapsSchema loads testdata/schema.sql, optionally extended with more DDL, once per test.
func gapsSchema(t *testing.T, extra string) *schema.Schema {
	t.Helper()
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(string(schemaSQL) + extra)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func gapsErr(t *testing.T, s *schema.Schema, sql string) *Error {
	t.Helper()
	_, err := Analyze(s, sql)
	if err == nil {
		t.Fatalf("%s: want error, got none", sql)
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("%s: want *Error, got %v", sql, err)
	}
	return aerr
}

// TestGapsParamBinding covers the parameter-typing helpers in expr.go: firstParam
// (which parameter to blame when neither side of an operator can be typed),
// boolOpName (the operator name in a boolean-argument type error), and setParam's
// "inconsistent types deduced" branch (the same untyped $n bound to two incompatible
// declared argument types before either occurrence is resolved).
func TestGapsParamBinding(t *testing.T) {
	s := gapsSchema(t, "")

	// firstParam: neither side of "+" can be typed, so the error blames $1 (the first
	// parameter found scanning left then right).
	if err := gapsErr(t, s, "SELECT $1 + $2"); err.Code != "42P18" || !strings.Contains(err.Error(), "parameter $1") {
		t.Errorf("$1 + $2: got %v", err)
	}

	// boolOpName: AND / OR / NOT each spell their own name in the type error.
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT id FROM users WHERE name AND true", "argument of AND must be type boolean"},
		{"SELECT id FROM users WHERE name OR true", "argument of OR must be type boolean"},
		{"SELECT NOT name FROM users", "argument of NOT must be type boolean"},
	}
	for _, c := range cases {
		if err := gapsErr(t, s, c.sql); err.Code != "42804" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want substring %q", c.sql, err, c.want)
		}
	}

	// setParam inconsistent types: yen_sum_step(bigint, yen) called as f($1, $1) analyzes
	// both (still-unknown) argument exprs before binding either, then binds the first to
	// bigint and the second to yen (yen_sum_step is defined at testdata/schema.sql:164).
	if err := gapsErr(t, s, "SELECT yen_sum_step($1, $1)"); err.Code != "42P18" || !strings.Contains(err.Error(), "inconsistent types deduced for parameter $1") {
		t.Errorf("yen_sum_step($1,$1): got %v", err)
	}
	if err := gapsErr(t, s, "SELECT save_order($1, $1)"); err.Code != "42P18" || !strings.Contains(err.Error(), "inconsistent types deduced for parameter $1") {
		t.Errorf("save_order($1,$1): got %v", err)
	}
}

// TestGapsNamedArgs covers catInputNames, both of its shapes: a plain catalog function
// (make_interval, ArgModes nil, names read straight off ArgNames by position) and one
// with OUT parameters mixed in (pg_options_to_table / jsonb_each: catInputNames must
// skip the "o" slots so the single named input still lines up), plus the equivalent
// path for a user-defined SQL function (whose ArgNames/ArgModes are schema.Function
// fields, not catalog.Func ones, so it never touches catInputNames at all).
func TestGapsNamedArgs(t *testing.T) {
	s := gapsSchema(t, "")

	if _, err := Analyze(s, "SELECT make_interval(years => 1, days => 2)"); err != nil {
		t.Errorf("make_interval named args: %v", err)
	}
	if _, err := Analyze(s, "SELECT make_interval(hours => 1, years => 2)"); err != nil {
		t.Errorf("make_interval named args, out of order: %v", err)
	}
	if _, err := Analyze(s, "SELECT * FROM pg_options_to_table(options_array => '{}')"); err != nil {
		t.Errorf("pg_options_to_table named arg (OUT params present): %v", err)
	}
	if _, err := Analyze(s, "SELECT * FROM jsonb_each(from_json => '{}'::jsonb)"); err != nil {
		t.Errorf("jsonb_each named arg (OUT params present): %v", err)
	}

	r, err := Analyze(s, "SELECT * FROM list_totals(min_total => 5)")
	if err != nil {
		t.Fatalf("list_totals(min_total => 5): %v", err)
	}
	if len(r.Columns) != 2 || r.Columns[0].Name != "user_id" || r.Columns[1].Name != "total" {
		t.Errorf("list_totals columns: %+v", r.Columns)
	}
	r2, err := Analyze(s, "SELECT order_count(p_user => 1)")
	if err != nil {
		t.Fatalf("order_count(p_user => 1): %v", err)
	}
	if len(r2.Columns) != 1 || r2.Columns[0].Type.OID != catalog.Int8 {
		t.Errorf("order_count columns: %+v", r2.Columns)
	}
}

// TestGapsAnalyzeView covers both branches of AnalyzeView (function.go): a relation
// with no defining query (a plain table) short-circuits to an empty Result, while a
// view's query is analyzed like any statement, surfacing its own cardinality proof.
func TestGapsAnalyzeView(t *testing.T) {
	s := gapsSchema(t, "")

	tbl := s.Relation("public", "users")
	if tbl == nil {
		t.Fatal("no users relation")
	}
	r, err := AnalyzeView(s, tbl)
	if err != nil {
		t.Fatalf("AnalyzeView(users): %v", err)
	}
	if len(r.Columns) != 0 || len(r.Violations) != 0 {
		t.Errorf("AnalyzeView(users) want empty Result, got %+v", r)
	}

	view := s.Relation("public", "order_summary")
	if view == nil {
		t.Fatal("no order_summary relation")
	}
	r2, err := AnalyzeView(s, view)
	if err != nil {
		t.Fatalf("AnalyzeView(order_summary): %v", err)
	}
	if len(r2.Columns) == 0 {
		t.Errorf("AnalyzeView(order_summary): want columns, got none")
	}
}

// TestGapsSubStatement covers analyze.go's subStatement Insert/Update/Delete branches
// (the SelectStmt branch is exercised everywhere else). COPY (query) TO accepts a
// data-modifying statement with a RETURNING clause since PG 14, and it is the only
// syntax the parser accepts for any of these three around a nested statement (DECLARE
// CURSOR and CREATE TABLE AS both reject INSERT/UPDATE/DELETE at parse time).
func TestGapsSubStatement(t *testing.T) {
	s := gapsSchema(t, "")
	cases := []string{
		"COPY (INSERT INTO orders (user_id, total) VALUES (1, 1) RETURNING id) TO STDOUT",
		"COPY (UPDATE orders SET total = 1 WHERE id = 1 RETURNING id) TO STDOUT",
		"COPY (DELETE FROM orders WHERE id = 1 RETURNING id) TO STDOUT",
	}
	for _, sql := range cases {
		if _, err := Analyze(s, sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

// TestGapsLocking covers stmt.go's checkLocking / lockStrength / joinHasAlias /
// windowInTargets / srfIn: every restriction CheckSelectLocking applies to a FOR
// UPDATE/SHARE clause, plus the locked-relation name resolution (join, WITH query,
// undefined name) and the three non-FOR-UPDATE locking strengths.
func TestGapsLocking(t *testing.T) {
	s := gapsSchema(t, "")
	errCases := []struct {
		sql  string
		want string
	}{
		{"SELECT DISTINCT id FROM orders FOR UPDATE", "FOR UPDATE is not allowed with DISTINCT clause"},
		{"SELECT id, count(*) FROM orders GROUP BY id FOR UPDATE", "FOR UPDATE is not allowed with GROUP BY clause"},
		{"SELECT count(*) FROM orders FOR UPDATE", "FOR UPDATE is not allowed with aggregate functions"},
		{"SELECT id, row_number() OVER () FROM orders FOR UPDATE", "FOR UPDATE is not allowed with window functions"},
		{"SELECT generate_series(1,3) FROM orders FOR UPDATE", "FOR UPDATE is not allowed with set-returning functions in the target list"},
		{"SELECT * FROM (orders o JOIN users u ON u.id=o.user_id) j FOR UPDATE OF j", "FOR UPDATE cannot be applied to a join"},
		{"SELECT id FROM orders o FOR UPDATE OF nope", `relation "nope" in FOR UPDATE clause not found in FROM clause`},
		{"WITH c AS (SELECT id FROM orders) SELECT * FROM c FOR UPDATE OF c", "FOR UPDATE cannot be applied to a WITH query"},
	}
	for _, c := range errCases {
		if err := gapsErr(t, s, c.sql); !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want substring %q", c.sql, err, c.want)
		}
	}

	okCases := []string{
		// FOR UPDATE OF a plain (non-join) alias inside a join is fine.
		"SELECT o.id FROM orders o JOIN users u ON u.id=o.user_id FOR UPDATE OF o",
		"SELECT id FROM orders FOR SHARE",
		"SELECT id FROM orders FOR KEY SHARE",
		"SELECT id FROM orders FOR NO KEY UPDATE",
	}
	for _, sql := range okCases {
		if _, err := Analyze(s, sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

// TestGapsXMLTable covers xmlTable (stmt.go), untested before this: ordinality
// columns, LATERAL correlation, the column-alias-list arity check, and plain aliasing.
func TestGapsXMLTable(t *testing.T) {
	s := gapsSchema(t, "")

	if _, err := Analyze(s, `SELECT * FROM XMLTABLE('/a/b' PASSING '<a><b>1</b></a>' COLUMNS n FOR ORDINALITY, v text PATH 'text()')`); err != nil {
		t.Errorf("ordinality: %v", err)
	}
	if _, err := Analyze(s, `SELECT * FROM users u, LATERAL XMLTABLE('/a/b' PASSING u.name COLUMNS v text PATH 'text()') AS x(v)`); err != nil {
		t.Errorf("lateral: %v", err)
	}
	if _, err := Analyze(s, `SELECT * FROM XMLTABLE('/a/b' PASSING '<a/>' COLUMNS v text PATH 'text()') AS t(x)`); err != nil {
		t.Errorf("alias: %v", err)
	}
	err := gapsErr(t, s, `SELECT * FROM XMLTABLE('/a/b' PASSING '<a/>' COLUMNS v text DEFAULT 'z' PATH 'text()') AS t(a,b)`)
	if err.Code != "42P10" || !strings.Contains(err.Error(), "XMLTABLE function has 1 columns available but 2 columns specified") {
		t.Errorf("xmltable arity: got %v", err)
	}
}

// TestGapsCycleClause covers checkCycleTypes (stmt.go, untested before this): the mark
// value / default type must unify, and a mismatch is reported at the CYCLE clause.
func TestGapsCycleClause(t *testing.T) {
	s := gapsSchema(t, "")
	const cte = `WITH RECURSIVE t(n) AS (
		SELECT id FROM users
		UNION ALL
		SELECT o.user_id FROM orders o JOIN t ON o.user_id = t.n
	) CYCLE n `

	if _, err := Analyze(s, cte+"SET is_cycle USING path SELECT * FROM t"); err != nil {
		t.Errorf("default bool mark: %v", err)
	}
	if _, err := Analyze(s, cte+"SET is_cycle TO 1 DEFAULT 0 USING path SELECT * FROM t"); err != nil {
		t.Errorf("integer mark/default: %v", err)
	}
	err := gapsErr(t, s, cte+"SET is_cycle TO true DEFAULT 42 USING path SELECT * FROM t")
	if err.Code != "42804" || !strings.Contains(err.Error(), "CYCLE types boolean and integer cannot be matched") {
		t.Errorf("mismatched mark/default: got %v", err)
	}
}

// TestGapsSetOpArmLiteral covers armLiteral (stmt.go): with three or more UNION arms,
// the leftmost arm is itself a set-operation node, so armLiteral's "not a plain SELECT
// arm" guard (sel.Op != SETOP_NONE) applies to it, and only the literal arms are
// checked against the pinned enum type.
func TestGapsSetOpArmLiteral(t *testing.T) {
	s := gapsSchema(t, "")
	if _, err := Analyze(s, "SELECT status FROM orders UNION SELECT 'paid' UNION SELECT 'pending'"); err != nil {
		t.Errorf("three-arm union of valid enum labels: %v", err)
	}
	err := gapsErr(t, s, "SELECT status FROM orders UNION SELECT 'bogus' UNION SELECT 'pending'")
	if err.Code != "22P02" || !strings.Contains(err.Error(), `invalid input value for enum order_status: "bogus"`) {
		t.Errorf("bad enum literal in a non-leftmost union arm: got %v", err)
	}
}

// flagsExtraDDL adds a table with a boolean column and a two-condition partial unique
// index whose predicate combines a BooleanTest and a TypeCast'd equality, used to
// exercise sameExpr's BooleanTest/TypeCast/BoolExpr(OR) branches (card.go) that the
// base schema's single-condition partial index (orders_uid_active) never reaches.
const flagsExtraDDL = `
CREATE TABLE flags (
    id   bigint PRIMARY KEY,
    code text,
    kind text,
    active boolean
);
CREATE UNIQUE INDEX flags_code_active ON flags (code) WHERE active IS TRUE;
CREATE UNIQUE INDEX flags_code_kind ON flags (code) WHERE kind::text = 'x';
CREATE UNIQUE INDEX flags_code_or ON flags (code) WHERE (kind = 'x' OR kind = 'y');
`

// TestGapsCardCardinality covers card.go's outputKeys star-expansion (a subquery's
// SELECT * / alias.* pinned by an outer WHERE) and sameExpr's BooleanTest, TypeCast and
// BoolExpr(OR) partial-index-predicate comparisons; the OR case is a real match (the
// two ORed branches recur structurally identical in the query and the index), while
// AND-of-two-conditions never matches because addQuals pre-splits every top-level AND
// in the query's WHERE into separate conjuncts before sameExpr ever runs (see the note
// below and the bug reported alongside this file).
func TestGapsCardCardinality(t *testing.T) {
	s := gapsSchema(t, flagsExtraDDL)
	cases := []struct {
		sql string
		one bool
		why string
	}{
		// outputKeys: SELECT * over a single (non-join) relation, and over a USING join,
		// both pinned by an outer equality on the subquery's alias.
		{sql: "SELECT s.id FROM (SELECT * FROM users u) s WHERE s.id = $1", one: true},
		{sql: "SELECT s.id FROM (SELECT * FROM users u JOIN orders o USING (id)) s WHERE s.id = $1", one: true},
		// sameExpr: BooleanTest ("active IS TRUE") alone satisfies its partial index.
		{sql: "SELECT id FROM flags WHERE code = $1 AND active IS TRUE", one: true},
		{sql: "SELECT id FROM flags WHERE code = $1 AND active IS FALSE", why: "flags: no unique key"},
		// sameExpr: TypeCast ("kind::text = 'x'") alone satisfies its partial index.
		{sql: "SELECT id FROM flags WHERE code = $1 AND kind::text = 'x'", one: true},
		{sql: "SELECT id FROM flags WHERE code = $1 AND kind::text = 'y'", why: "flags: no unique key"},
		// sameExpr: BoolExpr(OR) — the query repeats the index's OR predicate verbatim.
		{sql: "SELECT id FROM flags WHERE code = $1 AND (kind = 'x' OR kind = 'y')", one: true},
		{sql: "SELECT id FROM flags WHERE code = $1 AND (kind = 'x' OR kind = 'z')", why: "flags: no unique key"},
		{sql: "SELECT id FROM flags WHERE code = $1", why: "flags: no unique key"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if r.AtMostOne != c.one {
			t.Errorf("%s: AtMostOne = %v, want %v (why=%q)", c.sql, r.AtMostOne, c.one, r.ManyRowsWhy)
			continue
		}
		if !c.one && !strings.Contains(r.ManyRowsWhy, c.why) {
			t.Errorf("%s: why = %q, want substring %q", c.sql, r.ManyRowsWhy, c.why)
		}
	}
}

// A partial unique index whose predicate is a conjunction applies when the query repeats
// every conjunct (the query's WHERE is split into conjuncts before matching, so the
// predicate is matched conjunct by conjunct).
func TestGapsCardPartialIndexConjunction(t *testing.T) {
	const extra = `
CREATE TABLE t_and_bug (
    id   bigint PRIMARY KEY,
    code text,
    a    boolean,
    b    text
);
CREATE UNIQUE INDEX t_and_bug_code ON t_and_bug (code) WHERE a IS TRUE AND b = 'x';
`
	s := gapsSchema(t, extra)
	r, err := Analyze(s, "SELECT id FROM t_and_bug WHERE code = $1 AND a IS TRUE AND b = 'x'")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !r.AtMostOne {
		t.Errorf("the query repeats the partial index's whole predicate: want AtMostOne, got %q", r.ManyRowsWhy)
	}
	// half of the predicate is not enough
	r, err = Analyze(s, "SELECT id FROM t_and_bug WHERE code = $1 AND a IS TRUE")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if r.AtMostOne {
		t.Error("only one conjunct of the index predicate is repeated: must not prove AtMostOne")
	}
}
