package schema

import (
	"fmt"
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
	nextOID catalog.OID
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
		nextOID: FirstUserOID,
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
		if t := ts.byName["public."+name]; t != nil {
			return t
		}
		return ts.cat.TypeByName(name)
	}
	if schema == "pg_catalog" {
		return ts.cat.TypeByName(name)
	}
	return ts.byName[schema+"."+name]
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
	if s, ok := ts.Schemas[t.OID]; ok {
		if s == "public" {
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
		return "interval" // typmod spelling (fields + precision) not reproduced yet
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
