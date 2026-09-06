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
CREATE DOMAIN email AS text CHECK (VALUE LIKE '%@%');
CREATE TYPE money_pair AS (amount numeric, currency text);
CREATE VIEW paid_orders AS SELECT id, customer_id, total FROM orders WHERE status = 'paid';
CREATE MATERIALIZED VIEW order_totals AS SELECT customer_id, sum(total) AS total FROM orders GROUP BY customer_id;
CREATE UNIQUE INDEX order_totals_pk ON order_totals (customer_id);`, base: "1-tables"},
		{name: "functions and triggers", base: "1-tables", edit: `
CREATE FUNCTION order_count(p bigint) RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM orders WHERE customer_id = p);
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.created_at := now(); RETURN NEW; END $$;
CREATE TRIGGER orders_touch BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION touch();
CREATE RULE orders_protect AS ON DELETE TO orders WHERE old.status = 'shipped' DO INSTEAD NOTHING;`},
		{name: "everything grows", base: "3-everything", edit: `
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
		{name: "enum label removed", base: "1-tables", edit: `
-- @migrate enum order_status: drop 'cancelled' using 'pending'
ALTER TYPE order_status RENAME TO order_status_prev;
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
ALTER TABLE orders ALTER COLUMN status DROP DEFAULT;
ALTER TABLE orders ALTER COLUMN status TYPE order_status USING status::text::order_status;
ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'pending';
DROP TYPE order_status_prev;
CREATE VIEW open_orders AS SELECT id, status FROM orders WHERE status <> 'shipped';`},
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
		{"undeclared enum label", `ALTER TYPE order_status RENAME TO o; CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
ALTER TABLE orders ALTER COLUMN status DROP DEFAULT; ALTER TABLE orders ALTER COLUMN status TYPE order_status USING status::text::order_status; DROP TYPE o;`,
			"enum order_status: labels cancelled are removed, which no @migrate declares"},
		{"backfill type error", "-- @migrate backfill orders.total = 'abc'\nALTER TABLE orders ALTER COLUMN total SET DEFAULT 2;", "invalid input syntax for type numeric"},
		{"backfill unknown column", "-- @migrate backfill orders.nope = 1", "orders.nope is not in the target schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			to := mustCanonical(t, base+"\n"+c.edit)
			_, err := Plan(from.s, to.s, to.intents)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v\nwant a problem containing %q", err, c.want)
			}
		})
	}
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
	s := mustCanonical(t, example(t, "3-everything"))
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
