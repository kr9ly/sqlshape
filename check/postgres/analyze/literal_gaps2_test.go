package analyze

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiteralGaps2* is the second round on internal/analyze's ports of PostgreSQL's
// datetime.c, jsonpath scanner/parser, and the input-function checks in literal.go /
// literal_compound.go / collation.go. See literal_gaps_test.go for the first round and
// its harness notes (TestLiteralOracle, dtCode, quoteLit).

// --- datetime.go: parseDateTime tokenizer edge cases --------------------------------

func TestLiteralGaps2ParseDateTime(t *testing.T) {
	// a field starting with '.' (not preceded by a digit) is DTK_NUMBER: datetime.go:507-514
	if fields, ftypes, dterr := parseDateTime(".5", 128); dterr != 0 || len(fields) != 1 || fields[0] != ".5" || ftypes[0] != dtkNumber {
		t.Errorf(`parseDateTime(".5"): fields=%v ftypes=%v dterr=%d`, fields, ftypes, dterr)
	}
	// a lone sign followed by neither digit nor letter is a format error: datetime.go:565-566
	if _, _, dterr := parseDateTime("+", 128); dterr != dtErrBadFormat {
		t.Errorf(`parseDateTime("+"): dterr=%d want dtErrBadFormat`, dterr)
	}
	if _, _, dterr := parseDateTime("+!", 128); dterr != dtErrBadFormat {
		t.Errorf(`parseDateTime("+!"): dterr=%d want dtErrBadFormat`, dterr)
	}
	// punctuation between fields is skipped outright (not even a delimiter): datetime.go:568-570
	if fields, ftypes, dterr := parseDateTime("1997,01,02", 128); dterr != 0 || len(fields) != 3 || ftypes[0] != dtkNumber {
		t.Errorf(`parseDateTime("1997,01,02"): fields=%v ftypes=%v dterr=%d`, fields, ftypes, dterr)
	}
	// more than maxDateFields fields is a format error: datetime.go:453-455
	long := ""
	for i := 0; i < maxDateFields+2; i++ {
		long += "1 "
	}
	if _, _, dterr := parseDateTime(long, maxDateLen+maxDateFields); dterr != dtErrBadFormat {
		t.Errorf("parseDateTime(<%d fields>): dterr=%d want dtErrBadFormat", maxDateFields+2, dterr)
	}
	// a workbuf overflow (the C buflen bound) is also a format error: datetime.go:574-577
	if _, _, dterr := parseDateTime("12345 12345 12345", 5); dterr != dtErrBadFormat {
		t.Errorf(`parseDateTime with buflen=5: dterr=%d want dtErrBadFormat`, dterr)
	}
}

// strtod's own leading-whitespace and word-prefix handling: datetime.go:163-165. Called
// directly since every literal_in caller already strips leading space before reaching it.
func TestLiteralGaps2Strtod(t *testing.T) {
	if val, rest, ok := strtod("  5.5"); !ok || rest != "" || val != 5.5 {
		t.Errorf(`strtod("  5.5") = %v,%q,%v`, val, rest, ok)
	}
	if val, rest, ok := strtod("   -3"); !ok || rest != "" || val != -3 {
		t.Errorf(`strtod("   -3") = %v,%q,%v`, val, rest, ok)
	}
}

// --- datetime.go: DecodeDateTime / DecodeTimeOnly through validate*Literal ----------

func TestLiteralGaps2Timestamp(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ kind, in, want string }{
		// two-digit years (validateDate's is2digits branch)
		{"date", "01-02-70", "ok"}, // >= 70 -> 1970
		{"date", "01-02-69", "ok"}, // < 70 -> 2069
		{"date", "01-02-03", "ok"}, // < 100 -> 1903? (>=70 else 1900s), exercises the two branches
		{"ts", "01-02-03 04:05:06", "ok"},
		// BC dates
		{"date", "0001-01-01 BC", "ok"},
		{"date", "4715-01-01 BC", "22008"}, // past the Julian minimum, out of range
		{"ts", "0044-01-01 12:00:00 BC", "ok"},
		{"date", "0000-01-01 BC", "22008"}, // year 0 is invalid once BC is applied
		// AM/PM (12-hour forms), with and without a date
		{"ts", "1999-01-08 12:00:00 AM", "ok"},    // 12 AM -> hour 0
		{"ts", "1999-01-08 12:00:00 PM", "ok"},    // 12 PM -> hour 12 (unchanged)
		{"ts", "1999-01-08 01:00:00 PM", "ok"},    // 1 PM -> hour 13
		{"ts", "1999-01-08 13:00:00 PM", "22008"}, // 13 PM: mer set but hour already > 12
		{"time", "12:00:00 AM", "ok"},
		{"time", "11:59:59 PM", "ok"},
		// day-of-year (three-digit lone number in a year-only date field)
		{"date", "1999 032", "ok"}, // the 32nd day of 1999
		// T / t separators, with a following zone
		{"ts", "1999-01-08t04:05:06", "ok"},
		{"ts", "1999-01-08t04:05:06-05", "ok"},
		{"tstz", "1999-01-08t04:05:06 America/New_York", "ok"},
		// zone abbreviations, including DST ones
		{"tstz", "1999-07-08 04:05:06 EST", "ok"},
		{"tstz", "1999-07-08 04:05:06 PDT", "ok"},
		{"tstz", "1999-07-08 04:05:06 CEST", "ok"},
		{"tstz", "1999-07-08 04:05:06 JST", "ok"},
		{"tstz", "1999-07-08 04:05:06 MSK", "ok"}, // a DYNTZ abbreviation (Europe/Moscow)
		// a fixed-offset named zone needs no date for time-with-timezone
		{"time", "04:05:06 UTC+3", "ok"},
		{"time", "04:05:06 America/New_York", "22007"}, // not fixed-offset: needs a date (verified
		// against PG 17: 22007, the zone itself resolves fine)
		// julian day number literal
		{"date", "julian 2451545", "ok"},
		{"date", "j 2451545", "ok"},
		{"date", "J2451546.5", "ok"}, // a fractional julian day carries a time-of-day
		// special values
		{"date", "epoch", "ok"},
		{"date", "infinity", "ok"},
		{"date", "-infinity", "ok"},
		{"date", "+infinity", "ok"},
		{"ts", "epoch", "ok"},
		{"ts", "infinity", "ok"},
		{"ts", "-infinity", "ok"},
		// run-together (undelimited) numeric date / time
		{"date", "20050425", "ok"}, // 8-digit YYYYMMDD
		{"date", "990425", "ok"},   // 6-digit YYMMDD, two-digit year
		{"time", "040506", "ok"},   // 6-digit HHMMSS
		{"ts", "20050425 040506", "ok"},
		// a DST modifier applied to a zone abbreviation
		{"tstz", "1999-07-08 04:05:06 EST DST", "ok"},
		{"time", "04:05:06 EST DST", "ok"},
		// field overflow in the h:mm:ss decoder
		{"time", "99999999999999999999:00:00", "22008"},
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
			got = dtCode(validateTimeLiteral(c.in, true, sess, 0))
		}
		if got != c.want {
			t.Errorf("%s %q: got %s want %s", c.kind, c.in, got, c.want)
		}
	}
}

// --- datetime.go: DecodeInterval unit spellings and field ranges --------------------

func TestLiteralGaps2IntervalUnits(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ in, want string }{
		{"1 millennium", "ok"},
		{"1 millennia", "ok"},
		{"1 mil", "ok"},
		{"1 mils", "ok"},
		{"1 microsecond", "ok"},
		{"1 microseconds", "ok"},
		{"1 usec", "ok"},
		{"1 us", "ok"},
		{"1 millisecond", "ok"},
		{"1 milliseconds", "ok"},
		{"1 ms", "ok"},
		{"1 msec", "ok"},
		{"1 w", "ok"},
		{"1 week", "ok"},
		{"1 weeks", "ok"},
		{"1 y", "ok"},
		{"1 yr", "ok"},
		{"1 yrs", "ok"},
		{"1 mon", "ok"},
		{"1 mons", "ok"},
		{"1 m", "ok"}, // ambiguous single-letter unit: "m" is minute in deltatktbl
		// units PG's interval grammar does not accept
		{"1 quarter", "22007"},
		{"1 timezone", "22007"},
		// a huge hour field in the h:mm:ss interval form overflows in decodeTimeForInterval
		{"999999999999:00:00", "22015"},
		// ISO-8601 unit-letter interval typmod ranges (typmod encodes YEAR TO MONTH etc)
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, -1, sess, 0))
		if got != c.want {
			t.Errorf("interval %q: got %s want %s", c.in, got, c.want)
		}
	}
}

// year-to-month / day-to-second field ranges are the typmod interval_in reads (the high
// 16 bits are which fields the type declares, YEAR|MONTH etc.), constructed the way
// gram.y's INTERVAL '...' YEAR TO MONTH does: bit imYear|imMonth etc. shifted into place.
func TestLiteralGaps2IntervalTypmodRange(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	typmodFor := func(rangeMask int) int32 { return int32(rangeMask<<16) | intervalFullPrec }
	cases := []struct {
		in   string
		rng  int
		want string
	}{
		{"1-2", imYear | imMonth, "ok"},                           // '1-2' YEAR TO MONTH: 1 year 2 months
		{"1 2:03:04", imDay | imHour | imMinute | imSecond, "ok"}, // '1 2:03:04' DAY TO SECOND
		{"2:03", imHour | imMinute, "ok"},
		// MINUTE TO SECOND reinterprets "H:M" as minutes:seconds; a huge first field
		// overflows int32 in that reinterpretation (datetime.go:960-962)
		{"99999999999:30", imMinute | imSecond, "22015"},
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, typmodFor(c.rng), sess, 0))
		if got != c.want {
			t.Errorf("interval %q (typmod range %#x): got %s want %s", c.in, c.rng, got, c.want)
		}
	}
}

// --- datetime.go: DecodeISO8601Interval edge cases ----------------------------------

func TestLiteralGaps2ISO8601(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ in, want string }{
		{"PY", "22007"},            // a unit with no leading number: parseISO8601Number's first-char guard
		{"P9999999999Y", "22015"},  // year overflows int32 in adjustYears
		{"P9999999999M", "22015"},  // month overflows int32 in adjustMonths
		{"P9999999999W", "22015"},  // week overflows int32 in adjustDays
		{"P9999999999D", "22015"},  // day overflows int32 in adjustDays
		{"PT9999999999H", "22015"}, // hour-to-microseconds overflows int64
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, -1, sess, 0))
		if got != c.want {
			t.Errorf("interval %q: got %s want %s", c.in, got, c.want)
		}
	}
	// direct calls for the tokenizer guard itself
	if _, _, ret := decodeISO8601Interval(""); ret != dtErrBadFormat {
		t.Errorf(`decodeISO8601Interval(""): ret=%d want dtErrBadFormat`, ret)
	}
	if _, _, _, ret := parseISO8601Number(""); ret != dtErrBadFormat {
		t.Errorf(`parseISO8601Number(""): ret=%d want dtErrBadFormat`, ret)
	}
	if _, _, _, ret := parseISO8601Number("Y"); ret != dtErrBadFormat {
		t.Errorf(`parseISO8601Number("Y"): ret=%d want dtErrBadFormat`, ret)
	}
}

// decodeTimeCommon's own overflow / malformed-fraction branches, reached through the
// h:mm[:ss[.ffff]] interval time form (datetime.go:939-1002).
func TestLiteralGaps2TimeCommon(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	cases := []struct{ in, want string }{
		{"5:99999999999999999999:00", "22015"}, // minute field overflows in strtoNum
		{"99999999999:30.5", "22015"},          // hour field overflows ahead of a H:M.fff fraction
		{"5:30:99999999999999999999", "22015"}, // second field overflows in strtoNum
		{"5:30:20..5", "22007"},                // a malformed fraction (parseFractionalSecond fails)
	}
	for _, c := range cases {
		got := dtCode(validateIntervalLiteral(c.in, -1, sess, 0))
		if got != c.want {
			t.Errorf("interval %q: got %s want %s", c.in, got, c.want)
		}
	}
}

// --- collation.go --------------------------------------------------------------------

func TestLiteralGaps2CollName(t *testing.T) {
	if got := collName(nil); got != "" {
		t.Errorf("collName(nil) = %q, want empty", got)
	}
	if got := collName([]string{"pg_catalog", "default"}); got != "" {
		t.Errorf(`collName({"pg_catalog","default"}) = %q, want empty`, got)
	}
	if got := (collation{}).display(); got != "default" {
		t.Errorf(`collation{}.display() = %q, want "default"`, got)
	}
	if got := collDisplay(""); got != "default" {
		t.Errorf(`collDisplay("") = %q, want "default"`, got)
	}
}

func TestLiteralGaps2CollOf(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	a := newAnalyzer(s, nil, nil)
	es := []*expr{nil, {coll: collation{strength: collImplicit, name: "foo"}}, nil}
	acc, cerr := a.collOf(es)
	if cerr != nil || acc.strength != collImplicit || acc.name != "foo" {
		t.Errorf("collOf with nil elements: acc=%+v err=%v", acc, cerr)
	}
}

// explicitCollate's bind branch: a literal still typed "unknown" (NULL) is bound to text
// before the collatable check runs, rather than being rejected outright.
func TestLiteralGaps2ExplicitCollateBind(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(s, `SELECT NULL COLLATE "C"`); err != nil {
		t.Errorf(`NULL COLLATE "C": unexpected error %v`, err)
	}
}

// setOpColl: UNION (not ALL) checks the collation conflict at parse time; UNION ALL
// leaves it for run time (a Note, not an Error).
func TestLiteralGaps2SetOpColl(t *testing.T) {
	s, err := Load(`
		CREATE TABLE gaps2_t1 (a text COLLATE "C");
		CREATE TABLE gaps2_t2 (b text COLLATE "POSIX");
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(s, `SELECT a FROM gaps2_t1 UNION SELECT b FROM gaps2_t2`)
	var e *Error
	if !errors.As(aerr, &e) || e.Code != "42P21" {
		t.Errorf("UNION of conflicting collations: got %v, want 42P21", aerr)
	}
	res, err2 := Analyze(s, `SELECT a FROM gaps2_t1 UNION ALL SELECT b FROM gaps2_t2`)
	if err2 != nil {
		t.Errorf("UNION ALL of conflicting collations: unexpected error %v", err2)
	} else if res == nil {
		t.Errorf("UNION ALL of conflicting collations: no result")
	}
	// two different *explicit* collations conflicting across a UNION: mergeColl itself
	// raises the error (as it does within a single expression), not the indeterminate-
	// implicit-collation path.
	_, aerr3 := Analyze(s, `SELECT a COLLATE "C" FROM gaps2_t1 UNION SELECT b COLLATE "POSIX" FROM gaps2_t2`)
	var e3 *Error
	if !errors.As(aerr3, &e3) || e3.Code != "42P21" {
		t.Errorf("UNION of conflicting explicit collations: got %v, want 42P21", aerr3)
	}
}

// --- jsonpath.go -----------------------------------------------------------------------

func TestLiteralGaps2Jsonpath(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, want string }{
		// comments
		{"$.a /* a comment */ .b", ""},
		{"$.a /* unterminated", "42601"},
		// quoted variable names
		{`$"my var".a`, ""},
		// unterminated string
		{`"unterminated`, "42601"},
		// backslash at end of input
		{`"abc\`, "42601"},
		// \x escapes
		{`"\x4"`, "42601"},  // only one hex digit
		{`"\x00"`, "22P05"}, // NUL
		{`"\x41"`, ""},
		// \u escapes
		{`"\u{}"`, "42601"},         // no hex digits inside braces
		{`"\u{41"`, "42601"},        // missing closing brace
		{`"\u0000"`, "22P05"},       // NUL
		{`"\uD800X"`, "22P02"},      // lone high surrogate then a plain char
		{`"\uD800\n"`, "22P02"},     // lone high surrogate then a non-\u escape
		{`"\uD800"`, "22P02"},       // lone high surrogate at end of string
		{`"\uD800\u0041"`, "22P02"}, // lone high surrogate then another \u escape that
		// is itself a normal (non-surrogate) codepoint
		// trailing junk after a complete path
		{"$.a $.b", "42601"},
		// || / && with a non-predicate operand
		{"$ ? (@.a > 1 || 2)", "42601"},
		{"$ ? (@.a > 1 && 2)", "42601"},
		// ! not followed by a delimited predicate
		{"$ ? (!1)", "42601"},
		// exists() malformed
		{"$ ? (exists @.a)", "42601"}, // missing '('
		{"$ ? (exists(@.a", "42601"},  // missing ')' (both of them)
		// exists(...) at top level (not wrapped in a filter) with its own ')' missing
		{"exists(1", "42601"}, // valid inner expr (no @, so no nesting error), but no closing ')'
		// '!' delimited predicate: an inner parse error, and a missing ')'
		{"!(@.a >", "42601"},
		{"!(1 > 0", "42601"}, // valid inner predicate (no @), but no closing ')'
		// a comparison whose right operand fails to parse, at top level
		{"@.a >", "42601"},
		// a non-method keyword called like a method, and '.' followed by an unexpected token
		{"$.exists()", "42601"},
		{"$ ? ((@.a > 1", "42601"}, // missing ')' (both of them)
		// left side already a predicate
		{"$ ? ((@.a > 1) == 1)", "42601"},
		{"$ ? ((@.a > 1) starts with \"x\")", "42601"},
		{"$ ? ((@.a > 1) like_regex \"x\")", "42601"},
		{"$ ? (@.a like_regex \"x\" flag 5)", "42601"},
		{"$ ? ((@.a > 1) is (@.b > 2))", "42601"},
		// arithmetic on a predicate operand
		{"$ ? (@.a + (@.b > 1) > 2)", "42601"},
		{"$ ? (@.a * (@.b > 1) > 2)", "42601"},
		{"$ ? (-(@.a > 1) > 0)", "42601"},
		// a parenthesized predicate followed by an accessor is read as an expr
		{"$ ? ((@.a > 1).type() == \"boolean\")", ""},
		// accessor primary: unexpected token
		{"$.a + )", "42601"},
		// '.' followed by neither * nor ** nor a name
		{"$.5", "42601"},
		// a plain (non-method) name called like a method
		{"$.foo()", "42601"},
		// .decimal(...) with a bare trailing comma
		{"$.decimal(1,)", "42601"},
		{"$.decimal()", ""},
		// subscript: predicate where an index expr is expected
		{"$[1==1]", "42601"},
		{"$[0 to 1==1]", "42601"},
		// filter: missing '(' / a non-predicate body
		{"$ ? @.a", "42601"},
		{"$ ? (1+1)", "42601"},
		// an unterminated quoted variable name
		{`$"unterminated`, "42601"},
		// an escape error reached through the bare-identifier path (not a quoted string)
		{`$.\x4`, "42601"},
		// an identifier ends at a comment start, which is then skipped on the next token
		{"$.abc/**/.def", ""},
		// error propagation: the right side of || / && itself fails to parse
		{"$ ? (@.a > 1 || 1+)", "42601"},
		{"$ ? (@.a > 1 && 1+)", "42601"},
		// error propagation inside exists(...) and a delimited predicate
		{"$ ? (exists(1+))", "42601"},
		{"$ ? ((1+))", "42601"},
		// error propagation: an operand of + / * itself fails to parse
		{"$.a + (1+)", "42601"},
		{"$.a * (1+)", "42601"},
		// error propagation: the accessor primary '(' expr itself fails to parse, and the
		// unbalanced-parens case for it
		{"(1+", "42601"},
		{"(1+2", "42601"},
		// anyLevel: a non-INT/last bound
		{"$.**{x}", "42601"},
		// .decimal(...): a non-INT argument, and a non-INT after a comma
		{"$.decimal(x)", "42601"},
		{"$.decimal(1,x)", "42601"},
		// .datetime(...) / .time_tz(...) etc with a wrong-kind argument
		{"$.datetime(1)", "42601"},
		{`$.time_tz("x")`, "42601"},
		// error propagation inside a subscript index expr and its "to" bound
		{"$[1+]", "42601"},
		{"$[0 to 1+]", "42601"},
		// comparison: an error parsing the right operand
		{"$ ? (@.a == )", "42601"},
		// accessor primary '(' expr ')': the expr parses fine but ')' is missing
		{"(1", "42601"},
		// anyLevel: a non-INT/last bound after "to"
		{"$.**{2 to x}", "42601"},
		// .decimal(...): two arguments with no comma between them
		{"$.decimal(1 2)", "42601"},
		// a keyword that is not a method name, called like one
		{"$.unknown()", "42601"},
		// '.' followed by a token that is neither * nor ** nor a name
		{"$.@", "42601"},
	}
	for _, c := range cases {
		jpCheck(t, s, c.lit, c.want)
	}
}

// --- literal.go: reg* empty/zero forms, float overflow, money, tid, snapshot --------

func TestLiteralGaps2RegAndMisc(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		// regcollation / regrole / regnamespace: empty / "-" and a numeric OID all parse
		// without any catalog lookup happening (regcollation's -is-it-known? and
		// regrole/regnamespace's role/schema existence are not this analyzer's to check).
		{"", "regcollation", ""},
		{"-", "regcollation", ""},
		{"123", "regcollation", ""},
		{"", "regrole", ""},
		{"-", "regrole", ""},
		{"123", "regrole", ""},
		{"", "regnamespace", ""},
		{"123", "regnamespace", ""},
		{"", "regproc", ""},
		{"-", "regproc", ""},
		{"0", "regoper", ""}, // regoper's "unknown" spelling is "0", not "-"
		{"123", "regoper", ""},
		// regtype: a string that is not valid type syntax at all
		{"@#$", "regtype", "42601"},
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

func TestLiteralGaps2FloatOverflow(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		// strtod overflows to +-Inf on a huge exponent: PG's own nan/inf spellings don't
		// match, and the partial-parse + Inf/0 combination is reported as out of range
		// rather than a bad-format error.
		{"1e400", "float8", "22003"},
		{"-1e400", "float8", "22003"},
		{"1e400", "float4", "22003"},
		// float4 (single): a valid float8 magnitude that overflows float4 specifically
		{"1e39", "float4", "22003"},
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

func TestLiteralGaps2MoneyTidSnapshot(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		// money: a parenthesized amount is the accounting negative spelling
		{"($5.00)", "money", ""},
		{"+$5.00", "money", ""},
		// money: enough digits to overflow int64
		{"99999999999999999999999999999999999999999999", "money", "22003"},
		// tid: malformed forms
		{"not_a_tid", "tid", "22P02"},
		{"(1", "tid", "22P02"},      // missing ')'
		{"(1 2)", "tid", "22P02"},   // missing ','
		{"(1,2,3)", "tid", "22P02"}, // extra field (Cut only splits once, so this is
		//                                actually "2,3" as the offset text -- kept as a
		//                                documented case rather than assumed)
		// pg_snapshot: malformed forms
		{"5", "pg_snapshot", "22P02"},        // missing both ':'
		{"5:10", "pg_snapshot", "22P02"},     // missing the second ':'
		{"5:10:8,7", "pg_snapshot", "22P02"}, // xip list not ascending
		{"5:10:11", "pg_snapshot", "22P02"},  // xip value out of [xmin,xmax)
		{"5:10:6,7", "pg_snapshot", ""},
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

// validMacaddr8's final count check (only 6 or 8 octet pairs are valid), pg_snapshot's
// trailing-junk-after-xip-list check, regclass's empty/"-" shortcut, and an empty B”
// bit constant.
func TestLiteralGaps2Misc2(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"08:00:2b:01:02", "macaddr8", "22P02"}, // 5 pairs: neither 6 nor 8
		{"5:10:6x", "pg_snapshot", "22P02"},     // trailing junk after an xip entry
		{"", "regclass", ""},
		{"-", "regclass", ""},
		// jsonSurrogatesOK: a lone high surrogate not paired with a following low one
		{`{"a": "\uD800X"}`, "jsonb", "22P02"},  // a plain char after it
		{`{"a": "\uD800\n"}`, "jsonb", "22P02"}, // a non-\u escape after it
		{`{"a": "\uD800"}`, "jsonb", "22P02"},   // the string closes right after it
		// xid / xid8: an in-range-for-the-wider-type value that overflows the narrower one
		{"4294967296", "xid", "22003"},                  // 2^32: too big for uint32
		{"99999999999999999999999999", "xid8", "22003"}, // too big even for uint64
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
	// XML with a declared, non-UTF8 charset: xml_in still accepts it (dec.CharsetReader
	// is consulted, and passes the bytes through unchanged rather than transcoding).
	_, err = Analyze(s, `SELECT `+quoteLit(`<?xml version="1.0" encoding="ISO-8859-1"?><a/>`)+`::xml`)
	if err != nil {
		t.Errorf("xml with a declared charset: unexpected error %v", err)
	}
	// B'' : an empty bit-string constant is accepted.
	if _, err := Analyze(s, `SELECT B''`); err != nil {
		t.Errorf("B'': unexpected error %v", err)
	}
}

// validateAssignLength's bit / varbit branches: a plain string literal beginning with a
// literal 'b'/'B' or 'x'/'X' character is itself parsed as a binary/hex bit-string body
// (mirroring validateBitLiteral / real bit_in), reached through assignment coercion.
func TestLiteralGaps2AssignBitPrefix(t *testing.T) {
	s, err := Load(`
		CREATE TABLE gaps2_bit (b bit(4), vb bit varying(8));
	`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql      string
		wantFail bool
	}{
		{"INSERT INTO gaps2_bit (b) VALUES ('B1010')", false}, // 'B' prefix, 4 bits, exact fit
		{"INSERT INTO gaps2_bit (vb) VALUES ('x1F')", false},  // 'x' prefix, hex, 8 bits, exact fit
		{"INSERT INTO gaps2_bit (vb) VALUES ('X1FF')", true},  // hex, 12 bits, too long for varbit(8)
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

// jsonSurrogatesOK: a high surrogate immediately followed by another \u escape that is
// itself an ordinary (non-surrogate) codepoint.
func TestLiteralGaps2JsonSurrogateNormal(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(s, "SELECT "+quoteLit(`{"a": "\uD800\u0041"}`)+"::jsonb")
	var e *Error
	if !errors.As(aerr, &e) || e.Code != "22P02" {
		t.Errorf(`{"a": "\uD800\u0041"}::jsonb: got %v, want 22P02`, aerr)
	}
}

// validateTsvectorLiteral: trailing whitespace only (nothing follows it) breaks the
// outer loop cleanly, and a quoted lexeme's doubled ” is an escaped literal quote.
func TestLiteralGaps2Tsvector(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, want string }{
		{"hello ", ""},     // trailing whitespace with nothing after it
		{"'it''s'", ""},    // an escaped quote inside a quoted lexeme
		{"'it''", "42601"}, // the doubled quote never closes: unterminated
	}
	for _, c := range cases {
		_, aerr := Analyze(s, "SELECT "+quoteLit(c.lit)+"::tsvector")
		var e *Error
		got := ""
		if errors.As(aerr, &e) {
			got = e.Code
		}
		if got != c.want {
			t.Errorf("%q::tsvector: got %q want %q", c.lit, got, c.want)
		}
	}
}

// validateXMLLiteral: a non-DOCTYPE directive, a DOCTYPE after content already seen, a
// second <?xml?> declaration after content, and (under xmloption = document) text before
// the root element or no root element at all.
func TestLiteralGaps2XML(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, want string }{
		{`<!ENTITY foo "bar"><a/>`, "2200N"},   // a directive that isn't DOCTYPE
		{`<a/><!DOCTYPE a>`, "2200N"},          // DOCTYPE after content
		{`<a/><?xml version="1.0"?>`, "2200N"}, // a second xml declaration after content
	}
	for _, c := range cases {
		_, aerr := Analyze(s, "SELECT "+quoteLit(c.lit)+"::xml")
		var e *Error
		if !errors.As(aerr, &e) || e.Code != c.want {
			t.Errorf("%q::xml: got %v, want %s", c.lit, aerr, c.want)
		}
	}

	sDoc, err := Load("SET xmloption = document;")
	if err != nil {
		t.Fatal(err)
	}
	_, aerr := Analyze(sDoc, "SELECT "+quoteLit("text before <a/>")+"::xml")
	var e *Error
	if !errors.As(aerr, &e) || e.Code != "2200M" {
		t.Errorf(`"text before <a/>"::xml under xmloption=document: got %v, want 2200M`, aerr)
	}
	// no root element at all: roots stays 0, tripping the final "document && roots != 1" check
	_, aerr2 := Analyze(sDoc, "SELECT "+quoteLit("")+"::xml")
	var e2 *Error
	if !errors.As(aerr2, &e2) || e2.Code != "2200M" {
		t.Errorf(`""::xml under xmloption=document: got %v, want 2200M`, aerr2)
	}
}

// --- literal_compound.go --------------------------------------------------------------

func TestLiteralGaps2CompoundMisc(t *testing.T) {
	s, err := Load(`
		CREATE TABLE gaps2_row (a int, b text);
	`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		// record literal: an escape as the very last character
		{`(1,\`, "gaps2_row", "22P02"},
		// record literal: a field whose own input function rejects it
		{"(abc,x)", "gaps2_row", "22P02"},
		// inet: an empty mask, and more than 4 IPv4 octets
		{"192.168.1.1/", "inet", "22P02"},
		{"1.2.3.4.5", "inet", "22P02"},
		// macaddr: a group value over 255 (an oversized colon-separated octet)
		{"1ff:00:00:00:00:00", "macaddr", "22003"},
		// numeric range: a binary-integer bound big.Float can't parse (valid numeric
		// input, but not something SetString understands) — the lower<upper check is
		// silently skipped rather than erroring, even though 5 < 20
		{"[0b101,20)", "numrange", ""},
		// daterange: one bound is a special value (not a plain calendar date) — again
		// the ordering check is skipped
		{"[epoch,2020-01-01)", "daterange", ""},
		// daterange: equal bounds compare equal, which is not > 0
		{"[2020-01-01,2020-01-01)", "daterange", ""},
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

func TestLiteralGaps2Array(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ lit, typ, want string }{
		{"[+]={1}", "int[]", "22P02"},   // a lower bound that is only a sign, no digits
		{"[1:+]={1}", "int[]", "22P02"}, // same for the upper bound
		{"[1:2}={1}", "int[]", "22P02"}, // ']' expected after the bounds
		{"[1:2]{1}", "int[]", "22P02"},  // '=' expected before the brace
		{"[1:2]= 1}", "int[]", "22P02"}, // '{' expected after '='
		// content nesting deeper than the dimension limit, with no explicit bounds
		{"{{{{{{{1}}}}}}}", "int[]", "54000"},
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

// numeric typmods with a negative scale decode (numeric(2,-1) rounds to tens: 123 has
// 2 significant digits after rounding to 120, 1234 would need 3), and a value float64
// cannot hold still overflows the precision.
func TestLiteralGaps2NumericScaleAndOverflow(t *testing.T) {
	base, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(base) + "\nCREATE TABLE neg_scale (n numeric(2,-1), m numeric(3,1));")
	if err != nil {
		t.Fatal(err)
	}
	notes := func(sql string) string {
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var out []string
		for _, n := range r.Notes {
			out = append(out, n.Message)
		}
		return strings.Join(out, "; ")
	}
	if got := notes("INSERT INTO neg_scale (n) VALUES ('123')"); strings.Contains(got, "overflow") {
		t.Errorf("123 fits numeric(2,-1) (rounds to 120): %q", got)
	}
	if got := notes("INSERT INTO neg_scale (n) VALUES ('1234')"); !strings.Contains(got, "overflow") {
		t.Errorf("1234 does not fit numeric(2,-1): %q", got)
	}
	if got := notes("INSERT INTO neg_scale (m) VALUES ('1e400')"); !strings.Contains(got, "overflow") {
		t.Errorf("1e400 does not fit numeric(3,1): %q", got)
	}
	if got := notes("INSERT INTO neg_scale (m) VALUES ('Infinity')"); !strings.Contains(got, "overflow") {
		t.Errorf("Infinity does not fit numeric(3,1): %q", got)
	}
}
