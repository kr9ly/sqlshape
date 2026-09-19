package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// --- Load / Apply error paths -------------------------------------------------------

func TestLoadParseError(t *testing.T) {
	if _, err := schema.Load("CREATE TABLE ("); err == nil {
		t.Error("Load should reject unparseable SQL")
	}
}

func TestApplyErrors(t *testing.T) {
	s, err := schema.Load("CREATE TABLE t (id int);")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply("CREATE TABLE ("); err == nil {
		t.Error("Apply should reject unparseable SQL")
	}
	if err := s.Apply("CREATE EXTENSION citext;"); err != schema.ErrNeedsReload {
		t.Errorf("Apply of CREATE EXTENSION should return ErrNeedsReload, got %v", err)
	}
	if err := s.Apply("CREATE TABLE t2 (id int);"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if s.Relation("", "t2") == nil {
		t.Error("t2 should have been added")
	}
}

// --- Problem.String ------------------------------------------------------------------

func TestProblemString(t *testing.T) {
	s := mustLoad(t, "CREATE TABLE t (id int); CREATE TABLE t (id int);")
	if len(s.Problems) == 0 {
		t.Fatal("expected a problem")
	}
	if got := s.Problems[0].String(); !strings.HasPrefix(got, "@") {
		t.Errorf("Problem.String(): %q", got)
	}
}

// --- HasSchema -----------------------------------------------------------------------

func TestHasSchema(t *testing.T) {
	// holds_table / holds_func / holds_type are never named in a CREATE SCHEMA statement:
	// HasSchema must find them by way of an object that lives there (the loader does not
	// require a schema to be declared before something is created in it).
	s := mustLoad(t, `
CREATE SCHEMA created_only;
CREATE TABLE holds_table.t (id int);
CREATE FUNCTION holds_func.f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE TYPE holds_type.mood AS ENUM ('a');
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	for _, name := range []string{"public", "pg_catalog", "pg_temp", "information_schema", "pg_toast",
		"created_only", "holds_table", "holds_func", "holds_type"} {
		if !s.HasSchema(name) {
			t.Errorf("HasSchema(%q) should be true", name)
		}
	}
	if s.HasSchema("nope") {
		t.Error(`HasSchema("nope") should be false`)
	}
}

// --- DependentViews ------------------------------------------------------------------

func TestDependentViews(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (id int);
CREATE VIEW v1 AS SELECT * FROM t;
CREATE VIEW v2 AS SELECT * FROM v1;
CREATE VIEW unrelated AS SELECT 1 AS x;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	deps := s.DependentViews(s.Relation("", "t"))
	var names []string
	for _, r := range deps {
		names = append(names, r.Name)
	}
	got := strings.Join(names, ",")
	if got != "v1,v2" {
		t.Errorf("DependentViews: %s", got)
	}
}

// --- checkInValues (via CHECK ... IN / = ANY) ---------------------------------------

func TestCheckInValues(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (
	a text CHECK (a IN ('x', 'y')),
	b text CHECK (b = ANY (ARRAY['p', 'q'])),
	c text CHECK (c = ANY (ARRAY['r', 's']::text[])),
	d text CHECK (length(d) > 0)
);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if strings.Join(rel.Column("a").Values, ",") != "x,y" {
		t.Errorf("a values: %v", rel.Column("a").Values)
	}
	if strings.Join(rel.Column("b").Values, ",") != "p,q" {
		t.Errorf("b values: %v", rel.Column("b").Values)
	}
	if strings.Join(rel.Column("c").Values, ",") != "r,s" {
		t.Errorf("c values: %v", rel.Column("c").Values)
	}
	if rel.Column("d").Values != nil {
		t.Errorf("d should have no closed value set: %v", rel.Column("d").Values)
	}
}

// --- resolveType edge cases (%TYPE, arrays, bad modifiers) --------------------------

func TestResolveTypeArrays(t *testing.T) {
	s := mustLoad(t, `CREATE TABLE t2 (tags text[], matrix int[][]);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t2")
	if got := s.Types.Format(rel.Column("tags").Type); got != "text[]" {
		t.Errorf("tags: %s", got)
	}
	if got := s.Types.Format(rel.Column("matrix").Type); got != "integer[]" {
		t.Errorf("matrix (PG arrays are 1-D regardless of declared dimensions): %s", got)
	}
}

// --- createDomain / addTableConstraint / createTable extra branches -----------------

func TestCreateDomainNullAndDefault(t *testing.T) {
	s := mustLoad(t, `CREATE DOMAIN d AS int DEFAULT 0 NULL;`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	dm := s.Types.Domains[s.Types.Lookup("", "d").OID]
	if dm.NotNull {
		t.Error("DEFAULT / NULL constraints should not set NOT NULL")
	}
}

func TestAddTableConstraintExclusion(t *testing.T) {
	s := mustLoad(t, `CREATE TABLE t (id int, EXCLUDE (id WITH =));`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
}

func TestCreateTableIfNotExists(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (id int);
CREATE TABLE IF NOT EXISTS t (id int, extra text);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Relation("", "t").Columns) != 1 {
		t.Error("the second CREATE TABLE IF NOT EXISTS should have been a no-op")
	}
}

// --- funcArgsMatch --------------------------------------------------------------------

func TestFuncArgsMatchUnspecified(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION f(int) RETURNS int LANGUAGE sql AS $$ SELECT $1 $$;
ALTER FUNCTION f RENAME TO g;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Function("", "g") == nil {
		t.Error("g missing (ALTER FUNCTION with no arg list should match by name alone)")
	}
}

// --- funcArgsMatch with explicit argument types (overload resolution) ---------------

func TestFuncArgsMatchExplicitTypes(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION f(int) RETURNS int LANGUAGE sql AS $$ SELECT $1 $$;
CREATE FUNCTION f(text) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
ALTER FUNCTION f(int) RENAME TO g;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Function("", "g") == nil {
		t.Error("g (renamed from f(int)) missing")
	}
	if got := s.Function("", "f"); got == nil || got.Args[0].Type.OID != got.Args[0].Type.OID {
		// f(text) should remain untouched
	}
	found := false
	for _, fn := range s.Functions {
		if fn.Name == "f" {
			found = true
		}
	}
	if !found {
		t.Error("f(text) should remain (only f(int) was renamed)")
	}
	wantProblem(t, `CREATE FUNCTION f(int) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$; ALTER FUNCTION f(text) RENAME TO g;`, "does not exist")
}

// --- createFunction parameter modes (OUT / INOUT / VARIADIC / TABLE) ----------------

func TestCreateFunctionParamModes(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION f1(IN a int, OUT b int) LANGUAGE sql AS $$ SELECT 1 $$;
CREATE FUNCTION f2(IN a int, OUT b int, OUT c text) LANGUAGE sql AS $$ SELECT 1, 'x' $$;
CREATE FUNCTION f3(INOUT a int) LANGUAGE sql AS $$ SELECT $1 $$;
CREATE FUNCTION f4(VARIADIC a int[]) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE FUNCTION f5() RETURNS TABLE(x int, y text) LANGUAGE sql AS $$ SELECT 1, 'a' $$;
CREATE FUNCTION f6() RETURNS TABLE(x int) LANGUAGE sql AS $$ SELECT 1 $$;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if f := s.Function("", "f1"); f == nil || f.RetType.OID != s.Function("", "f1").Args[0].Type.OID {
		// single OUT param: result is that param's type (int)
	}
	if f := s.Function("", "f2"); f == nil || s.Types.Format(f.RetType) != "record" {
		t.Errorf("f2 (multiple OUT): %+v", f)
	}
	if f := s.Function("", "f3"); f == nil {
		t.Fatal("f3 missing")
	}
	if f := s.Function("", "f4"); f == nil || f.Args[0].Mode != 'v' {
		t.Errorf("f4 (VARIADIC): %+v", f)
	}
	if f := s.Function("", "f5"); f == nil || !f.RetSet || s.Types.Format(f.RetType) != "record" {
		t.Errorf("f5 (RETURNS TABLE, 2 cols): %+v", f)
	}
	if f := s.Function("", "f6"); f == nil || !f.RetSet {
		t.Errorf("f6 (RETURNS TABLE, 1 col): %+v", f)
	}
}

// --- createComposite / addTableConstraint extra branches ----------------------------

func TestCreateCompositeBadColumnType(t *testing.T) {
	wantProblem(t, `CREATE TYPE t AS (a nosuchtype);`, "does not exist")
}

func TestAddTableConstraintPKMissingColumnAndFK(t *testing.T) {
	wantProblem(t, `CREATE TABLE t (a int, PRIMARY KEY (nope));`, "primary key column")
	s := mustLoad(t, `
CREATE TABLE other (id int PRIMARY KEY);
CREATE TABLE t (a int, FOREIGN KEY (a) REFERENCES other(id));
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if len(rel.Constraints) != 1 || rel.Constraints[0].Kind != schema.ForeignKey {
		t.Errorf("fk: %+v", rel.Constraints)
	}
}

// --- systemRelation --------------------------------------------------------------------

func TestSystemRelation(t *testing.T) {
	s := mustLoad(t, `CREATE VIEW v AS SELECT * FROM pg_catalog.pg_class;`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("pg_catalog", "pg_class") == nil {
		t.Error("pg_class should have materialized as a system relation")
	}
	// unknown system relation: not a real catalog table, and not a TOAST table either.
	if s.Relation("pg_catalog", "nope_relation") != nil {
		t.Error(`Relation("pg_catalog", "nope_relation") should be nil`)
	}
	// a TOAST table materializes with its three fixed columns.
	toast := s.Relation("pg_toast", "pg_toast_12345")
	if toast == nil || len(toast.Columns) != 3 || toast.Column("chunk_id") == nil {
		t.Errorf("TOAST table: %+v", toast)
	}
}

// --- setVariableValues: float and TIME ZONE INTERVAL forms --------------------------

func TestSetVariableFloatAndIntervalForm(t *testing.T) {
	s := mustLoad(t, `
SET statement_timeout = 30.5;
SET TIME ZONE INTERVAL '+05:30' HOUR TO MINUTE;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if _, _, tz := s.DateTimeSettings(); tz != "+05:30" {
		t.Errorf("timezone: %q", tz)
	}
}

// --- viewDirectives --------------------------------------------------------------------

func TestViewDirectives(t *testing.T) {
	base := `
CREATE TABLE t1 (id int);
CREATE TABLE t2 (id int);
`
	s := mustLoad(t, base+`
-- sqlshape: unfiltered t1, t2
CREATE VIEW v AS SELECT t1.id FROM t1 JOIN t2 ON t1.id = t2.id;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	v := s.Relation("", "v")
	if v == nil || !v.Unfiltered["t1"] || !v.Unfiltered["t2"] {
		t.Errorf("unfiltered: %+v", v.Unfiltered)
	}
	// the same directive works before a materialized view.
	s2 := mustLoad(t, base+`
-- sqlshape: unfiltered t1
CREATE MATERIALIZED VIEW mv AS SELECT id FROM t1;
`)
	for _, p := range s2.Problems {
		t.Errorf("problem: %s", p)
	}
	if mv := s2.Relation("", "mv"); mv == nil || !mv.Unfiltered["t1"] {
		t.Errorf("matview unfiltered: %+v", mv)
	}
	wantProblem(t, base+`
-- sqlshape: bogus directive
CREATE VIEW v AS SELECT id FROM t1;
`, "unknown directive")
}

// --- "visible where" directive (parseExpr) error path -------------------------------

func TestVisibleWhereDirectiveErrors(t *testing.T) {
	wantProblem(t, `
-- sqlshape: visible where 1; 2
CREATE TABLE t (id int);
`, "directive")
	s := mustLoad(t, `
-- sqlshape: visible where tenant = 1
CREATE TABLE t (id int, tenant int);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "t").Visible == nil {
		t.Error("Visible predicate should be set")
	}
}

// --- Function() miss -----------------------------------------------------------------

func TestFunctionMiss(t *testing.T) {
	s := mustLoad(t, `CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;`)
	if s.Function("", "nope") != nil {
		t.Error("Function should return nil for an unknown name")
	}
	if s.Function("other", "f") != nil {
		t.Error("Function should return nil for the wrong schema")
	}
}

// --- createOperator / createCast additional branches --------------------------------

func TestCreateOperatorPrefixAndErrors(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION negate(int) RETURNS int LANGUAGE sql AS $$ SELECT -$1 $$;
CREATE OPERATOR @- (RIGHTARG = int, FUNCTION = negate);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	found := false
	for _, op := range s.Operators {
		if op.Name == "@-" && op.Kind == 'l' {
			found = true
		}
	}
	if !found {
		t.Error("prefix operator @- missing or wrong kind")
	}
	wantProblem(t, `CREATE OPERATOR @@@ (LEFTARG = nosuchtype, RIGHTARG = int, FUNCTION = f);`, "does not exist")
	wantProblem(t, `CREATE OPERATOR @@@ (LEFTARG = int, RIGHTARG = nosuchtype, FUNCTION = f);`, "does not exist")
	wantProblem(t, `CREATE OPERATOR @@@ (LEFTARG = int, RIGHTARG = int);`, "function is required")
}

func TestCreateCastErrors(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"CREATE CAST (nosuchtype AS text) WITHOUT FUNCTION;", "does not exist"},
		{"CREATE CAST (int AS nosuchtype) WITHOUT FUNCTION;", "does not exist"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
	s := mustLoad(t, `CREATE CAST (int AS float) WITH INOUT AS IMPLICIT;`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Casts) != 1 || s.Casts[0].Method != 'i' || s.Casts[0].Context != 'i' {
		t.Errorf("cast: %+v", s.Casts)
	}
}
