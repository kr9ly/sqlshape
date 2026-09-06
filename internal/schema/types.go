package schema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/internal/catalog"
)

// FirstUserOID is where synthetic OIDs for user-defined objects start
// (mirrors PG's FirstNormalObjectId). They are stable within one Schema only.
const FirstUserOID catalog.OID = 16384

// TypeRef is a resolved type: an OID in the Types universe plus a typmod.
type TypeRef struct {
	OID    catalog.OID
	Typmod int32
}

// Types is the type universe: the bootstrap catalog plus user types
// (enums, domains, composites, relation row types) and their array types.
type Types struct {
	cat    *catalog.Catalog
	user   []*catalog.Type
	byOID  map[catalog.OID]*catalog.Type
	byName map[string]*catalog.Type // "schema.name"
	// extras carried by user types
	Enums   map[catalog.OID][]string // enum oid → labels in sort order
	Domains map[catalog.OID]*Domain  // domain oid → details
	Schemas map[catalog.OID]string   // user type oid → schema name
	// Collations declared with CREATE COLLATION (name → true).
	Collations map[string]bool
	// Ranges are user range types (CREATE TYPE ... AS RANGE) with their multirange.
	Ranges     []catalog.Range
	nextOID    catalog.OID
	searchPath []string
}

// Domain is the CHECK / NOT NULL side of a domain type; the base type lives in catalog.Type.BaseType.
type Domain struct {
	NotNull bool
	Checks  []*Constraint // Kind Check, named <domain>_check like PG
}

func newTypes(cat *catalog.Catalog) *Types {
	return &Types{
		cat: cat, byOID: map[catalog.OID]*catalog.Type{}, byName: map[string]*catalog.Type{},
		Enums: map[catalog.OID][]string{}, Domains: map[catalog.OID]*Domain{}, Schemas: map[catalog.OID]string{},
		Collations: map[string]bool{},
		nextOID:    FirstUserOID,
	}
}

// ByOID finds a type in the catalog or among user types.
func (ts *Types) ByOID(oid catalog.OID) *catalog.Type {
	if t := ts.byOID[oid]; t != nil {
		return t
	}
	return ts.cat.TypeByOID(oid)
}

// Lookup resolves a possibly qualified type name against the search path (public, pg_catalog).
func (ts *Types) Lookup(schema, name string) *catalog.Type {
	if schema == "" {
		path := ts.searchPath
		if len(path) == 0 {
			path = []string{"public"}
		}
		for _, p := range path {
			if t := ts.byName[p+"."+name]; t != nil {
				return t
			}
		}
		// a catalog type: pg_catalog's, or one living in a schema on the path (an
		// extension's); information_schema's domains need qualifying
		if t := ts.cat.TypeByName(name); t != nil && (t.Schema == "" || slices.Contains(path, t.Schema)) {
			return t
		}
		return nil
	}
	if schema == "pg_catalog" {
		if t := ts.cat.TypeByName(name); t != nil && t.Schema == "" {
			return t
		}
		return nil
	}
	if t := ts.byName[schema+"."+name]; t != nil {
		return t
	}
	// an extension's type lives in the schema the extension was created in
	if t := ts.cat.TypeByName(name); t != nil && t.Schema == schema {
		return t
	}
	return nil
}

// addUser registers a user type and its array type; returns the base type.
func (ts *Types) addUser(schema, name string, kind, category byte, base catalog.OID, relid catalog.OID) *catalog.Type {
	t := &catalog.Type{OID: ts.nextOID, Name: name, Kind: kind, Category: category, Len: -1}
	arr := &catalog.Type{OID: ts.nextOID + 1, Name: "_" + name, Kind: 'b', Category: 'A', Len: -1, Elem: t.OID}
	ts.nextOID += 2
	t.Array = arr.OID
	t.BaseType = base
	t.RelID = relid
	if kind == 'd' && base != 0 {
		if bt := ts.ByOID(base); bt != nil {
			t.Category = bt.Category
			t.Len = bt.Len
			t.ByVal = bt.ByVal
		}
	}
	for _, x := range []*catalog.Type{t, arr} {
		ts.user = append(ts.user, x)
		ts.byOID[x.OID] = x
		ts.byName[schema+"."+x.Name] = x
		ts.Schemas[x.OID] = schema
	}
	return t
}

// ArrayOf returns the array type over elem, or 0 if none exists.
func (ts *Types) ArrayOf(elem catalog.OID) catalog.OID {
	if t := ts.ByOID(elem); t != nil {
		return t.Array
	}
	return 0
}

// Format renders a TypeRef the way PostgreSQL's format_type() does
// (the oracle's golden files use this spelling).
func (ts *Types) Format(r TypeRef) string {
	t := ts.ByOID(r.OID)
	if t == nil {
		return fmt.Sprintf("???(%d)", r.OID)
	}
	if t.Elem != 0 && strings.HasPrefix(t.Name, "_") {
		// format_type prints arrays as elem[] with the typmod applied to the element
		return ts.Format(TypeRef{t.Elem, r.Typmod}) + "[]"
	}
	s, ok := ts.Schemas[t.OID]
	if !ok && t.Schema != "" {
		s, ok = t.Schema, true // an extension's type
	}
	if ok {
		// format_type qualifies a type only when its schema is not on the search path
		if s == "public" || slices.Contains(ts.searchPath, s) {
			return quoteIdent(t.Name)
		}
		return quoteIdent(s) + "." + quoteIdent(t.Name)
	}
	m := r.Typmod
	switch t.OID {
	case catalog.Bool:
		return "boolean"
	case catalog.Int2:
		return "smallint"
	case catalog.Int4:
		return "integer"
	case catalog.Int8:
		return "bigint"
	case catalog.Float4:
		return "real"
	case catalog.Float8:
		return "double precision"
	case catalog.Char:
		return `"char"`
	case catalog.BPChar:
		if m >= 4 {
			return fmt.Sprintf("character(%d)", m-4)
		}
		return "bpchar" // format_type with an explicit typmod of -1 (as Describe reports) prints the raw name
	case catalog.Varchar:
		if m >= 4 {
			return fmt.Sprintf("character varying(%d)", m-4)
		}
		return "character varying"
	case catalog.Numeric:
		if m >= 4 {
			return fmt.Sprintf("numeric(%d,%d)", (m-4)>>16&0xffff, int16((m-4)&0xffff))
		}
		return "numeric"
	case catalog.Timestamp:
		return "timestamp" + precision(m) + " without time zone"
	case catalog.TimestampTZ:
		return "timestamp" + precision(m) + " with time zone"
	case catalog.Time:
		return "time" + precision(m) + " without time zone"
	case catalog.TimeTZ:
		return "time" + precision(m) + " with time zone"
	case catalog.Interval:
		return formatInterval(m)
	}
	switch t.Name {
	case "bit":
		if m >= 1 {
			return fmt.Sprintf("bit(%d)", m)
		}
		return `"bit"` // same TYPEMOD_GIVEN rule: bare bit is quoted so it is not read as bit(1)
	case "varbit":
		if m >= 1 {
			return fmt.Sprintf("bit varying(%d)", m)
		}
		return "bit varying"
	}
	return quoteIdent(t.Name)
}

func precision(m int32) string {
	if m >= 0 {
		return "(" + strconv.Itoa(int(m)) + ")"
	}
	return ""
}

// quoteIdent double-quotes an identifier when PG would (non-lowercase, non-simple, or keyword-like).
func quoteIdent(s string) string {
	simple := s != ""
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			simple = false
			break
		}
	}
	if simple {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// typmodFor computes the stored typmod from CREATE-time modifiers, per type (typmodin rules).
func typmodFor(t *catalog.Type, mods []int64) (int32, error) {
	if len(mods) == 0 {
		return -1, nil
	}
	switch t.OID {
	case catalog.BPChar, catalog.Varchar:
		if len(mods) != 1 || mods[0] < 1 {
			return 0, fmt.Errorf("invalid length modifier for %s", t.Name)
		}
		return int32(mods[0]) + 4, nil
	case catalog.Numeric:
		p := mods[0]
		var s int64
		if len(mods) > 1 {
			s = mods[1]
		}
		if p < 1 || p > 1000 {
			return 0, fmt.Errorf("NUMERIC precision %d must be between 1 and 1000", p)
		}
		return int32((p<<16)|(s&0xffff)) + 4, nil
	case catalog.Timestamp, catalog.TimestampTZ, catalog.Time, catalog.TimeTZ:
		return int32(mods[0]), nil
	case catalog.Interval:
		return intervalTypmod(mods), nil
	}
	switch t.Name {
	case "bit", "varbit":
		return int32(mods[0]), nil
	}
	return -1, fmt.Errorf("type modifier is not allowed for type %q", t.Name)
}

// BaseOf follows domain types down to the underlying non-domain type, keeping the
// typmod of the outermost declared one. PG reports result columns on the wire
// (RowDescription / Describe) with the base type, so the oracle never shows a domain.
func (ts *Types) BaseOf(r TypeRef) TypeRef {
	for {
		t := ts.ByOID(r.OID)
		if t == nil || t.Kind != 'd' {
			return r
		}
		typmod := r.Typmod
		if typmod == -1 {
			typmod = t.Typmod
		}
		r = TypeRef{OID: t.BaseType, Typmod: typmod}
	}
}

// addRange registers CREATE TYPE name AS RANGE (subtype = sub) and its multirange
// (<name>multirange when name ends in "range", <name>_multirange otherwise, as PG).
func (ts *Types) addRange(schema, name string, sub catalog.OID, mname string) *catalog.Type {
	r := ts.addUser(schema, name, 'r', 'R', 0, 0)
	if mname == "" {
		mname = name + "_multirange"
		if strings.HasSuffix(name, "range") {
			mname = strings.TrimSuffix(name, "range") + "multirange"
		}
	}
	// a multirange_type_name that an array type already holds pushes the array type
	// aside (moveArrayTypeName: another leading underscore until the name is free)
	if old := ts.byName[schema+"."+mname]; old != nil && old.Kind == 'b' && old.Category == 'A' && old.Elem != 0 && ts.Schemas[old.OID] == schema {
		free := "_" + mname
		for ts.byName[schema+"."+free] != nil {
			free = "_" + free
		}
		delete(ts.byName, schema+"."+mname)
		old.Name = free
		ts.byName[schema+"."+free] = old
	}
	m := ts.addUser(schema, mname, 'm', 'R', 0, 0)
	ts.Ranges = append(ts.Ranges, catalog.Range{OID: r.OID, Subtype: sub, Multi: m.OID})
	return r
}

// RangeOf is the pg_range row of a range type, user-defined or catalog.
func (ts *Types) RangeOf(rng catalog.OID) *catalog.Range {
	for i := range ts.Ranges {
		if ts.Ranges[i].OID == rng {
			return &ts.Ranges[i]
		}
	}
	return ts.cat.RangeOf(rng)
}

// RangeOfMulti is the pg_range row of a multirange type.
func (ts *Types) RangeOfMulti(multi catalog.OID) *catalog.Range {
	for i := range ts.Ranges {
		if ts.Ranges[i].Multi == multi {
			return &ts.Ranges[i]
		}
	}
	return ts.cat.RangeOfMulti(multi)
}

// RangeForSubtype is the (first) range type over sub.
func (ts *Types) RangeForSubtype(sub catalog.OID) *catalog.Range {
	if r := ts.cat.RangeForSubtype(sub); r != nil {
		return r
	}
	for i := range ts.Ranges {
		if ts.Ranges[i].Subtype == sub {
			return &ts.Ranges[i]
		}
	}
	return nil
}

// renameUser renames a user type (and its array type).
func (ts *Types) renameUser(oid catalog.OID, name string) {
	t := ts.byOID[oid]
	if t == nil {
		return
	}
	schema := ts.Schemas[oid]
	delete(ts.byName, schema+"."+t.Name)
	ts.claimName(schema, name)
	t.Name = name
	ts.byName[schema+"."+name] = t
	if arr := ts.byOID[t.Array]; arr != nil {
		delete(ts.byName, schema+"."+arr.Name)
		ts.claimName(schema, "_"+name)
		arr.Name = "_" + name
		ts.byName[schema+"."+arr.Name] = arr
	}
}

// claimName frees schema.name for a type about to take it: an array type sitting there
// moves to "_" + name (makeArrayTypeName / moveArrayTypeName), as many times as needed.
func (ts *Types) claimName(schema, name string) {
	other := ts.byName[schema+"."+name]
	if other == nil || other.Category != 'A' || ts.Schemas[other.OID] != schema {
		return
	}
	delete(ts.byName, schema+"."+name)
	ts.claimName(schema, "_"+name)
	other.Name = "_" + name
	ts.byName[schema+"."+other.Name] = other
}

// moveUser puts a user type (and its array type) in another schema; catalog types stay.
func (ts *Types) moveUser(oid catalog.OID, schema string) {
	t := ts.byOID[oid]
	if t == nil {
		return
	}
	for _, x := range []*catalog.Type{t, ts.byOID[t.Array]} {
		if x == nil {
			continue
		}
		delete(ts.byName, ts.Schemas[x.OID]+"."+x.Name)
		ts.Schemas[x.OID] = schema
		ts.byName[schema+"."+x.Name] = x
	}
}

// removeUser forgets a user type and its array type.
func (ts *Types) removeUser(oid catalog.OID) {
	t := ts.byOID[oid]
	if t == nil {
		return
	}
	schema := ts.Schemas[oid]
	for _, x := range []*catalog.Type{t, ts.byOID[t.Array]} {
		if x == nil {
			continue
		}
		delete(ts.byName, schema+"."+x.Name)
		delete(ts.byOID, x.OID)
		delete(ts.Schemas, x.OID)
	}
	delete(ts.Enums, oid)
	delete(ts.Domains, oid)
	var kept []*catalog.Type
	for _, u := range ts.user {
		if u.OID != oid && u.OID != t.Array {
			kept = append(kept, u)
		}
	}
	ts.user = kept
	var ranges []catalog.Range
	for _, r := range ts.Ranges {
		if r.OID != oid {
			ranges = append(ranges, r)
		}
	}
	ts.Ranges = ranges
}

// Interval typmods (utils/datetime.h): the field mask in the high half, the seconds
// precision in the low half (0xFFFF = none).
const (
	intervalMonth  = 1 << 1
	intervalYear   = 1 << 2
	intervalDay    = 1 << 3
	intervalHour   = 1 << 10
	intervalMinute = 1 << 11
	intervalSecond = 1 << 12
	intervalFull   = 0x7FFF
)

var intervalFields = map[int32]string{
	intervalYear: " year", intervalMonth: " month", intervalDay: " day", intervalHour: " hour", intervalMinute: " minute", intervalSecond: " second",
	intervalYear | intervalMonth: " year to month", intervalDay | intervalHour: " day to hour",
	intervalDay | intervalHour | intervalMinute: " day to minute", intervalDay | intervalHour | intervalMinute | intervalSecond: " day to second",
	intervalHour | intervalMinute: " hour to minute", intervalHour | intervalMinute | intervalSecond: " hour to second",
	intervalMinute | intervalSecond: " minute to second",
}

func intervalTypmod(mods []int64) int32 {
	rng := int64(intervalFull)
	prec := int64(0xFFFF)
	if len(mods) > 0 {
		rng = mods[0]
	}
	if len(mods) > 1 {
		prec = mods[1]
	}
	return int32((rng&0x7FFF)<<16 | (prec & 0xFFFF))
}

// formatInterval spells an interval typmod the way intervaltypmodout does.
func formatInterval(m int32) string {
	if m < 0 {
		return "interval"
	}
	fields := (m >> 16) & 0x7FFF
	prec := m & 0xFFFF
	out := "interval"
	if fields != intervalFull {
		out += intervalFields[fields]
	}
	if prec != 0xFFFF {
		out += "(" + strconv.Itoa(int(prec)) + ")"
	}
	return out
}

// removeSchema forgets every user type living in schema (DROP SCHEMA ... CASCADE).
func (ts *Types) removeSchema(schema string) {
	var oids []catalog.OID
	for _, t := range ts.user {
		if ts.Schemas[t.OID] == schema {
			oids = append(oids, t.OID)
		}
	}
	for _, oid := range oids {
		ts.removeUser(oid)
	}
}
