package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// problems joins a schema's Problems' messages, one per line, for substring assertions.
func problems(s *schema.Schema) string {
	var msgs []string
	for _, p := range s.Problems {
		msgs = append(msgs, p.Message)
	}
	return strings.Join(msgs, "\n")
}

func wantProblem(t *testing.T, sql, want string) {
	t.Helper()
	s := mustLoad(t, sql)
	got := problems(s)
	if want == "" {
		if got != "" {
			t.Errorf("%s\nunexpected problems:\n%s", sql, got)
		}
		return
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s\nwant problem containing %q, got:\n%s", sql, want, got)
	}
}

// --- ALTER TABLE variants ---------------------------------------------------------

func TestAlterTableVariants(t *testing.T) {
	base := `CREATE TABLE t (id int PRIMARY KEY, a int NOT NULL DEFAULT 1, b text, c numeric);`
	s := mustLoad(t, base+`
ALTER TABLE t ALTER COLUMN a DROP NOT NULL;
ALTER TABLE t ALTER COLUMN b SET DEFAULT 'x';
ALTER TABLE t ALTER COLUMN c TYPE numeric(10,2) USING c::numeric(10,2);
ALTER TABLE t ADD CONSTRAINT t_a_check CHECK (a > 0);
ALTER TABLE t DROP CONSTRAINT t_a_check;
ALTER TABLE t RENAME CONSTRAINT t_pkey TO t_pk;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if rel.Column("a").NotNull {
		t.Error("a should not be NOT NULL any more")
	}
	if rel.Column("b").Default == nil {
		t.Error("b should have a default")
	}
	if got := s.Types.Format(rel.Column("c").Type); got != "numeric(10,2)" {
		t.Errorf("c type: %s", got)
	}
	found := false
	for _, c := range rel.Constraints {
		if c.Name == "t_pk" {
			found = true
		}
		if c.Name == "t_a_check" {
			t.Error("t_a_check should have been dropped")
		}
	}
	if !found {
		t.Error("t_pkey should have been renamed to t_pk")
	}
}

func TestAlterTableInheritSchemaOwner(t *testing.T) {
	base := `
CREATE TABLE parent (id int PRIMARY KEY, note text);
CREATE TABLE child (id int PRIMARY KEY, extra text);
CREATE SCHEMA other;
`
	s := mustLoad(t, base+`
ALTER TABLE child INHERIT parent;
ALTER TABLE child NO INHERIT parent;
ALTER TABLE parent OWNER TO someone;
ALTER TABLE parent SET SCHEMA other;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("other", "parent") == nil {
		t.Error("parent should have moved to schema other")
	}
}

func TestAlterTablePartitions(t *testing.T) {
	base := `
CREATE TABLE events (id int, kind text, val int) PARTITION BY LIST (kind);
CREATE TABLE events_a (id int, kind text, val int);
`
	s := mustLoad(t, base+`
ALTER TABLE events ATTACH PARTITION events_a FOR VALUES IN ('a');
ALTER TABLE events DETACH PARTITION events_a;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "events_a").IsPartition {
		t.Error("events_a should no longer be a partition after DETACH")
	}
}

func TestAlterTableErrors(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"ALTER TABLE nope ADD COLUMN x int;", "does not exist"},
		{"CREATE TABLE t (id int); ALTER TABLE t DROP CONSTRAINT nope;", "does not exist"},
		{"CREATE TABLE t (id int) PARTITION BY LIST (id); ALTER TABLE t ATTACH PARTITION nope FOR VALUES IN (1);", "does not exist"},
		{"CREATE TABLE t (id int); ALTER TABLE t INHERIT nope;", "does not exist"},
		{"CREATE TABLE p (id int); CREATE TABLE c (id int) PARTITION BY LIST(id); ALTER TABLE c DROP COLUMN id;", "part of the partition key"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
}

func TestPartitionSpecProblems(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"CREATE TABLE parent (a int); CREATE TABLE t (a int) INHERITS (parent) PARTITION BY RANGE(a);", "inheritance child"},
		{"CREATE TABLE t (a int, b int) PARTITION BY LIST (a, b);", `"list" partition strategy`},
		{"CREATE TABLE t (a int) PARTITION BY RANGE (ctid);", "system column"},
		{"CREATE TABLE t (a int) PARTITION BY RANGE (nope);", "does not exist"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
}

func TestAlterTableDropInheritedColumn(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE parent (id int, note text);
CREATE TABLE child (extra text) INHERITS (parent);
ALTER TABLE child DROP COLUMN note;
`)
	if got := problems(s); !strings.Contains(got, "cannot drop inherited column") {
		t.Errorf("want inherited-column problem, got:\n%s", got)
	}
}

// --- typmodFor edge cases (via CREATE TABLE column types) --------------------------

func TestTypmodErrors(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"CREATE TABLE t (a varchar(0));", "invalid length modifier"},
		{"CREATE TABLE t (a numeric(1001));", "must be between 1 and 1000"},
		{"CREATE TABLE t (a numeric(0));", "must be between 1 and 1000"},
		{"CREATE TABLE t (a int4(3));", "type modifier is not allowed"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
	s := mustLoad(t, `CREATE TABLE t (a bit(3), b varbit(5));`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if got := s.Types.Format(rel.Column("a").Type); got != "bit(3)" {
		t.Errorf("bit(3): %s", got)
	}
	if got := s.Types.Format(rel.Column("b").Type); got != "bit varying(5)" {
		t.Errorf("varbit(5): %s", got)
	}
}

// --- ALTER TYPE (enum) ------------------------------------------------------------

func TestAlterEnum(t *testing.T) {
	base := `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');`
	s := mustLoad(t, base+`
ALTER TYPE mood ADD VALUE 'meh' BEFORE 'ok';
ALTER TYPE mood ADD VALUE 'ecstatic' AFTER 'happy';
ALTER TYPE mood RENAME VALUE 'meh' TO 'blah';
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	st := s.Types.Lookup("", "mood")
	labels := s.Types.Enums[st.OID]
	if strings.Join(labels, ",") != "sad,blah,ok,happy,ecstatic" {
		t.Errorf("labels: %v", labels)
	}
	for _, c := range []struct{ sql, problem string }{
		{base + "ALTER TYPE mood ADD VALUE 'sad';", "already exists"},
		{base + "ALTER TYPE mood ADD VALUE IF NOT EXISTS 'sad';", ""},
		{base + "ALTER TYPE mood ADD VALUE 'x' AFTER 'nope';", "does not exist"},
		{base + "ALTER TYPE mood RENAME VALUE 'nope' TO 'y';", "does not exist"},
		{"ALTER TYPE nope ADD VALUE 'x';", "does not exist"},
		{"CREATE TYPE notenum AS (a int); ALTER TYPE notenum ADD VALUE 'x';", "does not exist"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
}

// --- CREATE DOMAIN extra branches --------------------------------------------------

func TestCreateDomainNamedCheckAndBadBase(t *testing.T) {
	s := mustLoad(t, `CREATE DOMAIN d AS int CONSTRAINT named_check CHECK (VALUE > 0);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	dm := s.Types.Domains[s.Types.Lookup("", "d").OID]
	if len(dm.Checks) != 1 || dm.Checks[0].Name != "named_check" {
		t.Errorf("checks: %+v", dm.Checks)
	}
	wantProblem(t, `CREATE DOMAIN d AS nosuchtype;`, "does not exist")
}

// --- ALTER DOMAIN -----------------------------------------------------------------

func TestAlterDomain(t *testing.T) {
	base := `CREATE DOMAIN pos_int AS int;`
	s := mustLoad(t, base+`
ALTER DOMAIN pos_int SET NOT NULL;
ALTER DOMAIN pos_int ADD CONSTRAINT pos_check CHECK (VALUE > 0);
ALTER DOMAIN pos_int DROP NOT NULL;
ALTER DOMAIN pos_int VALIDATE CONSTRAINT pos_check;
ALTER DOMAIN pos_int DROP CONSTRAINT pos_check;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	d := s.Types.Domains[s.Types.Lookup("", "pos_int").OID]
	if d.NotNull {
		t.Error("NOT NULL should have been dropped")
	}
	if len(d.Checks) != 0 {
		t.Errorf("checks should be empty: %+v", d.Checks)
	}
	for _, c := range []struct{ sql, problem string }{
		{"ALTER DOMAIN nope SET NOT NULL;", "does not exist"},
		{base + "ALTER DOMAIN pos_int DROP CONSTRAINT nope;", "does not exist"},
		{base + "ALTER DOMAIN pos_int DROP CONSTRAINT IF EXISTS nope;", ""},
	} {
		wantProblem(t, c.sql, c.problem)
	}
}

// --- CREATE TYPE base / range, CREATE AGGREGATE / OPERATOR / CAST -----------------

func TestCreateBaseTypeShellAndOpaque(t *testing.T) {
	s := mustLoad(t, `
CREATE TYPE box3d;
CREATE TYPE mytype (INPUT = mytype_in, OUTPUT = mytype_out, LIKE = int4);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Types.Lookup("", "box3d") == nil {
		t.Error("shell type box3d missing")
	}
	mt := s.Types.Lookup("", "mytype")
	if mt == nil {
		t.Fatal("mytype missing")
	}
	if mt.Category != 'N' { // category copied from int4 (LIKE)
		t.Errorf("mytype category: %q", mt.Category)
	}

	wantProblem(t, `CREATE TYPE box3d; CREATE TABLE t (id int); COMMENT ON TABLE nope IS 'x';`, "")
	wantProblem(t, `CREATE TYPE t2 (INPUT = f_in, OUTPUT = f_out, LIKE = nosuchtype);`, "like")
	// createBaseType's "already exists" guard only fires when the conflicting catalog
	// type is looked up under its own (schema-less) name, which happens when the
	// CREATE TYPE is qualified with pg_catalog explicitly.
	wantProblem(t, `CREATE TYPE pg_catalog.int4;`, "already exists")
}

func TestCreateRangeType(t *testing.T) {
	s := mustLoad(t, `CREATE TYPE floatrange AS RANGE (subtype = float8);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Types.Lookup("", "floatrange") == nil {
		t.Error("floatrange missing")
	}
	if s.Types.Lookup("", "floatmultirange") == nil {
		t.Error("multirange missing")
	}
	wantProblem(t, `CREATE TYPE badrange AS RANGE (subtype = nosuchtype);`, "does not exist")
	wantProblem(t, `CREATE TYPE badrange AS RANGE (collation = "C");`, "subtype is required")
}

// TestCreateRangeMultirangeNameCollision covers addRange's moveArrayTypeName step: an
// explicit multirange_type_name that collides with an existing array type's synthesized
// name ("_" + elem) pushes that array type aside to "__" + elem instead.
func TestCreateRangeMultirangeNameCollision(t *testing.T) {
	s := mustLoad(t, `
CREATE TYPE foo AS (a int);
CREATE TYPE r AS RANGE (subtype = int4, multirange_type_name = _foo);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	foo := s.Types.Lookup("", "foo")
	arr := s.Types.ByOID(foo.Array)
	if arr == nil || arr.Name != "__foo" {
		t.Errorf("foo's array type should have been pushed aside to __foo, got %+v", arr)
	}
	if s.Types.Lookup("", "_foo") == nil {
		t.Error("_foo (the multirange) should exist now")
	}
}

func TestCreateAggregate(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION int_add_sfunc(int, int) RETURNS int LANGUAGE sql AS $$ SELECT $1 + $2 $$;
CREATE AGGREGATE my_sum(int) (SFUNC = int_add_sfunc, STYPE = int);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	found := false
	for _, f := range s.Functions {
		if f.Name == "my_sum" && f.IsAgg {
			found = true
		}
	}
	if !found {
		t.Error("my_sum aggregate missing")
	}
	wantProblem(t, `CREATE AGGREGATE bad(int) (SFUNC = nosuchfunc);`, "stype is required")
}

// TestCreateAggregateOldSyntaxAndCategory covers createAggregate's pre-8.2 (BASETYPE /
// STYPE1) spelling and createBaseType's CATEGORY definition element.
func TestCreateAggregateOldSyntaxAndCategory(t *testing.T) {
	s := mustLoad(t, `CREATE AGGREGATE myagg3 (BASETYPE = int4, SFUNC1 = int_add_sfunc, STYPE1 = int4);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	var found *schema.Function
	for _, f := range s.Functions {
		if f.Name == "myagg3" {
			found = f
		}
	}
	if found == nil || len(found.Args) != 1 {
		t.Fatalf("myagg3: %+v", found)
	}

	s2 := mustLoad(t, `CREATE TYPE mytype2 (INPUT = f_in, OUTPUT = f_out, CATEGORY = 'Z');`)
	for _, p := range s2.Problems {
		t.Errorf("problem: %s", p)
	}
	if got := s2.Types.Lookup("", "mytype2").Category; got != 'Z' {
		t.Errorf("category: %q", got)
	}
}

// TestResolveAggFinalPolymorphic exercises resolveAggFinal's three polymorphic-mapping
// branches through a finalfunc whose signature is anyarray / anyelement.
func TestResolveAggFinalPolymorphic(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION final1(anyarray) RETURNS anyarray LANGUAGE sql AS $$ SELECT NULL $$;
CREATE AGGREGATE agg1(*) (SFUNC = sfunc1, STYPE = int4[], FINALFUNC = final1);
CREATE FUNCTION final2(anyelement) RETURNS anyarray LANGUAGE sql AS $$ SELECT NULL $$;
CREATE AGGREGATE agg2(*) (SFUNC = sfunc2, STYPE = int4, FINALFUNC = final2);
CREATE FUNCTION final3(anyarray) RETURNS anyelement LANGUAGE sql AS $$ SELECT NULL $$;
CREATE AGGREGATE agg3(*) (SFUNC = sfunc3, STYPE = int4[], FINALFUNC = final3);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	byName := map[string]*schema.Function{}
	for _, f := range s.Functions {
		byName[f.Name] = f
	}
	if got := s.Types.Format(byName["agg1"].RetType); got != "integer[]" {
		t.Errorf("agg1 (first == ret): %s", got)
	}
	if got := s.Types.Format(byName["agg2"].RetType); got != "integer[]" {
		t.Errorf("agg2 (anyelement state -> anyarray result): %s", got)
	}
	if got := s.Types.Format(byName["agg3"].RetType); got != "integer" {
		t.Errorf("agg3 (anyarray state -> anyelement result): %s", got)
	}
}

func TestCreateOperator(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION plus(int, int) RETURNS int LANGUAGE sql AS $$ SELECT $1 + $2 $$;
CREATE OPERATOR @+ (LEFTARG = int, RIGHTARG = int, FUNCTION = plus);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	found := false
	for _, op := range s.Operators {
		if op.Name == "@+" {
			found = true
		}
	}
	if !found {
		t.Error("operator @+ missing")
	}
	wantProblem(t, `CREATE OPERATOR ~~~ (RIGHTARG = int, FUNCTION = nosuchfunc);`, "does not exist")
	wantProblem(t, `CREATE OPERATOR ~~~ (LEFTARG = int, FUNCTION = f);`, "rightarg is required")
}

func TestCreateCast(t *testing.T) {
	s := mustLoad(t, `
CREATE FUNCTION int_to_text(int) RETURNS text LANGUAGE sql AS $$ SELECT $1::text $$;
CREATE CAST (int AS text) WITH FUNCTION int_to_text(int) AS ASSIGNMENT;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Casts) != 1 || s.Casts[0].Context != 'a' {
		t.Errorf("cast: %+v", s.Casts)
	}
}

// --- DROP variants ------------------------------------------------------------------

func TestDropVariants(t *testing.T) {
	base := `
CREATE TABLE t (id int PRIMARY KEY);
CREATE VIEW v AS SELECT * FROM t;
CREATE INDEX t_idx ON t (id);
CREATE TYPE mood AS ENUM ('a', 'b');
CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
`
	s := mustLoad(t, base+`
DROP VIEW v;
DROP INDEX t_idx;
DROP FUNCTION f();
DROP TYPE mood;
DROP TABLE t;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "t") != nil || s.Relation("", "v") != nil {
		t.Error("t and v should be gone")
	}
	if s.Types.Lookup("", "mood") != nil {
		t.Error("mood should be gone")
	}
	for _, f := range s.Functions {
		if f.Name == "f" {
			t.Error("f should be gone")
		}
	}
}

func TestDropErrors(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"DROP TABLE nope;", "does not exist"},
		{"DROP TABLE IF EXISTS nope;", ""},
		{"DROP INDEX nope;", "does not exist"},
		{"DROP TYPE nope;", "does not exist"},
		{"DROP FUNCTION nope();", "does not exist"},
		{"CREATE TABLE t (id int) PARTITION BY LIST(id); CREATE FUNCTION keyf() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;", ""},
	} {
		wantProblem(t, c.sql, c.problem)
	}
	// DROP TYPE without CASCADE when a column depends on it.
	wantProblem(t, `
CREATE TYPE mood AS ENUM ('a', 'b');
CREATE TABLE t (id int, m mood);
DROP TYPE mood;
`, "other objects depend on it")
	// ... and with CASCADE it goes through, taking the column with it.
	s := mustLoad(t, `
CREATE TYPE mood AS ENUM ('a', 'b');
CREATE TABLE t (id int, m mood);
DROP TYPE mood CASCADE;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "t").Column("m") != nil {
		t.Error("column m should have been dropped along with its type")
	}
}

func TestDropSchemaCascade(t *testing.T) {
	s := mustLoad(t, `
CREATE SCHEMA s;
CREATE TABLE s.t (id int);
CREATE FUNCTION s.f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
DROP SCHEMA s CASCADE;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("s", "t") != nil {
		t.Error("s.t should be gone")
	}
	if s.HasSchema("s") {
		t.Error("schema s should be forgotten")
	}
}

func TestDropCascadeViews(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (id int);
CREATE VIEW v1 AS SELECT * FROM t;
CREATE VIEW v2 AS SELECT * FROM v1;
DROP TABLE t CASCADE;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "v1") != nil || s.Relation("", "v2") != nil {
		t.Error("both dependent views should be gone")
	}
}

// --- RENAME variants ------------------------------------------------------------

func TestRenameVariants(t *testing.T) {
	base := `
CREATE TABLE t (id int PRIMARY KEY, a int);
CREATE INDEX t_a_idx ON t (a);
CREATE TYPE mood AS ENUM ('a');
CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE TRIGGER trg AFTER INSERT ON t EXECUTE FUNCTION f();
`
	s := mustLoad(t, base+`
ALTER TABLE t RENAME COLUMN a TO b;
ALTER INDEX t_a_idx RENAME TO t_b_idx;
ALTER TYPE mood RENAME TO temperament;
ALTER FUNCTION f() RENAME TO g;
ALTER TRIGGER trg ON t RENAME TO trg2;
ALTER TABLE t RENAME TO renamed;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "renamed")
	if rel == nil {
		t.Fatal("renamed table missing")
	}
	if rel.Column("b") == nil {
		t.Error("column b missing")
	}
	if rel.Indexes[0].Name != "t_b_idx" {
		t.Errorf("index name: %s", rel.Indexes[0].Name)
	}
	if s.Types.Lookup("", "temperament") == nil {
		t.Error("temperament type missing")
	}
	if s.Function("", "g") == nil {
		t.Error("function g missing")
	}
	if s.Triggers[0].Name != "trg2" {
		t.Errorf("trigger name: %s", s.Triggers[0].Name)
	}
}

func TestRenameErrors(t *testing.T) {
	for _, c := range []struct{ sql, problem string }{
		{"ALTER TABLE nope RENAME TO renamed;", "does not exist"},
		{"ALTER TABLE nope RENAME COLUMN a TO b;", "does not exist"},
		{"CREATE TABLE t (id int); ALTER TABLE t RENAME COLUMN nope TO b;", "does not exist"},
		{"CREATE TABLE t (id int); ALTER TABLE t RENAME CONSTRAINT nope TO other;", "does not exist"},
		{"ALTER TABLE nope RENAME CONSTRAINT a TO b;", "does not exist"},
		{"ALTER TYPE nope RENAME TO other;", "does not exist"},
		{"ALTER FUNCTION nope() RENAME TO other;", "does not exist"},
		{"ALTER INDEX nope RENAME TO other;", "does not exist"},
	} {
		wantProblem(t, c.sql, c.problem)
	}
}

// --- COMMENT ON --------------------------------------------------------------------

func TestCommentOn(t *testing.T) {
	base := `
CREATE TABLE t (id int, note text);
CREATE VIEW v AS SELECT * FROM t;
CREATE TYPE mood AS ENUM ('a');
CREATE DOMAIN pos_int AS int;
`
	s := mustLoad(t, base+`
COMMENT ON TABLE t IS 'a table';
COMMENT ON COLUMN t.note IS 'a note';
COMMENT ON VIEW v IS 'a view';
COMMENT ON TYPE mood IS 'a mood';
COMMENT ON DOMAIN pos_int IS 'a domain';
COMMENT ON TABLE t IS NULL;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Comments["t"] != "" {
		t.Error("comment on t should have been cleared by IS NULL")
	}
	if s.Comments["t.note"] != "a note" {
		t.Errorf("column comment: %q", s.Comments["t.note"])
	}
	if s.Comments["v"] != "a view" {
		t.Errorf("view comment: %q", s.Comments["v"])
	}
	if s.Comments["type:mood"] != "a mood" {
		t.Errorf("type comment: %q", s.Comments["type:mood"])
	}
	if s.Comments["type:pos_int"] != "a domain" {
		t.Errorf("domain comment: %q", s.Comments["type:pos_int"])
	}
}

// --- SET variable / search_path -----------------------------------------------------

func TestSetVariables(t *testing.T) {
	s := mustLoad(t, `
SET datestyle = 'ISO, DMY';
SET intervalstyle = 'postgres_verbose';
SET timezone = 'UTC';
SET xmloption = document;
SET restrict_nonsystem_relation_kind = 'view';
CREATE SCHEMA a;
SET search_path = a, public;
CREATE TABLE t (id int);
RESET search_path;
CREATE TABLE t2 (id int);
RESET ALL;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	order, interval, tz := s.DateTimeSettings()
	if order != "dmy" || interval != "postgres_verbose" || tz != "UTC" {
		t.Errorf("datetime settings: %q %q %q", order, interval, tz)
	}
	if !s.XMLOptionDocument() {
		t.Error("xmloption should be document")
	}
	if !s.ViewsRestricted() {
		t.Error("views should be restricted")
	}
	if s.Relation("a", "t") == nil {
		t.Error("t should have landed in schema a")
	}
	if s.Relation("public", "t2") == nil {
		t.Error("t2 should have landed in public after RESET search_path")
	}
	if len(s.SearchPath()) != 1 || s.SearchPath()[0] != "public" {
		t.Errorf("search path after RESET ALL: %v", s.SearchPath())
	}
}

// --- quoteIdent (via Types.Format) --------------------------------------------------

func TestQuotedIdentifiers(t *testing.T) {
	s := mustLoad(t, `CREATE TYPE "MyType" AS (a int);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	got := s.Types.Format(schema.TypeRef{OID: s.Types.Lookup("", "MyType").OID, Typmod: -1})
	if got != `"MyType"` {
		t.Errorf("quoted type name: %q", got)
	}
}

// --- Deparse / DeparseStmt / DeparseBody --------------------------------------------

func TestDeparseFunctions(t *testing.T) {
	s := mustLoad(t, `
CREATE VIEW v AS SELECT 1 AS x;
CREATE FUNCTION f() RETURNS int LANGUAGE sql RETURN 1 + 1;
CREATE FUNCTION g() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT 2; END;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "v")
	if got := schema.DeparseStmt(rel.Query); !strings.Contains(got, "SELECT") {
		t.Errorf("DeparseStmt: %q", got)
	}
	f := s.Function("", "f")
	if got := schema.DeparseBody(f.SQLBody); !strings.Contains(got, "RETURN") {
		t.Errorf("DeparseBody RETURN form: %q", got)
	}
	g := s.Function("", "g")
	if got := schema.DeparseBody(g.SQLBody); !strings.Contains(got, "BEGIN ATOMIC") {
		t.Errorf("DeparseBody BEGIN ATOMIC form: %q", got)
	}
	if schema.DeparseBody(nil) != "" {
		t.Error("DeparseBody(nil) should be empty")
	}
	if schema.DeparseStmt(nil) != "" {
		t.Error("DeparseStmt(nil) should be empty")
	}
	if schema.Deparse(nil) != "" {
		t.Error("Deparse(nil) should be empty")
	}
}

// --- CREATE TABLE AS / SELECT INTO --------------------------------------------------

func TestCreateTableAs(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (id int, note text);
CREATE TABLE t2 AS SELECT id, note FROM t;
CREATE TABLE t3 (like_id) AS SELECT id FROM t;
SELECT id INTO t4 FROM t;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "t2") == nil {
		t.Error("t2 missing")
	}
	if c := s.Relation("", "t3").Column("like_id"); c == nil {
		t.Error("t3.like_id missing (renamed by INTO column list)")
	}
	if s.Relation("", "t4") == nil {
		t.Error("t4 (SELECT INTO) missing")
	}
	wantProblem(t, `CREATE TABLE t (id int); CREATE TABLE t AS SELECT 1;`, "already exists")
	wantProblem(t, `CREATE TABLE t2 AS SELECT * FROM nosuchtable;`, "CREATE TABLE AS")
}

func TestCreateTableAsOnCommitDrop(t *testing.T) {
	s := mustLoad(t, `
BEGIN;
CREATE TEMP TABLE t3 ON COMMIT DROP AS SELECT 1 AS x;
COMMIT;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Relation("", "t3") != nil {
		t.Error("t3 should have been dropped at COMMIT (ON COMMIT DROP)")
	}
}

// --- ALTER SEQUENCE ------------------------------------------------------------------

func TestAlterSequenceOwnedBy(t *testing.T) {
	s := mustLoad(t, `
CREATE SEQUENCE s;
CREATE TABLE t (id int);
ALTER SEQUENCE s OWNED BY t.id;
ALTER SEQUENCE s OWNED BY NONE;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	wantProblem(t, `ALTER SEQUENCE nope OWNED BY NONE;`, "does not exist")
}

// --- chooseIndexName / makeObjectName: collision numbering and long-name truncation -

func TestChooseIndexNameCollision(t *testing.T) {
	s := mustLoad(t, `
CREATE TABLE t (id int CONSTRAINT t_id_idx UNIQUE);
CREATE INDEX ON t (id);
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if len(rel.Indexes) != 1 || rel.Indexes[0].Name != "t_id_idx1" {
		t.Errorf("the auto-named index should have been numbered around the taken name: %+v", rel.Indexes)
	}
}

func TestChooseIndexNameLongIdentifiers(t *testing.T) {
	longTable := strings.Repeat("t", 60)
	longCol := strings.Repeat("c", 60)
	s := mustLoad(t, `CREATE TABLE `+longTable+` (`+longCol+` int); CREATE INDEX ON `+longTable+` (`+longCol+`);`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", longTable)
	if len(rel.Indexes) != 1 || len(rel.Indexes[0].Name) >= 64 || !strings.HasSuffix(rel.Indexes[0].Name, "_idx") {
		t.Errorf("truncated index name: %q (len %d)", rel.Indexes[0].Name, len(rel.Indexes[0].Name))
	}
}
