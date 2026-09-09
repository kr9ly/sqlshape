package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/pgparse"
)

const pg18ChecksSchema = `
CREATE DOMAIN d1 AS text COLLATE "C";
CREATE DOMAIN d2 AS text COLLATE "POSIX";
CREATE TABLE int4_tbl (f1 int);
`

// Checks PostgreSQL 18 added, each judged as 18 does and as 17 did.
func TestPG18Checks(t *testing.T) {
	cases := []struct {
		sql    string
		want17 string // "" = accepted; otherwise the SQLSTATE
		want18 string
	}{
		// JSON_VALUE: the DEFAULT's collation must be the RETURNING type's
		{`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C'::d2 ON EMPTY) = 'a'`, "", "42P21"},
		{`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C' COLLATE "C" ON EMPTY) = 'a'`, "", ""},
		{`SELECT JSON_VALUE('{"a": "A"}', '$.a' RETURNING d1 DEFAULT 'C' ON EMPTY) = 'a'`, "", ""},
		// the path must be a jsonpath (a string is read as one)
		{`SELECT JSON_QUERY(jsonb '{"a": 123}', ('$' || '.' || 'a' || NULL)::date WITH WRAPPER)`, "", "42804"},
		{`SELECT JSON_QUERY(jsonb '{"a": 123}', '$.a'::text)`, "", ""},
		// number fields are digits (and decimal points) only
		{`SELECT timestamp with time zone 'J2452271 T X03456-08'`, "", "22007"},
		{`SELECT timestamp with time zone 'J2452271 T X03456.001e6-08'`, "", "22007"},
		{`SELECT timestamp with time zone 'J2452271T04:05:06'`, "", ""},
		// aclitem: "" opens and closes a name in 18, so the grantor is missing
		{`SELECT '""=r*/""'::aclitem`, "", "22P02"},
		{`SELECT '=r'::aclitem`, "", ""},
		{`SELECT '=q'::aclitem`, "22P02", "22P02"},
		{`SELECT 'bob r'::aclitem`, "22P02", "22P02"},
		{`SELECT '=r/'::aclitem`, "22P02", "22P02"},
		// an outer-level aggregate may not use a CTE of the subquery it is written in
		{`select f1, (with cte1(x,y) as (select 1,2) select count((select i4.f1 from cte1))) as ss from int4_tbl i4`, "42803", "0A000"},
		// not 18-specific, found by 18's corpus: DISTINCT ON matches ORDER BY by expression, not by position in the text
		{`select distinct on (a, b+1) a, b+1 from (values (1, 0), (2, 1)) as t (a, b) where a = b+1 group by grouping sets((a, b+1), (a)) order by a, b+1`, "", ""},
	}
	schemas := map[pgparse.Version]string{pgparse.PG17: pg18ChecksSchema, pgparse.PG18: "-- sqlshape: postgres 18\n" + pg18ChecksSchema}
	for v, ddl := range schemas {
		s, err := Load(ddl)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			want := c.want17
			if v == pgparse.PG18 {
				want = c.want18
			}
			got := renderAnalyzer(s, c.sql)
			if want == "" && strings.HasPrefix(got, "error:") || want != "" && !strings.HasPrefix(got, "error: "+want) {
				t.Errorf("%d: %s\n got %s want %q", int(v), c.sql, got, want)
			}
		}
	}
	if testing.Short() {
		return
	}
	// the oracle of each version says the same (role checks aside: the checker has no roles)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for v, ddl := range schemas {
		o, err := oracle.StartVersion(ctx, v, pg18ChecksSchema)
		if err != nil {
			t.Skipf("PostgreSQL %d oracle: %v", int(v), err)
		}
		s, _ := Load(ddl)
		for _, c := range cases {
			want, got := renderOracle(ctx, o, c.sql), renderAnalyzer(s, c.sql)
			if strings.HasPrefix(want, "error: 42704") || strings.HasPrefix(want, "error: XX000") {
				continue // roles, and 17's own bugs on these inputs
			}
			if (strings.HasPrefix(want, "error:") || strings.HasPrefix(got, "error:")) && !match(want, got) {
				t.Errorf("%d: %s\n oracle %s analyzer %s", int(v), c.sql, want, got)
			}
		}
		o.Close()
	}
}
