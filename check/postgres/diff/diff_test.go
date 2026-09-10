package diff

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

func load(t *testing.T, sql string) *schema.Schema {
	t.Helper()
	s, err := analyze.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	return s
}

func compare(t *testing.T, from, to string) string {
	t.Helper()
	a, err := analyze.Load(from)
	if err != nil {
		t.Fatal(err)
	}
	b, err := analyze.Load(to)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(a.Problems, b.Problems...) {
		t.Fatalf("schema problem: %s", p)
	}
	var lines []string
	for _, c := range Compare(a, b) {
		lines = append(lines, c.String())
	}
	return strings.Join(lines, "\n")
}

func TestCompare(t *testing.T) {
	base := `
CREATE SCHEMA app;
CREATE TYPE status AS ENUM ('a', 'b');
CREATE DOMAIN yen AS numeric CHECK (VALUE >= 0);
CREATE TYPE pair AS (x int, y int);
CREATE TABLE t (
  id bigint PRIMARY KEY,
  name text NOT NULL DEFAULT '',
  s status NOT NULL,
  price yen,
  CONSTRAINT t_price_check CHECK (price > 1)
);
CREATE INDEX t_name_idx ON t (name);
CREATE VIEW v AS SELECT id, name FROM t;
CREATE FUNCTION f(p int) RETURNS int LANGUAGE sql IMMUTABLE RETURN p + 1;
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER t_trg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION trg();
COMMENT ON TABLE t IS 'things';
COMMENT ON COLUMN t.name IS 'the name';
`
	cases := []struct {
		name, edit, want string
	}{
		{"identical", "", ""},
		{"add table", "CREATE TABLE app.u (id int);", "+ table app.u"},
		{"drop column", "ALTER TABLE t DROP COLUMN price;",
			"~ table t\n    column order: id, name, s, price -> id, name, s\n- column t.price\n- constraint t.t_price_check"},
		{"alter column", "ALTER TABLE t ALTER COLUMN name TYPE varchar(20), ALTER COLUMN name DROP NOT NULL, ALTER COLUMN name SET DEFAULT 'x';",
			"~ column t.name\n    default: '' -> 'x'\n    not null: true -> false\n    type: text -> character varying(20)"},
		{"rename column is drop + add", "ALTER TABLE t RENAME COLUMN name TO title;",
			"~ table t\n    column order: id, name, s, price -> id, title, s, price\n- column t.name\n+ column t.title\n~ index t.t_name_idx\n    columns: name -> title"},
		{"constraint", "ALTER TABLE t DROP CONSTRAINT t_price_check, ADD CONSTRAINT t_price_check CHECK (price > 2), ADD UNIQUE (name);",
			"+ constraint t.t_name_key\n~ constraint t.t_price_check\n    check: price > 1 -> price > 2"},
		{"index", "DROP INDEX t_name_idx; CREATE UNIQUE INDEX t_name_idx ON t (name) WHERE name <> '';",
			"~ index t.t_name_idx\n    unique: false -> true\n    where:  -> name <> ''"},
		{"enum label", "ALTER TYPE status ADD VALUE 'c';", "~ enum status\n    labels: a, b -> a, b, c"},
		{"domain", "ALTER DOMAIN yen SET NOT NULL;", "~ domain yen\n    not null: false -> true"},
		{"composite", "ALTER TYPE pair ADD ATTRIBUTE z int;",
			"~ composite pair\n    attributes: x, y -> x, y, z\n    attribute z:  -> integer"},
		{"view", "CREATE OR REPLACE VIEW v AS SELECT id, name, s FROM t;",
			"~ view v\n    column order: id, name -> id, name, s\n    column types: bigint, text -> bigint, text, status\n    query: SELECT id, name FROM t -> SELECT id, name, s FROM t\n+ column v.s"},
		{"function body", "CREATE OR REPLACE FUNCTION f(p int) RETURNS int LANGUAGE sql STABLE RETURN p + 2;",
			"~ function f(integer)\n    body: RETURN p + 1 -> RETURN p + 2\n    volatility: i -> s"},
		{"function signature", "DROP FUNCTION f(int); CREATE FUNCTION f(p bigint) RETURNS int LANGUAGE sql RETURN 1;",
			"- function f(integer)\n+ function f(bigint)"},
		{"trigger", "DROP TRIGGER t_trg ON t; CREATE TRIGGER t_trg BEFORE INSERT OR UPDATE OF name ON t FOR EACH ROW EXECUTE FUNCTION trg();",
			"~ trigger t.t_trg\n    events: insert -> insert or update of name"},
		{"comment", "COMMENT ON TABLE t IS 'stuff'; COMMENT ON COLUMN t.name IS NULL; COMMENT ON COLUMN t.id IS 'pk';",
			"- comment t.name\n~ comment t\n    text: things -> stuff\n+ comment t.id"},
		{"extension", "CREATE EXTENSION citext;", "+ extension citext"},
		{"row security", "ALTER TABLE t ENABLE ROW LEVEL SECURITY; CREATE POLICY mine ON t USING (id > 0);",
			"~ table t\n    row security:  -> enabled\n+ policy t.mine"},
		{"drop schema", "DROP SCHEMA app;", "- schema app"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := compare(t, base, base+"\n"+c.edit)
			if got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
			// the reverse direction swaps + and - and From / To
			if c.edit != "" && compare(t, base+"\n"+c.edit, base) == "" {
				t.Errorf("reverse comparison is empty")
			}
		})
	}
}

func TestCompareRows(t *testing.T) {
	base := `
CREATE TABLE statuses (code text PRIMARY KEY, label text NOT NULL, sort_order int NOT NULL DEFAULT 0, note text);
`
	rows := func(additive bool, rows string) string {
		d := ""
		if additive {
			d = "-- sqlshape: seed\n"
		}
		return base + d + "INSERT INTO statuses (code, label, sort_order) VALUES " + rows + ";"
	}
	pending := "('pending', 'Pending', 10)"
	cases := []struct {
		name, from, to, want string
	}{
		{"same", rows(false, pending), rows(false, pending), ""},
		{"target not seeded", rows(false, pending), base, ""},
		{"new seed", base, rows(false, pending), "~ rows statuses\n    row 'pending':  -> 'pending', 'Pending', 10"},
		{"changed and added", rows(false, pending), rows(false, "('pending', 'Waiting', 10), ('paid', 'Paid', 20)"),
			"~ rows statuses\n    row 'pending': 'pending', 'Pending', 10 -> 'pending', 'Waiting', 10\n    row 'paid':  -> 'paid', 'Paid', 20"},
		{"removed", rows(false, "('pending', 'Pending', 10), ('paid', 'Paid', 20)"), rows(false, pending),
			"~ rows statuses\n    row 'paid': 'paid', 'Paid', 20 -> "},
		{"additive keeps extra rows", rows(false, "('pending', 'Pending', 10), ('paid', 'Paid', 20)"), rows(true, pending), ""},
		{"undeclared column does not count", base + "INSERT INTO statuses (code, label, sort_order, note) VALUES ('pending', 'Pending', 10, 'x');", rows(false, pending), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compare(t, c.from, c.to); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// TestTypesKindChange covers the branch of types() where a name survives from one
// schema to the next but changes what kind of type it is (a drop of the old kind and an
// add of the new, not an Alter).
func TestTypesKindChange(t *testing.T) {
	from := load(t, "CREATE DOMAIN d AS integer;")
	to := load(t, "CREATE TYPE d AS ENUM ('a', 'b');")
	var lines []string
	for _, c := range Compare(from, to) {
		lines = append(lines, c.String())
	}
	got := strings.Join(lines, "\n")
	want := "- domain d\n+ enum d"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRelPropsPartitionAndSequence covers relProps' partition-of / inherits / of-type /
// sequence owned-by / forced row-security branches, none of which the main table
// (identical) or dedicated row-security test case reach.
func TestRelPropsPartitionAndSequence(t *testing.T) {
	t.Run("partition of", func(t *testing.T) {
		base := `
CREATE TABLE p (id int, d date) PARTITION BY RANGE (d);
CREATE TABLE p_2024 PARTITION OF p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
`
		s := load(t, base)
		r := s.Relation("", "p_2024")
		if r == nil {
			t.Fatal("partition p_2024 not found")
		}
		props := relProps(s, r)
		if props["partition of"] != "p" {
			t.Errorf("partition of = %q, want %q", props["partition of"], "p")
		}
		parent := s.Relation("", "p")
		if got := relProps(s, parent)["partition key"]; got != "d" {
			t.Errorf("partition key = %q, want %q", got, "d")
		}
	})
	t.Run("inherits", func(t *testing.T) {
		s := load(t, "CREATE TABLE base (id int); CREATE TABLE child (extra text) INHERITS (base);")
		r := s.Relation("", "child")
		if got := relProps(s, r)["inherits"]; got != "base" {
			t.Errorf("inherits = %q, want %q", got, "base")
		}
	})
	t.Run("of type", func(t *testing.T) {
		s := load(t, "CREATE TYPE pt AS (x int, y int); CREATE TABLE p1 OF pt;")
		r := s.Relation("", "p1")
		if got := relProps(s, r)["of type"]; got != "pt" {
			t.Errorf("of type = %q, want %q", got, "pt")
		}
	})
	t.Run("sequence owned by", func(t *testing.T) {
		s := load(t, "CREATE TABLE t (id bigserial);")
		seq := s.Relation("", "t_id_seq")
		if seq == nil {
			t.Fatal("t_id_seq not found")
		}
		if got := relProps(s, seq)["owned by"]; got != "public.t.id" {
			t.Errorf("owned by = %q, want %q", got, "public.t.id")
		}
	})
	t.Run("row security forced", func(t *testing.T) {
		s := load(t, "CREATE TABLE t (id int); ALTER TABLE t ENABLE ROW LEVEL SECURITY; ALTER TABLE t FORCE ROW LEVEL SECURITY;")
		r := s.Relation("", "t")
		if got := relProps(s, r)["row security"]; got != "forced" {
			t.Errorf("row security = %q, want %q", got, "forced")
		}
	})
}

// TestPolPropsVariants covers polProps' restrictive / roles / with-check branches.
func TestPolPropsVariants(t *testing.T) {
	s := load(t, `
CREATE TABLE t (id int, owner text);
CREATE ROLE app_role;
CREATE POLICY p1 ON t AS RESTRICTIVE TO app_role USING (id > 0) WITH CHECK (id > 0);
`)
	r := s.Relation("", "t")
	if len(r.Policies) != 1 {
		t.Fatalf("policies: %+v", r.Policies)
	}
	p := polProps(r.Policies[0])
	if p["restrictive"] != "true" {
		t.Errorf("restrictive = %q, want true", p["restrictive"])
	}
	if p["roles"] != "app_role" {
		t.Errorf("roles = %q, want app_role", p["roles"])
	}
	if p["with check"] != "id > 0" {
		t.Errorf("with check = %q, want %q", p["with check"], "id > 0")
	}
}

// TestConProps covers conProps' foreign key ON DELETE / ON UPDATE / DEFERRABLE branches
// beyond the plain-check and unique-nulls-not-distinct scenarios TestCompare already
// exercises.
func TestConProps(t *testing.T) {
	s := load(t, `
CREATE TABLE parent (id int PRIMARY KEY);
CREATE TABLE child (
  parent_id int,
  CONSTRAINT child_fk FOREIGN KEY (parent_id) REFERENCES parent (id) ON DELETE CASCADE ON UPDATE SET NULL DEFERRABLE,
  CONSTRAINT child_check CHECK (parent_id > 0)
);
`)
	r := s.Relation("", "child")
	var fk *schema.Constraint
	for _, c := range r.Constraints {
		if c.Kind == schema.ForeignKey {
			fk = c
		}
	}
	if fk == nil {
		t.Fatal("no foreign key constraint found")
	}
	p := conProps(fk)
	if p["on delete"] != "c" {
		t.Errorf("on delete = %q, want %q", p["on delete"], "c")
	}
	if p["on update"] != "n" {
		t.Errorf("on update = %q, want %q", p["on update"], "n")
	}
	if p["deferrable"] != "true" {
		t.Errorf("deferrable = %q, want true", p["deferrable"])
	}
	if p["references"] != "parent (id)" {
		t.Errorf("references = %q, want %q", p["references"], "parent (id)")
	}
}

// TestFnPropsVariants covers fnProps' procedure / strict / window / aggregate branches.
func TestFnPropsVariants(t *testing.T) {
	t.Run("procedure", func(t *testing.T) {
		s := load(t, "CREATE PROCEDURE proc1(a int) LANGUAGE sql AS $$ SELECT 1 $$;")
		if len(s.Functions) != 1 {
			t.Fatalf("functions: %+v", s.Functions)
		}
		p := fnProps(s, s.Functions[0])
		if p["procedure"] != "true" {
			t.Errorf("procedure = %q, want true", p["procedure"])
		}
	})
	t.Run("strict and volatility", func(t *testing.T) {
		s := load(t, "CREATE FUNCTION f1(a int) RETURNS int LANGUAGE sql IMMUTABLE STRICT RETURN a;")
		p := fnProps(s, s.Functions[0])
		if p["strict"] != "true" {
			t.Errorf("strict = %q, want true", p["strict"])
		}
		if p["volatility"] != "i" {
			t.Errorf("volatility = %q, want i", p["volatility"])
		}
	})
	t.Run("window", func(t *testing.T) {
		s := load(t, "CREATE FUNCTION f2(a int) RETURNS int LANGUAGE internal WINDOW AS 'row_number';")
		p := fnProps(s, s.Functions[0])
		if p["window"] != "true" {
			t.Errorf("window = %q, want true", p["window"])
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		s := load(t, "CREATE AGGREGATE agg1(int) (SFUNC = int4pl, STYPE = int);")
		var agg *schema.Function
		for _, f := range s.Functions {
			if f.IsAgg {
				agg = f
			}
		}
		if agg == nil {
			t.Fatal("no aggregate function found")
		}
		p := fnProps(s, agg)
		if p["aggregate"] != "true" {
			t.Errorf("aggregate = %q, want true", p["aggregate"])
		}
	})
	t.Run("setof and sql-standard body", func(t *testing.T) {
		s := load(t, "CREATE FUNCTION f3(a int) RETURNS SETOF int LANGUAGE sql BEGIN ATOMIC SELECT a; END;")
		p := fnProps(s, s.Functions[0])
		if !strings.HasPrefix(p["returns"], "setof ") {
			t.Errorf("returns = %q, want setof prefix", p["returns"])
		}
		if p["body"] == "" {
			t.Errorf("body is empty for a SQL-standard body")
		}
	})
}

// TestSignatureOutParams covers Signature's skip of OUT / TABLE-column parameters: they
// do not count as call-site input arguments.
func TestSignatureOutParams(t *testing.T) {
	s := load(t, "CREATE FUNCTION f(a int, OUT b int) LANGUAGE sql AS $$ SELECT a $$;")
	got := Signature(s, s.Functions[0])
	if want := "f(integer)"; got != want {
		t.Errorf("Signature = %q, want %q", got, want)
	}
}

// TestTrgPropsUpdateOf covers trgProps' "update of columns" branch (as opposed to a bare
// UPDATE with no column list, or INSERT / DELETE alone).
func TestTrgPropsUpdateOf(t *testing.T) {
	s := load(t, `
CREATE TABLE t (id int, name text);
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER t_trg BEFORE UPDATE OF name ON t FOR EACH ROW EXECUTE FUNCTION trg();
`)
	if len(s.Triggers) != 1 {
		t.Fatalf("triggers: %+v", s.Triggers)
	}
	p := trgProps(s.Triggers[0])
	if p["events"] != "update of name" {
		t.Errorf("events = %q, want %q", p["events"], "update of name")
	}
}

// TestCollation covers collation()'s non-empty branch: a column with an explicit COLLATE.
func TestCollation(t *testing.T) {
	s := load(t, `CREATE TABLE t (name text COLLATE "C");`)
	r := s.Relation("", "t")
	c := r.Column("name")
	if c == nil {
		t.Fatal("column name not found")
	}
	p := colProps(s, c)
	if !strings.Contains(p["type"], "collate") {
		t.Errorf("type = %q, want a collate suffix", p["type"])
	}
}

// TestOrderOnly covers Change.OrderOnly: true only for an Alter whose sole differing
// field is "column order"; anything else (a different field, more than one field, a
// non-Alter op) is false.
func TestOrderOnly(t *testing.T) {
	if !(Change{Op: Alter, Fields: []Field{{Name: "column order"}}}).OrderOnly() {
		t.Error("want true for a lone column-order field")
	}
	if (Change{Op: Alter, Fields: []Field{{Name: "column order"}, {Name: "not null"}}}).OrderOnly() {
		t.Error("want false when another field also differs")
	}
	if (Change{Op: Alter, Fields: []Field{{Name: "not null"}}}).OrderOnly() {
		t.Error("want false for an unrelated field")
	}
	if (Change{Op: Add, Fields: []Field{{Name: "column order"}}}).OrderOnly() {
		t.Error("want false for a non-Alter op")
	}
}

// TestRelKindMatViewAndSequence covers relKind's matview / sequence branches, not
// exercised by TestCompare's table/view scenarios.
func TestRelKindMatViewAndSequence(t *testing.T) {
	s := load(t, "CREATE MATERIALIZED VIEW mv AS SELECT 1 AS n; CREATE SEQUENCE seq1;")
	if got := relKind(s.Relation("", "mv")); got != "matview" {
		t.Errorf("relKind(matview) = %q", got)
	}
	if got := relKind(s.Relation("", "seq1")); got != "sequence" {
		t.Errorf("relKind(sequence) = %q", got)
	}
}

// TestColPropsIdentityAndGenerated covers colProps' identity and generated branches.
func TestColPropsIdentityAndGenerated(t *testing.T) {
	s := load(t, `
CREATE TABLE t (
  id int GENERATED ALWAYS AS IDENTITY,
  full_name text GENERATED ALWAYS AS ('x') STORED
);
`)
	r := s.Relation("", "t")
	idProps := colProps(s, r.Column("id"))
	if idProps["identity"] == "" {
		t.Error("identity not set")
	}
	genProps := colProps(s, r.Column("full_name"))
	if genProps["generated"] != "'x'" {
		t.Errorf("generated = %q, want %q", genProps["generated"], "'x'")
	}
}

// TestConstraintCheckKind covers conProps' plain CHECK branch (as opposed to the
// PRIMARY KEY / UNIQUE / FOREIGN KEY ones already exercised elsewhere).
func TestConstraintCheckKind(t *testing.T) {
	s := load(t, "CREATE TABLE t (n int, CONSTRAINT t_n_check CHECK (n > 0));")
	r := s.Relation("", "t")
	p := conProps(r.Constraints[0])
	if p["check"] != "n > 0" {
		t.Errorf("check = %q, want %q", p["check"], "n > 0")
	}
}

// TestTriggerAddedAndDropped covers triggers()' plain add (nothing on the from side) and
// plain drop (nothing on the to side) branches - TestCompare's "trigger" case only
// exercises a changed trigger (present, and different, on both sides).
func TestTriggerAddedAndDropped(t *testing.T) {
	base := `
CREATE TABLE t (id int);
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
`
	withTrigger := base + "CREATE TRIGGER t_trg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION trg();\n"
	if got := compare(t, base, withTrigger); got != "+ trigger t.t_trg" {
		t.Errorf("added trigger: got %q", got)
	}
	if got := compare(t, withTrigger, base); got != "- trigger t.t_trg" {
		t.Errorf("dropped trigger: got %q", got)
	}
}

// TestSignatureNoArgs covers Signature's empty-argument-list join (strings.Join of a nil
// slice), not exercised by the other Signature tests which all have at least one input.
func TestSignatureNoArgs(t *testing.T) {
	s := load(t, "CREATE FUNCTION f() RETURNS int LANGUAGE sql RETURN 1;")
	if got, want := Signature(s, s.Functions[0]), "f()"; got != want {
		t.Errorf("Signature = %q, want %q", got, want)
	}
}

// TestUserTypesSchemaQualified covers UserTypes' schema-qualified naming branch for a
// non-public type, and its skip of a relation's row type for a plain table (only a
// CREATE TYPE ... AS composite counts).
func TestUserTypesSchemaQualified(t *testing.T) {
	s := load(t, "CREATE SCHEMA app; CREATE TYPE app.pair AS (x int, y int); CREATE TABLE app.t (id int);")
	types := UserTypes(s)
	if _, ok := types["app.pair"]; !ok {
		t.Errorf("want a schema-qualified key, got %v", types)
	}
	if _, ok := types["app.t"]; ok {
		t.Errorf("a plain table's row type should not be listed, got %v", types)
	}
}

// TestUserTypesRange covers UserTypes' range-kind branch.
func TestUserTypesRange(t *testing.T) {
	s := load(t, "CREATE TYPE r1 AS RANGE (subtype = int4);")
	types := UserTypes(s)
	rt, ok := types["r1"]
	if !ok || rt.Kind != "range" {
		t.Fatalf("types = %v", types)
	}
	if rt.Props["subtype"] != "integer" {
		t.Errorf("subtype = %q, want integer", rt.Props["subtype"])
	}
}

// TestSequenceChangeSkipsColumnsEtc covers relations()' "continue after props for a
// Sequence" branch: a changed sequence must not also be diffed for columns / constraints
// / indexes (a sequence has none), just its own properties.
func TestSequenceChangeSkipsColumnsEtc(t *testing.T) {
	base := "CREATE TABLE t (id int); CREATE SEQUENCE s1;"
	edit := "ALTER SEQUENCE s1 OWNED BY t.id;"
	got := compare(t, base, base+"\n"+edit)
	want := "~ sequence s1\n    owned by:  -> public.t.id"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestSignatureSchemaQualified covers Signature's schema-qualified naming branch for a
// function outside the public schema.
func TestSignatureSchemaQualified(t *testing.T) {
	s := load(t, "CREATE SCHEMA app; CREATE FUNCTION app.f(a int) RETURNS int LANGUAGE sql RETURN a;")
	if got, want := Signature(s, s.Functions[0]), "app.f(integer)"; got != want {
		t.Errorf("Signature = %q, want %q", got, want)
	}
}

// TestTrgPropsDelete covers trgProps' DELETE-event branch.
func TestTrgPropsDelete(t *testing.T) {
	s := load(t, `
CREATE TABLE t (id int);
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN OLD; END $$;
CREATE TRIGGER t_trg BEFORE DELETE ON t FOR EACH ROW EXECUTE FUNCTION trg();
`)
	p := trgProps(s.Triggers[0])
	if p["events"] != "delete" {
		t.Errorf("events = %q, want delete", p["events"])
	}
}

// TestPropsDispatch covers Props' dispatch to every object kind it recognizes (relation,
// column, constraint, index, rule, function, trigger, policy), and its default nil
// return for an unrecognized type.
func TestPropsDispatch(t *testing.T) {
	s := load(t, `
CREATE TABLE t (id int PRIMARY KEY, name text);
CREATE INDEX t_name_idx ON t (name);
CREATE RULE t_rule AS ON DELETE TO t DO INSTEAD NOTHING;
CREATE FUNCTION f() RETURNS int LANGUAGE sql RETURN 1;
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER t_trg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION trg();
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY t_pol ON t USING (id > 0);
`)
	r := s.Relation("", "t")
	if p := Props(s, r); p == nil {
		t.Error("Props(relation) = nil")
	}
	if p := Props(s, r.Column("id")); p == nil {
		t.Error("Props(column) = nil")
	}
	if p := Props(s, r.Constraints[0]); p == nil {
		t.Error("Props(constraint) = nil")
	}
	if p := Props(s, r.Indexes[0]); p == nil {
		t.Error("Props(index) = nil")
	}
	rules := r.Rules()
	if len(rules) != 1 {
		t.Fatalf("rules: %+v", rules)
	}
	for _, rd := range rules {
		if p := Props(s, rd); p == nil {
			t.Error("Props(rule) = nil")
		}
	}
	if p := Props(s, s.Functions[0]); p == nil {
		t.Error("Props(function) = nil")
	}
	if p := Props(s, s.Triggers[0]); p == nil {
		t.Error("Props(trigger) = nil")
	}
	if p := Props(s, r.Policies[0]); p == nil {
		t.Error("Props(policy) = nil")
	}
	if p := Props(s, "not an object"); p != nil {
		t.Errorf("Props(unrecognized) = %v, want nil", p)
	}
}
