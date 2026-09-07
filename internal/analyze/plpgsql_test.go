package analyze

import (
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// PL/pgSQL bodies are checked statement by statement with the PL variables in scope.
func TestAnalyzePLpgSQL(t *testing.T) {
	base, _ := os.ReadFile("testdata/schema.sql")
	cases := []struct {
		name  string
		def   string // appended to the schema (may contain a CREATE TRIGGER)
		want  string // substring of the error, "" for OK
		rels  string // relations referenced, comma-separated (order of first reference)
		raise string // SQLSTATEs raised, comma-separated
		notes string // substring expected among the notes
	}{
		{name: "declared scalars and a query into them", def: `
CREATE FUNCTION p1(p_user bigint) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE n bigint; total numeric;
BEGIN
  SELECT count(*), coalesce(sum(o.total), 0) INTO n, total FROM orders o WHERE o.user_id = p_user;
  IF total > 100 THEN RETURN n; END IF;
  RETURN 0;
END $$;`, rels: "orders"},
		{name: "%TYPE and %ROWTYPE", def: `
CREATE FUNCTION p2(p_id bigint) RETURNS numeric LANGUAGE plpgsql AS $$
DECLARE r orders%ROWTYPE; t orders.total%TYPE;
BEGIN
  SELECT * INTO r FROM orders WHERE id = p_id;
  t := r.total * 2;
  RETURN t;
END $$;`, rels: "orders"},
		{name: "record from a FOR query", def: `
CREATE FUNCTION p3() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record; acc bigint := 0;
BEGIN
  FOR rec IN SELECT id, total FROM orders LOOP
    acc := acc + rec.id;
  END LOOP;
  RETURN acc;
END $$;`, rels: "orders"},
		{name: "a record field that does not exist", def: `
CREATE FUNCTION p4() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN SELECT id, total FROM orders LOOP
    RETURN rec.nope;
  END LOOP;
  RETURN 0;
END $$;`, want: `line 5: record "rec" has no field "nope"`},
		{name: "a column that does not exist in an embedded statement", def: `
CREATE FUNCTION p5() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  UPDATE orders SET nope = 1;
END $$;`, want: `line 3: column "nope" of relation "orders" does not exist`},
		{name: "assignment type mismatch", def: `
CREATE FUNCTION p6() RETURNS void LANGUAGE plpgsql AS $$
DECLARE n integer;
BEGIN
  n := 'abc'::text;
END $$;`, want: "cannot be assigned to integer"},
		{name: "RETURN type mismatch", def: `
CREATE FUNCTION p7() RETURNS integer LANGUAGE plpgsql AS $$
BEGIN
  RETURN now();
END $$;`, want: "cannot be assigned to integer"},
		{name: "a variable and a column with the same name are ambiguous", def: `
CREATE FUNCTION p8(p_id bigint) RETURNS numeric LANGUAGE plpgsql AS $$
DECLARE total numeric;
BEGIN
  SELECT total INTO total FROM orders WHERE id = p_id;
  RETURN total;
END $$;`, want: `column reference "total" is ambiguous`},
		{name: "a trigger function typed by its table", def: `
CREATE FUNCTION p9() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total > 1000 AND TG_OP = 'INSERT' THEN
    RAISE EXCEPTION 'too large: %', NEW.total USING ERRCODE = 'P0401';
  END IF;
  UPDATE users SET name = name WHERE id = NEW.user_id;
  RETURN NEW;
END $$;
CREATE TRIGGER p9_t BEFORE INSERT OR UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION p9();`, rels: "users", raise: "P0401"},
		{name: "a trigger reading a column its table lacks", def: `
CREATE FUNCTION p10() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.nope IS NULL THEN RETURN NULL; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER p10_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p10();`, want: `trigger p10_t: 42703: line 3: column "nope" of row variable "new" does not exist`},
		{name: "RAISE without ERRCODE is P0001; a condition name maps", def: `
CREATE FUNCTION p11(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF x < 0 THEN RAISE EXCEPTION 'negative'; END IF;
  IF x = 0 THEN RAISE unique_violation; END IF;
  RAISE NOTICE 'fine';
END $$;`, raise: "P0001,23505"},
		{name: "EXECUTE with a constant is checked; a dynamic one is noted", def: `
CREATE FUNCTION p12(tbl text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'UPDATE orders SET note = $1' USING tbl;
  EXECUTE 'DELETE FROM ' || tbl;
END $$;`, rels: "orders", notes: "EXECUTE runs SQL built at run time"},
		{name: "a constant EXECUTE with a bad statement", def: `
CREATE FUNCTION p13() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'UPDATE orders SET nope = 1';
END $$;`, want: `column "nope"`},
		{name: "RETURN QUERY against RETURNS TABLE", def: `
CREATE FUNCTION p14() RETURNS TABLE (id bigint, total numeric) LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY SELECT o.id, o.total FROM orders o;
END $$;`, rels: "orders"},
		{name: "RETURN QUERY with the wrong shape", def: `
CREATE FUNCTION p15() RETURNS TABLE (id bigint, total numeric) LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY SELECT o.id FROM orders o;
END $$;`, want: "returns 1 columns, 2 expected"},
		{name: "exception block variables and FOUND", def: `
CREATE FUNCTION p16(p_id bigint) RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  UPDATE orders SET note = 'x' WHERE id = p_id;
  IF NOT FOUND THEN RETURN 'none'; END IF;
  RETURN 'ok';
EXCEPTION WHEN unique_violation THEN
  RETURN SQLSTATE || SQLERRM;
END $$;`, rels: "orders"},
		{name: "OUT parameters are typed variables", def: `
CREATE FUNCTION p17(p_id bigint, OUT n bigint) LANGUAGE plpgsql AS $$
BEGIN
  SELECT count(*) INTO n FROM orders WHERE user_id = p_id;
  n := n + 'x';
END $$;`, want: "line 4"},
		{name: "a write's violations are collected", def: `
CREATE FUNCTION p18(p_user bigint) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO orders (user_id, total) VALUES (p_user, 1);
END $$;`, rels: "orders"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Load(string(base) + "\n" + c.def)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var fn *schema.Function
			for _, f := range s.Functions {
				if strings.HasPrefix(f.Name, "p") && strings.TrimLeft(f.Name, "p") != "" && isPLpgSQL(f) && strings.Contains(c.def, "FUNCTION "+f.Name+"(") {
					fn = f
				}
			}
			if fn == nil {
				t.Fatal("function not found")
			}
			r, err := AnalyzeFunction(s, fn)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
			if err != nil {
				return
			}
			if c.rels != "" {
				var names []string
				seen := map[string]bool{}
				for _, ref := range r.Relations {
					if !seen[ref.Name] {
						seen[ref.Name] = true
						names = append(names, ref.Name)
					}
				}
				if got := strings.Join(names, ","); got != c.rels {
					t.Errorf("relations %q, want %q", got, c.rels)
				}
			}
			if c.raise != "" {
				var codes []string
				for _, re := range raisedErrors(s, fn) {
					codes = append(codes, re.Code)
				}
				if got := strings.Join(codes, ","); got != c.raise {
					t.Errorf("raises %q, want %q", got, c.raise)
				}
			}
			if c.notes != "" {
				found := false
				for _, n := range r.Notes {
					if strings.Contains(n.Message, c.notes) {
						found = true
					}
				}
				if !found {
					t.Errorf("no note containing %q in %v", c.notes, r.Notes)
				}
			}
			if c.name == "a write's violations are collected" && len(r.Violations) == 0 {
				t.Error("no violations collected from the body's INSERT")
			}
		})
	}
}
