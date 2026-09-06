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
	s, text, err := server.Canonical(context.Background(), sql)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	return canonical{s, text}
}

// roundTrip plans from → to, verifies the plan reaches to, and the same backwards.
func roundTrip(t *testing.T, from, to canonical, oneWay bool) {
	t.Helper()
	for _, dir := range []struct {
		name     string
		from, to canonical
	}{{"forward", from, to}, {"backward", to, from}} {
		if oneWay && dir.name == "backward" {
			continue
		}
		ddl := strings.Join(Plan(dir.from.s, dir.to.s), "\n")
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
		oneWay           bool // the way back needs an intent declaration
	}{
		{name: "columns", base: "1-tables", edit: `
ALTER TABLE customers ADD COLUMN nickname text DEFAULT 'anon';
ALTER TABLE customers ADD COLUMN score integer NOT NULL DEFAULT 0;
ALTER TABLE orders ALTER COLUMN total SET DEFAULT 1;
ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);
ALTER TABLE customers ALTER COLUMN name DROP NOT NULL;
ALTER TABLE order_items DROP COLUMN qty;`},
		{name: "tables and keys", base: "1-tables", edit: `
CREATE TABLE tags (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE order_tags (order_id bigint REFERENCES orders ON DELETE CASCADE, tag_id bigint REFERENCES tags, PRIMARY KEY (order_id, tag_id));
CREATE INDEX order_tags_tag_idx ON order_tags (tag_id) WHERE tag_id > 0;
ALTER TABLE orders ADD CONSTRAINT orders_total_max CHECK (total < 1000000);
COMMENT ON TABLE tags IS 'labels';
COMMENT ON COLUMN tags.name IS 'unique label';`},
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
COMMENT ON COLUMN core.rooms.capacity IS 'seats';`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base := example(t, c.base)
			roundTrip(t, mustCanonical(t, base), mustCanonical(t, base+"\n"+c.edit), c.oneWay)
		})
	}
}

func TestPlanEmptyForIdentical(t *testing.T) {
	requirePgDump(t)
	s := mustCanonical(t, example(t, "3-everything"))
	if p := Plan(s.s, s.s); len(p) > 0 {
		t.Errorf("plan for identical schemas: %v", p)
	}
	_ = diff.Compare
}
