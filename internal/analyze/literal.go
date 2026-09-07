package analyze

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

var uuidRe = regexp.MustCompile(`^\{?[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}\}?$`)

// validateLiteral mirrors the input-function errors PG raises at parse time when an
// untyped string literal is coerced to a type (SQLSTATE 22P02). Only types whose
// syntax is simple and stable are checked; others are accepted.
func (a *analyzer) validateLiteral(s string, to catalog.OID, loc int32) *Error {
	return a.validateLiteralTypmod(s, to, -1, loc)
}

// validateLiteralTypmod is validateLiteral with the target's typmod, which changes how an
// interval literal is read (INTERVAL '1' YEAR).
func (a *analyzer) validateLiteralTypmod(s string, to catalog.OID, typmod int32, loc int32) *Error {
	base := a.baseType(to)
	bad := func(what string) *Error {
		return errAt("22P02", loc, "invalid input syntax for type %s: %q", what, s)
	}
	v := strings.TrimSpace(s)
	switch base {
	case catalog.Int2, catalog.Int4, catalog.Int8:
		bits := map[catalog.OID]int{catalog.Int2: 16, catalog.Int4: 32, catalog.Int8: 64}[base]
		if _, err := parsePGInt(v, bits); err != nil {
			name := map[catalog.OID]string{catalog.Int2: "smallint", catalog.Int4: "integer", catalog.Int8: "bigint"}[base]
			if _, e2 := parsePGInt(v, 64); e2 == nil || errors.Is(e2, strconv.ErrRange) {
				return errAt("22003", loc, "value %q is out of range for type %s", s, name)
			}
			return bad(name)
		}
	case catalog.Numeric:
		if strings.Contains(v, "__") || strings.HasPrefix(v, "_") || strings.HasSuffix(v, "_") {
			return bad("numeric") // a digit separator sits between two digits, alone
		}
		if _, err := parsePGInt(v, 64); err == nil || isNonDecimalInt(v) {
			return nil // non-decimal integer literals and digit separators are numeric input too
		}
		v = stripDigitSeparators(v)
		if !isNumberish(v) || strings.Count(v, ".") > 1 {
			switch strings.ToLower(strings.TrimLeft(v, "+-")) {
			case "inf", "infinity": // [+-]inf, [+-]Infinity
			case "nan":
				if v[0] == '+' || v[0] == '-' {
					return bad("numeric") // NaN takes no sign
				}
			default:
				return bad("numeric")
			}
		}
	case catalog.Float4, catalog.Float8:
		return validateFloatLiteral(s, base == catalog.Float4, loc)
	case catalog.JSON, catalog.JSONB:
		if !json.Valid([]byte(s)) || (base == catalog.JSONB && !jsonSurrogatesOK(s)) {
			return bad(map[catalog.OID]string{catalog.JSON: "json", catalog.JSONB: "jsonb"}[base])
		}
		if base == catalog.JSONB && jsonHasNulEscape(s) {
			return errAt("22P05", loc, "unsupported Unicode escape sequence")
		}
	case catalog.JSONPath:
		return validateJsonpathLiteral(s, loc)
	case catalog.XML:
		return validateXMLLiteral(s, a.s.XMLOptionDocument(), loc)
	case catalog.RegType:
		return a.validateRegtypeLiteral(s, loc)
	case catalog.RegClass:
		return a.validateRegclassLiteral(s, loc)
	case catalog.RegProc, catalog.RegProcedure:
		return a.validateRegprocLiteral(s, base == catalog.RegProcedure, loc)
	case catalog.RegOper, catalog.RegOperator:
		return a.validateRegoperLiteral(s, base == catalog.RegOperator, loc)
	case catalog.RegCollation:
		// regcollationin: only the schema qualification is ours to check
		if v == "" || v == "-" {
			return nil
		}
		if _, err := parsePGInt(v, 64); err == nil {
			return nil
		}
		if parts := qualifiedNameParts(v); len(parts) > 1 && !a.s.HasSchema(parts[len(parts)-2]) {
			return errAt("42704", loc, "collation %q for encoding \"UTF8\" does not exist", strings.Join(parts, "."))
		}
	case catalog.RegRole, catalog.RegNamespace:
		// regrolein / regnamespacein: a single identifier; roles and schemas are not ours
		// to know, so only the shape is checked
		if v == "" || v == "-" {
			return nil
		}
		if _, err := parsePGInt(v, 64); err == nil {
			return nil
		}
		if parts := qualifiedNameParts(v); len(parts) > 1 {
			return errAt("42602", loc, "invalid name syntax")
		} else if base == catalog.RegNamespace && !a.s.HasSchema(parts[0]) {
			return errAt("3F000", loc, "schema %q does not exist", parts[0])
		}
	case catalog.Money:
		return validateMoneyLiteral(s, loc)
	case catalog.MacAddr8:
		if !validMacaddr8(s) {
			return bad("macaddr8")
		}
	case catalog.Tid:
		if !validTid(s) {
			return bad("tid")
		}
	case catalog.Xid, catalog.Xid8, catalog.OIDType, catalog.Cid:
		name := map[catalog.OID]string{catalog.Xid: "xid", catalog.Xid8: "xid8", catalog.OIDType: "oid", catalog.Cid: "cid"}[base]
		bits := 32
		if base == catalog.Xid8 {
			bits = 64
		}
		if code := uintInSubr(s, bits); code != "" {
			if code == "22003" {
				return errAt("22003", loc, "value %q is out of range for type %s", s, name)
			}
			return bad(name)
		}
	case catalog.PgSnapshot, catalog.TxidSnapshot:
		if !validSnapshot(s) {
			return bad(map[catalog.OID]string{catalog.PgSnapshot: "pg_snapshot", catalog.TxidSnapshot: "txid_snapshot"}[base])
		}
	case catalog.TSVector:
		return validateTsvectorLiteral(v, loc)
	case catalog.Bool:
		switch strings.ToLower(v) {
		case "t", "true", "f", "false", "y", "yes", "n", "no", "on", "off", "1", "0", "tr", "tru", "fa", "fal", "fals", "ye", "of":
		default:
			return bad("boolean")
		}
	case catalog.UUID:
		if !uuidRe.MatchString(s) { // uuid_in does not trim
			return bad("uuid")
		}
	case catalog.Timestamp, catalog.TimestampTZ:
		return validateTimestampLiteral(s, base == catalog.TimestampTZ, a.dtSession(), loc)
	case catalog.Date:
		return validateDateLiteral(s, a.dtSession(), loc)
	case catalog.Time, catalog.TimeTZ:
		return validateTimeLiteral(s, base == catalog.TimeTZ, a.dtSession(), loc)
	case catalog.Interval:
		return validateIntervalLiteral(s, typmod, a.dtSession(), loc)
	default:
		if e, ok := a.validateCompoundLiteral(s, base, typmod, loc); ok {
			return e
		}
		if labels, ok := a.s.Types.Enums[base]; ok {
			for _, l := range labels {
				if l == s {
					return nil
				}
			}
			return errAt("22P02", loc, "invalid input value for enum %s: %q", a.typ(base).Name, s)
		}
	}
	return nil
}

func isNumberish(v string) bool {
	if v == "" {
		return false
	}
	digits := 0
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.', r == 'e', r == 'E':
		case (r == '-' || r == '+') && (i == 0 || v[i-1] == 'e' || v[i-1] == 'E'):
		default:
			return false
		}
	}
	return digits > 0
}

// parsePGInt parses an integer the way pg_strtoint does: optional sign, decimal or
// 0x / 0o / 0b prefixed digits, with single underscores allowed between digits.
func parsePGInt(v string, bits int) (int64, error) {
	neg := false
	if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "+") {
		neg = v[0] == '-'
		v = v[1:]
	}
	base := 10
	if len(v) > 2 && v[0] == '0' {
		switch v[1] {
		case 'x', 'X':
			base, v = 16, v[2:]
		case 'o', 'O':
			base, v = 8, v[2:]
		case 'b', 'B':
			base, v = 2, v[2:]
		}
	}
	if base != 10 {
		v = strings.TrimPrefix(v, "_") // one separator may follow the prefix: 0x_ff
	}
	if v == "" || strings.HasPrefix(v, "_") || strings.HasSuffix(v, "_") || strings.Contains(v, "__") {
		return 0, strconv.ErrSyntax
	}
	v = strings.ReplaceAll(v, "_", "")
	if neg {
		v = "-" + v
	}
	return strconv.ParseInt(v, base, bits)
}

// validateFloatLiteral is float4in / float8in: strtod with PG's spellings of NaN and
// Infinity, out-of-range (including underflow to zero) is 22003, trailing junk 22P02.
func validateFloatLiteral(s string, single bool, loc int32) *Error {
	name := "double precision"
	if single {
		name = "real"
	}
	bad := func() *Error { return errAt("22P02", loc, "invalid input syntax for type %s: %q", name, s) }
	num := strings.TrimLeft(s, " \t\n\v\f\r")
	if num == "" {
		return bad()
	}
	f, rest, ok := strtod(num)
	if !ok || len(rest) == len(num) {
		// strtod failed or overflowed: PG then tries its own spellings
		low := strings.ToLower(num)
		matched := false
		for _, w := range []string{"nan", "infinity", "+infinity", "-infinity", "inf", "+inf", "-inf"} {
			if strings.HasPrefix(low, w) {
				rest, matched = num[len(w):], true
				break
			}
		}
		if !matched {
			if len(rest) != len(num) && (math.IsInf(f, 0) || f == 0) {
				return errAt("22003", loc, "%q is out of range for type %s", strings.TrimSpace(num[:len(num)-len(rest)]), name)
			}
			return bad()
		}
	} else if single && !math.IsInf(f, 0) && !math.IsNaN(f) {
		// strtof: beyond float4 range, or a nonzero value that rounds to zero
		if f32 := float64(float32(f)); math.IsInf(f32, 0) || (f != 0 && f32 == 0) {
			return errAt("22003", loc, "%q is out of range for type real", strings.TrimSpace(num[:len(num)-len(rest)]))
		}
	}
	if strings.TrimRight(rest, " \t\n\v\f\r") != "" {
		return bad()
	}
	return nil
}

// jsonHasNulEscape reports a \u0000 inside a JSON string, which jsonb rejects.
func jsonHasNulEscape(s string) bool {
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !inStr:
			inStr = c == '"'
		case c == '\\':
			if i+5 < len(s) && s[i+1] == 'u' && s[i+2:i+6] == "0000" {
				return true
			}
			i++
		case c == '"':
			inStr = false
		}
	}
	return false
}

// validateMoneyLiteral is cash_in under the C locale.
func validateMoneyLiteral(str string, loc int32) *Error {
	outOfRange := func() *Error { return errAt("22003", loc, "value %q is out of range for type money", str) }
	s := strings.TrimLeft(str, " \t\n\v\f\r")
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	switch {
	case strings.HasPrefix(s, "-"):
		s = s[1:]
	case strings.HasPrefix(s, "("):
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	var value int64
	dec, seenDot := 0, false
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case cIsDigit(c) && (!seenDot || dec < 2):
			var over bool
			if value, over = mul64(value, 10); over {
				return outOfRange()
			}
			if value, over = add64(value, -int64(c-'0')); over {
				return outOfRange()
			}
			if seenDot {
				dec++
			}
		case c == '.' && !seenDot:
			seenDot = true
		case c == ',':
		default:
			goto done
		}
	}
done:
	if i < len(s) && cIsDigit(s[i]) && s[i] >= '5' {
		var over bool
		if value, over = add64(value, -1); over {
			return outOfRange()
		}
	}
	for ; dec < 2; dec++ {
		var over bool
		if value, over = mul64(value, 10); over {
			return outOfRange()
		}
	}
	for i < len(s) && cIsDigit(s[i]) {
		i++
	}
	for i < len(s) {
		switch c := s[i]; {
		case cIsSpace(c) || c == ')' || c == '-' || c == '+' || c == '$':
			i++
		default:
			return errAt("22P02", loc, "invalid input syntax for type money: %q", str)
		}
	}
	// the sign is applied last; only -value can overflow
	neg := strings.Contains(str, "-") || strings.Contains(str, "(")
	if !neg && value == math.MinInt64 {
		return outOfRange()
	}
	return nil
}

// validMacaddr8 is macaddr8_in: 6 or 8 hex pairs with one consistent separator.
func validMacaddr8(s string) bool {
	i := 0
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	count := 0
	var spacer byte
	hex := func(c byte) bool { return cIsDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
	for i < len(s) && i+1 < len(s) {
		count++
		if count > 8 || !hex(s[i]) || !hex(s[i+1]) {
			return false
		}
		i += 2
		if i < len(s) && (s[i] == ':' || s[i] == '-' || s[i] == '.') {
			if spacer == 0 {
				spacer = s[i]
			} else if spacer != s[i] {
				return false
			}
			i++
		}
		if (count == 6 || count == 8) && i < len(s) && cIsSpace(s[i]) {
			for i < len(s) && cIsSpace(s[i]) {
				i++
			}
			if i < len(s) {
				return false
			}
		}
	}
	if count != 6 && count != 8 {
		return false
	}
	return i >= len(s)
}

// validTid is tidin: "(block,offset)" with block a uint32 (or int32) and offset a uint16.
func validTid(s string) bool {
	if !strings.HasPrefix(s, "(") {
		return false
	}
	body, _, ok := strings.Cut(s[1:], ")")
	if !ok {
		return false
	}
	a, b, ok := strings.Cut(body, ",")
	if !ok {
		return false
	}
	blk, rest, erange := strtoNum(a, 64)
	if erange || rest != "" || len(a) == 0 || blk < math.MinInt32 || blk > math.MaxUint32 {
		return false
	}
	off, rest, erange := strtoNum(b, 64)
	return !erange && rest == "" && len(b) > 0 && off >= 0 && off <= math.MaxUint16
}

// uintInSubr is uint32in_subr / uint64in_subr (strtoul base 0, trailing whitespace ok):
// "" for a good value, else "22P02" or "22003".
func uintInSubr(s string, bits int) string {
	i := 0
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	base := 10
	if i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && i+2 < len(s) && isHexDigit(s[i+2]) {
		base, i = 16, i+2
	} else if i < len(s) && s[i] == '0' {
		base = 8
	}
	start := i
	var v uint64
	over := false
	for i < len(s) {
		d, ok := digitVal(s[i], base)
		if !ok {
			break
		}
		if v > (math.MaxUint64-uint64(d))/uint64(base) {
			over = true
		} else {
			v = v*uint64(base) + uint64(d)
		}
		i++
	}
	if i == start {
		return "22P02"
	}
	if bits == 32 {
		// strtoul on a 64-bit long: the value must fit uint32 after sign wrap
		if over || (!neg && v > math.MaxUint32) || (neg && v > 1<<31 && v != 0 && (math.MaxUint64-v+1) > math.MaxUint32) {
			return "22003"
		}
	} else if over {
		return "22003"
	}
	if strings.TrimRight(s[i:], " \t\n\v\f\r") != "" {
		return "22P02"
	}
	return ""
}

func isHexDigit(c byte) bool { return cIsDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

func digitVal(c byte, base int) (int, bool) {
	var d int
	switch {
	case cIsDigit(c):
		d = int(c - '0')
	case c >= 'a' && c <= 'z':
		d = int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		d = int(c-'A') + 10
	default:
		return 0, false
	}
	return d, d < base
}

// validSnapshot is parse_snapshot: "xmin:xmax:[xip,...]" with 0 < xmin <= xmax and the
// xip list ascending within [xmin, xmax).
func validSnapshot(s string) bool {
	u64 := func(str string) (uint64, string, bool) {
		v, rest, erange := strtoNum(str, 64)
		if erange || len(rest) == len(str) || v < 0 {
			return 0, str, false
		}
		return uint64(v), rest, true
	}
	xmin, rest, ok := u64(s)
	if !ok || !strings.HasPrefix(rest, ":") {
		return false
	}
	xmax, rest, ok := u64(rest[1:])
	if !ok || !strings.HasPrefix(rest, ":") {
		return false
	}
	rest = rest[1:]
	if xmin == 0 || xmax == 0 || xmax < xmin {
		return false
	}
	var last uint64
	for rest != "" {
		v, r, ok := u64(rest)
		if !ok || v < xmin || v >= xmax || v < last {
			return false
		}
		last, rest = v, r
		if strings.HasPrefix(rest, ",") {
			rest = rest[1:]
		} else if rest != "" {
			return false
		}
	}
	return true
}

// stripDigitSeparators removes the single underscores numeric_in allows between digits;
// an underscore anywhere else is left in place so the syntax check rejects it.
func stripDigitSeparators(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '_' && i > 0 && i+1 < len(v) && cIsDigit(v[i-1]) && cIsDigit(v[i+1]) {
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// validateRegtypeLiteral is regtypein: a numeric OID, or a type name the parser accepts
// (42601 otherwise) that resolves (42704 otherwise).
func (a *analyzer) validateRegtypeLiteral(s string, loc int32) *Error {
	v := strings.TrimSpace(s)
	if v == "" || v == "-" {
		return nil
	}
	if _, err := parsePGInt(v, 64); err == nil {
		return nil
	}
	res, err := pg_query.Parse("SELECT NULL::" + v)
	if err != nil || len(res.Stmts) != 1 {
		return errAt("42601", loc, "syntax error at or near %q", v)
	}
	tc := res.Stmts[0].Stmt.GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal().GetTypeCast()
	if tc == nil || tc.TypeName == nil {
		return errAt("42601", loc, "syntax error at or near %q", v)
	}
	if names := tc.TypeName.Names; len(names) >= 2 {
		if sch := names[len(names)-2].GetString_().GetSval(); !a.s.HasSchema(sch) {
			return errAt("3F000", loc, "schema %q does not exist", sch)
		}
	}
	if _, rerr := a.s.ResolveType(tc.TypeName); rerr != nil {
		return errAt("42704", loc, "type %q does not exist", v)
	}
	return nil
}

// qualifiedNameParts is textToQualifiedNameList: a dotted name split into identifiers,
// quoted ones kept as written, unquoted ones folded to lower case.
func qualifiedNameParts(v string) []string {
	parts := strings.Split(v, ".")
	for i := range parts {
		p := strings.TrimSpace(parts[i])
		if strings.HasPrefix(p, `"`) {
			parts[i] = strings.ReplaceAll(strings.Trim(p, `"`), `""`, `"`)
		} else {
			parts[i] = strings.ToLower(p)
		}
	}
	return parts
}

// validateRegprocLiteral is regprocin / regprocedurein on the function's name only: a
// numeric OID, or a (qualified) function name that exists somewhere.
func (a *analyzer) validateRegprocLiteral(s string, withArgs bool, loc int32) *Error {
	v := strings.TrimSpace(s)
	if v == "" || v == "-" {
		return nil
	}
	if _, err := parsePGInt(v, 64); err == nil {
		return nil
	}
	name := v
	if withArgs {
		name, _, _ = strings.Cut(v, "(")
		name = strings.TrimSpace(name)
	}
	parts := qualifiedNameParts(name)
	schemaName, fname := "", parts[len(parts)-1]
	if len(parts) > 1 {
		schemaName = parts[len(parts)-2]
	}
	for _, fn := range a.s.Functions {
		if fn.Name == fname && (schemaName == "" || fn.Schema == schemaName) {
			return nil // the session's search_path is not ours to know: any schema will do
		}
	}
	if schemaName == "" || schemaName == "pg_catalog" {
		if len(a.s.Catalog.FuncsByName(fname)) > 0 {
			return nil
		}
	}
	return errAt("42883", loc, "function %q does not exist", v)
}

// validateRegoperLiteral is regoperin / regoperatorin on the operator's symbol only: a
// numeric OID, or a (qualified) operator name that exists somewhere.
func (a *analyzer) validateRegoperLiteral(s string, withArgs bool, loc int32) *Error {
	v := strings.TrimSpace(s)
	if v == "" || v == "0" {
		return nil
	}
	if _, err := parsePGInt(v, 64); err == nil {
		return nil
	}
	name := v
	if withArgs {
		name, _, _ = strings.Cut(v, "(")
		name = strings.TrimSpace(name)
	}
	// OPERATOR(schema.+) style qualification: the symbol is the last dotted part
	if i := strings.LastIndex(name, "."); i >= 0 {
		if sch := strings.ToLower(strings.TrimSpace(name[:i])); !a.s.HasSchema(sch) {
			return errAt("42883", loc, "operator does not exist: %s", v)
		}
		name = name[i+1:]
	}
	if len(a.s.Catalog.OperatorsByName(name)) > 0 {
		return nil
	}
	for _, op := range a.s.Operators {
		if op.Name == name {
			return nil
		}
	}
	return errAt("42883", loc, "operator does not exist: %s", v)
}

// jsonSurrogatesOK checks the \uXXXX escapes inside JSON strings pair their UTF-16
// surrogates the way PG's json parser demands (Go's validator lets lone ones through).
func jsonSurrogatesOK(s string) bool {
	inStr := false
	hi := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !inStr:
			inStr = c == '"'
		case c == '\\':
			if i+5 < len(s) && s[i+1] == 'u' {
				v, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
				if err == nil {
					switch {
					case v >= 0xD800 && v <= 0xDBFF:
						if hi {
							return false
						}
						hi = true
					case v >= 0xDC00 && v <= 0xDFFF:
						if !hi {
							return false
						}
						hi = false
					default:
						if hi {
							return false
						}
					}
					i += 5
					continue
				}
			}
			if hi {
				return false
			}
			i++
		case c == '"':
			if hi {
				return false
			}
			inStr = false
		default:
			if hi {
				return false
			}
		}
	}
	return true
}

// validateAssignLength is the assignment-context length coercion PG applies after the
// input function when a literal is stored into a column with a type modifier: bit(n) must
// match exactly (22026), varbit(n) / char(n) / varchar(n) must fit once trailing spaces
// are trimmed (22001), numeric(p,s) must not overflow its integer digits (22003).
func (a *analyzer) validateAssignLength(s string, target schema.TypeRef, loc int32) *Error {
	if target.Typmod < 0 {
		return nil
	}
	base := a.baseType(target.OID)
	n := int(target.Typmod)
	switch base {
	case oidBit, oidVarbit:
		digits := s
		hex := false
		if len(s) > 0 && (s[0] == 'b' || s[0] == 'B') {
			digits = s[1:]
		} else if len(s) > 0 && (s[0] == 'x' || s[0] == 'X') {
			digits, hex = s[1:], true
		}
		bitlen := len(digits)
		if hex {
			bitlen *= 4
		}
		if base == oidBit && bitlen != n {
			return errAt("22026", loc, "bit string length %d does not match type bit(%d)", bitlen, n)
		}
		if base == oidVarbit && bitlen > n {
			return errAt("22001", loc, "bit string too long for type bit varying(%d)", n)
		}
	case catalog.BPChar, catalog.Varchar:
		n -= 4 // VARHDRSZ
		if n < 0 {
			return nil
		}
		if utf8.RuneCountInString(s) > n {
			// excess characters may only be spaces
			runes := []rune(s)
			for _, r := range runes[n:] {
				if r != ' ' {
					name := "character varying"
					if base == catalog.BPChar {
						name = "character"
					}
					return errAt("22001", loc, "value too long for type %s(%d)", name, n)
				}
			}
		}
	case catalog.Numeric:
		// numeric_typmod_precision / numeric_typmod_scale: the scale is 11 bits, offset so
		// that negative scales (numeric(2,-1)) fit
		precision, scale := (n-4)>>16&0xFFFF, (n-4)&0x7FF
		if scale >= 0x400 {
			scale -= 0x800
		}
		v := strings.TrimSpace(stripDigitSeparators(s))
		// infinity never fits a precision (NaN does)
		if w := strings.ToLower(strings.TrimLeft(v, "+-")); w == "infinity" || w == "inf" {
			return errAt("22003", loc, "numeric field overflow")
		}
		if !isNumberish(v) {
			return nil
		}
		// count integer digits of the value rounded to scale; numeric has no range limit
		// of its own, so a value float64 cannot hold (1e400) is counted exactly
		r := new(big.Float).SetPrec(200)
		if _, ok := r.SetString(v); !ok {
			return nil
		}
		r.Abs(r)
		mult := new(big.Float).SetPrec(200).SetFloat64(math.Pow10(scale))
		if scale >= 0 {
			r.Mul(r, mult)
		} else {
			r.Quo(r, new(big.Float).SetPrec(200).SetFloat64(math.Pow10(-scale)))
		}
		// round half away from zero
		r.Add(r, big.NewFloat(0.5))
		i, _ := r.Int(nil)
		if i == nil {
			return errAt("22003", loc, "numeric field overflow")
		}
		limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
		if i.Cmp(limit) >= 0 {
			return errAt("22003", loc, "numeric field overflow")
		}
	}
	return nil
}

// validateXMLLiteral is xml_in: under XMLOPTION content well-formed XML content, where a
// DOCTYPE turns the value into a document with exactly one root element; under XMLOPTION
// document a document. The SQLSTATE follows the option, not what the value turned out to be.
func validateXMLLiteral(s string, docMode bool, loc int32) *Error {
	document := docMode
	bad := func() *Error {
		if docMode {
			return errAt("2200M", loc, "invalid XML document")
		}
		return errAt("2200N", loc, "invalid XML content")
	}
	if strings.HasPrefix(s, "<?xml") {
		if end := strings.Index(s, "?>"); end > 0 {
			decl := strings.Replace(s[:end], `version="1.1"`, `version="1.0"`, 1)
			decl = strings.Replace(decl, `version='1.1'`, `version='1.0'`, 1)
			s = decl + s[end:]
		}
	}
	dec := xml.NewDecoder(strings.NewReader(s))
	dec.Strict = true
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	sawContent := false // an element or non-blank text
	roots, depth := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bad()
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if document && roots > 1 {
					return bad()
				}
			}
			depth++
			sawContent = true
		case xml.EndElement:
			depth--
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				if depth == 0 && document {
					return bad()
				}
				sawContent = true
			}
		case xml.Directive:
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(t))), "DOCTYPE") {
				if sawContent {
					return bad()
				}
				document = true
			} else {
				return bad()
			}
		case xml.ProcInst:
			if t.Target == "xml" && sawContent {
				return bad()
			}
		}
	}
	if depth != 0 || (document && roots != 1) {
		return bad()
	}
	return nil
}

// isNonDecimalInt is a 0x / 0o / 0b integer of any length, digit separators allowed
// (numeric_in has no range limit).
func isNonDecimalInt(v string) bool {
	v = strings.TrimLeft(v, "+-")
	if len(v) < 3 || v[0] != '0' {
		return false
	}
	base := 0
	switch v[1] {
	case 'x', 'X':
		base = 16
	case 'o', 'O':
		base = 8
	case 'b', 'B':
		base = 2
	default:
		return false
	}
	v = strings.TrimPrefix(v[2:], "_")
	if v == "" || strings.HasSuffix(v, "_") || strings.Contains(v, "__") {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '_' {
			continue
		}
		if _, ok := digitVal(v[i], base); !ok {
			return false
		}
	}
	return true
}

// validateRegclassLiteral is regclassin: a numeric OID, or the name of a table, view,
// sequence, index or composite type (42P01 otherwise).
func (a *analyzer) validateRegclassLiteral(s string, loc int32) *Error {
	v := strings.TrimSpace(s)
	if v == "" || v == "-" {
		return nil
	}
	if _, err := parsePGInt(v, 64); err == nil {
		return nil
	}
	parts := qualifiedNameParts(v)
	schemaName, name := "", parts[len(parts)-1]
	if len(parts) > 1 {
		schemaName = parts[len(parts)-2]
	}
	if a.s.Relation(schemaName, name) != nil {
		return nil
	}
	for _, r := range a.s.Relations {
		if schemaName != "" && r.Schema != schemaName {
			continue
		}
		for _, ix := range r.Indexes {
			if ix.Name == name {
				return nil
			}
		}
		for _, c := range r.Constraints {
			if c.Name == name && (c.Kind == schema.PrimaryKey || c.Kind == schema.Unique) {
				return nil // the index behind a PK / UNIQUE constraint
			}
		}
	}
	return errAt("42P01", loc, "relation %q does not exist", v)
}

// validateBitConst checks a B'...' / X'...' constant: only binary / hex digits inside.
func validateBitConst(s string, loc int32) *Error {
	if s == "" {
		return nil
	}
	hex := s[0] == 'x' || s[0] == 'X'
	for _, ch := range s[1:] {
		if hex && !isHexDigit(byte(ch)) {
			return errAt("22P02", loc, "%q is not a valid hexadecimal digit", string(ch))
		}
		if !hex && ch != '0' && ch != '1' {
			return errAt("22P02", loc, "%q is not a valid binary digit", string(ch))
		}
	}
	return nil
}

// validateTsvectorLiteral is the shape of tsvector_in it is safe to enforce: a quoted
// lexeme must not be empty and must be closed; the position list after ':' is not checked.
func validateTsvectorLiteral(s string, loc int32) *Error {
	bad := func() *Error { return errAt(codeSyntaxError, loc, "syntax error in tsvector: %q", s) }
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '\'' {
			i++
			content := false
			for {
				if i >= len(s) {
					return bad()
				}
				if s[i] == '\\' {
					i += 2
					content = true
					continue
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						content = true
						continue
					}
					break
				}
				i++
				content = true
			}
			if !content {
				return bad()
			}
			i++
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != ':' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
		}
		// an optional position list: up to the next white space
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			i++
		}
	}
	return nil
}
