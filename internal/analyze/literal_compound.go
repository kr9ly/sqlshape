package analyze

// Input syntax of the container and geometric types, after PG's array_in, range_in,
// multirange_in, geo_ops.c, pg_lsn_in and byteain. Elements and bounds are handed back
// to validateLiteralTypmod, so an array of enums or a range of dates is checked the way
// PG checks it: container syntax first, then each member with its own input function.

import (
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

const (
	oidPoint   catalog.OID = 600
	oidLseg    catalog.OID = 601
	oidPath    catalog.OID = 602
	oidBox     catalog.OID = 603
	oidPolygon catalog.OID = 604
	oidLine    catalog.OID = 628
	oidCircle  catalog.OID = 718
	oidPgLSN   catalog.OID = 3220

	oidInt2Vector catalog.OID = 22
	oidOidVector  catalog.OID = 30
	oidInet       catalog.OID = 869
	oidCidr       catalog.OID = 650
	oidMacaddr    catalog.OID = 829
	oidBit        catalog.OID = 1560
	oidVarbit     catalog.OID = 1562
)

// scannerIsSpace is scanner_isspace: the SQL lexer's notion of whitespace.
func scannerIsSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// validateCompoundLiteral dispatches the container / geometric types; ok is false when
// the type is not one of them.
func (a *analyzer) validateCompoundLiteral(s string, base catalog.OID, typmod int32, loc int32) (err *Error, ok bool) {
	t := a.typ(base)
	if t == nil {
		return nil, false
	}
	switch {
	case base == oidInt2Vector || base == oidOidVector:
		// int2vectorin / oidvectorin: whitespace-separated values, no braces
		name, elem := "int2vector", catalog.Int2
		if base == oidOidVector {
			name, elem = "oidvector", catalog.OIDType
		}
		for _, f := range strings.Fields(s) {
			if e := a.validateLiteralTypmod(f, elem, -1, loc); e != nil {
				return errAt("22P02", loc, "invalid input syntax for type %s: %q", name, s), true
			}
		}
		return nil, true
	case t.IsArray():
		delim := byte(',')
		if t.Elem == oidBox {
			delim = ';'
		}
		return a.validateArrayLiteral(s, t.Elem, typmod, delim, loc), true
	case t.Kind == 'r':
		return a.validateRangeLiteral(s, base, loc), true
	case t.Kind == 'm':
		return a.validateMultirangeLiteral(s, base, loc), true
	case t.Kind == 'c':
		if rel := relByRowType(a.s, base); rel != nil {
			return a.validateRecordLiteral(s, rel, loc), true
		}
		return nil, true
	}
	switch base {
	case oidPoint, oidLseg, oidPath, oidBox, oidPolygon, oidLine, oidCircle:
		return validateGeoLiteral(s, base, loc), true
	case oidPgLSN:
		if !validPgLSN(s) {
			return errAt("22P02", loc, "invalid input syntax for type pg_lsn: %q", s), true
		}
		return nil, true
	case catalog.Bytea:
		return validateByteaLiteral(s, loc), true
	case oidInet, oidCidr:
		return validateInetLiteral(s, base == oidCidr, loc), true
	case oidMacaddr:
		return validateMacaddrLiteral(s, loc), true
	case oidBit, oidVarbit:
		return validateBitLiteral(s, base == oidVarbit, typmod, loc), true
	}
	return nil, false
}

// --- records ------------------------------------------------------------------------------

// validateRecordLiteral is record_in: "(f1,f2,...)" with one field per attribute, each
// read by its own input function.
func (a *analyzer) validateRecordLiteral(str string, rel *schema.Relation, loc int32) *Error {
	malformed := func() *Error { return errAt("22P02", loc, "malformed record literal: %q", str) }
	p := 0
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	if p >= len(str) || str[p] != '(' {
		return malformed()
	}
	p++
	needComma := false
	for _, col := range rel.Columns {
		if needComma {
			if p >= len(str) || str[p] != ',' {
				return malformed()
			}
			p++
		}
		if p < len(str) && (str[p] == ',' || str[p] == ')') {
			needComma = true
			continue // NULL field
		}
		var buf []byte
		inquote := false
		for inquote || !(p < len(str) && (str[p] == ',' || str[p] == ')')) {
			if p >= len(str) {
				return malformed()
			}
			ch := str[p]
			p++
			switch {
			case ch == '\\':
				if p >= len(str) {
					return malformed()
				}
				buf = append(buf, str[p])
				p++
			case ch == '"':
				if !inquote {
					inquote = true
				} else if p < len(str) && str[p] == '"' {
					buf = append(buf, '"')
					p++
				} else {
					inquote = false
				}
			default:
				buf = append(buf, ch)
			}
		}
		if e := a.validateLiteralTypmod(string(buf), col.Type.OID, col.Type.Typmod, loc); e != nil {
			return e
		}
		needComma = true
	}
	if p >= len(str) || str[p] != ')' {
		return malformed()
	}
	p++
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	if p != len(str) {
		return malformed()
	}
	return nil
}

// --- network / mac / bit ------------------------------------------------------------------

// validateInetLiteral is network_in: dotted IPv4 with 1-4 octets (cidr fills the rest
// with zeros and takes 8 bits per octet given) or an IPv6 address, an optional /bits;
// a cidr may not have bits set right of the mask.
func validateInetLiteral(s string, isCidr bool, loc int32) *Error {
	name := "inet"
	if isCidr {
		name = "cidr"
	}
	bad := func() *Error { return errAt("22P02", loc, "invalid input syntax for type %s: %q", name, s) }
	addr, maskStr, hasMask := strings.Cut(s, "/")
	var ip net.IP
	maxBits := 32
	bits := -1
	if hasMask {
		v, rest, erange := strtoNum(maskStr, 32)
		if erange || rest != "" || len(maskStr) == 0 || maskStr[0] == '+' || maskStr[0] == '-' {
			return bad()
		}
		bits = int(v)
	}
	if strings.Contains(addr, ":") {
		maxBits = 128
		ip = net.ParseIP(addr)
		if ip == nil || strings.Contains(addr, "%") {
			return bad()
		}
		if bits < 0 {
			bits = 128
		}
	} else {
		parts := strings.Split(addr, ".")
		if len(parts) < 1 || len(parts) > 4 {
			return bad()
		}
		ip = make(net.IP, 4)
		for i, part := range parts {
			v, rest, erange := strtoNum(part, 32)
			if part == "" || erange || rest != "" || v < 0 || v > 255 || !cIsDigit(part[0]) {
				return bad()
			}
			ip[i] = byte(v)
		}
		if len(parts) < 4 && !isCidr && !hasMask {
			return bad()
		}
		if bits < 0 {
			bits = 32
			if isCidr {
				bits = 8 * len(parts)
			}
		}
	}
	if bits > maxBits {
		return bad()
	}
	if isCidr {
		mask := net.CIDRMask(bits, maxBits)
		for i := range ip {
			if ip[i]&^mask[i] != 0 {
				return errAt("22P02", loc, "invalid cidr value: %q", s)
			}
		}
	}
	return nil
}

var macaddrForms = []*regexp.Regexp{
	regexp.MustCompile(`^\s*([0-9a-fA-F]+):([0-9a-fA-F]+):([0-9a-fA-F]+):([0-9a-fA-F]+):([0-9a-fA-F]+):([0-9a-fA-F]+)$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]+)-([0-9a-fA-F]+)-([0-9a-fA-F]+)-([0-9a-fA-F]+)-([0-9a-fA-F]+)-([0-9a-fA-F]+)$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2}):([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})-([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]{2})([0-9a-fA-F]{2})\.([0-9a-fA-F]{2})([0-9a-fA-F]{2})\.([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]{2})([0-9a-fA-F]{2})-([0-9a-fA-F]{2})([0-9a-fA-F]{2})-([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`),
	regexp.MustCompile(`^\s*([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`),
}

// validateMacaddrLiteral is macaddr_in's sscanf ladder.
func validateMacaddrLiteral(s string, loc int32) *Error {
	for _, re := range macaddrForms {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		for _, g := range m[1:] {
			if v, err := strconv.ParseUint(g, 16, 64); err != nil || v > 255 {
				return errAt("22003", loc, "invalid octet value in \"macaddr\" value: %q", s)
			}
		}
		return nil
	}
	return errAt("22P02", loc, "invalid input syntax for type macaddr: %q", s)
}

// validateBitLiteral is bit_in / varbit_in: B'...' binary or X'...' hex digits (a bare
// string is binary), and the length against bit(n) / varbit(n).
func validateBitLiteral(s string, varying bool, typmod int32, loc int32) *Error {
	digits := s
	hex := false
	if len(s) > 0 && (s[0] == 'b' || s[0] == 'B') {
		digits = s[1:]
	} else if len(s) > 0 && (s[0] == 'x' || s[0] == 'X') {
		digits, hex = s[1:], true
	}
	bitlen := len(digits)
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if hex {
			if !isHexDigit(c) {
				return errAt("22P02", loc, "%q is not a valid hexadecimal digit", string(c))
			}
		} else if c != '0' && c != '1' {
			return errAt("22P02", loc, "%q is not a valid binary digit", string(c))
		}
	}
	if hex {
		bitlen *= 4
	}
	if typmod > 0 {
		if !varying && bitlen != int(typmod) {
			return errAt("22026", loc, "bit string length %d does not match type bit(%d)", bitlen, typmod)
		}
		if varying && bitlen > int(typmod) {
			return errAt("22001", loc, "bit string too long for type bit varying(%d)", typmod)
		}
	}
	return nil
}

// --- arrays ----------------------------------------------------------------------------

const maxDim = 6

func (a *analyzer) validateArrayLiteral(str string, elem catalog.OID, typmod int32, delim byte, loc int32) *Error {
	malformed := func() *Error { return errAt("22P02", loc, "malformed array literal: %q", str) }
	p := 0
	// dimensions: [lb:ub][...]=
	var dim [maxDim]int
	for i := range dim {
		dim[i] = -1
	}
	ndim := 0
	for {
		for p < len(str) && scannerIsSpace(str[p]) {
			p++
		}
		if p >= len(str) || str[p] != '[' {
			break
		}
		p++
		if ndim >= maxDim {
			return errAt("54000", loc, "number of array dimensions exceeds the maximum allowed (%d)", maxDim)
		}
		readInt := func() (int, bool, *Error) {
			if p >= len(str) || !(cIsDigit(str[p]) || str[p] == '-' || str[p] == '+') {
				return 0, false, nil
			}
			v, rest, erange := strtoNum(str[p:], 64)
			if erange || v > math.MaxInt32 || v < math.MinInt32 {
				return 0, true, errAt("54000", loc, "array bound is out of integer range")
			}
			if len(rest) == len(str[p:]) {
				return 0, false, nil
			}
			p = len(str) - len(rest)
			return int(v), true, nil
		}
		i, got, e := readInt()
		if e != nil {
			return e
		}
		if !got {
			return malformed()
		}
		lb, ub := 1, i
		if p < len(str) && str[p] == ':' {
			p++
			lb = i
			if ub, got, e = readInt(); e != nil {
				return e
			} else if !got {
				return malformed()
			}
		}
		if p >= len(str) || str[p] != ']' {
			return malformed()
		}
		p++
		if ub < lb {
			return errAt("2202E", loc, "upper bound cannot be less than lower bound")
		}
		if ub == math.MaxInt32 {
			return errAt("54000", loc, "array upper bound is too large: %d", ub)
		}
		dim[ndim] = ub - lb + 1
		ndim++
	}
	if ndim == 0 {
		if p >= len(str) || str[p] != '{' {
			return malformed()
		}
	} else {
		if p >= len(str) || str[p] != '=' {
			return malformed()
		}
		p++
		for p < len(str) && scannerIsSpace(str[p]) {
			p++
		}
		if p >= len(str) || str[p] != '{' {
			return malformed()
		}
	}
	// contents
	dimensionsSpecified := ndim != 0
	ndimFrozen := dimensionsSpecified
	nestLevel := 0
	expectDelim := false
	var nelems [maxDim]int
	dimensionError := func() *Error { return malformed() }
	for {
		tok, elemText, isNull, e := readArrayToken(str, &p, delim, malformed)
		if e != nil {
			return e
		}
		switch tok {
		case atokLevelStart:
			if expectDelim {
				return malformed()
			}
			if nestLevel >= maxDim {
				return errAt("54000", loc, "number of array dimensions exceeds the maximum allowed (%d)", maxDim)
			}
			nelems[nestLevel] = 0
			nestLevel++
			if nestLevel > ndim {
				if ndimFrozen {
					return dimensionError()
				}
				ndim = nestLevel
			}
		case atokLevelEnd:
			if nelems[nestLevel-1] > 0 && !expectDelim {
				return malformed()
			}
			nestLevel--
			if nestLevel > 0 {
				nelems[nestLevel-1]++
			}
			if dim[nestLevel] < 0 {
				dim[nestLevel] = nelems[nestLevel]
			} else if nelems[nestLevel] != dim[nestLevel] {
				return dimensionError()
			}
			expectDelim = true
		case atokDelim:
			if !expectDelim {
				return malformed()
			}
			expectDelim = false
		case atokElem:
			if expectDelim {
				return malformed()
			}
			if !isNull {
				if e := a.validateLiteralTypmod(elemText, elem, typmod, loc); e != nil {
					return e
				}
			}
			ndimFrozen = true
			if nestLevel != ndim {
				return dimensionError()
			}
			nelems[nestLevel-1]++
			expectDelim = true
		}
		if nestLevel <= 0 {
			break
		}
	}
	for p < len(str) {
		if !scannerIsSpace(str[p]) {
			return malformed()
		}
		p++
	}
	return nil
}

const (
	atokLevelStart = iota
	atokLevelEnd
	atokDelim
	atokElem
)

// readArrayToken is ReadArrayToken; the element text is returned unescaped.
func readArrayToken(str string, pp *int, delim byte, malformed func() *Error) (tok int, elem string, isNull bool, err *Error) {
	p := *pp
	defer func() { *pp = p }()
	var buf []byte
	for {
		if p >= len(str) {
			return 0, "", false, malformed()
		}
		switch c := str[p]; {
		case c == '{':
			p++
			return atokLevelStart, "", false, nil
		case c == '}':
			p++
			return atokLevelEnd, "", false, nil
		case c == '"':
			p++
			// quoted element
			for {
				if p >= len(str) {
					return 0, "", false, malformed()
				}
				switch str[p] {
				case '\\':
					p++
					if p >= len(str) {
						return 0, "", false, malformed()
					}
					buf = append(buf, str[p])
					p++
				case '"':
					p++
					for p < len(str) {
						if str[p] == delim || str[p] == '}' || str[p] == '{' {
							return atokElem, string(buf), false, nil
						}
						if !scannerIsSpace(str[p]) {
							return 0, "", false, malformed()
						}
						p++
					}
					return 0, "", false, malformed()
				default:
					buf = append(buf, str[p])
					p++
				}
			}
		case c == delim:
			p++
			return atokDelim, "", false, nil
		case scannerIsSpace(c):
			p++
			continue
		}
		// unquoted element
		dstlen := 0
		hasEscapes := false
		for {
			if p >= len(str) {
				return 0, "", false, malformed()
			}
			switch c := str[p]; {
			case c == '{', c == '"':
				return 0, "", false, malformed()
			case c == '\\':
				p++
				if p >= len(str) {
					return 0, "", false, malformed()
				}
				buf = append(buf, str[p])
				p++
				dstlen = len(buf)
				hasEscapes = true
			case c == delim || c == '}':
				buf = buf[:dstlen]
				if !hasEscapes && strings.EqualFold(string(buf), "NULL") {
					return atokElem, "", true, nil
				}
				return atokElem, string(buf), false, nil
			default:
				buf = append(buf, c)
				if !scannerIsSpace(c) {
					dstlen = len(buf)
				}
				p++
			}
		}
	}
}

// --- ranges ----------------------------------------------------------------------------

// rangeParse is range_parse: it returns the bound texts (nil for an infinite bound).
func rangeParse(str string) (lower, upper *string, empty bool, ok bool) {
	p := 0
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	if len(str)-p >= 5 && strings.EqualFold(str[p:p+5], "empty") {
		p += 5
		for p < len(str) && cIsSpace(str[p]) {
			p++
		}
		return nil, nil, true, p == len(str)
	}
	if p >= len(str) || (str[p] != '[' && str[p] != '(') {
		return nil, nil, false, false
	}
	p++
	bound := func() (*string, bool) {
		if p < len(str) && (str[p] == ',' || str[p] == ')' || str[p] == ']') {
			return nil, true
		}
		var buf []byte
		inquote := false
		for inquote || !(p < len(str) && (str[p] == ',' || str[p] == ')' || str[p] == ']')) {
			if p >= len(str) {
				return nil, false
			}
			ch := str[p]
			p++
			switch {
			case ch == '\\':
				if p >= len(str) {
					return nil, false
				}
				buf = append(buf, str[p])
				p++
			case ch == '"':
				if !inquote {
					inquote = true
				} else if p < len(str) && str[p] == '"' {
					buf = append(buf, '"')
					p++
				} else {
					inquote = false
				}
			default:
				buf = append(buf, ch)
			}
		}
		s := string(buf)
		return &s, true
	}
	if lower, ok = bound(); !ok {
		return nil, nil, false, false
	}
	if p >= len(str) || str[p] != ',' {
		return nil, nil, false, false
	}
	p++
	if upper, ok = bound(); !ok {
		return nil, nil, false, false
	}
	if p >= len(str) || (str[p] != ']' && str[p] != ')') {
		return nil, nil, false, false
	}
	p++
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	return lower, upper, false, p == len(str)
}

func (a *analyzer) validateRangeLiteral(str string, rng catalog.OID, loc int32) *Error {
	lower, upper, empty, ok := rangeParse(str)
	if !ok {
		return errAt("22P02", loc, "malformed range literal: %q", str)
	}
	if empty {
		return nil
	}
	r := a.s.Types.RangeOf(rng)
	if r == nil {
		return nil
	}
	for _, b := range []*string{lower, upper} {
		if b != nil {
			if e := a.validateLiteralTypmod(*b, r.Subtype, -1, loc); e != nil {
				return e
			}
		}
	}
	return nil
}

func (a *analyzer) validateMultirangeLiteral(str string, multi catalog.OID, loc int32) *Error {
	malformed := func() *Error { return errAt("22P02", loc, "malformed multirange literal: %q", str) }
	r := a.s.Types.RangeOfMulti(multi)
	p := 0
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	if p >= len(str) || str[p] != '{' {
		return malformed()
	}
	p++
	const (
		beforeRange = iota
		inRange
		inRangeEscaped
		inRangeQuoted
		inRangeQuotedEscaped
		afterRange
		finished
	)
	state := beforeRange
	rangesSeen := 0
	rangeStart := 0
	for ; state != finished; p++ {
		if p >= len(str) {
			return malformed()
		}
		ch := str[p]
		if cIsSpace(ch) {
			continue
		}
		switch state {
		case beforeRange:
			switch {
			case ch == '[' || ch == '(':
				rangeStart = p
				state = inRange
			case ch == '}' && rangesSeen == 0:
				state = finished
			case len(str)-p >= 5 && strings.EqualFold(str[p:p+5], "empty"):
				rangesSeen++
				p += 4
				state = afterRange
			default:
				return malformed()
			}
		case inRange:
			switch ch {
			case ']', ')':
				rangesSeen++
				if r != nil {
					if e := a.validateRangeLiteral(str[rangeStart:p+1], r.OID, loc); e != nil {
						return e
					}
				}
				state = afterRange
			case '"':
				state = inRangeQuoted
			case '\\':
				state = inRangeEscaped
			}
		case inRangeEscaped:
			state = inRange
		case inRangeQuoted:
			if ch == '"' {
				if p+1 < len(str) && str[p+1] == '"' {
					p++
				} else {
					state = inRange
				}
			} else if ch == '\\' {
				state = inRangeQuotedEscaped
			}
		case afterRange:
			switch ch {
			case ',':
				state = beforeRange
			case '}':
				state = finished
			default:
				return malformed()
			}
		case inRangeQuotedEscaped:
			state = inRangeQuoted
		}
	}
	for p < len(str) && cIsSpace(str[p]) {
		p++
	}
	if p != len(str) {
		return malformed()
	}
	return nil
}

// --- geometry ---------------------------------------------------------------------------

// geoScan reads coordinates the way geo_ops.c does; fail is sticky.
type geoScan struct {
	s    string
	p    int
	fail bool
}

func (g *geoScan) skipSpace() {
	for g.p < len(g.s) && cIsSpace(g.s[g.p]) {
		g.p++
	}
}

func (g *geoScan) peek() byte {
	if g.p >= len(g.s) {
		return 0
	}
	return g.s[g.p]
}

func (g *geoScan) expect(c byte) {
	if g.peek() != c {
		g.fail = true
		return
	}
	g.p++
}

// single is single_decode: float8in_internal with an end pointer.
func (g *geoScan) single() {
	if g.fail {
		return
	}
	g.skipSpace()
	rest := g.s[g.p:]
	if rest == "" {
		g.fail = true
		return
	}
	f, tail, ok := strtod(rest)
	if !ok || len(tail) == len(rest) {
		low := strings.ToLower(rest)
		matched := false
		for _, w := range []string{"nan", "infinity", "+infinity", "-infinity", "inf", "+inf", "-inf"} {
			if strings.HasPrefix(low, w) {
				tail, matched = rest[len(w):], true
				break
			}
		}
		if !matched {
			if len(tail) != len(rest) && (math.IsInf(f, 0) || f == 0) {
				g.fail = true // out of range: 22003 in PG, reported as the same class below
				return
			}
			g.fail = true
			return
		}
	}
	g.p = len(g.s) - len(tail)
	g.skipSpace()
}

// pair is pair_decode: x,y or (x,y).
func (g *geoScan) pair() {
	if g.fail {
		return
	}
	g.skipSpace()
	hasDelim := g.peek() == '('
	if hasDelim {
		g.p++
	}
	g.single()
	g.expect(',')
	g.single()
	if hasDelim {
		g.expect(')')
		g.skipSpace()
	}
}

// path is path_decode for npts points.
func (g *geoScan) path(opentype bool, npts int) (isopen bool) {
	if g.fail {
		return
	}
	g.skipSpace()
	depth := 0
	if g.peek() == '[' {
		isopen = true
		if !opentype {
			g.fail = true
			return
		}
		depth++
		g.p++
	} else if g.peek() == '(' {
		cp := g.p + 1
		for cp < len(g.s) && cIsSpace(g.s[cp]) {
			cp++
		}
		if cp < len(g.s) && g.s[cp] == '(' {
			depth++
			g.p = cp
		} else if strings.LastIndexByte(g.s[g.p:], '(') == 0 {
			depth++
			g.p = cp
		}
	}
	for i := 0; i < npts; i++ {
		g.pair()
		if g.fail {
			return
		}
		if g.peek() == ',' {
			g.p++
		}
	}
	for depth > 0 {
		if g.peek() == ')' || (g.peek() == ']' && isopen && depth == 1) {
			depth--
			g.p++
			g.skipSpace()
		} else {
			g.fail = true
			return
		}
	}
	return
}

func (g *geoScan) atEnd() bool { return g.p >= len(g.s) }

// pairCount is pair_count: the number of points in a comma-separated list, -1 if odd.
func pairCount(s string) int {
	n := strings.Count(s, ",")
	if n%2 == 1 {
		return (n + 1) / 2
	}
	return -1
}

func validateGeoLiteral(str string, typ catalog.OID, loc int32) *Error {
	name := map[catalog.OID]string{oidPoint: "point", oidLseg: "lseg", oidPath: "path", oidBox: "box", oidPolygon: "polygon", oidLine: "line", oidCircle: "circle"}[typ]
	bad := func() *Error { return errAt("22P02", loc, "invalid input syntax for type %s: %q", name, str) }
	g := &geoScan{s: str}
	switch typ {
	case oidPoint:
		g.pair()
		if g.fail || !g.atEnd() {
			return bad()
		}
	case oidBox:
		g.path(false, 2)
		if g.fail || !g.atEnd() {
			return bad()
		}
	case oidLseg:
		g.path(true, 2)
		if g.fail || !g.atEnd() {
			return bad()
		}
	case oidLine:
		g.skipSpace()
		if g.peek() == '{' {
			g.p++
			g.single()
			g.expect(',')
			g.single()
			g.expect(',')
			g.single()
			g.expect('}')
			g.skipSpace()
			if g.fail || !g.atEnd() {
				return bad()
			}
			// A and B cannot both be zero: values are needed; re-read them
			body := strings.TrimSpace(str)
			body = strings.TrimSuffix(strings.TrimPrefix(body, "{"), "}")
			parts := strings.Split(body, ",")
			if len(parts) == 3 {
				a, _, _ := strtod(strings.TrimSpace(parts[0]))
				b, _, _ := strtod(strings.TrimSpace(parts[1]))
				if a == 0 && b == 0 {
					return errAt("22P02", loc, "invalid line specification: A and B cannot both be zero")
				}
			}
		} else {
			g.path(true, 2)
			if g.fail || !g.atEnd() {
				return bad()
			}
			if nums := extractFloats(str); len(nums) == 4 && nums[0] == nums[2] && nums[1] == nums[3] {
				return errAt("22P02", loc, "invalid line specification: must be two distinct points")
			}
		}
	case oidPath:
		npts := pairCount(str)
		if npts <= 0 {
			return bad()
		}
		g.skipSpace()
		depth := 0
		if g.peek() == '(' && strings.LastIndexByte(str[g.p:], '(') == 0 {
			g.p++
			depth++
		}
		g.path(true, npts)
		if g.fail {
			return bad()
		}
		if depth >= 1 {
			g.expect(')')
			g.skipSpace()
		}
		if g.fail || !g.atEnd() {
			return bad()
		}
	case oidPolygon:
		npts := pairCount(str)
		if npts <= 0 {
			return bad()
		}
		g.path(false, npts)
		if g.fail || !g.atEnd() {
			return bad()
		}
	case oidCircle:
		g.skipSpace()
		depth := 0
		if g.peek() == '<' {
			depth++
			g.p++
		} else if g.peek() == '(' {
			cp := g.p + 1
			for cp < len(str) && cIsSpace(str[cp]) {
				cp++
			}
			if cp < len(str) && str[cp] == '(' {
				depth++
				g.p = cp
			}
		}
		g.pair()
		if g.peek() == ',' {
			g.p++
		}
		rstart := g.p
		g.single()
		if g.fail {
			return bad()
		}
		if r, _, ok := strtod(strings.TrimSpace(str[rstart:g.p])); ok && r < 0 {
			return bad()
		}
		for depth > 0 {
			if g.peek() == ')' || (g.peek() == '>' && depth == 1) {
				depth--
				g.p++
				g.skipSpace()
			} else {
				return bad()
			}
		}
		if !g.atEnd() {
			return bad()
		}
	}
	return nil
}

// extractFloats pulls every number out of a geometric literal, in order.
func extractFloats(s string) []float64 {
	var out []float64
	for i := 0; i < len(s); {
		c := s[i]
		if cIsDigit(c) || c == '-' || c == '+' || c == '.' {
			f, rest, ok := strtod(s[i:])
			if ok && len(rest) < len(s)-i {
				out = append(out, f)
				i = len(s) - len(rest)
				continue
			}
		}
		i++
	}
	return out
}

// --- pg_lsn / bytea ---------------------------------------------------------------------

func validPgLSN(s string) bool {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	hex := func(x string) bool {
		if len(x) < 1 || len(x) > 8 {
			return false
		}
		for i := 0; i < len(x); i++ {
			if !isHexDigit(x[i]) {
				return false
			}
		}
		return true
	}
	return hex(a) && hex(b)
}

func validateByteaLiteral(s string, loc int32) *Error {
	if strings.HasPrefix(s, `\x`) {
		digits := 0
		for i := 2; i < len(s); i++ {
			c := s[i]
			if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
				continue
			}
			if !isHexDigit(c) {
				return errAt("22023", loc, "invalid hexadecimal digit: %q", string(c))
			}
			digits++
		}
		if digits%2 == 1 {
			return errAt("22023", loc, "invalid hexadecimal data: odd number of digits")
		}
		return nil
	}
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			i++
			continue
		}
		switch {
		case i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && s[i+2] >= '0' && s[i+2] <= '7' && s[i+3] >= '0' && s[i+3] <= '7':
			i += 4
		case i+1 < len(s) && s[i+1] == '\\':
			i += 2
		default:
			return errAt("22P02", loc, "invalid input syntax for type bytea")
		}
	}
	return nil
}
