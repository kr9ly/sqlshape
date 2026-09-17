package analyze

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

// A literal written straight into a column -- INSERT ... VALUES ('2004-13-15'), UPDATE
// ... SET tiny = 128 -- is stored by the server's Field::store, which in strict mode
// turns every value it cannot keep into an error: the statement then fails on every
// execution, so the checker reports the failure as the statement's own error, with the
// number and message the server would use (measured on mysqld 8.4, TestLiteralStoreServer;
// found by the corpus probe, whose largest run-time classes were these). Outside strict
// mode the same values are stored adjusted with a warning, and under INSERT / UPDATE
// IGNORE too, so nothing is reported then. The rules are field.cc's and my_time.cc's:
//
//   - an integer column (Field_tiny ... Field_longlong) rejects a value outside its range
//     (1264 "Out of range value"), a string that holds no number (1366 "Incorrect integer
//     value") or one followed by other text (1265 "Data truncated"), a real rounded first
//     (rint), a decimal rounded half away from zero;
//   - a YEAR takes 0, 1-99 (as 2001-2069 / 1970-1999) and 1901-2155, anything else is 1264;
//   - a DECIMAL(M,D) rejects more than M-D integer digits and a negative value into an
//     UNSIGNED column (1264); a FLOAT a value beyond FLT_MAX (1264);
//   - a DATE / DATETIME / TIMESTAMP column parses a string with str_to_datetime under the
//     session's sql_mode (NO_ZERO_DATE, NO_ZERO_IN_DATE, ALLOW_INVALID_DATES) and a number
//     with number_to_datetime, a TIME column with str_to_time / number_to_time; whatever
//     they cut, find out of range or find zero where zero is forbidden is 1292 "Incorrect
//     date / datetime / time value" (a DATE that merely loses a time part is a note, and
//     stored);
//   - a CHAR / VARCHAR / BINARY / VARBINARY (n) column rejects a string longer than n
//     characters (bytes when binary) once trailing spaces are dropped (1406 "Data too
//     long");
//   - an ENUM rejects a string that is none of its members (1265; a short number is an
//     index, 1 to the member count), a SET a list with a member it lacks (1265);
//
// A spatial column is judged by geom.go's geometryStore (1416), whatever the sql_mode.
//
// Not read: a hex / bit literal (a number in a numeric column, a string elsewhere), the
// double types (the lexer already bounds their literals), JSON and BIT columns, character
// set conversion between the literal's and the column's, and a value inside a routine or
// trigger body (the body's own run-time failures are its raised violations, not the
// statement's error).

// storeChecks says whether literalStore judges an assignment of row into table: strict
// mode for that table (strictFor: STRICT_TRANS_TABLES alone is strict for a
// nontransactional engine only while nothing has been written, i.e. the first row -- a
// later row's bad value is adjusted with a warning, measured on MyISAM), no IGNORE, a
// top-level statement.
func (a *analyzer) storeChecks(table *schema.Table, row int) bool {
	if a.write == nil || a.write.ignore || a.routine != nil || a.trig != nil {
		return false
	}
	if a.strictFor(table) {
		return true
	}
	return a.s.Settings.SQLMode.StrictTransOnly() && row <= 1
}

// literalStore is the error the server raises when the literal v is stored into col, nil
// when v is not a literal or is stored as written. row is the VALUES row (1-based) the
// message names.
func (a *analyzer) literalStore(col *schema.Column, v mysqlast.Value, row int) *Error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	at := a.ph.Back(n.Start)
	fail := func(code int, msg string) *Error {
		return &Error{Message: fmt.Sprintf("%s for column '%s' at row %d", msg, col.Name, row), Code: code, Position: at}
	}
	num, isNum := numericLiteral(n)
	str, isStr := stringLiteral(n)
	if !isNum && !isStr {
		return nil
	}
	t := col.Type
	switch t.Name {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		lo, hi := intRange(t)
		var val *big.Rat
		if isNum {
			val = num.rat
			if num.real {
				val = roundHalfEven(val)
			} else {
				val = roundHalfAway(val)
			}
		} else {
			r, tail, ok := numericString(str)
			if !ok {
				return fail(1366, fmt.Sprintf("Incorrect integer value: '%s'", str))
			}
			val = roundHalfAway(r)
			if tail {
				if val.Cmp(lo) < 0 || val.Cmp(hi) > 0 {
					return fail(1264, "Out of range value")
				}
				return fail(1265, "Data truncated")
			}
		}
		if val.Cmp(lo) < 0 || val.Cmp(hi) > 0 {
			return fail(1264, "Out of range value")
		}
	case "year":
		var val *big.Rat
		if isNum {
			val = roundHalfAway(num.rat)
		} else {
			r, tail, ok := numericString(str)
			if !ok {
				return fail(1366, fmt.Sprintf("Incorrect integer value: '%s'", str))
			}
			val = roundHalfAway(r)
			if tail {
				return fail(1265, "Data truncated")
			}
		}
		if !val.IsInt() {
			return nil
		}
		y := val.Num()
		if y.Sign() < 0 || y.Cmp(big.NewInt(2155)) > 0 || (y.Cmp(big.NewInt(100)) >= 0 && y.Cmp(big.NewInt(1901)) < 0) {
			return fail(1264, "Out of range value")
		}
	case "decimal":
		var val *big.Rat
		if isNum {
			val = num.rat
		} else {
			r, tail, ok := numericString(str)
			if !ok {
				return fail(1366, fmt.Sprintf("Incorrect decimal value: '%s'", str))
			}
			if tail {
				return fail(1265, "Data truncated")
			}
			val = r
		}
		if t.Unsigned && val.Sign() < 0 {
			return fail(1264, "Out of range value")
		}
		m, d := t.Length, t.Dec
		if m <= 0 {
			m = 10
		}
		if d < 0 {
			d = 0
		}
		// rounded to the scale first: 999.999 into a DECIMAL(5,2) is 1000.00, an overflow
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil))
		rounded := roundHalfAway(new(big.Rat).Mul(val, scale))
		limit := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(m)), nil))
		if new(big.Rat).Abs(rounded).Cmp(limit) >= 0 {
			return fail(1264, "Out of range value")
		}
	case "float":
		if !isNum {
			return nil
		}
		f, _ := num.rat.Float64()
		if math.Abs(f) > math.MaxFloat32 {
			return fail(1264, "Out of range value")
		}
	case "date", "datetime", "timestamp":
		flags := a.dateFlags(t.Name)
		var tm mysqlTime
		var warn int
		if isNum {
			ip := new(big.Int).Quo(num.rat.Num(), num.rat.Denom())
			if !ip.IsInt64() || ip.Sign() < 0 {
				return fail(1292, fmt.Sprintf("Incorrect %s value: '%s'", temporalWord(t.Name), num.text))
			}
			if !numberToDatetime(ip.Int64(), flags, &tm, &warn) {
				return fail(1292, fmt.Sprintf("Incorrect %s value: '%s'", temporalWord(t.Name), num.text))
			}
		} else {
			if !strToDatetime(str, flags, &tm, &warn) || warn&(timeWarnTruncated|timeWarnOutOfRange|timeWarnZeroDate|timeWarnZeroInDate) != 0 {
				return fail(1292, fmt.Sprintf("Incorrect %s value: '%s'", temporalWord(t.Name), str))
			}
		}
		if t.Name == "timestamp" && tm.year != 0 && !timestampInRange(tm) {
			// the TIMESTAMP range, 1970-01-01 00:00:01 to 2038-01-19 03:14:07 UTC: exact when
			// the literal carries a time zone displacement, otherwise judged only where the
			// session's time zone (within +-14 hours) cannot move the verdict
			return fail(1292, fmt.Sprintf("Incorrect datetime value: '%s'", literalSpelling(num, isNum, str)))
		}
	case "time":
		var tm mysqlTime
		var warn int
		if isNum {
			ip := new(big.Int).Quo(num.rat.Num(), num.rat.Denom())
			if !ip.IsInt64() || !numberToTime(ip.Int64(), &tm, &warn) {
				return fail(1292, fmt.Sprintf("Incorrect time value: '%s'", num.text))
			}
		} else if !strToTime(str, &tm, &warn) || warn != 0 {
			return fail(1292, fmt.Sprintf("Incorrect time value: '%s'", str))
		}
	case "char", "varchar", "binary", "varbinary":
		if !isStr || t.Length < 0 {
			return nil
		}
		s := strings.TrimRight(str, " ")
		length := utf8.RuneCountInString(s)
		if isBinary(t) {
			length = len(s)
		}
		if length > t.Length {
			return fail(1406, "Data too long")
		}
	case "enum":
		if isStr {
			s := strings.TrimRight(str, " ")
			if enumIndex(t, s) > 0 {
				return nil
			}
			if len(s) < 6 {
				if r, tail, ok := numericString(s); ok && !tail && r.IsInt() && r.Sign() > 0 && r.Num().Cmp(big.NewInt(int64(len(t.Values)))) <= 0 {
					return nil
				}
			}
			return fail(1265, "Data truncated")
		}
		val := roundHalfAway(num.rat)
		if !val.IsInt() || val.Sign() <= 0 || val.Num().Cmp(big.NewInt(int64(len(t.Values)))) > 0 {
			return fail(1265, "Data truncated")
		}
	case "set":
		if !isStr {
			return nil
		}
		s := strings.TrimRight(str, " ")
		if s == "" {
			return nil
		}
		for _, member := range strings.Split(s, ",") {
			if enumIndex(t, strings.TrimRight(member, " ")) == 0 {
				return fail(1265, "Data truncated")
			}
		}
	}
	return nil
}

// temporalLiteral is item_create.cc's create_temporal_literal check on a DATE'...' /
// TIME'...' / TIMESTAMP'...' literal: the value must parse as exactly the named type with
// no warning, under TIME_FUZZY_DATE plus the sql_mode's NO_ZERO_IN_DATE / NO_ZERO_DATE /
// ALLOW_INVALID_DATES; otherwise the statement fails with 1525 (ER_WRONG_VALUE).
func (a *analyzer) temporalLiteral(typ, value string, start int) *Error {
	flags := a.dateFlags("date") // TIME_FUZZY_DATE and the mode's flags, whatever the type
	var tm mysqlTime
	var warn int
	ok := false
	word := "DATETIME"
	switch typ {
	case "date":
		word = "DATE"
		ok = strToDatetime(value, flags, &tm, &warn) && warn == 0 && tm.fields <= 3
	case "time":
		word = "TIME"
		ok = strToTime(value, &tm, &warn) && warn == 0
	default:
		ok = strToDatetime(value, flags, &tm, &warn) && warn == 0 && tm.fields > 3
	}
	if ok {
		return nil
	}
	return &Error{Message: fmt.Sprintf("Incorrect %s value: '%s'", word, value), Code: 1525, Position: a.ph.Back(start)}
}

// dateFlags is Field_*::date_flags for a column type under the schema's sql_mode: a DATE /
// DATETIME is fuzzy (a 0 month or day is allowed unless NO_ZERO_IN_DATE), a TIMESTAMP never
// is; NO_ZERO_DATE and ALLOW_INVALID_DATES follow the mode.
func (a *analyzer) dateFlags(typ string) timeFlags {
	mode := a.s.Settings.SQLMode.Expand()
	var f timeFlags
	if typ != "timestamp" {
		f |= timeFuzzyDate
	} else {
		f |= timeNoZeroInDate
	}
	if mode.Has(sqlmode.NoZeroDate) {
		f |= timeNoZeroDate
	}
	if mode.Has(sqlmode.NoZeroInDate) {
		f |= timeNoZeroInDate
	}
	if mode.Has(sqlmode.AllowInvalidDates) {
		f |= timeInvalidDates
	}
	return f
}

// timestampInRange says a non-zero datetime fits a TIMESTAMP column (see literalStore).
func timestampInRange(tm mysqlTime) bool {
	if tm.hasTZ {
		utc := time.Date(int(tm.year), time.Month(tm.month), int(tm.day), int(tm.hour), int(tm.minute), int(tm.second), 0, time.UTC).Add(-time.Duration(tm.tzOffset) * time.Second)
		return !utc.Before(time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC)) && !utc.After(time.Date(2038, 1, 19, 3, 14, 7, 0, time.UTC))
	}
	local := time.Date(int(tm.year), time.Month(tm.month), int(tm.day), int(tm.hour), int(tm.minute), int(tm.second), 0, time.UTC)
	return !local.Before(time.Date(1969, 12, 31, 10, 0, 1, 0, time.UTC)) && !local.After(time.Date(2038, 1, 19, 17, 14, 7, 0, time.UTC))
}

func temporalWord(typ string) string {
	switch typ {
	case "date":
		return "date"
	case "time":
		return "time"
	}
	return "datetime"
}

func literalSpelling(num numLiteral, isNum bool, str string) string {
	if isNum {
		return num.text
	}
	return str
}

// intRange is the value range of an integer column type.
func intRange(t schema.Type) (lo, hi *big.Rat) {
	bits := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "bigint": 64}[t.Name]
	if t.Unsigned {
		max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))
		return new(big.Rat), new(big.Rat).SetInt(max)
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits-1), big.NewInt(1))
	min := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), bits-1))
	return new(big.Rat).SetInt(min), new(big.Rat).SetInt(max)
}

// enumIndex is the 1-based member of an ENUM / SET the string names, compared the way the
// column's collation does (case-insensitively unless it is a _bin or _cs collation); 0 when
// none.
func enumIndex(t schema.Type, s string) int {
	exact := strings.HasSuffix(t.Collation, "_bin") || strings.HasSuffix(t.Collation, "_cs") || isBinary(t)
	for i, v := range t.Values {
		if v == s || !exact && strings.EqualFold(v, s) {
			return i + 1
		}
	}
	return 0
}

// numLiteral is a numeric literal's value: rat is exact, real says it was written as a
// float (1.5E3, which Field_num::store(double) rounds half to even rather than half away),
// text is its spelling for a message.
type numLiteral struct {
	rat  *big.Rat
	real bool
	text string
}

// numericLiteral reads Item_int / Item_uint / Item_decimal / Item_float, TRUE / FALSE, and
// any of them under a unary minus.
func numericLiteral(n *mysqlast.Node) (numLiteral, bool) {
	neg := false
	for n.Class == "Item_func_neg" {
		inner, ok := n.Arg("a").(*mysqlast.Node)
		if !ok {
			return numLiteral{}, false
		}
		neg = !neg
		n = inner
	}
	var text string
	real := false
	switch n.Class {
	case "Item_int":
		text = str(n.Arg("i"))
	case "Item_uint", "Item_decimal":
		text = str(n.Arg("str"))
	case "Item_float":
		text = str(n.Arg("str"))
		real = true
	case "Item_func_true":
		text = "1"
	case "Item_func_false":
		text = "0"
	default:
		return numLiteral{}, false
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return numLiteral{}, false
	}
	if neg {
		r.Neg(r)
		text = "-" + text
	}
	return numLiteral{rat: r, real: real, text: text}, true
}

// stringLiteral reads a quoted string literal's value.
func stringLiteral(n *mysqlast.Node) (string, bool) {
	switch n.Class {
	case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
		if tok, ok := n.Arg("literal").(mysqlast.Token); ok {
			return tok.Value, true
		}
	}
	return "", false
}

// numericString reads a number out of a string the way my_strntoull10rnd / str2my_decimal
// do: leading spaces and tabs, an optional sign, digits with an optional fraction and
// exponent. ok is false when no digit was read (EDOM); tail says other text followed the
// number (truncation).
func numericString(s string) (r *big.Rat, tail bool, ok bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return nil, false, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '-' || s[j] == '+') {
			j++
		}
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j {
			i = k
		}
	}
	r, ok = new(big.Rat).SetString(s[start:i])
	if !ok {
		return nil, false, false
	}
	return r, strings.TrimRight(s[i:], " ") != "", true
}

// roundHalfAway rounds to the nearest integer, halves away from zero (my_decimal2int).
func roundHalfAway(r *big.Rat) *big.Rat {
	if r.IsInt() {
		return r
	}
	half := big.NewRat(1, 2)
	x := new(big.Rat).Abs(r)
	x.Add(x, half)
	q := new(big.Int).Quo(x.Num(), x.Denom())
	out := new(big.Rat).SetInt(q)
	if r.Sign() < 0 {
		out.Neg(out)
	}
	return out
}

// roundHalfEven rounds to the nearest integer, halves to even (rint).
func roundHalfEven(r *big.Rat) *big.Rat {
	if r.IsInt() {
		return r
	}
	f, _ := r.Float64()
	return new(big.Rat).SetFloat64(math.RoundToEven(f))
}

// ---- my_time.cc: the temporal parsers, ported for literal validation ----

type timeFlags uint

const (
	timeFuzzyDate timeFlags = 1 << iota
	timeDatetimeOnly
	timeNoZeroInDate
	timeNoZeroDate
	timeInvalidDates
)

const (
	timeWarnTruncated = 1 << iota
	timeWarnOutOfRange
	timeWarnZeroDate
	timeWarnZeroInDate
)

type mysqlTime struct {
	year, month, day, hour, minute, second, secondPart uint
	neg                                                bool
	isTime                                             bool
	fields                                             int
	// hasTZ / tzOffset: a {+-}HH:MM displacement written after the value, in seconds
	hasTZ    bool
	tzOffset int
}

var daysInMonth = [12]uint{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isLeap(y uint) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0 || y == 0) }

// checkDate is my_time.cc's check_date: a zero month or day, an impossible day of month, a
// zero date, each as the flags allow.
func checkDate(t mysqlTime, notZero bool, flags timeFlags, warn *int) bool {
	if notZero {
		if (flags&timeNoZeroInDate != 0 || flags&timeFuzzyDate == 0) && (t.month == 0 || t.day == 0) {
			*warn = timeWarnZeroInDate
			return true
		}
		if flags&timeInvalidDates == 0 && t.month != 0 && t.day > daysInMonth[t.month-1] && !(t.month == 2 && isLeap(t.year) && t.day == 29) {
			*warn = timeWarnOutOfRange
			return true
		}
	} else if flags&timeNoZeroDate != 0 {
		*warn = timeWarnZeroDate
		return true
	}
	return false
}

func checkDatetimeRange(t mysqlTime) bool {
	maxHour := uint(23)
	if t.isTime {
		maxHour = 838
	}
	return t.year > 9999 || t.month > 12 || t.day > 31 || t.minute > 59 || t.second > 59 || t.secondPart > 999999 || t.hour > maxHour
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
func isPunct(c byte) bool {
	return c > ' ' && c < 0x7f && !isDigit(c) && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z')
}

// tzDisplacement parses {+-}HH:MM to the end of s (trailing spaces allowed), in seconds.
func tzDisplacement(s string) (int, bool) {
	s = strings.TrimRight(s, " \t\n\r\f\v")
	if len(s) != 6 || (s[0] != '+' && s[0] != '-') || !isDigit(s[1]) || !isDigit(s[2]) || s[3] != ':' || !isDigit(s[4]) || !isDigit(s[5]) {
		return 0, false
	}
	h := int(s[1]-'0')*10 + int(s[2]-'0')
	m := int(s[4]-'0')*10 + int(s[5]-'0')
	if m > 59 || h > 14 || (h == 14 && m > 0) || (s[0] == '-' && h == 0 && m == 0) {
		return 0, false
	}
	off := h*3600 + m*60
	if s[0] == '-' {
		off = -off
	}
	return off, true
}

// strToDatetime is my_time.cc's str_to_datetime: parseDatetime's verdict as a bool (false on
// tdNone and tdError alike; warn says which); on success warn may still carry
// timeWarnTruncated for trailing text.
func strToDatetime(s string, flags timeFlags, t *mysqlTime, warn *int) bool {
	return parseDatetime(s, flags, t, warn) == tdOK
}

// parseDatetime's verdicts: read; not a datetime at all (MYSQL_TIMESTAMP_NONE: no leading
// digit, a rejected delimiter, a bad displacement -- str_to_time then tries its own
// shapes); a datetime shape the value does not fit (MYSQL_TIMESTAMP_ERROR: a field out of
// range, a zero the flags forbid).
const (
	tdOK = iota
	tdNone
	tdError
)

func parseDatetime(s string, flags timeFlags, t *mysqlTime, warn *int) int {
	const maxParts = 8
	var date, dateLen [maxParts]uint
	end := len(s)
	i := 0
	for i < end && isSpace(s[i]) {
		i++
	}
	if i >= end || !isDigit(s[i]) {
		*warn = timeWarnTruncated
		return tdNone
	}
	pos := i
	for pos < end && (isDigit(s[pos]) || s[pos] == 'T') {
		pos++
	}
	digits := pos - i
	internal := false
	yearLength := 0
	fieldLength := 4
	if pos == end || s[pos] == '.' {
		if digits == 4 || digits == 8 || digits >= 14 {
			yearLength = 4
		} else {
			yearLength = 2
		}
		fieldLength = yearLength
		internal = true
	}
	const allowSpace = (1 << 2) | (1 << 6)
	const allowHyphen = (1 << 0) | (1 << 1)
	const allowColon = (1 << 3) | (1 << 4)
	notZero := uint(0)
	foundDelimiter, foundSpace, foundDisplacement := false, false, false
	displacement := 0
	lastFieldPos := i
	part := 0
	for part = 0; part < maxParts-1 && i < end && isDigit(s[i]); part++ {
		start := i
		tmp := uint(s[i] - '0')
		i++
		scanUntilDelim := !internal && part != 6
		remaining := fieldLength - 1
		for i < end && isDigit(s[i]) && (scanUntilDelim || remaining > 0) {
			tmp = tmp*10 + uint(s[i]-'0')
			i++
			remaining--
			if tmp > 999999 {
				*warn = timeWarnTruncated
				return tdNone
			}
		}
		dateLen[part] = uint(i - start)
		date[part] = tmp
		notZero |= tmp
		fieldLength = 2
		lastFieldPos = i
		if i == end {
			part++
			break
		}
		if part == 2 && s[i] == 'T' {
			i++
			continue
		}
		if part == 5 {
			if s[i] == '.' {
				i++
				lastFieldPos = i
				fieldLength = 6
			} else if isDigit(s[i]) {
				part++
				break
			} else if s[i] == '+' || s[i] == '-' {
				off, ok := tzDisplacement(s[i:])
				if !ok {
					*warn = timeWarnTruncated
					return tdNone
				}
				foundDisplacement, displacement = true, off
				i = end
				lastFieldPos = i
			}
			continue
		}
		if part == 6 && (s[i] == '+' || s[i] == '-') {
			off, ok := tzDisplacement(s[i:])
			if !ok {
				*warn = timeWarnTruncated
				return tdNone
			}
			foundDisplacement, displacement = true, off
			i = end
			lastFieldPos = i
		}
		for i < end && (isPunct(s[i]) || isSpace(s[i])) {
			if isSpace(s[i]) {
				if allowSpace&(1<<part) == 0 {
					*warn = timeWarnTruncated
					return tdNone
				}
				foundSpace = true
			} else if !((s[i] == '-' && allowHyphen&(1<<part) != 0) || (s[i] == ':' && allowColon&(1<<part) != 0)) && part != 2 {
				// a delimiter of the wrong kind: deprecated, still read
			}
			i++
			foundDelimiter = true
		}
		if part == 6 {
			part++
		}
		lastFieldPos = i
	}
	if foundDelimiter && !foundSpace && flags&timeDatetimeOnly != 0 {
		*warn = timeWarnTruncated
		return tdNone
	}
	i = lastFieldPos
	numberOfFields := part
	if !internal {
		yearLength = int(dateLen[0])
		if yearLength == 0 {
			*warn = timeWarnTruncated
			return tdNone
		}
	}
	*t = mysqlTime{year: date[0], month: date[1], day: date[2], hour: date[3], minute: date[4], second: date[5]}
	frac := date[6]
	for k := dateLen[6]; k < 6; k++ {
		frac *= 10
	}
	t.secondPart = frac
	fracDigits := int(dateLen[6])
	if yearLength == 2 && notZero != 0 {
		if t.year < 70 {
			t.year += 2000
		} else {
			t.year += 1900
		}
	}
	t.fields = numberOfFields
	t.hasTZ, t.tzOffset = foundDisplacement, displacement
	if numberOfFields < 3 || checkDatetimeRange(*t) {
		if notZero == 0 {
			for k := i; k < end; k++ {
				if !isSpace(s[k]) {
					notZero = 1
					break
				}
			}
		}
		if notZero != 0 {
			*warn |= timeWarnTruncated
		} else {
			*warn |= timeWarnZeroDate
		}
		return tdError
	}
	if checkDate(*t, notZero != 0, flags, warn) {
		return tdError
	}
	if fracDigits == 6 && i < end && isDigit(s[i]) {
		for i < end && isDigit(s[i]) {
			i++
		}
	}
	if i < end && (s[i] == '+' || s[i] == '-') {
		off, ok := tzDisplacement(s[i:])
		if !ok {
			*warn = timeWarnTruncated
			return tdNone
		}
		t.hasTZ, t.tzOffset = true, off
		return tdOK
	}
	for ; i < end; i++ {
		if !isSpace(s[i]) {
			*warn |= timeWarnTruncated
			break
		}
	}
	return tdOK
}

// numberToDatetime is my_time.cc's number_to_datetime: YYMMDD, YYYYMMDD, YYMMDDHHMMSS and
// YYYYMMDDHHMMSS numbers.
func numberToDatetime(nr int64, flags timeFlags, t *mysqlTime, warn *int) bool {
	*t = mysqlTime{}
	switch {
	case nr == 0 || nr >= 10000101000000:
		if nr > 99999999999999 {
			*warn = timeWarnOutOfRange
			return false
		}
	case nr < 101:
		*warn = timeWarnTruncated
		return false
	case nr <= (70-1)*10000+1231:
		nr = (nr + 20000000) * 1000000
	case nr < 70*10000+101:
		*warn = timeWarnTruncated
		return false
	case nr <= 991231:
		nr = (nr + 19000000) * 1000000
	case nr < 10000101 && flags&timeFuzzyDate == 0:
		*warn = timeWarnTruncated
		return false
	case nr <= 99991231:
		nr *= 1000000
	case nr < 101000000:
		*warn = timeWarnTruncated
		return false
	case nr <= (70-1)*10000000000+1231235959:
		nr += 20000000000000
	case nr < 70*10000000000+101000000:
		*warn = timeWarnTruncated
		return false
	case nr <= 991231235959:
		nr += 19000000000000
	}
	part1 := nr / 1000000
	part2 := nr - part1*1000000
	t.year = uint(part1 / 10000)
	part1 %= 10000
	t.month = uint(part1 / 100)
	t.day = uint(part1 % 100)
	t.hour = uint(part2 / 10000)
	part2 %= 10000
	t.minute = uint(part2 / 100)
	t.second = uint(part2 % 100)
	if !checkDatetimeRange(*t) && !checkDate(*t, nr != 0, flags, warn) {
		return true
	}
	if *warn == 0 {
		*warn = timeWarnTruncated
	}
	return false
}

// strToTime is my_time.cc's str_to_time: a full datetime (its time part), [-] D HH:MM:SS,
// HH:MM:SS, MM:SS, HHMMSS and their shorter forms, with a fraction. false on error; on
// success warn carries the range and truncation warnings.
func strToTime(s string, t *mysqlTime, warn *int) bool {
	end := len(s)
	i := 0
	for i < end && isSpace(s[i]) {
		i++
	}
	*t = mysqlTime{isTime: true}
	if i < end && s[i] == '-' {
		t.neg = true
		i++
	}
	if i == end {
		return false
	}
	start := i
	if end-i >= 12 {
		var dt mysqlTime
		var w int
		switch parseDatetime(s[i:], timeFuzzyDate|timeDatetimeOnly, &dt, &w) {
		case tdOK:
			*t = mysqlTime{hour: dt.hour, minute: dt.minute, second: dt.second, secondPart: dt.secondPart, isTime: true}
			*warn = w &^ timeWarnTruncated
			return true
		case tdError:
			// a datetime shape the value does not fit is this function's error too; tdNone
			// (not a datetime at all) falls through to the time shapes
			*warn = w
			return false
		}
	}
	var date [5]uint64
	var value uint64
	for i < end && isDigit(s[i]) {
		value = value*10 + uint64(s[i]-'0')
		if value > math.MaxUint32 {
			return false
		}
		i++
	}
	endOfDays := i
	for i < end && isSpace(s[i]) {
		i++
	}
	state := 0
	foundDays, foundHours := false, false
	switch {
	case end-i > 1 && i != endOfDays && isDigit(s[i]):
		date[0] = value
		state = 1
		foundDays = true
	case end-i > 1 && s[i] == ':' && isDigit(s[i+1]):
		date[1] = value
		state = 2
		foundHours = true
		i++
	default:
		date[1] = value / 10000
		date[2] = value / 100 % 100
		date[3] = value % 100
		state = 4
	}
	if state != 4 {
		for {
			value = 0
			for i < end && isDigit(s[i]) {
				value = value*10 + uint64(s[i]-'0')
				i++
			}
			date[state] = value
			state++
			if state == 4 || end-i < 2 || s[i] != ':' || !isDigit(s[i+1]) {
				break
			}
			i++
		}
		if state != 4 {
			if !foundHours && !foundDays {
				// the fields read are the last ones: [M]M:SS, [S]S
				shift := 4 - state
				for k := 3; k >= 0; k-- {
					if k-shift >= 0 {
						date[k] = date[k-shift]
					} else {
						date[k] = 0
					}
				}
			}
		}
	}
	// the fraction
	if end-i >= 2 && s[i] == '.' && isDigit(s[i+1]) {
		i++
		fieldLength := 5
		value = uint64(s[i] - '0')
		i++
		for i < end && isDigit(s[i]) {
			if fieldLength > 0 {
				value = value*10 + uint64(s[i]-'0')
			}
			fieldLength--
			i++
		}
		for k := fieldLength; k > 0; k-- {
			value *= 10
		}
		date[4] = value
	} else if end-i == 1 && s[i] == '.' {
		i++
	}
	if end-i > 1 && (s[i] == 'e' || s[i] == 'E') && (isDigit(s[i+1]) || ((s[i+1] == '-' || s[i+1] == '+') && end-i > 2 && isDigit(s[i+2]))) {
		return false
	}
	for _, d := range date {
		if d > math.MaxUint32 {
			return false
		}
	}
	t.hour = uint(date[1] + date[0]*24)
	t.minute = uint(date[2])
	t.second = uint(date[3])
	t.secondPart = uint(date[4])
	if t.minute >= 60 || t.second >= 60 || t.secondPart > 999999 {
		*warn |= timeWarnOutOfRange
		return false
	}
	if t.hour > 838 || (t.hour == 838 && t.minute == 59 && t.second == 59 && t.secondPart != 0) {
		*warn |= timeWarnOutOfRange
	}
	for ; i < end; i++ {
		if !isSpace(s[i]) {
			*warn |= timeWarnTruncated
			if i == start {
				return false
			}
			break
		}
	}
	return true
}

// numberToTime is my_time.cc's number_to_time: HHMMSS, or a full datetime number.
func numberToTime(nr int64, t *mysqlTime, warn *int) bool {
	const timeMax = 8385959
	if nr > timeMax {
		if nr >= 10000000000 {
			var w int
			if numberToDatetime(nr, 0, t, &w) {
				t.isTime = true
				return true
			}
		}
		*warn |= timeWarnOutOfRange
		return false
	}
	if nr < -timeMax {
		*warn |= timeWarnOutOfRange
		return false
	}
	*t = mysqlTime{isTime: true}
	if nr < 0 {
		t.neg = true
		nr = -nr
	}
	if nr%100 >= 60 || nr/100%100 >= 60 {
		*warn |= timeWarnOutOfRange
		return false
	}
	t.hour = uint(nr / 10000)
	t.minute = uint(nr / 100 % 100)
	t.second = uint(nr % 100)
	return true
}
