package diff

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
)

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
			"~ view v\n    column order: id, name -> id, name, s\n    query: SELECT id, name FROM t -> SELECT id, name, s FROM t\n+ column v.s"},
		{"function body", "CREATE OR REPLACE FUNCTION f(p int) RETURNS int LANGUAGE sql STABLE RETURN p + 2;",
			"~ function f(integer)\n    body: RETURN p + 1 -> RETURN p + 2\n    volatility: i -> s"},
		{"function signature", "DROP FUNCTION f(int); CREATE FUNCTION f(p bigint) RETURNS int LANGUAGE sql RETURN 1;",
			"- function f(integer)\n+ function f(bigint)"},
		{"trigger", "DROP TRIGGER t_trg ON t; CREATE TRIGGER t_trg BEFORE INSERT OR UPDATE OF name ON t FOR EACH ROW EXECUTE FUNCTION trg();",
			"~ trigger t.t_trg\n    events: insert -> insert or update of name"},
		{"comment", "COMMENT ON TABLE t IS 'stuff'; COMMENT ON COLUMN t.name IS NULL; COMMENT ON COLUMN t.id IS 'pk';",
			"- comment t.name\n~ comment t\n    text: things -> stuff\n+ comment t.id"},
		{"extension", "CREATE EXTENSION citext;", "+ extension citext"},
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
