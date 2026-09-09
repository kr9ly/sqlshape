package migrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/dump"
	"github.com/kr9ly/sqlshape/internal/schema"
)

type canonical struct {
	s    *schema.Schema
	text string
	// intents are the @migrate declarations of the source this form was made from: what
	// the step *into* this state declares
	intents []Intent
}

// server is the one PostgreSQL every test in the package canonicalizes on (booting one
// costs seconds; a database on it, milliseconds).
var server *dump.Server

func TestMain(m *testing.M) {
	if _, err := exec.LookPath(dump.Binary()); err == nil {
		srv, err := dump.NewServer(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		server = srv
	}
	code := m.Run()
	if server != nil {
		server.Close()
	}
	os.Exit(code)
}

func requirePgDump(t *testing.T) {
	t.Helper()
	if server == nil {
		t.Skipf("%s not found", dump.Binary())
	}
}

func mustCanonical(t *testing.T, sql string) canonical {
	t.Helper()
	s, text, err := server.Canonical(context.Background(), sql, nil)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	in, err := ParseIntents(sql)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	return canonical{s, text, in}
}

// roundTrip plans from → to with to's declarations, verifies the plan reaches to, and the
// same backwards with the back declarations.
func roundTrip(t *testing.T, from, to canonical, back string, oneWay bool) {
	t.Helper()
	backIntents, err := ParseIntents(back)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []struct {
		name     string
		from, to canonical
		intents  []Intent
	}{{"forward", from, to, to.intents}, {"backward", to, from, backIntents}} {
		if oneWay && dir.name == "backward" {
			continue
		}
		plan, err := Plan(dir.from.s, dir.to.s, dir.intents)
		ddl := strings.Join(plan, "\n")
		if err != nil {
			t.Errorf("%s: plan: %v\nplan:\n%s", dir.name, err, ddl)
			continue
		}
		t.Logf("%s plan:\n%s", dir.name, ddl)
		changes, _, err := Verify(context.Background(), server, dir.from.text, ddl, dir.to.s)
		if err != nil {
			t.Errorf("%s: %v\nplan:\n%s", dir.name, err, ddl)
			continue
		}
		if len(changes) > 0 {
			var lines []string
			for _, c := range changes {
				lines = append(lines, c.String())
			}
			t.Errorf("%s: plan does not reach the target:\n%s\nplan:\n%s", dir.name, strings.Join(lines, "\n"), ddl)
		}
	}
}

func example(t *testing.T, name string) string {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join("../../examples", name, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(sql)
}

func TestPlan(t *testing.T) {
	requirePgDump(t)
	cases := []struct {
		name, base, edit string
		back             string // declarations for the way back
		oneWay           bool   // the way back needs more than the plan can do
	}{
		{name: "columns", base: "1-tables", edit: `
-- @migrate drop order_items.qty
ALTER TABLE customers ADD COLUMN nickname text DEFAULT 'anon';
ALTER TABLE customers ADD COLUMN score integer NOT NULL DEFAULT 0;
ALTER TABLE orders ALTER COLUMN total SET DEFAULT 1;
ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);
ALTER TABLE customers ALTER COLUMN name DROP NOT NULL;
ALTER TABLE order_items DROP COLUMN qty;`,
			back: "-- @migrate drop customers.nickname\n-- @migrate drop customers.score"},
		{name: "tables and keys", base: "1-tables", edit: `
CREATE TABLE tags (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE order_tags (order_id bigint REFERENCES orders ON DELETE CASCADE, tag_id bigint REFERENCES tags, PRIMARY KEY (order_id, tag_id));
CREATE INDEX order_tags_tag_idx ON order_tags (tag_id) WHERE tag_id > 0;
ALTER TABLE orders ADD CONSTRAINT orders_total_max CHECK (total < 1000000);
COMMENT ON TABLE tags IS 'labels';
COMMENT ON COLUMN tags.name IS 'unique label';`,
			back: "-- @migrate drop tags\n-- @migrate drop order_tags"},
		{name: "types and views", oneWay: true, edit: `
ALTER TYPE order_status ADD VALUE 'refunded' AFTER 'paid';
CREATE DOMAIN phone AS text CHECK (VALUE LIKE '+%');
CREATE TYPE money_pair AS (amount numeric, currency text);
CREATE VIEW paid_orders AS SELECT id, customer_id, shipping FROM orders WHERE status = 'paid';
CREATE MATERIALIZED VIEW order_totals AS SELECT customer_id, sum(shipping) AS total FROM orders GROUP BY customer_id;
CREATE UNIQUE INDEX order_totals_pk ON order_totals (customer_id);`, base: "3-database-api"},
		{name: "functions and triggers", base: "1-tables", edit: `
CREATE FUNCTION order_count(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.created_at := now(); RETURN NEW; END $$;
CREATE TRIGGER orders_touch BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION touch();
CREATE RULE orders_protect AS ON DELETE TO orders WHERE old.status = 'shipped' DO INSTEAD NOTHING;`},
		{name: "everything grows", base: "4-everything", edit: `
ALTER TABLE core.rooms ADD COLUMN floor integer NOT NULL DEFAULT 1;
CREATE OR REPLACE VIEW app.rooms AS SELECT tenant_id, id, name, capacity, hourly, 1 AS one FROM core.rooms;
COMMENT ON COLUMN core.rooms.capacity IS 'seats';`,
			back: "-- @migrate drop core.rooms.floor"},
		{name: "renames", base: "1-tables", edit: `
-- @migrate rename customers.name -> customers.full_name
-- @migrate rename orders -> purchases
-- @migrate rename orders.total -> purchases.amount
ALTER TABLE customers RENAME COLUMN name TO full_name;
ALTER TABLE orders RENAME TO purchases;
ALTER TABLE purchases RENAME COLUMN total TO amount;
ALTER TABLE purchases ALTER COLUMN amount TYPE numeric(14,2);`,
			back: "-- @migrate rename customers.full_name -> customers.name\n-- @migrate rename purchases -> orders\n-- @migrate rename purchases.amount -> orders.total"},
		{name: "enum label removed", base: "3-database-api", edit: `
-- @migrate enum order_status: drop 'cancelled' using 'pending'
DROP VIEW order_view;
DROP MATERIALIZED VIEW sales_by_day;
DROP FUNCTION pay_order(bigint);
ALTER TYPE order_status RENAME TO order_status_prev;
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
ALTER TABLE orders ALTER COLUMN status DROP DEFAULT;
ALTER TABLE orders ALTER COLUMN status TYPE order_status USING status::text::order_status;
ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'pending';
DROP TYPE order_status_prev;
CREATE VIEW open_orders AS SELECT id, status FROM orders WHERE status <> 'shipped';`},
		{name: "row level security", base: "1-tables", edit: `
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders FORCE ROW LEVEL SECURITY;
CREATE POLICY orders_owner ON orders USING (customer_id = current_setting('app.customer')::bigint);
CREATE POLICY orders_paid ON orders AS RESTRICTIVE FOR DELETE USING (status <> 'paid');`},
		{name: "backfill", base: "1-tables", edit: `
-- @migrate backfill customers.tier = 'basic'
-- @migrate backfill orders.memo = upper(status::text) where memo IS NULL
ALTER TABLE customers ADD COLUMN tier text NOT NULL;
ALTER TABLE orders ADD COLUMN memo text;`,
			back: "-- @migrate drop customers.tier\n-- @migrate drop orders.memo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base := example(t, c.base)
			roundTrip(t, mustCanonical(t, base), mustCanonical(t, base+"\n"+c.edit), c.back, c.oneWay)
		})
	}
}

// Declarations the schemas do not bear out, and changes no declaration explains, are
// errors (the DDL still comes back for reading).
func TestPlanProblems(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables")
	from := mustCanonical(t, base)
	cases := []struct {
		name, edit, want string
	}{
		{"undeclared column drop", "ALTER TABLE order_items DROP COLUMN qty;", "column order_items.qty is dropped, which no @migrate declares"},
		{"undeclared table drop", "DROP TABLE order_items;", "table order_items is dropped, which no @migrate declares"},
		{"stale drop", "-- @migrate drop order_items.qty", "order_items.qty still exists in the target schema"},
		{"stale rename", "-- @migrate rename customers.name -> customers.full_name", "customers.full_name is not in the target schema"},
		{"undeclared seed row", "DELETE FROM order_statuses WHERE code = 'cancelled';", ""},
		{"backfill type error", "-- @migrate backfill orders.total = 'abc'\nALTER TABLE orders ALTER COLUMN total SET DEFAULT 2;", "invalid input syntax for type numeric"},
		{"backfill unknown column", "-- @migrate backfill orders.nope = 1", "orders.nope is not in the target schema"},
	}
	for _, c := range cases {
		if c.want == "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			to := mustCanonical(t, base+"\n"+c.edit)
			_, err := Plan(from.s, to.s, to.intents)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v\nwant a problem containing %q", err, c.want)
			}
		})
	}
	t.Run("undeclared enum label", func(t *testing.T) {
		base := example(t, "3-database-api")
		from := mustCanonical(t, base)
		to := mustCanonical(t, base+`
DROP VIEW order_view; DROP MATERIALIZED VIEW sales_by_day; DROP FUNCTION pay_order(bigint);
ALTER TYPE order_status RENAME TO o; CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
ALTER TABLE orders ALTER COLUMN status DROP DEFAULT; ALTER TABLE orders ALTER COLUMN status TYPE order_status USING status::text::order_status; DROP TYPE o;`)
		_, err := Plan(from.s, to.s, to.intents)
		if want := "enum order_status: labels cancelled are removed, which no @migrate declares"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v\nwant a problem containing %q", err, want)
		}
	})
}

func TestParseIntents(t *testing.T) {
	in, err := ParseIntents(`
-- @migrate rename orders.state -> orders.status
--@migrate drop core.legacy
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
-- @migrate backfill orders.n = 1
CREATE TABLE t (id int); -- @migrate not a declaration (not at line start)
`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range in {
		got = append(got, fmt.Sprintf("%d:%s", i.Line, i))
	}
	want := []string{
		"2:rename orders.state -> orders.status",
		"3:drop core.legacy",
		"4:enum order_status: drop 'canceled' using 'cancelled'",
		"5:backfill orders.status = 'pending' where status is null",
		"6:backfill orders.n = 1",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if _, err := ParseIntents("-- @migrate frobnicate x"); err == nil {
		t.Error("unknown declaration accepted")
	}
}

func TestPlanEmptyForIdentical(t *testing.T) {
	requirePgDump(t)
	s := mustCanonical(t, example(t, "4-everything"))
	if p, err := Plan(s.s, s.s, nil); len(p) > 0 || err != nil {
		t.Errorf("plan for identical schemas: %v, %v", p, err)
	}
	_ = diff.Compare
}

// Seed rows: the plan brings a seeded table's content to its declared rows with a MERGE,
// after the table and its keys exist and parents before children; an additive seed
// leaves undeclared rows alone.
func TestPlanSeeds(t *testing.T) {
	requirePgDump(t)
	lookup := `
CREATE TABLE order_kinds (code text PRIMARY KEY, label text NOT NULL, sort_order integer NOT NULL DEFAULT 0, note text);
CREATE TABLE kind_groups (kind text NOT NULL REFERENCES order_kinds, grp text NOT NULL, PRIMARY KEY (kind, grp));
`
	v1 := lookup + `
INSERT INTO order_kinds (code, label, sort_order) VALUES ('retail', 'Retail', 10), ('bulk', 'Bulk', 20), ('gift', 'Gift', 30);
INSERT INTO kind_groups VALUES ('retail', 'b2c'), ('gift', 'b2c');
`
	v2 := lookup + `
INSERT INTO order_kinds (code, label, sort_order) VALUES ('retail', 'Retail', 10), ('bulk', 'Wholesale', 20), ('sample', 'Sample', 40);
INSERT INTO kind_groups VALUES ('retail', 'b2c'), ('sample', 'b2b');
`
	base := example(t, "1-tables")
	t.Run("new seeded tables", func(t *testing.T) {
		roundTrip(t, mustCanonical(t, base), mustCanonical(t, base+v1), "-- @migrate drop order_kinds\n-- @migrate drop kind_groups", false)
	})
	t.Run("rows change", func(t *testing.T) {
		from, to := mustCanonical(t, base+v1), mustCanonical(t, base+v2)
		plan, err := Plan(from.s, to.s, nil)
		if err != nil {
			t.Fatal(err)
		}
		ddl := strings.Join(plan, "\n")
		if n := strings.Count(ddl, "MERGE INTO"); n != 2 || strings.Index(ddl, `"order_kinds"`) > strings.Index(ddl, `"kind_groups"`) {
			t.Errorf("want two MERGEs, parent first:\n%s", ddl)
		}
		roundTrip(t, from, to, "", false)
	})
	t.Run("additive", func(t *testing.T) {
		from := mustCanonical(t, base+v1)
		to := mustCanonical(t, base+lookup+"-- sqlshape: seed\nINSERT INTO order_kinds (code, label) VALUES ('retail', 'Retail'), ('bulk', 'Bulk');")
		plan, err := Plan(from.s, to.s, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan) != 0 {
			t.Errorf("declared rows are all there; want an empty plan, got:\n%s", strings.Join(plan, "\n"))
		}
		to = mustCanonical(t, base+lookup+"-- sqlshape: seed\nINSERT INTO order_kinds (code, label) VALUES ('retail', 'Retail'), ('sample', 'Sample');")
		plan, err = Plan(from.s, to.s, nil)
		if err != nil {
			t.Fatal(err)
		}
		ddl := strings.Join(plan, "\n")
		if strings.Contains(ddl, "BY SOURCE") || !strings.Contains(ddl, "MERGE INTO") {
			t.Errorf("additive seed: want a MERGE without the delete arm:\n%s", ddl)
		}
		changes, _, err := Verify(context.Background(), server, from.text, ddl, to.s)
		if err != nil || len(changes) > 0 {
			t.Errorf("verify: %v %v\n%s", err, changes, ddl)
		}
	})
}

// --- render.go: pure function coverage (no database needed) ------------------------

// constraintText's PRIMARY KEY / UNIQUE / FOREIGN KEY branches only fire when a
// constraint's Definition is empty. In practice a pg_dump canonical schema never leaves
// Definition empty for these kinds (pg_dump always renders them as their own standalone
// ALTER TABLE ADD CONSTRAINT statement, never inline in CREATE TABLE - verified against
// pg_dump directly), so this path is unreachable through Plan with a real, canonicalized
// schema. Only CHECK constraints (which pg_dump keeps inline) reach it. These are
// therefore direct unit tests of the renderer itself.
func TestConstraintText(t *testing.T) {
	cases := []struct {
		name string
		c    *schema.Constraint
		want string
	}{
		{"primary key", &schema.Constraint{Kind: schema.PrimaryKey, Columns: []string{"id"}},
			`PRIMARY KEY ("id")`},
		{"unique", &schema.Constraint{Kind: schema.Unique, Columns: []string{"email"}},
			`UNIQUE ("email")`},
		{"unique nulls not distinct", &schema.Constraint{Kind: schema.Unique, Columns: []string{"email"}, NullsNotDistinct: true},
			`UNIQUE NULLS NOT DISTINCT ("email")`},
		{"foreign key, no action", &schema.Constraint{Kind: schema.ForeignKey, Columns: []string{"customer_id"}, RefTable: "customers", RefColumns: []string{"id"}},
			`FOREIGN KEY ("customer_id") REFERENCES "customers" ("id")`},
		{"foreign key cascade/set null/deferrable", &schema.Constraint{Kind: schema.ForeignKey, Columns: []string{"customer_id"}, RefTable: "customers", RefColumns: []string{"id"}, OnDelete: 'c', OnUpdate: 'n', Deferrable: true},
			`FOREIGN KEY ("customer_id") REFERENCES "customers" ("id") ON DELETE CASCADE ON UPDATE SET NULL DEFERRABLE`},
		{"foreign key restrict/set default", &schema.Constraint{Kind: schema.ForeignKey, Columns: []string{"customer_id"}, RefTable: "customers", RefColumns: []string{"id"}, OnDelete: 'r', OnUpdate: 'd'},
			`FOREIGN KEY ("customer_id") REFERENCES "customers" ("id") ON DELETE RESTRICT ON UPDATE SET DEFAULT`},
		// PostgreSQL 18
		{"temporal primary key", &schema.Constraint{Kind: schema.PrimaryKey, Columns: []string{"id", "valid_at"}, WithoutOverlaps: true},
			`PRIMARY KEY ("id", "valid_at" WITHOUT OVERLAPS)`},
		{"temporal unique", &schema.Constraint{Kind: schema.Unique, Columns: []string{"id", "valid_at"}, WithoutOverlaps: true},
			`UNIQUE ("id", "valid_at" WITHOUT OVERLAPS)`},
		{"temporal foreign key, not enforced", &schema.Constraint{Kind: schema.ForeignKey, Columns: []string{"pid", "valid_at"}, RefTable: "p", RefColumns: []string{"id", "valid_at"}, WithPeriod: true, NotEnforced: true},
			`FOREIGN KEY ("pid", PERIOD "valid_at") REFERENCES "p" ("id", PERIOD "valid_at") NOT ENFORCED`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := constraintText(nil, c.c); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
	t.Run("check", func(t *testing.T) {
		s, err := schema.Load("CREATE TABLE t (n integer, CONSTRAINT t_n_check CHECK (n > 0));")
		if err != nil {
			t.Fatal(err)
		}
		r := s.Relation("", "t")
		if len(r.Constraints) != 1 {
			t.Fatalf("constraints: %+v", r.Constraints)
		}
		if got, want := constraintText(nil, r.Constraints[0]), "CHECK (n > 0)"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	// A schema-qualified referenced table must be dot-joined ("core"."tenants"), not
	// comma-joined - constraintText now uses qdot (the same helper render.go already has
	// for this exact purpose) instead of qlist(strings.Split(...)).
	t.Run("schema-qualified ref table", func(t *testing.T) {
		c := &schema.Constraint{Kind: schema.ForeignKey, Columns: []string{"tenant_id"}, RefTable: "core.tenants", RefColumns: []string{"id"}}
		if got, want := constraintText(nil, c), `FOREIGN KEY ("tenant_id") REFERENCES "core"."tenants" ("id")`; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestActionWord(t *testing.T) {
	cases := []struct {
		b    byte
		want string
	}{
		{'r', "RESTRICT"}, {'c', "CASCADE"}, {'n', "SET NULL"}, {'d', "SET DEFAULT"}, {'a', ""}, {0, ""},
	}
	for _, c := range cases {
		if got := actionWord(c.b); got != c.want {
			t.Errorf("actionWord(%q) = %q, want %q", c.b, got, c.want)
		}
	}
}

// commentRelation and commentObjectSurvives are not called anywhere in the migrate
// package (grepped: only their own definitions in render.go). commentText / the comment
// loops in migrate.go already do the "does the object still exist" check inline via
// commentTarget, so these two look like leftover helpers from a refactor. Direct unit
// tests here for coverage; if they are truly dead, they are candidates for removal.
func TestCommentHelpers(t *testing.T) {
	s, err := schema.Load("CREATE TABLE t (n integer);")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := commentRelation("t.n"), "t"; got != want {
		t.Errorf("commentRelation(%q) = %q, want %q", "t.n", got, want)
	}
	if got, want := commentRelation("t"), "t"; got != want {
		t.Errorf("commentRelation(%q) = %q, want %q", "t", got, want)
	}
	if !commentObjectSurvives(s, "t.n") {
		t.Error("t.n should survive")
	}
	if commentObjectSurvives(s, "t.nope") {
		t.Error("t.nope should not survive")
	}
	if commentObjectSurvives(s, "nope") {
		t.Error("nope should not survive")
	}
}

// TestPlanNotes covers the "-- " notes the plan leaves for changes it cannot express as
// DDL: a domain's base type, a range's subtype, and a table's INHERITS / PARTITION OF /
// OF type - none of which can be altered in place. These operate on statically parsed
// schemas (schema.Load), no database needed.
func TestPlanNotes(t *testing.T) {
	load := func(sql string) *schema.Schema {
		t.Helper()
		s, err := schema.Load(sql)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("domain base type", func(t *testing.T) {
		from, to := load("CREATE DOMAIN d AS integer;"), load("CREATE DOMAIN d AS text;")
		plan, err := Plan(from, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "-- domain d: base type integer -> text cannot be altered") {
			t.Errorf("want a note about the domain's base type, got:\n%s", ddl)
		}
		if strings.Contains(ddl, "ALTER DOMAIN") {
			t.Errorf("want no ALTER for an unalterable base type change:\n%s", ddl)
		}
	})
	t.Run("range subtype", func(t *testing.T) {
		from := load("CREATE TYPE r1 AS RANGE (subtype = integer);")
		to := load("CREATE TYPE r1 AS RANGE (subtype = numeric);")
		plan, err := Plan(from, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "-- range r1:") || !strings.Contains(ddl, "cannot be altered") {
			t.Errorf("want a note about the range's subtype, got:\n%s", ddl)
		}
	})
	t.Run("table property the plan cannot alter", func(t *testing.T) {
		composite := "CREATE TYPE point2 AS (x integer, y integer);\n"
		from := load(composite + "CREATE TABLE p1 OF point2;")
		to := load(composite + "CREATE TABLE p1 (x integer, y integer);")
		plan, err := Plan(from, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, `-- table p1: of type "point2" -> "" cannot be altered by the plan`) {
			t.Errorf("want a note about the unalterable \"of type\" property, got:\n%s", ddl)
		}
		if strings.Contains(ddl, "ALTER TABLE") {
			t.Errorf("want no ALTER TABLE for a property the plan cannot change:\n%s", ddl)
		}
	})
}

// TestAlterTypeEnumAddValueBefore covers alterType's enum branch positioning a new label
// with BEFORE (as opposed to AFTER, or no position at all for a label appended at the
// end) - reached only when the new label isn't last and nothing already-known precedes
// it.
func TestAlterTypeEnumAddValueBefore(t *testing.T) {
	from := mustLoad(t, "CREATE TYPE e AS ENUM ('a', 'c');")
	to := mustLoad(t, "CREATE TYPE e AS ENUM ('b', 'a', 'c');")
	plan, err := Plan(from, to, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, "ADD VALUE 'b' BEFORE 'a'") {
		t.Errorf("want a BEFORE-positioned ADD VALUE, got:\n%s", ddl)
	}
}

// TestAlterTypeDomainChecks covers alterType's domain-check add and drop branches.
func TestAlterTypeDomainChecks(t *testing.T) {
	from := mustLoad(t, "CREATE DOMAIN d AS integer CONSTRAINT c_old CHECK (VALUE > 0);")
	to := mustLoad(t, "CREATE DOMAIN d AS integer CONSTRAINT c_new CHECK (VALUE < 100);")
	plan, err := Plan(from, to, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, `ALTER DOMAIN d DROP CONSTRAINT "c_old"`) {
		t.Errorf("want a DROP CONSTRAINT, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ALTER DOMAIN d ADD CONSTRAINT "c_new" CHECK (value < 100)`) {
		t.Errorf("want an ADD CONSTRAINT, got:\n%s", ddl)
	}
}

// TestAlterTypeComposite covers alterType's composite branches: dropping an attribute,
// adding one, and changing an existing one's type.
func TestAlterTypeComposite(t *testing.T) {
	from := mustLoad(t, "CREATE TYPE pt AS (x integer, y integer);")
	to := mustLoad(t, "CREATE TYPE pt AS (x bigint, z text);")
	plan, err := Plan(from, to, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, `DROP ATTRIBUTE "y"`) {
		t.Errorf("want a DROP ATTRIBUTE, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ADD ATTRIBUTE "z" text`) {
		t.Errorf("want an ADD ATTRIBUTE, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ALTER ATTRIBUTE "x" TYPE bigint`) {
		t.Errorf("want an ALTER ATTRIBUTE TYPE, got:\n%s", ddl)
	}
}

// --- migrate.go / render.go: scenarios that need the database -----------------------

// TestPlanFunctions covers fnProps and the function-alter path in migrate.go (~line
// 373): a body-only change goes through CREATE OR REPLACE FUNCTION; a RETURNS-type or
// argument-count change cannot use CREATE OR REPLACE and instead DROPs and recreates the
// function.
func TestPlanFunctions(t *testing.T) {
	requirePgDump(t)
	t.Run("body change: CREATE OR REPLACE", func(t *testing.T) {
		base := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
`
		edited := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) + 1 FROM orders WHERE customer_id = p);
`
		from, to := mustCanonical(t, base), mustCanonical(t, edited)
		plan, err := Plan(from.s, to.s, to.intents)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "CREATE OR REPLACE FUNCTION") {
			t.Errorf("want CREATE OR REPLACE FUNCTION for a body-only change, got:\n%s", ddl)
		}
		if strings.Contains(ddl, "DROP FUNCTION") {
			t.Errorf("did not want a DROP FUNCTION for a body-only change:\n%s", ddl)
		}
		changes, _, err := Verify(context.Background(), server, from.text, ddl, to.s)
		if err != nil || len(changes) > 0 {
			t.Errorf("verify: %v %v\n%s", err, changes, ddl)
		}
	})
	t.Run("return type change: DROP and CREATE", func(t *testing.T) {
		base := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
`
		edited := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS numeric LANGUAGE sql STABLE RETURN (SELECT sum(total) FROM orders WHERE customer_id = p);
`
		roundTrip(t, mustCanonical(t, base), mustCanonical(t, edited), "", false)
	})
	// When a view depends on the function (here, selecting its result directly, so the
	// view's own frozen column type also changes), the view must be dropped before the
	// DROP FUNCTION and recreated after the CREATE FUNCTION. diff.Props for a view now
	// includes its frozen columns' types, not just their names/order and the query text,
	// so drops() correctly sees the view as changed (same() is false) even though its own
	// query text is byte-for-byte identical - the same mechanism that already lets a view
	// whose selected column widens be recreated (or replaced in place) applies here too.
	t.Run("return type change with a dependent view", func(t *testing.T) {
		base := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
CREATE VIEW customer_totals AS SELECT id, order_total(id) AS cnt FROM customers;
`
		edited := example(t, "1-tables") + `
CREATE FUNCTION order_total(p bigint) RETURNS numeric LANGUAGE sql STABLE RETURN (SELECT sum(total) FROM orders WHERE customer_id = p);
CREATE VIEW customer_totals AS SELECT id, order_total(id) AS cnt FROM customers;
`
		from, to := mustCanonical(t, base), mustCanonical(t, edited)
		plan, err := Plan(from.s, to.s, to.intents)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "DROP VIEW") {
			t.Errorf("want the dependent view dropped ahead of the function, got:\n%s", ddl)
		}
		if !strings.Contains(ddl, "DROP FUNCTION") {
			t.Errorf("want a DROP FUNCTION for the return-type change, got:\n%s", ddl)
		}
		if strings.Index(ddl, "DROP VIEW") > strings.Index(ddl, "DROP FUNCTION") {
			t.Errorf("want the view dropped before the function, got:\n%s", ddl)
		}
		roundTrip(t, from, to, "", false)
	})
}

// TestPlanIdentity covers identityWord and the identity-alter branches in migrate.go
// (~494-496): switching GENERATED ALWAYS <-> BY DEFAULT on an existing identity column
// works both ways; dropping identity while the column survives works too.
func TestPlanIdentity(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables") + "ALTER TABLE order_items ADD COLUMN seq bigint NOT NULL;\nALTER TABLE order_items ALTER COLUMN seq ADD GENERATED ALWAYS AS IDENTITY;\n"
	always := mustCanonical(t, base)
	t.Run("ALWAYS <-> BY DEFAULT", func(t *testing.T) {
		byDefault := mustCanonical(t, base+"ALTER TABLE order_items ALTER COLUMN seq SET GENERATED BY DEFAULT;")
		roundTrip(t, always, byDefault, "", false)
	})
	// ADD GENERATED ... AS IDENTITY on a column that already exists implicitly creates a
	// backing sequence, exactly like a brand-new serial/identity column does. The "was
	// this column just added" check (migrate.go ~574, ownerColumn/ownerRelation) now
	// looks at whether the owning column has (or is gaining) IDENTITY, not just whether
	// it is new, so it correctly skips the redundant explicit CREATE SEQUENCE here.
	t.Run("add identity to an existing column, drop identity, column survives", func(t *testing.T) {
		noIdentity := mustCanonical(t, example(t, "1-tables")+"ALTER TABLE order_items ADD COLUMN seq bigint NOT NULL;\n")
		roundTrip(t, always, noIdentity, "", false)
	})
}

// TestPlanTypeDrop covers typeWord and the type-drop path in migrate.go (~307): an enum,
// composite and domain disappearing from the target schema are each dropped with the
// right keyword (DROP TYPE vs DROP DOMAIN), no @migrate declaration required.
func TestPlanTypeDrop(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables")
	withTypes := base + `
CREATE TYPE color AS ENUM ('red', 'green', 'blue');
CREATE TYPE money_pair2 AS (amount numeric, currency text);
CREATE DOMAIN posint AS integer CHECK (VALUE > 0);
`
	roundTrip(t, mustCanonical(t, withTypes), mustCanonical(t, base), "", false)

	// A range type's implicit multirange constructor functions (floatmultirange(),
	// floatrange(double precision, double precision), ...) are loaded as Function
	// entries (Language "internal") so the analyzer can type-check calls to them, but
	// PostgreSQL creates and drops them together with the type itself - migrate.go's
	// functions() now excludes them, so the plan neither tries to DROP FUNCTION them
	// ahead of the type nor CREATE FUNCTION them when the type is added.
	t.Run("range type drop and create", func(t *testing.T) {
		withRange := base + "CREATE TYPE floatrange AS RANGE (subtype = float8);\n"
		from, to := mustCanonical(t, withRange), mustCanonical(t, base)
		plan, err := Plan(from.s, to.s, to.intents)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "DROP TYPE floatrange") {
			t.Errorf("want DROP TYPE floatrange, got:\n%s", ddl)
		}
		if strings.Contains(ddl, "DROP FUNCTION") {
			t.Errorf("did not want a DROP FUNCTION for the range's own support functions:\n%s", ddl)
		}
		roundTrip(t, from, to, "", false)
	})
}

// TestPlanSequenceOwnerAddedColumn covers ownerColumn / ownerRelation (migrate.go ~574)
// for a bigserial column added to a table that already existed: unlike identity, a
// classic serial-style column (a plain nextval() default, no IDENTITY marker in the
// canonical schema) needs its sequence created explicitly, since nothing else in the
// plan creates it when the owning table itself is not new.
func TestPlanSequenceOwnerAddedColumn(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables")
	from := mustCanonical(t, base)
	to := mustCanonical(t, base+"ALTER TABLE customers ADD COLUMN ref bigserial;")
	plan, err := Plan(from.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, "CREATE SEQUENCE") {
		t.Errorf("want CREATE SEQUENCE for the new bigserial column's sequence, got:\n%s", ddl)
	}
	roundTrip(t, from, to, "-- @migrate drop customers.ref", false)
}

// TestPlanComments covers commentText / commentTarget for the four object kinds it
// resolves (TABLE, COLUMN, a non-public-schema TYPE/DOMAIN, FUNCTION): added, changed,
// removed, and suppressed when the commented object is itself dropped in the same plan.
func TestPlanComments(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables")

	t.Run("added: table, column, type, function", func(t *testing.T) {
		withComments := base + `
CREATE SCHEMA extra;
CREATE DOMAIN extra.phone AS text CHECK (VALUE LIKE '+%');
COMMENT ON TABLE customers IS 'people';
COMMENT ON COLUMN customers.name IS 'full name';
COMMENT ON DOMAIN extra.phone IS 'e.164-ish';
CREATE FUNCTION order_count(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
COMMENT ON FUNCTION order_count(bigint) IS 'count orders';
`
		roundTrip(t, mustCanonical(t, base), mustCanonical(t, withComments), "", false)
	})

	t.Run("changed and removed", func(t *testing.T) {
		commented := base + "COMMENT ON TABLE customers IS 'people';\n"
		changed := base + "COMMENT ON TABLE customers IS 'clients';\n"
		roundTrip(t, mustCanonical(t, commented), mustCanonical(t, changed), "", false)
		roundTrip(t, mustCanonical(t, commented), mustCanonical(t, base), "", false)
	})

	t.Run("dropped object suppresses its comment", func(t *testing.T) {
		withColComment := base + "COMMENT ON COLUMN order_items.qty IS 'quantity';\n"
		from := mustCanonical(t, withColComment)
		to := mustCanonical(t, base+"-- @migrate drop order_items.qty\nALTER TABLE order_items DROP COLUMN qty;")
		plan, err := Plan(from.s, to.s, to.intents)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ddl := strings.Join(plan, "\n")
		if strings.Contains(ddl, "COMMENT") && strings.Contains(ddl, "qty") {
			t.Errorf("did not want a COMMENT on a column dropped in the same plan:\n%s", ddl)
		}
		roundTrip(t, from, to, "", false)
	})

	// A public-schema type/domain comment must round-trip too: the comment key is
	// "type:" + the type's name with "public." trimmed first, matching how
	// diff.UserTypes keys public-schema types (plain "foo", not "public.foo").
	t.Run("public-schema type comment", func(t *testing.T) {
		withComment := base + "CREATE DOMAIN phone AS text CHECK (VALUE LIKE '+%');\nCOMMENT ON DOMAIN phone IS 'e.164-ish';\n"
		from, to := mustCanonical(t, base), mustCanonical(t, withComment)
		plan, err := Plan(from.s, to.s, to.intents)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ddl := strings.Join(plan, "\n")
		if !strings.Contains(ddl, "COMMENT ON DOMAIN") {
			t.Errorf("want a COMMENT ON DOMAIN for a public-schema domain, got:\n%s", ddl)
		}
		roundTrip(t, from, to, "", false)
	})
}

// TestBackfillTwoNewColumnsOnlyOneDeclared covers backfill()'s per-column filter
// (b.Column != col: continue) and hasBackfill()'s false branch: two new NOT NULL
// columns are added in one plan, but only one of them has a @migrate backfill declared,
// so the other must take the plain "ADD COLUMN ... NOT NULL" path instead of the
// add-nullable/fill/constrain sequence.
func TestBackfillTwoNewColumnsOnlyOneDeclared(t *testing.T) {
	requirePgDump(t)
	base := example(t, "1-tables")
	edit := `
-- @migrate backfill customers.tier = 'basic'
ALTER TABLE customers ADD COLUMN tier text NOT NULL;
ALTER TABLE customers ADD COLUMN flag boolean NOT NULL DEFAULT false;
`
	from, to := mustCanonical(t, base), mustCanonical(t, base+edit)
	plan, err := Plan(from.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, `ADD COLUMN "tier" text`) || strings.Contains(ddl, `"tier" text NOT NULL`) {
		t.Errorf("want tier added nullable first (backfilled), got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ADD COLUMN "flag" boolean DEFAULT false NOT NULL`) {
		t.Errorf("want flag added directly NOT NULL (no backfill declared for it), got:\n%s", ddl)
	}
	roundTrip(t, from, to, "-- @migrate drop customers.tier\n-- @migrate drop customers.flag", false)
}

// TestEnumColumnSkipsPlainTypeAlter covers alterTable's use of enumColumn: when a table
// whose enum-typed column is being recreated (its ALTER COLUMN TYPE is emitted by
// recreateEnum, not the generic column-alteration loop) also gets another, unrelated
// structural change, alterTable must still walk its columns (to emit that other change)
// without also emitting a second, plain ALTER COLUMN TYPE for the enum column itself.
func TestEnumColumnSkipsPlainTypeAlter(t *testing.T) {
	requirePgDump(t)
	base := example(t, "3-database-api")
	edit := `
-- @migrate enum order_status: drop 'cancelled' using 'pending'
DROP VIEW order_view;
DROP MATERIALIZED VIEW sales_by_day;
DROP FUNCTION pay_order(bigint);
ALTER TYPE order_status RENAME TO order_status_prev;
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
ALTER TABLE orders ALTER COLUMN status DROP DEFAULT;
ALTER TABLE orders ALTER COLUMN status TYPE order_status USING status::text::order_status;
ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'pending';
DROP TYPE order_status_prev;
CREATE VIEW open_orders AS SELECT id, status FROM orders WHERE status <> 'shipped';
ALTER TABLE orders ADD COLUMN priority integer NOT NULL DEFAULT 0;
`
	from, to := mustCanonical(t, base), mustCanonical(t, base+edit)
	plan, err := Plan(from.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if n := strings.Count(ddl, `ALTER TABLE "public"."orders" ALTER COLUMN "status" TYPE`); n != 1 {
		t.Errorf("want exactly one ALTER COLUMN status TYPE (from recreateEnum, not a second one from the generic column loop), got %d:\n%s", n, ddl)
	}
	if !strings.Contains(ddl, `ADD COLUMN "priority"`) {
		t.Errorf("want the unrelated new column too, got:\n%s", ddl)
	}
	roundTrip(t, from, to, "-- @migrate drop orders.priority", false)
}

// TestRenameAcrossSchemas covers renames()' SET SCHEMA branch: a table moving to a
// different schema (as well as being renamed) needs both an ALTER ... SET SCHEMA and,
// when the name also changes, a separate ALTER ... RENAME TO afterward.
func TestRenameAcrossSchemas(t *testing.T) {
	requirePgDump(t)
	base := "CREATE SCHEMA app;\nCREATE TABLE widgets (id int PRIMARY KEY);\n"
	edit := "-- @migrate rename widgets -> app.gadgets\nDROP TABLE widgets;\nCREATE TABLE app.gadgets (id int PRIMARY KEY);\n"
	from, to := mustCanonical(t, base), mustCanonical(t, base+edit)
	plan, err := Plan(from.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, `SET SCHEMA "app"`) {
		t.Errorf("want a SET SCHEMA, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `RENAME TO "gadgets"`) {
		t.Errorf("want a RENAME TO, got:\n%s", ddl)
	}
	roundTrip(t, from, to, "-- @migrate rename app.gadgets -> widgets", false)
}

// TestResolveNameTwoPartSchemaTable covers resolveName's 2-part "schema.table" branch
// (as opposed to "table.column"), for a relation in a non-public schema.
func TestResolveNameTwoPartSchemaTable(t *testing.T) {
	s := mustLoad(t, "CREATE SCHEMA app; CREATE TABLE app.t (id int);")
	r, col := resolveName(s, "app.t")
	if r == nil || col != "" {
		t.Errorf("resolveName(app.t) = %v, %q, want the relation and no column", r, col)
	}
}

// TestVerifyDirect covers Verify's own two branches that roundTrip's use of it never
// reaches: a ddl PostgreSQL itself refuses (Canonical's error, passed straight back), and
// a difference that is nothing but column order (filed as a note, not a change).
func TestVerifyDirect(t *testing.T) {
	requirePgDump(t)
	t.Run("ddl error", func(t *testing.T) {
		target := mustCanonical(t, "CREATE TABLE t (a int);")
		_, _, err := Verify(context.Background(), server, "CREATE TABLE t (a int);", "THIS IS NOT SQL;", target.s)
		if err == nil {
			t.Error("expected an error from invalid DDL")
		}
	})
	t.Run("order-only difference is a note, not a change", func(t *testing.T) {
		target := mustCanonical(t, "CREATE TABLE t (b int, a int);")
		changes, notes, err := Verify(context.Background(), server, "CREATE TABLE t (a int, b int);", "", target.s)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(changes) != 0 {
			t.Errorf("changes = %v, want none (column order should be a note)", changes)
		}
		if len(notes) != 1 || !notes[0].OrderOnly() {
			t.Errorf("notes = %v, want exactly one order-only note", notes)
		}
	})
}
