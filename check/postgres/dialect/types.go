// Package dialect is the PostgreSQL adapter to the checker's dialect-neutral contract
// (x/dialect): it spells the analyzer's types, results and findings in the shape every
// dialect shares, so that the frontend reads PostgreSQL the way it reads MySQL. The Go
// type table lives here, as data: what pgx v5 scans a type into and encodes it from.
package dialect

import (
	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

const pgtype = "github.com/jackc/pgx/v5/pgtype."

// Traits of pgx: any Go string encodes as a parameter of any type (text format), and
// every pgtype value carries Valid.
var Traits = dialect.Traits{TextParams: true, NullWrappers: []string{"jackc/pgx/v5/pgtype"}}

func fits(gos ...string) []dialect.GoFit {
	out := make([]dialect.GoFit, len(gos))
	for i, g := range gos {
		out[i] = dialect.GoFit{Go: g}
	}
	return out
}

func lossy(g, why string) dialect.GoFit { return dialect.GoFit{Go: g, Lossy: why} }

// TypeOf spells a PostgreSQL type as the contract's Type: domains keep their name over
// their Base, arrays and ranges carry their Elem, composites their Fields. Result and
// Param are pgx's table, best first (the first is what a struct written from the SQL
// uses; an option the program already imports is preferred by the checker).
func TypeOf(s *schema.Schema, ref schema.TypeRef) dialect.Type {
	return typeOf(s, ref, 0)
}

func typeOf(s *schema.Schema, ref schema.TypeRef, depth int) dialect.Type {
	pt := s.Types.ByOID(ref.OID)
	if pt == nil || depth > 8 {
		return dialect.Type{Name: s.Types.Format(ref)}
	}
	t := dialect.Type{Name: s.Types.Format(ref)}
	if pt.Schema != "" && pt.Schema != "pg_catalog" || pt.Kind == 'e' || pt.Kind == 'd' || pt.Kind == 'c' || pt.Kind == 'r' || pt.Kind == 'm' {
		t.Named = qualified(s, pt)
	}
	switch pt.Kind {
	case 'd':
		base := typeOf(s, schema.TypeRef{OID: pt.BaseType, Typmod: -1}, depth+1)
		t.Kind, t.Base = dialect.Domain, &base
		t.Result, t.Param = base.Result, base.Param
		t.Elem, t.Fields, t.Labels = base.Elem, base.Fields, base.Labels
		return t
	case 'e':
		t.Kind, t.Labels = dialect.Enum, s.Types.Enums[pt.OID]
		t.Result, t.Param = fits("string"), fits("string")
		return t
	case 'c':
		t.Kind = dialect.Composite
		t.Fields = rowFields(s, pt.OID, depth)
		t.Result, t.Param = fits("struct"), fits("struct")
		return t
	case 'r':
		if rng := s.Types.RangeOf(pt.OID); rng != nil {
			sub := typeOf(s, schema.TypeRef{OID: rng.Subtype, Typmod: -1}, depth+1)
			t.Kind, t.Elem = dialect.Range, &sub
			t.Result, t.Param = fits(pgtype+"Range[$elem]"), fits(pgtype+"Range[$elem]")
		}
		return t
	case 'm':
		if rng := s.Types.RangeOfMulti(pt.OID); rng != nil {
			r := typeOf(s, schema.TypeRef{OID: rng.OID, Typmod: -1}, depth+1)
			t.Kind, t.Elem = dialect.Multirange, &r
			t.Result, t.Param = fits(pgtype+"Multirange[$elem]"), fits(pgtype+"Multirange[$elem]")
		}
		return t
	}
	if pt.IsArray() {
		elem := typeOf(s, schema.TypeRef{OID: pt.Elem, Typmod: -1}, depth+1)
		t.Kind, t.Elem = dialect.Array, &elem
		t.Result, t.Param = fits("[]$elem"), fits("[]$elem")
		if elem.Unknown() {
			t.Result, t.Param = nil, nil
		}
		return t
	}
	t.Result, t.Param = scalarFits(pt)
	if pt.OID == catalog.Record {
		t.Kind = dialect.Record
	}
	return t
}

// NamedOf is the canonical name of a type (dialect.Type.Named), what a `// sqlshape: type
// X` declaration resolves to.
func NamedOf(s *schema.Schema, oid catalog.OID) string {
	pt := s.Types.ByOID(oid)
	if pt == nil {
		return ""
	}
	return qualified(s, pt)
}

// qualified is the name a `// sqlshape: type X` declaration uses for a user type.
func qualified(s *schema.Schema, pt *catalog.Type) string {
	if sch := s.Types.Schemas[pt.OID]; sch != "" && sch != "public" {
		return sch + "." + pt.Name
	}
	if pt.Schema != "" && pt.Schema != "pg_catalog" && pt.Schema != "public" {
		return pt.Schema + "." + pt.Name
	}
	return pt.Name
}

// rowFields are a composite type's columns: the relation whose row type it is.
func rowFields(s *schema.Schema, oid catalog.OID, depth int) []dialect.Column {
	for _, r := range s.Relations {
		if r.RowType != oid {
			continue
		}
		var out []dialect.Column
		for _, c := range r.Columns {
			out = append(out, dialect.Column{Name: c.Name, Type: typeOf(s, c.Type, depth+1), Nullable: !c.NotNull})
		}
		return out
	}
	return nil
}

// scalarFits is pgx's table for a base type, result and parameter directions. Each entry is
// what pgx v5 actually scans or encodes, verified against a running PG; keep it honest
// rather than generous, since a wrong fit only fails at runtime.
func scalarFits(pt *catalog.Type) (result, param []dialect.GoFit) {
	switch pt.OID {
	case catalog.Int2:
		return fits("int16", "int32", "int64", "int"),
			[]dialect.GoFit{{Go: "int16"}, lossy("int32", "int32 into smallint may overflow"), lossy("int64", "int64 into smallint may overflow"), lossy("int", "int into smallint may overflow")}
	case catalog.Int4:
		return []dialect.GoFit{{Go: "int32"}, {Go: "int64"}, {Go: "int"}, lossy("int16", "integer into int16")},
			[]dialect.GoFit{{Go: "int16"}, {Go: "int32"}, lossy("int64", "int64 into integer may overflow"), lossy("int", "int into integer may overflow")}
	case catalog.Int8:
		return []dialect.GoFit{{Go: "int64"}, {Go: "int"}, lossy("int32", "bigint into int32")},
			fits("int32", "int64", "int")
	case catalog.OIDType:
		return fits("uint32"), fits("uint32", "int32", "int64", "int", "uint64", "uint")
	case catalog.Float4:
		return fits("float32", "float64"),
			[]dialect.GoFit{{Go: "float32"}, lossy("float64", "float64 into real loses precision")}
	case catalog.Float8:
		return []dialect.GoFit{{Go: "float64"}, lossy("float32", "double precision into float32")},
			fits("float32", "float64")
	case catalog.Numeric:
		exact := []string{pgtype + "Numeric", "decimal.Decimal", "apd.Decimal", "big.Rat", "string"}
		return append(fits(exact...),
				lossy("float64", "numeric into float64 loses precision"), lossy("float32", "numeric into float32 loses precision"),
				lossy("int64", "numeric into int64 drops the fraction"), lossy("int", "numeric into int drops the fraction"), lossy("int32", "numeric into int32 drops the fraction")),
			fits(append(exact, "float64", "float32", "int64", "int", "int32")...)
	case catalog.Bool:
		return fits("bool"), fits("bool")
	case catalog.Text, catalog.Varchar, catalog.BPChar, catalog.Name, catalog.Char, catalog.Cstring:
		return fits("string"), fits("string")
	case catalog.Bytea:
		return fits("[]byte"), fits("[]byte")
	case catalog.UUID:
		return fits("string", "uuid.UUID", "[16]byte"), fits("string", "uuid.UUID", "[16]byte")
	case catalog.Date, catalog.Timestamp, catalog.TimestampTZ:
		return fits("time.Time"), fits("time.Time")
	case catalog.Time:
		return fits("time.Time", "string"), fits("time.Time", "string")
	case catalog.TimeTZ:
		return fits("string"), fits("string") // pgx has no timetz codec: text only
	case catalog.Interval:
		return fits("time.Duration"), fits("time.Duration")
	case catalog.JSON, catalog.JSONB:
		return fits("encoding/json.RawMessage", "json"), fits("encoding/json.RawMessage", "json")
	case catalog.Record:
		return fits("struct"), fits("struct")
	}
	if pt.Schema == "" || pt.Schema == "pg_catalog" {
		switch pt.Name {
		case "inet":
			return fits("net/netip.Prefix", "net/netip.Addr"), fits("net/netip.Prefix", "net/netip.Addr")
		case "cidr":
			return fits("net/netip.Prefix"), fits("net/netip.Prefix")
		case "macaddr", "macaddr8":
			return fits("net.HardwareAddr", "string", "[]byte"), fits("net.HardwareAddr", "string", "[]byte")
		case "bit", "varbit":
			return fits(pgtype + "Bits"), fits(pgtype + "Bits")
		case "point":
			return fits(pgtype + "Point"), fits(pgtype + "Point")
		case "lseg":
			return fits(pgtype + "Lseg"), fits(pgtype + "Lseg")
		case "path":
			return fits(pgtype + "Path"), fits(pgtype + "Path")
		case "box":
			return fits(pgtype + "Box"), fits(pgtype + "Box")
		case "polygon":
			return fits(pgtype + "Polygon"), fits(pgtype + "Polygon")
		case "line":
			return fits(pgtype + "Line"), fits(pgtype + "Line")
		case "circle":
			return fits(pgtype + "Circle"), fits(pgtype + "Circle")
		case "tsvector":
			return fits(pgtype + "TSVector"), fits(pgtype + "TSVector")
		case "xml":
			return fits("string", "[]byte"), fits("string", "[]byte")
		case "money", "tsquery", "jsonpath", "tid", "pg_lsn", "txid_snapshot", "pg_snapshot", "aclitem", "regclass", "regtype", "regproc", "regprocedure", "regoper", "regoperator", "regnamespace", "regrole", "regconfig", "regdictionary", "regcollation":
			return fits("string"), fits("string")
		}
	}
	switch pt.Name {
	case "hstore":
		// the runtime registers pgx's HstoreCodec: map[string]*string (NULL values) or pgtype.Hstore
		return []dialect.GoFit{{Go: "map[string]*string"}, {Go: pgtype + "Hstore"}, lossy("map[string]string", "hstore into map[string]string fails at scan time when a value is NULL; use map[string]*string")},
			fits("map[string]*string", pgtype+"Hstore", "map[string]string")
	case "citext", "ltree", "lquery", "ltxtquery":
		return fits("string"), fits("string")
	}
	return nil, nil
}
