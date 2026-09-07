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
		{name: "condition names map to their SQLSTATEs", def: `
CREATE FUNCTION p19(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF x = 1 THEN RAISE no_data_found; END IF;
  IF x = 2 THEN RAISE too_many_rows; END IF;
  IF x = 3 THEN RAISE assert_failure; END IF;
  IF x = 4 THEN RAISE foreign_key_violation; END IF;
  IF x = 5 THEN RAISE check_violation; END IF;
  IF x = 6 THEN RAISE not_null_violation; END IF;
  IF x = 7 THEN RAISE restrict_violation; END IF;
  IF x = 8 THEN RAISE integrity_constraint_violation; END IF;
  IF x = 9 THEN RAISE invalid_parameter_value; END IF;
  IF x = 10 THEN RAISE division_by_zero; END IF;
  IF x = 11 THEN RAISE numeric_value_out_of_range; END IF;
  IF x = 12 THEN RAISE insufficient_privilege; END IF;
  IF x = 13 THEN RAISE invalid_text_representation; END IF;
  IF x = 14 THEN RAISE serialization_failure; END IF;
  IF x = 15 THEN RAISE deadlock_detected; END IF;
  IF x = 16 THEN RAISE lock_not_available; END IF;
  IF x = 17 THEN RAISE data_exception; END IF;
  IF x = 18 THEN RAISE invalid_transaction_state; END IF;
  IF x = 19 THEN RAISE feature_not_supported; END IF;
  IF x = 20 THEN RAISE internal_error; END IF;
  RAISE DEBUG 'd'; RAISE LOG 'l'; RAISE INFO 'i'; RAISE WARNING 'w';
END $$;`, raise: "P0002,P0003,P0004,23503,23514,23502,23001,23000,22023,22012,22003,42501,22P02,40001,40P01,55P03,22000,25000,0A000,XX000"},
		{name: "RAISE USING DETAIL / HINT checks their expressions", def: `
CREATE FUNCTION p20(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'bad %', x USING DETAIL = 'the value was ' || x::text, HINT = 'try again';
END $$;`, raise: "P0001"},
		{name: "CASE searched form", def: `
CREATE FUNCTION p21(x integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE y integer;
BEGIN
  CASE
    WHEN x > 0 THEN y := 1;
    WHEN x < 0 THEN y := -1;
    ELSE y := 0;
  END CASE;
  RETURN y;
END $$;`},
		{name: "CASE simple form", def: `
CREATE FUNCTION p22(x integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE y integer;
BEGIN
  CASE x
    WHEN 1 THEN y := 10;
    WHEN 2 THEN y := 20;
    ELSE y := 0;
  END CASE;
  RETURN y;
END $$;`},
		{name: "WHILE loop", def: `
CREATE FUNCTION p23() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n integer := 0;
BEGIN
  WHILE n < 5 LOOP
    n := n + 1;
  END LOOP;
  RETURN n;
END $$;`},
		{name: "LOOP with EXIT WHEN", def: `
CREATE FUNCTION p24() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n integer := 0;
BEGIN
  LOOP
    n := n + 1;
    EXIT WHEN n >= 5;
  END LOOP;
  RETURN n;
END $$;`},
		{name: "FOREACH over an array", def: `
CREATE FUNCTION p25() RETURNS void LANGUAGE plpgsql AS $$
DECLARE arr text[] := ARRAY['a', 'b']; x text;
BEGIN
  FOREACH x IN ARRAY arr LOOP
    RAISE NOTICE '%', x;
  END LOOP;
END $$;`},
		{name: "array element assignment", def: `
CREATE FUNCTION p26() RETURNS text LANGUAGE plpgsql AS $$
DECLARE arr text[] := ARRAY['a', 'b'];
BEGIN
  arr[1] := 'z';
  RETURN arr[1];
END $$;`},
		{name: "FOR ... IN a bound cursor, OPEN / FETCH / CLOSE", def: `
CREATE FUNCTION p27() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE cur CURSOR FOR SELECT id, total FROM orders; acc bigint := 0;
BEGIN
  FOR rec IN cur LOOP
    acc := acc + rec.id;
  END LOOP;
  RETURN acc;
END $$;`},
		{name: "OPEN / FETCH INTO a record / CLOSE", def: `
CREATE FUNCTION p28() RETURNS void LANGUAGE plpgsql AS $$
DECLARE cur CURSOR FOR SELECT id, total FROM orders; rec record;
BEGIN
  OPEN cur;
  FETCH cur INTO rec;
  CLOSE cur;
  RAISE NOTICE 'id=%', rec.id;
END $$;`},
		{name: "OPEN cur FOR EXECUTE with a dynamic query", def: `
CREATE FUNCTION p29() RETURNS void LANGUAGE plpgsql AS $$
DECLARE cur refcursor; tbl text := 'orders';
BEGIN
  OPEN cur FOR EXECUTE 'SELECT id FROM ' || tbl;
  CLOSE cur;
END $$;`, notes: "EXECUTE runs SQL built at run time"},
		{name: "CALL of a procedure", def: `
CREATE PROCEDURE p30(p_order bigint) LANGUAGE plpgsql AS $$
BEGIN
  CALL mark_paid(p_order);
END $$;`},
		{name: "GET DIAGNOSTICS", def: `
CREATE FUNCTION p31() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n integer;
BEGIN
  UPDATE orders SET note = note;
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n;
END $$;`, rels: "orders"},
		{name: "ASSERT", def: `
CREATE FUNCTION p32(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  ASSERT x > 0, 'x must be positive';
END $$;`},
		{name: "an unnamed parameter", def: `
CREATE FUNCTION p33(bigint) RETURNS bigint LANGUAGE plpgsql AS $$
BEGIN
  RETURN $1;
END $$;`},
		{name: "a quoted pg_catalog keyword type", def: `
CREATE FUNCTION p34() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n pg_catalog."int4";
BEGIN
  n := 5;
  RETURN n;
END $$;`},
		{name: "%ROWTYPE of a missing table", def: `
CREATE FUNCTION p35() RETURNS void LANGUAGE plpgsql AS $$
DECLARE r nope%ROWTYPE;
BEGIN
  NULL;
END $$;`, want: `relation "nope" does not exist`},
		{name: "%ROWTYPE schema-qualified", def: `
CREATE FUNCTION p36() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE r public.orders%ROWTYPE;
BEGIN
  SELECT * INTO r FROM orders LIMIT 1;
  RETURN r.id;
END $$;`, rels: "orders"},
		{name: "%TYPE of a missing table", def: `
CREATE FUNCTION p37() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t nope.col%TYPE;
BEGIN
  NULL;
END $$;`, want: `relation "nope" does not exist`},
		{name: "%TYPE of a missing column", def: `
CREATE FUNCTION p38() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t orders.nope%TYPE;
BEGIN
  NULL;
END $$;`, want: `column "nope" of relation "orders" does not exist`},
		{name: "%TYPE of a missing variable", def: `
CREATE FUNCTION p39() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t nosuchvar%TYPE;
BEGIN
  NULL;
END $$;`, want: `variable "nosuchvar" does not exist`},
		{name: "%TYPE of a parameter", def: `
CREATE FUNCTION p40(p_total numeric) RETURNS numeric LANGUAGE plpgsql AS $$
DECLARE t p_total%TYPE;
BEGIN
  t := p_total * 2;
  RETURN t;
END $$;`},
		{name: "an unknown type name", def: `
CREATE FUNCTION p41() RETURNS void LANGUAGE plpgsql AS $$
DECLARE x nosuchtype;
BEGIN
  NULL;
END $$;`, want: `type "nosuchtype" does not exist`},
		{name: "a record variable assigned from a row expression", def: `
CREATE FUNCTION p42() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  rec := (SELECT o FROM orders o LIMIT 1);
  RETURN rec.id;
END $$;`, rels: "orders"},
		{name: "SELECT ... INTO a record variable", def: `
CREATE FUNCTION p43() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  SELECT id, total INTO rec FROM orders LIMIT 1;
  RETURN rec.id;
END $$;`, rels: "orders"},
		{name: "INTO with the wrong number of targets", def: `
CREATE FUNCTION p44() RETURNS void LANGUAGE plpgsql AS $$
DECLARE a bigint; b bigint; c bigint;
BEGIN
  SELECT id, total INTO a, b, c FROM orders LIMIT 1;
END $$;`, want: "INTO has 3 target(s) but the query returns 2 column(s)"},
		{name: "INTO a scalar target with the wrong type", def: `
CREATE FUNCTION p45() RETURNS void LANGUAGE plpgsql AS $$
DECLARE a bigint; b integer;
BEGIN
  SELECT id, note INTO a, b FROM orders LIMIT 1;
END $$;`, want: "cannot be assigned to integer"},
		{name: "INTO an input parameter", def: `
CREATE FUNCTION p46(p_id bigint) RETURNS bigint LANGUAGE plpgsql AS $$
BEGIN
  SELECT count(*) INTO p_id FROM orders;
  RETURN p_id;
END $$;`, rels: "orders"},
		{name: "FOR a, b IN query: scalar loop variables", def: `
CREATE FUNCTION p47() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE a bigint; b numeric; acc bigint := 0;
BEGIN
  FOR a, b IN SELECT id, total FROM orders LOOP
    acc := acc + a;
  END LOOP;
  RETURN acc;
END $$;`, rels: "orders"},
		{name: "RETURN void with a value is an error", def: `
CREATE FUNCTION p48() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RETURN 1;
END $$;`, want: "RETURN cannot have a parameter in function returning void"},
		{name: "RETURN in a SETOF function must use RETURN NEXT / RETURN QUERY", def: `
CREATE FUNCTION p49() RETURNS SETOF integer LANGUAGE plpgsql AS $$
BEGIN
  RETURN 1;
END $$;`, want: "use RETURN NEXT or RETURN QUERY"},
		{name: "RETURN NEXT of a mismatched scalar", def: `
CREATE FUNCTION p50() RETURNS SETOF integer LANGUAGE plpgsql AS $$
BEGIN
  RETURN NEXT 'x'::text;
END $$;`, want: "cannot be assigned to integer"},
		{name: "RETURN NEXT of a row from a RETURNS SETOF table", def: `
CREATE FUNCTION p51() RETURNS SETOF orders LANGUAGE plpgsql AS $$
DECLARE r orders%ROWTYPE;
BEGIN
  FOR r IN SELECT * FROM orders LOOP
    RETURN NEXT (r);
  END LOOP;
  RETURN;
END $$;`, rels: "orders"},
		{name: "RETURN NEXT of a record from a RETURNS SETOF record", def: `
CREATE FUNCTION p52() RETURNS SETOF record LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN SELECT o.id, o.total FROM orders o LOOP
    RETURN NEXT (rec);
  END LOOP;
END $$;`, rels: "orders"},
		{name: "RETURN QUERY EXECUTE of a dynamic query", def: `
CREATE FUNCTION p53() RETURNS SETOF integer LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY EXECUTE 'SELECT id FROM orders WHERE id > ' || 1;
END $$;`, notes: "EXECUTE runs SQL built at run time"},
		{name: "a trigger returning a row of its own table's type", def: `
CREATE FUNCTION p54() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r orders%ROWTYPE;
BEGIN
  SELECT * INTO r FROM orders LIMIT 1;
  RETURN r;
END $$;
CREATE TRIGGER p54_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p54();`, rels: "orders"},
		{name: "a trigger returning something other than NEW / OLD / a row / NULL", def: `
CREATE FUNCTION p55() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RETURN 5;
END $$;
CREATE TRIGGER p55_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p55();`, want: "a trigger function returns NEW, OLD or NULL"},
		{name: "a trigger function attached to two tables", def: `
CREATE TABLE p56_audit (id bigserial PRIMARY KEY, tbl text);
CREATE FUNCTION p56() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO p56_audit (tbl) VALUES (TG_TABLE_NAME);
  RETURN NEW;
END $$;
CREATE TRIGGER p56_t1 BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION p56();
CREATE TRIGGER p56_t2 BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p56();`, rels: "p56_audit"},
		{name: "CREATE TRIGGER with a schema-qualified function", def: `
CREATE FUNCTION p57() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RETURN NEW;
END $$;
CREATE TRIGGER p57_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION public.p57();`},
		{name: "an unattached trigger function is not analyzed", def: `
CREATE FUNCTION p58() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RETURN NEW.nope;
END $$;`, rels: ""},
		{name: "a function that calls itself terminates", def: `
CREATE FUNCTION p59(n bigint) RETURNS bigint LANGUAGE plpgsql AS $$
BEGIN
  IF n <= 0 THEN RETURN 0; END IF;
  RETURN p59(n - 1);
END $$;`},
		{name: "a trigger that writes its own table (in-progress marker)", def: `
CREATE FUNCTION p60() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE orders SET note = note WHERE id = NEW.id;
  RETURN NEW;
END $$;
CREATE TRIGGER p60_t AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p60();`, rels: "orders"},
		{name: "nested block DECLARE (current shadowing behavior)", def: `
CREATE FUNCTION p61() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE n bigint := 1;
BEGIN
  DECLARE n text := 'x'; BEGIN n := n || 'y'; END;
  RETURN n;
END $$;`, want: "cannot be assigned to bigint"},
		{name: "a schema-qualified trigger function and table", def: `
CREATE SCHEMA p62s;
CREATE TABLE p62s.p62_t (id int PRIMARY KEY);
CREATE FUNCTION p62s.p62() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RETURN NEW;
END $$;
CREATE TRIGGER p62_trig BEFORE INSERT ON p62s.p62_t FOR EACH ROW EXECUTE FUNCTION p62s.p62();`},
		{name: "a syntax error in the body", def: `
CREATE FUNCTION p63() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  THIS IS NOT VALID PLPGSQL;
END $$;`, want: "at or near"},
		{name: "an INOUT parameter", def: `
CREATE PROCEDURE p64(INOUT p_total numeric) LANGUAGE plpgsql AS $$
BEGIN
  p_total := p_total + 1;
END $$;`},
		{name: "a VARIADIC parameter", def: `
CREATE FUNCTION p65(VARIADIC arr integer[]) RETURNS integer LANGUAGE plpgsql AS $$
BEGIN
  RETURN arr[1];
END $$;`},
		{name: "RETURNS record with OUT parameters", def: `
CREATE FUNCTION p66(OUT id bigint, OUT total numeric) LANGUAGE plpgsql AS $$
BEGIN
  id := 1;
  total := 2.5;
END $$;`},
		{name: "%TYPE of a local variable", def: `
CREATE FUNCTION p67() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE a bigint := 1; b a%TYPE;
BEGIN
  b := a + 1;
  RETURN b;
END $$;`},
		{name: "an EXCEPTION handler's own statement errors", def: `
CREATE FUNCTION p68() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  NULL;
EXCEPTION WHEN OTHERS THEN
  UPDATE orders SET nope = 1;
END $$;`, want: `column "nope" of relation "orders" does not exist`},
		{name: "PERFORM of a bad expression", def: `
CREATE FUNCTION p69() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
		{name: "FOR ... IN query with a bad column", def: `
CREATE FUNCTION p70() RETURNS void LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN SELECT nope FROM orders LOOP
    NULL;
  END LOOP;
END $$;`, want: `column "nope" does not exist`},
		{name: "FOR ... IN EXECUTE of a bad constant query", def: `
CREATE FUNCTION p71() RETURNS void LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN EXECUTE 'SELECT nope FROM orders' LOOP
    NULL;
  END LOOP;
END $$;`, want: `column "nope" does not exist`},
		{name: "FOR ... IN EXECUTE of a dynamic query", def: `
CREATE FUNCTION p72() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record; tbl text := 'orders'; acc bigint := 0;
BEGIN
  FOR rec IN EXECUTE 'SELECT id FROM ' || tbl LOOP
    acc := acc + 1;
  END LOOP;
  RETURN acc;
END $$;`, notes: "EXECUTE runs SQL built at run time"},
		{name: "FOR i IN a numeric range", def: `
CREATE FUNCTION p73() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE acc integer := 0;
BEGIN
  FOR i IN 1..5 LOOP
    acc := acc + i;
  END LOOP;
  RETURN acc;
END $$;`},
		{name: "FOR i IN a numeric range with a bad bound", def: `
CREATE FUNCTION p73b() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  FOR i IN (SELECT nope FROM orders)..5 LOOP
    NULL;
  END LOOP;
END $$;`, want: `column "nope" does not exist`},
		{name: "FOREACH with a bad array expression", def: `
CREATE FUNCTION p74() RETURNS void LANGUAGE plpgsql AS $$
DECLARE x integer;
BEGIN
  FOREACH x IN ARRAY (SELECT nope FROM orders) LOOP
    NULL;
  END LOOP;
END $$;`, want: `column "nope" does not exist`},
		{name: "IF THEN body errors", def: `
CREATE FUNCTION p75() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF true THEN
    UPDATE orders SET nope = 1;
  END IF;
END $$;`, want: `column "nope" of relation "orders" does not exist`},
		{name: "IF ELSIF condition errors", def: `
CREATE FUNCTION p76() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF false THEN
    NULL;
  ELSIF (SELECT nope FROM orders) IS NOT NULL THEN
    NULL;
  END IF;
END $$;`, want: `column "nope" does not exist`},
		{name: "IF ELSIF body errors", def: `
CREATE FUNCTION p77() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF false THEN
    NULL;
  ELSIF true THEN
    UPDATE orders SET nope = 1;
  END IF;
END $$;`, want: `column "nope" of relation "orders" does not exist`},
		{name: "CASE simple form t_expr errors", def: `
CREATE FUNCTION p78() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  CASE (SELECT nope FROM orders)
    WHEN 1 THEN NULL;
  END CASE;
END $$;`, want: `column "nope" does not exist`},
		{name: "CASE searched form WHEN errors", def: `
CREATE FUNCTION p79() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  CASE
    WHEN (SELECT nope FROM orders) IS NOT NULL THEN NULL;
  END CASE;
END $$;`, want: `column "nope" does not exist`},
		{name: "CASE body errors", def: `
CREATE FUNCTION p80() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  CASE
    WHEN true THEN UPDATE orders SET nope = 1;
  END CASE;
END $$;`, want: `column "nope" of relation "orders" does not exist`},
		{name: "WHILE condition errors", def: `
CREATE FUNCTION p81() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  WHILE (SELECT nope FROM orders) IS NOT NULL LOOP
    NULL;
  END LOOP;
END $$;`, want: `column "nope" does not exist`},
		{name: "EXIT with no WHEN", def: `
CREATE FUNCTION p82() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  LOOP
    EXIT;
  END LOOP;
END $$;`},
		{name: "RETURN QUERY with a bad query", def: `
CREATE FUNCTION p85() RETURNS TABLE (id bigint) LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY SELECT nope FROM orders;
END $$;`, want: `column "nope" does not exist`},
		{name: "ASSERT condition errors", def: `
CREATE FUNCTION p86() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  ASSERT (SELECT nope FROM orders) IS NOT NULL, 'msg';
END $$;`, want: `column "nope" does not exist`},
		{name: "OPEN cur FOR a bad constant query", def: `
CREATE FUNCTION p87() RETURNS void LANGUAGE plpgsql AS $$
DECLARE cur refcursor;
BEGIN
  OPEN cur FOR SELECT nope FROM orders;
END $$;`, want: `column "nope" does not exist`},
		{name: "an embedded SQL syntax error", def: `
CREATE FUNCTION p90() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 +;
END $$;`, want: "at end of input"},
		{name: "an empty dynamic query has nothing to analyze", def: `
CREATE FUNCTION p91() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE '-- nothing here';
END $$;`},
		{name: "RETURN in a void function with no value", def: `
CREATE FUNCTION p92() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RETURN;
END $$;`},
		{name: "a trigger RETURN expression that errors", def: `
CREATE FUNCTION p93() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RETURN (SELECT nope FROM orders);
END $$;
CREATE TRIGGER p93_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION p93();`, want: `column "nope" does not exist`},
		{name: "RETURN of a row from a function RETURNS a table type", def: `
CREATE FUNCTION p94() RETURNS orders LANGUAGE plpgsql AS $$
DECLARE r orders%ROWTYPE;
BEGIN
  SELECT * INTO r FROM orders LIMIT 1;
  RETURN r;
END $$;`, rels: "orders"},
		{name: "RAISE params error", def: `
CREATE FUNCTION p95() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'bad %', (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
		{name: "RAISE USING option error", def: `
CREATE FUNCTION p96() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'x' USING DETAIL = (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
		{name: "the same RAISE code is only listed once", def: `
CREATE FUNCTION p97(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF x = 1 THEN RAISE unique_violation; END IF;
  IF x = 2 THEN RAISE unique_violation; END IF;
END $$;`, raise: "23505"},
		{name: "RAISE raise_exception names P0001 explicitly", def: `
CREATE FUNCTION p98(x integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF x = 1 THEN RAISE raise_exception; END IF;
END $$;`, raise: "P0001"},
		{name: "a quoted-identifier assignment target", def: `
CREATE FUNCTION p99() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE "MyVar" integer;
BEGIN
  "MyVar" := 5;
  RETURN "MyVar";
END $$;`},
		{name: "a quoted-identifier assignment target with a type mismatch", def: `
CREATE FUNCTION p99b() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE "MyVar" integer;
BEGIN
  "MyVar" := now();
  RETURN "MyVar";
END $$;`, want: "cannot be assigned to integer"},
		{name: "a record field assignment", def: `
CREATE FUNCTION p100() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN SELECT id, total FROM orders LOOP
    rec.id := rec.id + 1;
    RETURN rec.id;
  END LOOP;
  RETURN 0;
END $$;`, rels: "orders"},
		{name: "assigning into a %ROWTYPE variable's field does not even parse", def: `
CREATE FUNCTION p101() RETURNS numeric LANGUAGE plpgsql AS $$
DECLARE r orders%ROWTYPE;
BEGIN
  SELECT * INTO r FROM orders LIMIT 1;
  r.total := r.total + 1;
  RETURN r.total;
END $$;`, want: `"r.total" is not a known variable`},
		{name: "EXECUTE literal with a bad USING param", def: `
CREATE FUNCTION p102() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'SELECT $1' USING (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
		{name: "EXECUTE dynamic expression itself errors", def: `
CREATE FUNCTION p103() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'DELETE FROM ' || nope_var;
END $$;`, want: `"nope_var" does not exist`},
		{name: "EXECUTE dynamic with a bad USING param", def: `
CREATE FUNCTION p104() RETURNS void LANGUAGE plpgsql AS $$
DECLARE tbl text := 'orders';
BEGIN
  EXECUTE 'DELETE FROM ' || tbl USING (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
		{name: "a scalar variable used with dot notation", def: `
CREATE FUNCTION p105() RETURNS void LANGUAGE plpgsql AS $$
DECLARE n bigint;
BEGIN
  RAISE NOTICE '%', n.foo;
END $$;`, want: `is not a row or record variable`},
		{name: "a bare unresolved qualified name", def: `
CREATE FUNCTION p106() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM bogus_alias.col;
END $$;`, want: `bogus_alias`},
		{name: "SELECT with LIMIT but no ORDER BY is noted", def: `
CREATE FUNCTION p107() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM orders LIMIT 5;
END $$;`, rels: "orders", notes: "LIMIT without ORDER BY"},
		{name: "bare RETURN NEXT with no value (OUT parameters supply it)", def: `
CREATE FUNCTION p108(OUT id bigint) RETURNS SETOF bigint LANGUAGE plpgsql AS $$
BEGIN
  id := 1;
  RETURN NEXT;
  RETURN;
END $$;`},
		{name: "RETURN NULL in a void function", def: `
CREATE FUNCTION p109() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RETURN NULL;
END $$;`},
		{name: "RAISE ERRCODE given as a condition name", def: `
CREATE FUNCTION p110() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'x' USING ERRCODE = 'unique_violation';
END $$;`, raise: "23505"},
		{name: "NULL and string literals assign to any type", def: `
CREATE FUNCTION p111(p_id bigint) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE n bigint; d date;
BEGIN
  n := NULL;
  d := '2024-01-01';
  IF p_id < 0 THEN RETURN NULL; END IF;
  RETURN n;
END $$;`},
		{name: "RETURN QUERY EXECUTE of a constant is checked", def: `
CREATE FUNCTION p113() RETURNS SETOF bigint LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY EXECUTE 'SELECT nope FROM orders';
END $$;`, want: `column "nope"`},
		{name: "a bound cursor's query is checked at OPEN and shapes the fetched record", def: `
CREATE FUNCTION p114() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE cur CURSOR FOR SELECT id, total FROM orders; r record;
BEGIN
  OPEN cur; FETCH cur INTO r; CLOSE cur;
  RETURN r.id;
END $$;`, rels: "orders"},
		{name: "a bound cursor's query with a bad column", def: `
CREATE FUNCTION p115() RETURNS void LANGUAGE plpgsql AS $$
DECLARE cur CURSOR FOR SELECT nope FROM orders;
BEGIN
  OPEN cur; CLOSE cur;
END $$;`, want: `column "nope"`},
		{name: "FOR over a bound cursor shapes the loop record", def: `
CREATE FUNCTION p116() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE cur CURSOR FOR SELECT id, total FROM orders; r record;
BEGIN
  FOR r IN cur LOOP RETURN r.nope; END LOOP;
  RETURN 0;
END $$;`, want: `record "r" has no field "nope"`},
		{name: "RETURN NEXT of a bare variable is type-checked", def: `
CREATE FUNCTION p117() RETURNS SETOF integer LANGUAGE plpgsql AS $$
DECLARE n text := 'not a number';
BEGIN
  RETURN NEXT n;
END $$;`, want: "cannot be assigned to integer"},
		{name: "RETURN of a bare variable is type-checked", def: `
CREATE FUNCTION p118() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE d date := '2024-01-01';
BEGIN
  RETURN d;
END $$;`, want: "cannot be assigned to integer"},
		{name: "a record field assignment with a type mismatch", def: `
CREATE FUNCTION p112() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  FOR rec IN SELECT id, total FROM orders LOOP
    rec.id := 'not a number'::text;
  END LOOP;
  RETURN 0;
END $$;`, want: "cannot be assigned to"},
		{name: "a record var assigned an erroring row expression", def: `
CREATE FUNCTION p113() RETURNS void LANGUAGE plpgsql AS $$
DECLARE rec record;
BEGIN
  rec := (SELECT nope FROM orders);
END $$;`, want: `column "nope" does not exist`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Load(string(base) + "\n" + c.def)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var fn *schema.Function
			for _, f := range s.Functions {
				if strings.HasPrefix(f.Name, "p") && strings.TrimLeft(f.Name, "p") != "" && isPLpgSQL(f) &&
					(strings.Contains(c.def, "FUNCTION "+f.Name+"(") || strings.Contains(c.def, "PROCEDURE "+f.Name+"(") ||
						strings.Contains(c.def, "."+f.Name+"(")) {
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
