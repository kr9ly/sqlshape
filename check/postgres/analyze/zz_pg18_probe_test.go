package analyze

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/pgparse"
)

func TestZZPG18Probe(t *testing.T) {
	const ddl = `
CREATE DOMAIN d1 AS text COLLATE "C";
CREATE DOMAIN d2 AS text COLLATE "POSIX";
CREATE TABLE jsonpaths (jsonpaths jsonpath);
CREATE TABLE int4_tbl (f1 int);
CREATE TABLE foo (f1 int, f2 text);
CREATE RULE foo_del_rule AS ON DELETE TO foo DO INSTEAD UPDATE foo SET f2 = f2 || ' (deleted)' WHERE f1 = OLD.f1;
`
	stmts := []string{
		`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C'::d2 ON EMPTY) = 'a'`,
		`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C' COLLATE "C" ON EMPTY) = 'a'`,
		`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C' ON EMPTY) = 'a'`,
		`SELECT JSON_QUERY(jsonb '{"a": 123}', ('$' || '.' || 'a' || NULL)::date WITH WRAPPER)`,
		`SELECT json_value('"aaa"', jsonpaths RETURNING json) FROM jsonpaths`,
		`SELECT JSON_QUERY(jsonb '{"a": 123}', '$.a'::text)`,
		`SELECT timestamp with time zone 'J2452271 T X03456-08'`,
		`SELECT timestamp with time zone 'J2452271 T X03456.001e6-08'`,
		`SELECT timestamp with time zone 'J2452271T04:05:06'`,
		`select distinct on (a, b+1) a, b+1 from (values (1, 0), (2, 1)) as t (a, b) where a = b+1 group by grouping sets((a, b+1), (a)) order by a, b+1`,
		`SELECT '""=r*/""'::aclitem`,
		`SELECT '=r/postgres'::aclitem`,
		`select f1, (with cte1(x,y) as (select 1,2) select count((select i4.f1 from cte1))) as ss from int4_tbl i4`,
		`DELETE FROM foo WHERE f1 = 4 RETURNING old.*, new.*, *`,
		`DELETE FROM foo WHERE f1 = 4 RETURNING *`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, v := range []pgparse.Version{pgparse.PG17, pgparse.PG18} {
		o, err := oracle.StartVersion(ctx, v, ddl)
		if err != nil {
			t.Fatal(err)
		}
		for _, sql := range stmts {
			out := renderOracle(ctx, o, sql)
			if v == pgparse.PG18 {
				// 18 statements RETURNING old/new only parse on 18
			}
			fmt.Printf("[%d] %s\n      %s", int(v), sql, out)
		}
		o.Close()
	}
}
