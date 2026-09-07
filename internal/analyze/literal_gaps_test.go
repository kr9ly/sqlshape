package analyze

import (
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestLiteralGaps* exercises less-traveled branches of the ported PostgreSQL input
// functions: datetime.go (timezones, POSIX zone names, ISO 8601 intervals, special
// values), jsonpath.go (predicates, methods, any-levels), literal.go and
// literal_compound.go (reg* types, bit/varbit, arrays, records, geometry, network
// types). Cases are pinned against PostgreSQL 17 behavior (verified with
// TestLiteralOracle where the code comments alone left it ambiguous).

func dtCode(e *Error) string {
	if e == nil {
		return "ok"
	}
	return e.Code
}

// --- datetime.go: timezone decoding, POSIX zone strings ------------------------------

func TestLiteralGapsDecodeTimezone(t *testing.T) {
	cases := []struct {
		in       string
		wantErr  bool
		wantSecs int // -tz, PG sign convention (positive east once negated back)
	}{
		{"+05", false, 0},
		{"-05", false, 0},
		{"+05:30", false, 0},
		{"+05:30:15", false, 0},
		{"+0530", false, 0},
		{"", true, 0},
		{"05", true, 0},                        // missing sign
		{"+99", true, 0},                       // hour out of range
		{"+05:99", true, 0},                    // minute out of range
		{"+05:30:99", true, 0},                 // second out of range
		{"+05x", true, 0},                      // trailing junk
		{"+999999999999999999999999", true, 0}, // overflow -> range error
	}
	for _, c := range cases {
		_, dterr := decodeTimezone(c.in)
		got := dterr != 0
		if got != c.wantErr {
			t.Errorf("decodeTimezone(%q): dterr=%d wantErr=%v", c.in, dterr, c.wantErr)
		}
	}
}

func TestLiteralGapsPosixZoneName(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantRest string
	}{
		{"EST5EDT", true, "EST", "5EDT"},
		{"<+05>-5", true, "+05", "-5"},
		{"<-05>5", true, "-05", "5"},
		{"<no close", false, "", ""},
		{"ab5", false, "", ""}, // fewer than 3 letters
		{"5EST", false, "", ""},
	}
	for _, c := range cases {
		name, rest, ok := posixZoneName(c.in)
		if ok != c.wantOK {
			t.Errorf("posixZoneName(%q): ok=%v want=%v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (name != c.wantName || rest != c.wantRest) {
			t.Errorf("posixZoneName(%q) = %q,%q want %q,%q", c.in, name, rest, c.wantName, c.wantRest)
		}
	}
}

func TestLiteralGapsPosixOffset(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"5", true},
		{"+5", true},
		{"-5", true},
		{"5:30", true},
		{"5:30:15", true},
		{"", false},        // no digits
		{"25", false},      // hour > 24
		{"5:99", false},    // minute > 59
		{"5:30:99", false}, // second > 59
		{"123", false},     // more than 2 digits
	}
	for _, c := range cases {
		_, _, ok := posixOffset(c.in)
		if ok != c.wantOK {
			t.Errorf("posixOffset(%q): ok=%v want=%v", c.in, ok, c.wantOK)
		}
	}
}

func TestLiteralGapsPosixTZ(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"EST5EDT", true},
		{"EST5EDT,M3.2.0,M11.1.0", true},
		{"UTC0", true},
		{"UTC+3", true},
		{"<+05>-5", true},
		{"EST5EDT9", true}, // dst offset given explicitly too
		{"E5", false},      // zone name too short
		{"EST", false},     // no offset at all
		// the rule after the first ',' is not parsed further, only its presence is checked
		{"EST5EDT,M3.2.0,M11.1.0,junk", true},
	}
	for _, c := range cases {
		loc := posixTZ(c.in)
		got := loc != nil
		if got != c.wantOK {
			t.Errorf("posixTZ(%q): got=%v want=%v", c.in, got, c.wantOK)
		}
	}
}

func TestLiteralGapsPgTzSet(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"UTC", true},
		{"America/New_York", true},
		{"america/new_york", true}, // case-insensitive retry
		{"Europe/Paris", true},
		{"EST5EDT", true},
		{"", false},
		{"../etc/passwd", false},
		{"/etc/passwd", false},
		{"Not/AZone", false},
	}
	for _, c := range cases {
		got := pgTzSet(c.in) != nil
		if got != c.wantOK {
			t.Errorf("pgTzSet(%q): got=%v want=%v", c.in, got, c.wantOK)
		}
	}
}

// --- datetime.go: timestamp / date / time / interval literals via validate* ---------

func TestLiteralGapsTimestampSpecials(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ kind, in, want string }{
		{"ts", "1999-01-08 04:05:06 UTC+3", "ok"},
		{"ts", "1999-01-08 04:05:06 Europe/Paris", "ok"},
		{"ts", "1999-01-08T04:05:06", "ok"},  // ISO 8601 T separator
		{"ts", "1999-01-08T04:05:06Z", "ok"}, // Z zulu
		{"ts", "1999-01-08T04:05:06+05:30", "ok"},
		{"ts", "yesterday", "ok"},
		{"ts", "today", "ok"},
		{"ts", "allballs", "22007"}, // a time-only special is not a valid timestamp
		{"date", "yesterday", "ok"},
		{"date", "today", "ok"},
		{"date", "tomorrow", "ok"},
		{"date", "now", "ok"},
		{"date", "allballs", "22007"}, // a time-only special is not a date
		{"time", "now", "ok"},         // "now" resolves to the current time-of-day
		{"ts", "January 8 04:05:06 1999", "ok"},
		{"ts", "1999-01-08 04:05:06.123456", "ok"},
		{"ts", "1999-01-08 04:05:06.1234567890", "ok"}, // extra fraction digits truncated
		{"ts", "1999-01-08 04:05:06 +05", "ok"},
		{"ts", "1999-01-08 04:05:06-05", "ok"},
		{"tstz", "1999-01-08 04:05:06 America/New_York", "ok"},
		{"tstz", "1999-01-08 04:05:06 Not/AZone", "22023"},
	}
	for _, c := range cases {
		var got string
		switch c.kind {
		case "ts":
			got = dtCode(validateTimestampLiteral(c.in, false, sess, 0))
		case "tstz":
			got = dtCode(validateTimestampLiteral(c.in, true, sess, 0))
		case "date":
			got = dtCode(validateDateLiteral(c.in, sess, 0))
		case "time":
			got = dtCode(validateTimeLiteral(c.in, false, sess, 0))
		}
		if got != c.want {
			t.Errorf("%s %q: got %s want %s", c.kind, c.in, got, c.want)
		}
	}
}

func TestLiteralGapsIntervalISO8601(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ in, want string }{
		{"P1Y", "ok"},
		{"P1M", "ok"},
		{"P1W", "ok"},
		{"P1D", "ok"},
		{"P1Y2M", "ok"},
		{"P1Y2M3W4D", "ok"},
		{"PT1H", "ok"},
		{"PT1M", "ok"},
		{"PT1S", "ok"},
		{"PT1H2M3S", "ok"},
		{"P1Y2M3DT4H5M6S", "ok"},
		{"P-1Y-2M", "ok"},
		{"P1.5Y", "ok"},
		{"PT1.5S", "ok"},
		{"P0002-06-07", "ok"},          // extended-format date only, no time
		{"P00020607", "ok"},            // compact-format date only
		{"P00020607T013000", "ok"},     // compact-format date+time
		{"P0002-06-07T01:30:00", "ok"}, // extended-format date+time
		{"PT010203", "ok"},             // compact-format time only
		{"PT01:02:03", "ok"},           // colon-separated time
		{"PT", "ok"},
		{"P", "22007"}, // too short to carry a field ("P" alone is bad format)
		{"", "22007"},
		{"P1Q", "22007"},  // bad unit
		{"PT1Q", "22007"}, // bad unit in time part
		{"P1Y1Y", "ok"},   // repeated field: accumulates rather than erroring
		{"PT1H1H", "ok"},
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, -1, sess, 0))
		if got != c.want {
			t.Errorf("interval %q: got %s want %s", c.in, got, c.want)
		}
	}
}

// decodeISO8601Interval is called directly (bypassing the interval tokenizer, which does
// not reliably route an overflowing ISO number through decodeISO8601Interval) to reach
// parseISO8601Number's range check.
func TestLiteralGapsISO8601NumberOverflow(t *testing.T) {
	// a value that parses as a float64 fine but exceeds ISO8601Number's own +-1e15 bound
	// (a true overflow like "1e400" fails ParseFloat outright with ErrRange, which
	// strtod already turns into !ok / dtErrBadFormat before this check is ever reached)
	if _, _, ret := decodeISO8601Interval("P9999999999999999Y"); ret != dtErrFieldOverflow {
		t.Errorf("decodeISO8601Interval(%q): ret=%d want dtErrFieldOverflow(%d)", "P9999999999999999Y", ret, dtErrFieldOverflow)
	}
	if _, _, ret := decodeISO8601Interval("P"); ret != dtErrBadFormat {
		t.Errorf(`decodeISO8601Interval("P"): ret=%d want dtErrBadFormat(%d)`, ret, dtErrBadFormat)
	}
	if _, _, ret := decodeISO8601Interval("X1Y"); ret != dtErrBadFormat {
		t.Errorf(`decodeISO8601Interval("X1Y"): ret=%d want dtErrBadFormat(%d)`, ret, dtErrBadFormat)
	}
}

func TestLiteralGapsIntervalMisc(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ in, want string }{
		{"1 day 2:03:04", "ok"},
		{"@ 1 day 2 hours 3 mins ago", "ok"},
		{"1 year 2 months 3 days 4 hours 5 minutes 6 seconds", "ok"},
		{"1.5 days", "ok"},
		{"-1.5 days", "ok"},
		{"1 d", "ok"},
		{"1 hr", "ok"},
		{"1 min", "ok"},
		{"1 sec", "ok"},
		{"1 decade", "ok"},
		{"1 decades", "ok"},
		{"1 century", "ok"},
		{"1 centuries", "ok"},
		{"+1:00:00", "ok"},
		{"-1:00:00", "ok"},
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, -1, sess, 0))
		if got != c.want {
			t.Errorf("interval %q: got %s want %s", c.in, got, c.want)
		}
	}
}

// --- jsonpath.go: predicates, methods, any-levels, like_regex flags -----------------

func jpCheck(t *testing.T, s *schema.Schema, lit, want string) {
	t.Helper()
	_, err := Analyze(s, "SELECT "+quoteLit(lit)+"::jsonpath")
	got := ""
	var aerr *Error
	if errors.As(err, &aerr) {
		got = aerr.Code
	} else if err != nil {
		got = err.Error()
	}
	if got != want {
		t.Errorf("%q::jsonpath: got %q want %q", lit, got, want)
	}
}

func TestLiteralGapsJsonpath(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, want string }{
		// methods
		{"$.type()", ""},
		{"$.size()", ""},
		{"$.double()", ""},
		{"$.ceiling()", ""},
		{"$.floor()", ""},
		{"$.abs()", ""},
		{"$.bigint()", ""},
		{"$.boolean()", ""},
		{"$.integer()", ""},
		{"$.number()", ""},
		{"$.string()", ""},
		{"$.keyvalue()", ""},
		{"$.datetime()", ""},
		{`$.datetime("YYYY-MM-DD")`, ""},
		{"$.time()", ""},
		{"$.time(3)", ""},
		{"$.time_tz()", ""},
		{"$.time_tz(2)", ""},
		{"$.timestamp()", ""},
		{"$.timestamp(1)", ""},
		{"$.timestamp_tz()", ""},
		{"$.timestamp_tz(2)", ""},
		{"$.decimal()", ""},
		{"$.decimal(5)", ""},
		{"$.decimal(5,2)", ""},
		{"$.decimal(+5,-2)", ""},
		{"$.decimal(1,2,3)", "42601"},
		// any-level
		{"$.**", ""},
		{"$.**{2}", ""},
		{"$.**{2 to 3}", ""},
		{"$.**{last}", ""},
		{"$.**{2 to last}", ""},
		{"$.**{}", "42601"},
		// subscripts / last
		{"$[*]", ""},
		{"$[0]", ""},
		{"$[0,1]", ""},
		{"$[0 to 2]", ""},
		{"$[last]", ""},
		{"$[last - 1]", ""},
		{"last", "42601"}, // LAST outside subscript
		// predicates
		{"$ ? (@.a > 1 && @.b < 2)", ""},
		{"$ ? (@.a > 1 || @.b < 2)", ""},
		{"$ ? (!(@.a > 1))", ""},
		{"$ ? (!exists(@.a))", ""},
		{`$ ? (@.a like_regex "^a" flag "i")`, ""},
		{`$ ? (@.a like_regex "^a" flag "is")`, ""},
		{`$ ? (@.a like_regex "^a" flag "isim")`, ""},
		{`$ ? (@.a like_regex "^a" flag "q")`, ""},
		{`$ ? (@.a like_regex "^a" flag "xq")`, ""},
		{`$ ? (@.a like_regex "^a" flag "x")`, "0A000"},
		{`$ ? (@.a like_regex "^a" flag "z")`, "42601"},
		{`$ ? (@.a like_regex "(")`, "2201B"},
		{"$ ? (@.a starts with \"foo\")", ""},
		{"$ ? (@.a starts with $x)", ""},
		{"$ ? (@.a starts with 1)", "42601"},
		{"$ ? ((@.a > 1) is unknown)", ""},
		{"$ ? (1 is unknown)", "42601"},    // "is unknown" needs a parenthesized predicate
		{"$ ? (exists(@.a > 1))", "42601"}, // exists() takes an expr, not a predicate
		{"$ ? (!(1 + 1))", "42601"},        // ! needs a predicate
		{"$ ? (1 == (@.a > 1))", "42601"},  // comparison operand cannot be a predicate
		{"strict $.a", ""},
		{"lax $.a", ""},
		{"$.a + $.b", ""},
		{"$.a - $.b", ""},
		{"$.a * $.b", ""},
		{"$.a / $.b", ""},
		{"$.a % $.b", ""},
		{"-$.a", ""},
		{"+$.a", ""},
		{"($.a + $.b) * 2", ""},
		{"@", "42601"}, // @ at root nesting <= 0
		{"$ ? (@.a == 1)", ""},
		{"$ ? (@.a <> 1)", ""},
		{"$ ? (@.a != 1)", ""},
	}
	for _, c := range cases {
		jpCheck(t, s, c.lit, c.want)
	}
}

// --- literal.go: reg* types -----------------------------------------------------------

func TestLiteralGapsRegTypes(t *testing.T) {
	s, err := Load(`
		CREATE TABLE gaps_t (a int PRIMARY KEY, b text);
		CREATE INDEX gaps_t_b_idx ON gaps_t (b);
	`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"int4", "regtype", ""},
		{"pg_catalog.int4", "regtype", ""},
		{"integer", "regtype", ""},
		{"no_such_type_xyz", "regtype", "42704"}, // parses fine as a type name, just unknown
		{"nosuchschema.int4", "regtype", "3F000"},
		{"123", "regtype", ""},
		{"-", "regtype", ""},
		{"sum", "regproc", ""},
		{"pg_catalog.sum", "regproc", ""},
		{"no_such_func_xyz", "regproc", "42883"},
		{"123", "regproc", ""},
		{"=", "regoper", ""},
		{"no_such_op_xyz", "regoper", "42883"},
		{"nosuchschema.+", "regoper", "42883"},
		{"gaps_t", "regclass", ""},
		{"gaps_t_b_idx", "regclass", ""},
		{"gaps_t_pkey", "regclass", ""},
		{"no_such_rel_xyz", "regclass", "42P01"},
		{"123", "regclass", ""},
		{"public.gaps_t", "regclass", ""},
		{"\"default\"", "regrole", ""},
		{"a.b.c", "regrole", "42602"},
		{"public", "regnamespace", ""},
		{"nosuchschema", "regnamespace", "3F000"},
		{"a.b.c", "regnamespace", "42602"},
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

// --- literal.go/literal_compound.go: bit strings, bool, uuid, xml, tsvector ---------

func TestLiteralGapsMisc(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"101", "bit(3)", ""},
		// an explicit CAST reads the input function unconstrained and then length-coerces
		// (never fails); validateBitLiteral's typmod-mismatch branches are reached through
		// assignment context instead (TestLiteralGapsAssignLength).
		{"101", "bit(4)", ""},
		{"1010", "bit varying(3)", ""},
		{"1010", "bit varying(4)", ""},
		{"1010", "bit varying", ""},
		{"B101", "bit(3)", ""},
		{"X1F", "bit varying", ""},
		{"X1G", "bit varying", "22P02"},
		{"102", "bit(3)", "22P02"},
		{"t", "bool", ""},
		{"tru", "bool", ""},
		{"yes", "bool", ""},
		{"n", "bool", ""},
		{"of", "bool", ""},
		{"maybe", "bool", "22P02"},
		{"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "uuid", ""},
		{"not-a-uuid", "uuid", "22P02"},
		{"<a>1</a>", "xml", ""},
		{"<a><b/></a>", "xml", ""},
		{"<a><b></a>", "xml", "2200N"},
		{"hello world", "tsvector", ""},
		{"'a b':1,2 'c'", "tsvector", ""},
		{"''", "tsvector", "42601"},
		{"'unterminated", "tsvector", "42601"},
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

// TestLiteralGapsAssignLength reaches validateAssignLength's typmod-mismatch branches:
// unlike an explicit CAST (coerce_type, typmod-unconstrained), assignment into a table
// column enforces the column's length/precision and PG fails at execution, which this
// analyzer reports as a noteAlwaysFails Note rather than an Error (Prepare still succeeds).
func TestLiteralGapsAssignLength(t *testing.T) {
	s, err := Load(`
		CREATE TABLE gaps_assign (
			b  bit(4),
			vb bit varying(3),
			nc numeric(3,1),
			vc varchar(2),
			c  char(2)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql      string
		wantFail bool
	}{
		{"INSERT INTO gaps_assign (b) VALUES ('101')", true},    // 3 bits into bit(4)
		{"INSERT INTO gaps_assign (b) VALUES ('1010')", false},  // exact fit
		{"INSERT INTO gaps_assign (vb) VALUES ('1010')", true},  // 4 bits into varbit(3)
		{"INSERT INTO gaps_assign (vb) VALUES ('101')", false},  // exact fit
		{"INSERT INTO gaps_assign (nc) VALUES ('123.4')", true}, // overflow precision 3
		{"INSERT INTO gaps_assign (nc) VALUES ('12.3')", false}, // fits
		{"INSERT INTO gaps_assign (vc) VALUES ('hello')", true}, // too long for varchar(2)
		{"INSERT INTO gaps_assign (c) VALUES ('h ')", false},    // trailing space padding, fits
	}
	for _, c := range cases {
		res, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: unexpected Error %v", c.sql, err)
			continue
		}
		got := false
		for _, n := range res.Notes {
			if n.Code == noteAlwaysFails {
				got = true
			}
		}
		if got != c.wantFail {
			t.Errorf("%s: noteAlwaysFails=%v want %v (notes=%v)", c.sql, got, c.wantFail, res.Notes)
		}
	}
}

// --- literal_compound.go: arrays, records, geometry, network types ------------------

func TestLiteralGapsArrays(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"{{1,2},{3,4}}", "int[]", ""},
		{"[1:2][1:2]={{1,2},{3,4}}", "int[]", ""},
		{"[1:2][1:2]={{1,2},{3,4,5}}", "int[]", "22P02"},
		{`{"a\"b","c"}`, "text[]", ""},
		{"{a,{b,c}}", "text[]", "22P02"}, // ragged nesting
		{"1 2 3", "int2vector", ""},
		{"1 a 3", "int2vector", "22P02"},
		{"1 2 3", "oidvector", ""},
		{"a b c", "oidvector", "22P02"},
		{"[1:1000000000][1:1000000000][1:1000000000][1:1000000000][1:1000000000][1:1000000000][1:1000000000]={}", "int[]", "54000"},
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

func TestLiteralGapsRecords(t *testing.T) {
	s, err := Load("CREATE TABLE gaps_row (a int, b text);")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, want string }{
		{"(1,hello)", ""},
		{`(1,"he said ""hi""")`, ""},
		{"(,)", ""},                  // both fields NULL
		{"(1,)", ""},                 // trailing NULL
		{"(1,hello", "22P02"},        // missing close paren
		{"1,hello)", "22P02"},        // missing open paren
		{"(1,hello,extra)", "22P02"}, // too many fields
		{"(1,hello) x", "22P02"},     // trailing junk
	}
	for _, c := range cases {
		_, err := Analyze(s, "SELECT "+quoteLit(c.lit)+"::gaps_row")
		got := ""
		var aerr *Error
		if errors.As(err, &aerr) {
			got = aerr.Code
		} else if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%q::gaps_row: got %q want %q", c.lit, got, c.want)
		}
	}
}

func TestLiteralGapsGeometry(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"[(0,0),(1,1)]", "line", ""},
		{"[(1,1),(1,1)]", "line", "22P02"}, // must be two distinct points
		{"{0,0,5}", "line", "22P02"},       // A and B both zero
		{"{1,2,3}", "line", ""},
		{"((1,2),(3,4))", "path", ""}, // explicit outer parens (closed)
		{"(1,2),(3,4)", "path", ""},
		{"[(1,2),(3,4)]", "path", ""},
		{"((1,2),(3,4)", "path", "22P02"}, // unbalanced parens
		{"((0,0),3)", "circle", ""},       // alternate paren form
		{"<(0,0),3>", "circle", ""},
		{"<(0,0),-3>", "circle", "22P02"},
		{"((1,2),(3,4),(5,6))", "polygon", ""},
		{"((1,2),(3,4)", "polygon", "22P02"},
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

func TestLiteralGapsNetwork(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"192.168.1.1", "inet", ""},
		{"192.168.1.1/24", "inet", ""},
		{"192.168.1", "inet", "22P02"}, // a partial address needs a mask or cidr
		{"::1", "inet", ""},
		{"::1/128", "inet", ""},
		{"192.168.1.1/33", "inet", "22P02"},
		{"192.168.1.1%eth0", "inet", "22P02"},
		{"192.168.1", "cidr", ""},
		{"192.168.1.5/24", "cidr", "22P02"}, // host bits set
		{"192.168.1.0/24", "cidr", ""},
		{"::/0", "cidr", ""},
		{"08:00:2b:01:02:03", "macaddr", ""},
		{"08-00-2b-01-02-03", "macaddr", ""},
		{"0800.2b01.0203", "macaddr", ""},
		{"08002b:010203", "macaddr", ""},
		{"0800.2b01.0203.99", "macaddr", "22P02"},
		{"0/12345678", "pg_lsn", ""},
		{"0/1234567890ABCDEF", "pg_lsn", "22P02"},
		{"nope", "pg_lsn", "22P02"},
		{`\\`, "bytea", ""},
		{`\052`, "bytea", ""},
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

func TestLiteralGapsRanges(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"(1,10)", "int4range", ""},
		{"[1,)", "int4range", ""},
		{"(,10]", "int4range", ""},
		{"(,)", "int4range", ""},
		{"{[1,2),[3,4)}", "int4multirange", ""},
		{"{}", "int4multirange", ""},
		{"{[1,2),[10,4)}", "int4multirange", "22000"},
		{"[5,1)", "numrange", "22000"}, // compareLiterals: numeric branch
		{"[1,5)", "numrange", ""},
		{"[2020-02-01,2020-01-01)", "daterange", "22000"}, // compareLiterals: date branch (timestampValue)
		{"[2020-01-01,2020-02-01)", "daterange", ""},
		{"[2020-02-01 00:00:00,2020-01-01 00:00:00)", "tsrange", "22000"},
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

// --- literal.go: bit string / numeric string constants, non-decimal integers -------

func TestLiteralGapsBitConstAndNonDecimal(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	sqlCases := []struct{ sql, want string }{
		{"SELECT B'101'", ""},
		{"SELECT B'102'", "22P02"},
		{"SELECT X'1F'", ""},
		{"SELECT X'1G'", "22P02"},
	}
	for _, c := range sqlCases {
		_, err := Analyze(s, c.sql)
		got := ""
		var aerr *Error
		if errors.As(err, &aerr) {
			got = aerr.Code
		} else if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
	litCases := []struct{ lit, typ, want string }{
		{"0x1F", "numeric", ""},
		{"0o17", "numeric", ""},
		{"0b101", "numeric", ""},
		{"0x1_F", "numeric", ""},
		{"0x_1F", "numeric", ""},
		{"0x1__F", "numeric", "22P02"}, // double separator
		{"0x1F_", "numeric", "22P02"},  // trailing separator
		{"0x1G", "numeric", "22P02"},
		{"0x1F", "int4", ""},
		{"0o17", "int4", ""},
		{"0b101", "int4", ""},
		{"1_000", "int4", ""},
	}
	for _, c := range litCases {
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
