package analyze

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// The spatial types. A geometry value's type is one of seven (POINT ... GEOMETRYCOLLECTION,
// wkb_point .. wkb_geometrycollection in spatial.h), a column is declared with one of them
// or with GEOMETRY (any). The checker carries a value's type as the typed's Name when it
// knows it -- a constructor's (POINT(1, 1) is a point), a typed reader's (ST_PointFromText
// returns a point or fails), a constant WKT's or WKB's -- and "geometry" when it does not
// (a GEOMETRY column, ST_Buffer). What it judges with that, each measured on mysqld 8.4
// (TestGeometryServer; found by the corpus probe's gis files):
//
//   - ST_GeomFromText and the typed variants over a constant WKT parse it the way
//     spatial.cc's create_from_wkt / Gis_*::init_from_wkt do (parseWKT): a malformed text,
//     a LINESTRING of one point, a ring of fewer than four points or not closed, a
//     MULTIPOINT mixing its two bracket styles, an empty MULTIPOINT, is 3037 "Invalid GIS
//     data provided to function <name>"; a text of another type than the typed variant's
//     (ST_PointFromText('LINESTRING(...)'); the GEOMCOLL variants also take the MULTI*
//     types) is 3516 "WKT value is a geometry of unexpected type ...". ST_GeomFromWKB and
//     its variants over a constant WKB (a hex literal, an UNHEX of a constant) read it as
//     create_from_wkb does (readWKB), either byte order per header, and refuse trailing
//     bytes, the same way; a geometry value given to them (the internal format, not a WKB)
//     is 3037;
//   - a constant SRID argument of those readers outside 0 .. 4294967295 is 1690 "SRID value
//     is out of range in '<name>'" (validate_srid_arg);
//   - the constructors LINESTRING / POLYGON / MULTIPOINT / MULTILINESTRING / MULTIPOLYGON
//     take arguments of one type each (points, linestrings, points, linestrings, polygons)
//     and an argument known to be another geometry type is 1210 "Incorrect arguments to
//     <name>"; LINESTRING of one argument is 3037 "linestring"; a POLYGON ring the checker
//     can read whole (a LINESTRING of POINT constants) with fewer than four points or not
//     closed is 3037 "polygon" (Item_func_spatial_collection::val_str);
//   - a function whose resolve_type rejects geometry arguments (reject_geometry_args: the
//     arithmetic and bit operators, the numeric functions, BETWEEN ...) given an argument
//     known to be a geometry is 1210 "Incorrect arguments to <name>" (a comparison of two
//     geometries is accepted, measured, though the catalog's Item_bool_func2 fact says
//     otherwise: the operator's own class does not run it);
//   - a value stored into a spatial column (Field_geom::store_internal) must be the
//     internal format -- a 4-byte SRID and a little-endian WKB, well-formed, of the column's
//     type (any for GEOMETRY; the MULTI* types for GEOMETRYCOLLECTION), no trailing bytes
//     -- or it is 1416 "Cannot get geometry object from data you send to the GEOMETRY
//     field", whatever the sql_mode and under IGNORE too: a number or a character string
//     literal can never be one, a hex / UNHEX constant is read (wellFormedWKB), and a value
//     of a known other geometry type fails on every execution (the statement's error) or
//     on every non-NULL value (a nullable expression: the 1416 violation, keyed "1416" the
//     way 1442 is).
//
// Not read: the SRID a stored value must match when the column declares one (3643), the
// spatial reference system a constant SRID must name (3548), the geometry functions'
// own run-time validity checks (ST_Centroid over a degenerate ring is 3037 at run time,
// which only the data decides), a constructor's or reader's non-constant argument (the
// same checks run per row), and GeoJSON. A reader's or constructor's error over constants
// is reported as the statement's own, though a SELECT over an empty table never evaluates
// it: the constant is wrong whatever the data.

const (
	wkbPoint = 1 + iota
	wkbLinestring
	wkbPolygon
	wkbMultipoint
	wkbMultilinestring
	wkbMultipolygon
	wkbGeometrycollection
)

var geometryNames = map[int]string{
	wkbPoint: "point", wkbLinestring: "linestring", wkbPolygon: "polygon", wkbMultipoint: "multipoint",
	wkbMultilinestring: "multilinestring", wkbMultipolygon: "multipolygon", wkbGeometrycollection: "geometrycollection",
}

// geometryCode is the wkb type of a spatial type name (a column's or a typed's), 0 for
// GEOMETRY (any) and -1 for a type that is not spatial.
func geometryCode(name string) int {
	switch name {
	case "geometry":
		return 0
	case "geomcollection":
		return wkbGeometrycollection
	}
	for code, n := range geometryNames {
		if n == name {
			return code
		}
	}
	return -1
}

func isGeometry(t schema.Type) bool { return geometryCode(t.Name) >= 0 }

// geometryFits says a value of type sub is accepted where super is expected: the same
// type, any under GEOMETRY, a MULTI* under GEOMETRYCOLLECTION (is_subtype_of).
func geometryFits(sub, super int) bool {
	return super == 0 || sub == super || (super == wkbGeometrycollection && (sub == wkbMultipoint || sub == wkbMultilinestring || sub == wkbMultipolygon))
}

func geometryType(code int, nullable bool) typed {
	name := "geometry"
	if code > 0 {
		name = geometryNames[code]
	}
	return known(name, nullable)
}

// ---- WKT (spatial.cc's Gis_read_stream and init_from_wkt) ----

type wktReader struct {
	s string
	i int
}

func (r *wktReader) skipSpace() {
	for r.i < len(r.s) && isSpace(r.s[r.i]) {
		r.i++
	}
}

func (r *wktReader) end() bool {
	r.skipSpace()
	return r.i >= len(r.s)
}

func isVarStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isVar(c byte) bool      { return isVarStart(c) || isDigit(c) }

func (r *wktReader) numericBeginning() bool {
	c := r.s[r.i]
	return isDigit(c) || c == '-' || c == '+' || (c == '.' && r.i+1 < len(r.s) && isDigit(r.s[r.i+1]))
}

// tok is get_next_toc_type: the kind of the next token.
func (r *wktReader) tok() byte {
	r.skipSpace()
	switch {
	case r.i >= len(r.s):
		return 0
	case isVarStart(r.s[r.i]):
		return 'w'
	case r.numericBeginning():
		return 'n'
	case r.s[r.i] == '(', r.s[r.i] == ')', r.s[r.i] == ',':
		return r.s[r.i]
	}
	return '?'
}

func (r *wktReader) word() (string, bool) {
	r.skipSpace()
	if r.i >= len(r.s) || !isVarStart(r.s[r.i]) {
		return "", false
	}
	start := r.i
	for r.i < len(r.s) && isVar(r.s[r.i]) {
		r.i++
	}
	return r.s[start:r.i], true
}

// number is get_next_number: my_strntod's longest float prefix.
func (r *wktReader) number() (float64, bool) {
	r.skipSpace()
	if r.i >= len(r.s) || !r.numericBeginning() {
		return 0, false
	}
	j := r.i
	if r.s[j] == '-' || r.s[j] == '+' {
		j++
	}
	digits := 0
	for j < len(r.s) && isDigit(r.s[j]) {
		j++
		digits++
	}
	if j < len(r.s) && r.s[j] == '.' {
		j++
		for j < len(r.s) && isDigit(r.s[j]) {
			j++
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	if j < len(r.s) && (r.s[j] == 'e' || r.s[j] == 'E') {
		k := j + 1
		if k < len(r.s) && (r.s[k] == '-' || r.s[k] == '+') {
			k++
		}
		if k < len(r.s) && isDigit(r.s[k]) {
			for k < len(r.s) && isDigit(r.s[k]) {
				k++
			}
			j = k
		}
	}
	f, err := strconv.ParseFloat(r.s[r.i:j], 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	r.i = j
	return f, true
}

func (r *wktReader) symbol(c byte) bool {
	r.skipSpace()
	if r.i >= len(r.s) || r.s[r.i] != c {
		return false
	}
	r.i++
	return true
}

// parseWKT reads a WKT the way create_from_wkt does with check_trailing: the geometry's
// wkb type, or 0 when the text is not a well-formed geometry.
func parseWKT(s string) int {
	r := &wktReader{s: s}
	code, ok := r.geometry()
	if !ok || !r.end() {
		return 0
	}
	return code
}

func (r *wktReader) geometry() (int, bool) {
	w, ok := r.word()
	if !ok {
		return 0, false
	}
	code := 0
	switch strings.ToUpper(w) {
	case "POINT":
		code = wkbPoint
	case "LINESTRING":
		code = wkbLinestring
	case "POLYGON":
		code = wkbPolygon
	case "MULTIPOINT":
		code = wkbMultipoint
	case "MULTILINESTRING":
		code = wkbMultilinestring
	case "MULTIPOLYGON":
		code = wkbMultipolygon
	case "GEOMETRYCOLLECTION":
		code = wkbGeometrycollection // GEOMCOLLECTION is a function name, not a WKT word (measured)
	default:
		return 0, false
	}
	return code, r.body(code)
}

func (r *wktReader) point(parens bool) ([2]uint64, bool) {
	if parens && !r.symbol('(') {
		return [2]uint64{}, false
	}
	x, ok := r.number()
	if !ok {
		return [2]uint64{}, false
	}
	y, ok := r.number()
	if !ok {
		return [2]uint64{}, false
	}
	if parens && !r.symbol(')') {
		return [2]uint64{}, false
	}
	return [2]uint64{math.Float64bits(x), math.Float64bits(y)}, true
}

// linestring reads "(x y, x y ...)": at least two points; a polygon ring at least four,
// closed (the first and last points equal, compared as the stored doubles are).
func (r *wktReader) linestring(ring bool) bool {
	if !r.symbol('(') {
		return false
	}
	var first, last [2]uint64
	n := 0
	for {
		p, ok := r.point(false)
		if !ok {
			return false
		}
		if n == 0 {
			first = p
		}
		last = p
		n++
		if !r.symbol(',') {
			break
		}
	}
	if n < 2 || ring && (n < 4 || first != last) {
		return false
	}
	return r.symbol(')')
}

func (r *wktReader) body(code int) bool {
	switch code {
	case wkbPoint:
		_, ok := r.point(true)
		return ok
	case wkbLinestring:
		return r.linestring(false)
	case wkbPolygon:
		if !r.symbol('(') {
			return false
		}
		for {
			if !r.linestring(true) {
				return false
			}
			if !r.symbol(',') {
				break
			}
		}
		return r.symbol(')')
	case wkbMultipoint:
		if !r.symbol('(') {
			return false
		}
		parens := r.tok() == '('
		for {
			if _, ok := r.point(parens); !ok {
				return false
			}
			if !r.symbol(',') {
				break
			}
		}
		return r.symbol(')')
	case wkbMultilinestring, wkbMultipolygon:
		if !r.symbol('(') {
			return false
		}
		for {
			var ok bool
			if code == wkbMultilinestring {
				ok = r.linestring(false)
			} else {
				ok = r.body(wkbPolygon)
			}
			if !ok {
				return false
			}
			if !r.symbol(',') {
				break
			}
		}
		return r.symbol(')')
	case wkbGeometrycollection:
		if r.tok() == 'w' {
			w, ok := r.word()
			return ok && strings.EqualFold(w, "EMPTY")
		}
		if !r.symbol('(') {
			return false
		}
		n := 0
		for {
			if n == 0 && r.tok() == ')' {
				break
			}
			if _, ok := r.geometry(); !ok {
				return false
			}
			n++
			if !r.symbol(',') {
				break
			}
		}
		return r.symbol(')')
	}
	return false
}

// ---- WKB (spatial.cc's wkb_scanner / Geometry_well_formed_checker and init_from_wkb) ----

type wkbReader struct {
	b []byte
	// ndrOnly: every header must be little-endian (a stored value); closed: polygon rings
	// must be closed (create_from_wkb's readers; the stored value's checker does not look)
	ndrOnly, closed bool
}

func (r *wkbReader) uint32At(pos int, le bool) uint32 {
	if le {
		return binary.LittleEndian.Uint32(r.b[pos:])
	}
	return binary.BigEndian.Uint32(r.b[pos:])
}

// element reads one geometry at pos: with a header (byte order, type) when hasHdr, else of
// type code in byte order le; outer is the type it sits in (0 at the top, or an expected
// type). It returns the position after it and the type read, or ok false.
func (r *wkbReader) element(pos int, hasHdr bool, code int, le bool, outer int, depth int) (int, int, bool) {
	if depth > 64 {
		return 0, 0, false
	}
	if hasHdr {
		if pos+5 > len(r.b) {
			return 0, 0, false
		}
		switch r.b[pos] {
		case 0:
			le = false
		case 1:
			le = true
		default:
			return 0, 0, false
		}
		if r.ndrOnly && !le {
			return 0, 0, false
		}
		code = int(r.uint32At(pos+1, le))
		pos += 5
		if code < wkbPoint || code > wkbGeometrycollection {
			return 0, 0, false
		}
		// what may sit where (R3, R4): the expected type at the top, any in a collection,
		// the MULTI* types' own components
		switch {
		case depth == 0:
			if outer != 0 && !geometryFits(code, outer) {
				return 0, 0, false
			}
		case outer == wkbGeometrycollection:
		case outer == wkbMultipoint && code == wkbPoint, outer == wkbMultilinestring && code == wkbLinestring, outer == wkbMultipolygon && code == wkbPolygon:
		default:
			return 0, 0, false
		}
	}
	switch code {
	case wkbPoint:
		if pos+16 > len(r.b) {
			return 0, 0, false
		}
		return pos + 16, code, true
	case wkbLinestring:
		if pos+4 > len(r.b) {
			return 0, 0, false
		}
		n := int(r.uint32At(pos, le))
		pos += 4
		ring := outer == wkbPolygon
		if n < 2 || ring && n < 4 || n > (len(r.b)-pos)/16 {
			return 0, 0, false
		}
		if ring && r.closed && string(r.b[pos:pos+16]) != string(r.b[pos+(n-1)*16:pos+n*16]) {
			return 0, 0, false
		}
		return pos + n*16, code, true
	case wkbPolygon, wkbMultipoint, wkbMultilinestring, wkbMultipolygon, wkbGeometrycollection:
		if pos+4 > len(r.b) {
			return 0, 0, false
		}
		n := int(r.uint32At(pos, le))
		pos += 4
		if n == 0 && (code == wkbPolygon || r.ndrOnly && code != wkbGeometrycollection) {
			// a polygon needs a ring (init_from_wkb, R7); a stored value's MULTI* needs an
			// element too (R8), which the readers do not require
			return 0, 0, false
		}
		compHdr := code != wkbPolygon
		for i := 0; i < n; i++ {
			var ok bool
			pos, _, ok = r.element(pos, compHdr, wkbLinestring, le, code, depth+1)
			if !ok {
				return 0, 0, false
			}
		}
		return pos, code, true
	}
	return 0, 0, false
}

// readWKB reads a WKB (a header and its data, either byte order) as create_from_wkb does:
// the geometry's type, or 0 when it is malformed or has trailing bytes.
func readWKB(b []byte) int {
	r := &wkbReader{b: b, closed: true}
	end, code, ok := r.element(0, true, 0, true, 0, 0)
	if !ok || end != len(b) {
		return 0
	}
	return code
}

// wellFormedWKB judges a value in the internal format (a 4-byte SRID, then a little-endian
// WKB) for a column of the given type code (0: GEOMETRY), as Field_geom::store_internal
// does: is_well_formed with the column's type, no trailing bytes.
func wellFormedWKB(b []byte, column int) bool {
	if len(b) < 13 {
		return false
	}
	r := &wkbReader{b: b[4:], ndrOnly: true}
	end, _, ok := r.element(0, true, 0, true, column, 0)
	return ok && end == len(r.b)
}

// ---- the readers' and constructors' typing ----

// fromTextType is the geometry type a ST_*FromText / ST_*FromWKB variant returns (0: any),
// by the function's name; ok false for another function.
func fromTextType(name string) (code int, ok bool) {
	up := strings.ToUpper(name)
	if !strings.HasPrefix(up, "ST_") {
		return 0, false
	}
	body := strings.TrimPrefix(up, "ST_")
	suffix := ""
	for _, s := range []string{"FROMTEXT", "FROMTXT", "FROMWKB"} {
		if strings.HasSuffix(body, s) {
			suffix = s
			body = strings.TrimSuffix(body, s)
		}
	}
	if suffix == "" {
		return 0, false
	}
	switch body {
	case "GEOM", "GEOMETRY":
		return 0, true
	case "POINT":
		return wkbPoint, true
	case "LINE", "LINESTRING":
		return wkbLinestring, true
	case "POLY", "POLYGON":
		return wkbPolygon, true
	case "MPOINT", "MULTIPOINT":
		return wkbMultipoint, true
	case "MLINE", "MULTILINESTRING":
		return wkbMultilinestring, true
	case "MPOLY", "MULTIPOLYGON":
		return wkbMultipolygon, true
	case "GEOMCOLL", "GEOMETRYCOLLECTION":
		return wkbGeometrycollection, true
	}
	return 0, false
}

// hexConstant is the bytes of a hex literal (0x..., x'...') or of UNHEX over a string
// literal; ok false for anything else.
func hexConstant(v mysqlast.Value) ([]byte, bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil, false
	}
	text := ""
	switch n.Class {
	case "Item_hex_string", "PTI_literal_underscore_charset_hex_num":
		text = str(n.Arg("literal"))
	case "PTI_function_call_generic_ident_sys":
		name, args := funcCallParts(n)
		if !strings.EqualFold(name, "UNHEX") || len(args) != 1 {
			return nil, false
		}
		arg, ok := args[0].(*mysqlast.Node)
		if !ok {
			return nil, false
		}
		text, ok = stringLiteral(arg)
		if !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	if len(text)%2 == 1 {
		text = "0" + text
	}
	b, err := hex.DecodeString(text)
	if err != nil {
		return nil, false
	}
	return b, true
}

// geometryReader types ST_GeomFromText / ST_GeomFromWKB and their typed variants: a
// constant text or WKB is read and judged (3037), a constant SRID bounded (1690), and the
// result carries the type it is known to have.
func (a *analyzer) geometryReader(name string, args []mysqlast.Value, ts []typed, at int) (typed, error) {
	allowed, _ := fromTextType(name)
	fname := strings.ToLower(name)
	pos := a.ph.Back(at)
	invalid := &Error{Message: fmt.Sprintf("Invalid GIS data provided to function %s.", fname), Code: 3037, Position: pos}
	if len(args) >= 2 {
		if num, ok := numericLiteral(argNode(args[1])); ok {
			if num.rat.Sign() < 0 || num.rat.Cmp(new(big.Rat).SetUint64(math.MaxUint32)) > 0 {
				return unknown, &Error{Message: fmt.Sprintf("SRID value is out of range in '%s'", fname), Code: 1690, Position: pos}
			}
		}
	}
	unexpected := func(kind string, code int) *Error {
		return &Error{Message: fmt.Sprintf("%s value is a geometry of unexpected type %s in %s.", kind, strings.ToUpper(geometryNames[code]), fname), Code: 3516, Position: pos}
	}
	result := allowed
	if len(args) >= 1 {
		if strings.HasSuffix(strings.ToUpper(name), "WKB") {
			if b, ok := hexConstant(args[0]); ok {
				code := readWKB(b)
				if code == 0 {
					return unknown, invalid
				}
				if !geometryFits(code, allowed) {
					return unknown, unexpected("WKB", code)
				}
				result = code
			} else if len(ts) > 0 && ts[0].known && isGeometry(ts[0].typ) {
				// a geometry value is the internal format, an SRID before the WKB: never a
				// WKB itself (measured: 3037 on 8.4, unlike what older documentation says)
				return unknown, invalid
			}
		} else if lit, ok := stringLiteral(argNode(args[0])); ok {
			code := parseWKT(lit)
			if code == 0 {
				return unknown, invalid
			}
			if !geometryFits(code, allowed) {
				return unknown, unexpected("WKT", code)
			}
			result = code
		}
	}
	if allowed == wkbGeometrycollection && result == allowed && !(len(args) >= 1 && isConstantArg(args[0])) {
		result = 0 // a MULTI* passes the GEOMCOLL variants too
	}
	return geometryType(result, true), nil
}

func argNode(v mysqlast.Value) *mysqlast.Node {
	n, _ := v.(*mysqlast.Node)
	if n == nil {
		return &mysqlast.Node{}
	}
	return n
}

func isConstantArg(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	if _, ok := stringLiteral(n); ok {
		return true
	}
	_, ok = hexConstant(n)
	return ok
}

// spatialCollection types LINESTRING(...) / POLYGON(...) / MULTIPOINT(...) /
// MULTILINESTRING(...) / MULTIPOLYGON(...) / GEOMETRYCOLLECTION(...): the arguments must
// be geometries of the constructor's component type (1210), LINESTRING needs two (3037),
// and a POLYGON ring the checker can read whole must have four closed points (3037).
func (a *analyzer) spatialCollection(sc scope, n *mysqlast.Node, where string) (typed, error) {
	list, _ := n.Arg("list").(mysqlast.List)
	ts, err := a.exprs(sc, list, where)
	if err != nil {
		return unknown, err
	}
	code := wkbCode(str(n.Arg("ct")))
	item := wkbCode(str(n.Arg("it")))
	fname := geometryNames[code]
	if code == wkbGeometrycollection {
		fname = "geomcollection"
	}
	pos := a.ph.Back(n.Start)
	invalid := &Error{Message: fmt.Sprintf("Invalid GIS data provided to function %s.", fname), Code: 3037, Position: pos}
	// val_str's checks run per row: a constant argument fails on every execution the
	// statement evaluates, a column's value is the data's (measured: polygon(t.a) over an
	// empty table runs), so only constants are judged
	if code != wkbGeometrycollection {
		for i, t := range ts {
			if t.known && isGeometry(t.typ) && constantGeometry(list[i]) {
				if c := geometryCode(t.typ.Name); c > 0 && c != item {
					return unknown, &Error{Message: fmt.Sprintf("Incorrect arguments to %s", fname), Code: 1210, Position: pos}
				}
			}
		}
	}
	allConstant := true
	for _, e := range list {
		if !constantGeometry(e) {
			allConstant = false
		}
	}
	switch code {
	case wkbLinestring:
		if len(list) < 2 && allConstant {
			return unknown, invalid
		}
	case wkbPolygon:
		for _, ring := range list {
			if pts, ok := constantRing(ring); ok && (len(pts) < 4 || pts[0] != pts[len(pts)-1]) {
				return unknown, invalid
			}
		}
	}
	return geometryType(code, anyNullable(ts)), nil
}

// constantRing reads a LINESTRING(POINT(x, y), ...) of numeric literals as its points'
// stored doubles; ok false when any part is not such a constant.
func constantRing(v mysqlast.Value) ([][2]uint64, bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "Item_func_spatial_collection" || wkbCode(str(n.Arg("ct"))) != wkbLinestring {
		return nil, false
	}
	list, _ := n.Arg("list").(mysqlast.List)
	var out [][2]uint64
	for _, e := range list {
		p, ok := e.(*mysqlast.Node)
		if !ok || p.Class != "Item_func_point" {
			return nil, false
		}
		var xy [2]uint64
		for i, arg := range []mysqlast.Value{p.Arg("a"), p.Arg("b")} {
			num, ok := numericLiteral(argNode(arg))
			if !ok {
				return nil, false
			}
			f, _ := num.rat.Float64()
			xy[i] = math.Float64bits(f)
		}
		out = append(out, xy)
	}
	return out, true
}

// wkbCode reads the grammar's Geometry::wkb_* constant.
func wkbCode(s string) int {
	s = strings.TrimPrefix(s, "Geometry::wkb_")
	for code, name := range geometryNames {
		if name == s {
			return code
		}
	}
	return 0
}

// rejectsGeometry is the reject_geometry_args check a class's resolve_type runs: an
// argument known to be a geometry is 1210 "Incorrect arguments to <name>".
func (a *analyzer) rejectsGeometry(class, name string, ts []typed, at int) *Error {
	found := false
	for _, f := range classFacts(class) {
		if strings.HasPrefix(f, "reject_geometry_args(") {
			found = true
		}
	}
	if !found || !geometryOperand(ts) {
		return nil
	}
	return a.geometryRejected(name, at)
}

// geometryOperand: an argument is known to be a geometry.
func geometryOperand(ts []typed) bool {
	for _, t := range ts {
		if t.known && isGeometry(t.typ) {
			return true
		}
	}
	return false
}

func (a *analyzer) geometryRejected(name string, at int) *Error {
	return &Error{Message: fmt.Sprintf("Incorrect arguments to %s", name), Code: 1210, Position: a.ph.Back(at)}
}

// constantGeometry: the expression is a geometry built from constants alone (a
// constructor over literals, a reader over a constant text or WKB), so it is never NULL.
func constantGeometry(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "Item_func_point":
		_, okA := numericLiteral(argNode(n.Arg("a")))
		_, okB := numericLiteral(argNode(n.Arg("b")))
		return okA && okB
	case "Item_func_spatial_collection":
		list, _ := n.Arg("list").(mysqlast.List)
		for _, e := range list {
			if !constantGeometry(e) {
				return false
			}
		}
		return true
	case "PTI_function_call_generic_ident_sys":
		name, args := funcCallParts(n)
		if _, ok := fromTextType(name); !ok || len(args) == 0 || !isConstantArg(args[0]) {
			return false
		}
		for _, arg := range args[1:] {
			if _, ok := numericLiteral(argNode(arg)); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// operatorName is func_name() of the grammar-built operator classes, for a message.
func operatorName(class string) string {
	switch class {
	case "Item_func_plus":
		return "+"
	case "Item_func_minus", "Item_func_neg":
		return "-"
	case "Item_func_mul":
		return "*"
	case "Item_func_div":
		return "/"
	case "Item_func_int_div":
		return "DIV"
	case "Item_func_mod":
		return "%"
	case "Item_func_bit_or":
		return "|"
	case "Item_func_bit_and":
		return "&"
	case "Item_func_bit_xor":
		return "^"
	case "Item_func_shift_left":
		return "<<"
	case "Item_func_shift_right":
		return ">>"
	case "Item_func_bit_neg":
		return "~"
	case "Item_func_between":
		return "between"
	}
	return strings.ToLower(strings.TrimPrefix(class, "Item_func_"))
}

// geometryStore judges a value stored into a spatial column (Field_geom::store_internal):
// nil when nothing is known against it, an *Error when every execution fails, or a
// Violation when every non-NULL value does.
func (a *analyzer) geometryStore(table *schema.Table, col *schema.Column, v mysqlast.Value, t typed) (*Error, *Violation) {
	column := geometryCode(col.Type.Name)
	if column < 0 {
		return nil, nil
	}
	fail := &Error{Message: "Cannot get geometry object from data you send to the GEOMETRY field", Code: 1416, Position: -1}
	if n, ok := v.(*mysqlast.Node); ok {
		fail.Position = a.ph.Back(n.Start)
		if b, ok := hexConstant(v); ok {
			if !wellFormedWKB(b, column) {
				return fail, nil
			}
			return nil, nil
		}
		if _, ok := numericLiteral(n); ok {
			return fail, nil
		}
		if _, ok := stringLiteral(n); ok {
			return fail, nil
		}
	}
	if t.known && isGeometry(t.typ) {
		if c := geometryCode(t.typ.Name); c > 0 && !geometryFits(c, column) {
			if !t.nullable || constantGeometry(v) {
				return fail, nil
			}
			return nil, &Violation{Code: 1416, Constraint: "1416", Table: table.Name, Columns: []string{col.Name}, SQLState: "22003"}
		}
	}
	return nil, nil
}
