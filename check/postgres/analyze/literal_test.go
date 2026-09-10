package analyze

import (
	"errors"
	"testing"
)

// TestLiteralInput pins the input-function ports (arrays, ranges, geometry, jsonpath,
// money, ...) on hand-picked cases; TestLiteralOracle checks the whole regress corpus
// against a real PG when -regress is given.
func TestLiteralInput(t *testing.T) {
	s, err := Load("CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"{1,2,3}", "int[]", ""}, {"{1,2", "int[]", "22P02"}, {"{1,a}", "int[]", "22P02"}, {"{{1,2},{3}}", "int[]", "22P02"},
		{"[1:2]={1,2}", "int[]", ""}, {"[2:1]={}", "int[]", "2202E"}, {`{"a b",NULL,c}`, "text[]", ""}, {"{1,2} x", "int[]", "22P02"},
		{"{sad,happy}", "mood[]", ""}, {"{sad,angry}", "mood[]", "22P02"}, {"{(1,2),(3,4);(5,6),(7,8)}", "box[]", ""}, {"{(1,2);(3,4)}", "box[]", "22P02"},
		{"[1,10)", "int4range", ""}, {"empty", "int4range", ""}, {"[1,10", "int4range", "22P02"}, {"[a,b)", "int4range", "22P02"},
		{"[2020-01-01,2020-02-30)", "daterange", "22008"}, {"{[1,2),[3,4)}", "int4multirange", ""}, {"{[1,2),(3,4]", "int4multirange", "22P02"},
		{"(1,2)", "point", ""}, {"1,2", "point", ""}, {"(1,2,3)", "point", "22P02"}, {"(1,2),(3,4)", "box", ""}, {"[(1,2),(3,4)]", "lseg", ""},
		{"[(1,2),(3,4)]", "box", "22P02"}, {"{1,2,3}", "line", ""}, {"{0,0,3}", "line", "22P02"}, {"[(1,2),(1,2)]", "line", "22P02"},
		{"((1,2),(3,4),(5,6))", "polygon", ""}, {"(1,2),(3,4),(5,6)", "path", ""}, {"[(1,2),(3,4)]", "path", ""}, {"<(1,2),3>", "circle", ""}, {"<(1,2),-3>", "circle", "22P02"},
		{"0/12345678", "pg_lsn", ""}, {"0/123456789", "pg_lsn", "22P02"}, {`\xDEADBEEF`, "bytea", ""}, {`\xDEADBEEZ`, "bytea", "22023"}, {`\101`, "bytea", ""}, {`\401`, "bytea", "22P02"}, {`\9`, "bytea", "22P02"},
		{"$.a[*] ? (@.b > 1)", "jsonpath", ""}, {`$ ? (@ like_regex "^a" flag "i")`, "jsonpath", ""}, {"", "jsonpath", "22P02"},
		{"@ + 1", "jsonpath", "42601"}, {"last", "jsonpath", "42601"}, {"$[last]", "jsonpath", ""}, {"1.2a", "jsonpath", "42601"},
		{`"\u0000"`, "jsonpath", "22P05"}, {`$ ? (@ like_regex "p" flag "x")`, "jsonpath", "0A000"}, {`$ ? (@ like_regex "(p")`, "jsonpath", "2201B"},
		{"strict $.**{2 to last}.type()", "jsonpath", ""}, {"$.decimal(5,2)", "jsonpath", ""}, {"$.decimal(1,2,3)", "jsonpath", "42601"},
		{"$ ? (exists(@.a) && !(@.b == 1))", "jsonpath", ""}, {"$ ? ((@.a > 1) is unknown)", "jsonpath", ""}, {"$.a && $.b", "jsonpath", "42601"},
		{"12.34", "money", ""}, {"$1,234.56", "money", ""}, {"(12)", "money", ""}, {"92233720368547758.08", "money", "22003"}, {"12x", "money", "22P02"},
		{"08:00:2b:01:02:03", "macaddr8", ""}, {"08:00:2b:01:02:03:04:05:06", "macaddr8", "22P02"}, {"(1,1)", "tid", ""}, {"(1,65536)", "tid", "22P02"},
		{"12:16:14", "pg_snapshot", ""}, {"12:16:14,13", "pg_snapshot", "22P02"}, {"0x10", "xid", ""}, {"asdf", "xid", "22P02"},
		{"1e400", "float8", "22003"}, {"1e-400", "float8", "22003"}, {"infinity", "float4", ""}, {"1e39", "float4", "22003"},
	}
	for _, c := range cases {
		_, err := Analyze(s, "SELECT "+quoteLit(c.lit)+"::"+c.typ)
		got := ""
		var aerr *Error
		if errors.As(err, &aerr) {
			got = aerr.Code
		} else if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%q::%s: got %q want %q", c.lit, c.typ, got, c.want)
		}
	}
}
