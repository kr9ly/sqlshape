package vet

import (
	"fmt"
	"go/types"
	"strings"
	"unicode"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// fit is the verdict of matching a Go type against a PostgreSQL type.
type fit struct {
	ok       bool   // the Go type can carry the PG value
	nullable bool   // the Go type can carry NULL (pointer, sql.Null*, pgtype.*)
	lossy    string // non-empty when the mapping loses information (e.g. numeric → float64)
	unknown  bool   // PG type has no known Go mapping; accepted with a note
}

// unwrapNullable strips one level of nullability wrapper and reports it.
func unwrapNullable(t types.Type) (types.Type, bool) {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem(), true
	}
	if n, ok := t.(*types.Named); ok {
		obj := n.Obj()
		if obj.Pkg() != nil {
			switch {
			case obj.Pkg().Path() == "database/sql" && strings.HasPrefix(obj.Name(), "Null"):
				return nil, true // sql.NullString etc.: type checked loosely
			case strings.HasSuffix(obj.Pkg().Path(), "jackc/pgx/v5/pgtype"):
				if n.TypeArgs() != nil {
					return t, true // Range[T] / Multirange[T]: nullable, and T is checked
				}
				return nil, true // pgtype.* all carry Valid
			}
		}
	}
	return t, false
}

// isNamed reports whether t is the named type pkg.name (pkg matched by suffix).
func isNamed(t types.Type, pkgSuffix, name string) bool {
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	return strings.HasSuffix(n.Obj().Pkg().Path(), pkgSuffix) && n.Obj().Name() == name
}

func basicKind(t types.Type) (types.BasicKind, bool) {
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return 0, false
	}
	return b.Kind(), true
}

func isByteSlice(t types.Type) bool {
	s, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	k, ok := basicKind(s.Elem())
	return ok && (k == types.Byte || k == types.Uint8)
}

// match decides whether Go type t can carry PG type pg (domains flattened) as a result.
func (c *checker) match(pg schema.TypeRef, t types.Type) fit {
	return c.matchDir(pg, t, false)
}

// matchDir is match in one direction: param = false for a result column scanned into t,
// param = true for a Go value encoded as a parameter. The two differ for types pgx
// decodes only in binary (inet, interval, ranges, bit, ...): any string encodes as a
// parameter (text format), but a result column cannot be scanned into one.
func (c *checker) matchDir(pg schema.TypeRef, t types.Type, param bool) fit {
	inner, nullable := unwrapNullable(t)
	if inner == nil {
		return fit{ok: true, nullable: true}
	}
	f := c.matchValue(pg, inner, param)
	// a nil slice / map receives a NULL array, record[] or hstore without a pointer
	switch inner.Underlying().(type) {
	case *types.Slice, *types.Map:
		nullable = true
	}
	// a Scanner sees NULL as Scan(nil) and represents it itself
	if _, declared := c.declaredOf(inner); declared && implementsScanner(inner) {
		nullable = true
	}
	f.nullable = nullable
	return f
}

// The Go type table. Each entry is what pgx v5 actually scans (result) or encodes
// (parameter) for the PG type, verified against a running PG; keep it honest rather than
// generous, since a wrong "ok" only fails at runtime. pgtype.* values are accepted for
// every type by unwrapNullable (they all carry Valid), so they need no entry here.
func (c *checker) matchValue(pg schema.TypeRef, t types.Type, param bool) fit {
	base := c.s.Types.BaseOf(pg)
	pt := c.s.Types.ByOID(base.OID)
	if pt == nil {
		return fit{ok: true, unknown: true}
	}
	kind, isBasic := basicKind(t)
	is := func(kinds ...types.BasicKind) bool {
		if !isBasic {
			return false
		}
		for _, k := range kinds {
			if kind == k {
				return true
			}
		}
		return false
	}
	// a Go type that declares the PG type it carries (`// sqlshape: type X`) is checked by that alone
	if dt, ok := c.declaredOf(t); ok {
		return c.matchDeclared(dt, t, pg.OID, base.OID, param)
	}
	// a string encodes as any parameter type (text format)
	if param && is(types.String) {
		return fit{ok: true}
	}
	// arrays (record[] is a pseudo-type array, so not IsArray). Elements go through
	// matchDir (not matchValue directly) so a pointer/NULL-able element is unwrapped the
	// same way a top-level parameter or result is, and so the per-element fit — including
	// the param-direction overflow notes below — is the single place that decides an
	// element's fit; nothing here duplicates that decision.
	if pt.Elem != 0 && strings.HasPrefix(pt.Name, "_") {
		elemPG := schema.TypeRef{OID: pt.Elem, Typmod: -1}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			ef := c.matchDir(elemPG, u.Elem(), param)
			return fit{ok: ef.ok, lossy: ef.lossy, unknown: ef.unknown}
		case *types.Array:
			ef := c.matchDir(elemPG, u.Elem(), param)
			return fit{ok: ef.ok, lossy: ef.lossy, unknown: ef.unknown}
		}
		return fit{}
	}
	// The notes below are direction-specific: a narrower Go type given a wider PG value
	// only matters when scanning a result (may truncate what's already there); a wider Go
	// type given a narrower PG parameter only matters when encoding a parameter (the PG
	// column may reject a value the Go type has room for). Each case picks the note for
	// the direction it's actually asked about, so paramFit needs no separate pass over
	// these same cases: matchDir(..., param: true) already carries the right note,
	// scalar or, via the array branch above, per element.
	switch base.OID {
	case catalog.Int2:
		if param && is(types.Int32, types.Int64, types.Int) {
			return fit{ok: true, lossy: t.String() + " into smallint may overflow"}
		}
		return fit{ok: is(types.Int16, types.Int32, types.Int64, types.Int)}
	case catalog.Int4:
		if param {
			if is(types.Int64, types.Int) {
				return fit{ok: true, lossy: t.String() + " into integer may overflow"}
			}
			return fit{ok: is(types.Int16, types.Int32, types.Int64, types.Int)}
		}
		if is(types.Int16) {
			return fit{ok: true, lossy: "integer into int16"}
		}
		return fit{ok: is(types.Int32, types.Int64, types.Int)}
	case catalog.Int8:
		if param {
			return fit{ok: is(types.Int32, types.Int64, types.Int)}
		}
		if is(types.Int32) {
			return fit{ok: true, lossy: "bigint into int32"}
		}
		return fit{ok: is(types.Int64, types.Int)}
	case catalog.OIDType:
		// pgx scans oid into uint32 only; it encodes any integer
		if param {
			return fit{ok: is(types.Uint32, types.Int32, types.Int64, types.Int, types.Uint64, types.Uint)}
		}
		return fit{ok: is(types.Uint32)}
	case catalog.Float4:
		if param && is(types.Float64) {
			return fit{ok: true, lossy: "float64 into real loses precision"}
		}
		return fit{ok: is(types.Float32, types.Float64)}
	case catalog.Float8:
		if param {
			return fit{ok: is(types.Float32, types.Float64)}
		}
		if is(types.Float32) {
			return fit{ok: true, lossy: "double precision into float32"}
		}
		return fit{ok: is(types.Float64)}
	case catalog.Numeric:
		switch {
		case is(types.String), isNamed(t, "pgtype", "Numeric"), isNamed(t, "math/big", "Rat"),
			isNamed(t, "decimal", "Decimal"), isNamed(t, "apd", "Decimal"):
			return fit{ok: true}
		case is(types.Float64, types.Float32):
			if param {
				return fit{ok: true}
			}
			return fit{ok: true, lossy: "numeric into " + t.String() + " loses precision"}
		case is(types.Int64, types.Int, types.Int32):
			if param {
				return fit{ok: true}
			}
			return fit{ok: true, lossy: "numeric into " + t.String() + " drops the fraction"}
		}
		return fit{}
	case catalog.Bool:
		return fit{ok: is(types.Bool)}
	case catalog.Text, catalog.Varchar, catalog.BPChar, catalog.Name, catalog.Char, catalog.Cstring:
		return fit{ok: is(types.String)}
	case catalog.Bytea:
		return fit{ok: isByteSlice(t)}
	case catalog.UUID:
		if is(types.String) || isNamed(t, "uuid", "UUID") {
			return fit{ok: true}
		}
		if arr, ok := t.Underlying().(*types.Array); ok && arr.Len() == 16 {
			return fit{ok: true}
		}
		return fit{}
	case catalog.Date, catalog.Timestamp, catalog.TimestampTZ:
		return fit{ok: isNamed(t, "time", "Time")}
	case catalog.Time:
		return fit{ok: isNamed(t, "time", "Time") || is(types.String)}
	case catalog.TimeTZ:
		// pgx has no timetz codec: text only (time.Time fails to parse the zone suffix)
		return fit{ok: is(types.String)}
	case catalog.Interval:
		return fit{ok: isNamed(t, "time", "Duration")}
	case catalog.JSON, catalog.JSONB:
		// pgx decodes JSON into any target; anything but a plain number/bool is plausible
		if isBasic && !is(types.String) {
			return fit{}
		}
		return fit{ok: true}
	case catalog.Record:
		_, isStruct := t.Underlying().(*types.Struct)
		return fit{ok: isStruct}
	}
	switch pt.Kind {
	case 'e': // enum: string or a named string type
		return fit{ok: is(types.String)}
	case 'c': // composite / row type: a struct
		_, isStruct := t.Underlying().(*types.Struct)
		return fit{ok: isStruct}
	case 'r': // range: pgtype.Range[T] with T carrying the subtype
		if rng := c.s.Types.RangeOf(base.OID); rng != nil {
			return c.matchGeneric(t, "Range", schema.TypeRef{OID: rng.Subtype, Typmod: -1}, param)
		}
		return fit{ok: true, unknown: true}
	case 'm': // multirange: pgtype.Multirange[pgtype.Range[T]]
		if rng := c.s.Types.RangeOfMulti(base.OID); rng != nil {
			if !isNamed(t, "pgtype", "Multirange") {
				return fit{}
			}
			args := t.(*types.Named).TypeArgs()
			if args == nil || args.Len() != 1 {
				return fit{}
			}
			return c.matchGeneric(args.At(0), "Range", schema.TypeRef{OID: rng.Subtype, Typmod: -1}, param)
		}
		return fit{ok: true, unknown: true}
	}
	// pg_catalog types pgx knows by name, and extension types the runtime registers
	if pt.Schema == "" || pt.Schema == "pg_catalog" {
		switch pt.Name {
		case "inet":
			return fit{ok: isNamed(t, "net/netip", "Prefix") || isNamed(t, "net/netip", "Addr")}
		case "cidr":
			return fit{ok: isNamed(t, "net/netip", "Prefix")}
		case "macaddr", "macaddr8":
			return fit{ok: is(types.String) || isNamed(t, "net", "HardwareAddr") || isByteSlice(t)}
		case "bit", "varbit":
			return fit{} // pgtype.Bits only (accepted above)
		case "point", "lseg", "path", "box", "polygon", "line", "circle":
			return fit{} // pgtype.Point / Box / ... only
		case "tsvector":
			return fit{} // pgtype.TSVector only
		case "xml":
			return fit{ok: is(types.String) || isByteSlice(t)}
		case "money", "tsquery", "jsonpath", "tid", "pg_lsn", "txid_snapshot", "pg_snapshot", "aclitem", "regclass", "regtype", "regproc", "regprocedure", "regoper", "regoperator", "regnamespace", "regrole", "regconfig", "regdictionary", "regcollation":
			return fit{ok: is(types.String)}
		}
	}
	switch pt.Name {
	case "hstore":
		// the runtime registers pgx's HstoreCodec: map[string]*string (NULL values) or pgtype.Hstore
		if m, ok := t.Underlying().(*types.Map); ok {
			k, _ := basicKind(m.Key())
			if k != types.String {
				return fit{}
			}
			if ptr, ok := m.Elem().(*types.Pointer); ok {
				ek, _ := basicKind(ptr.Elem())
				return fit{ok: ek == types.String}
			}
			if ek, _ := basicKind(m.Elem()); ek == types.String {
				if param {
					return fit{ok: true}
				}
				return fit{ok: true, lossy: "hstore into map[string]string fails at scan time when a value is NULL; use map[string]*string"}
			}
		}
		return fit{}
	case "citext", "ltree", "lquery", "ltxtquery":
		// text-codec scalars the runtime registers as text
		return fit{ok: is(types.String)}
	}
	return fit{ok: true, unknown: true}
}

// matchGeneric matches t against pgtype.<name>[T] where T must carry elem.
func (c *checker) matchGeneric(t types.Type, name string, elem schema.TypeRef, param bool) fit {
	if !isNamed(t, "pgtype", name) {
		return fit{}
	}
	args := t.(*types.Named).TypeArgs()
	if args == nil || args.Len() != 1 {
		return fit{}
	}
	ef := c.matchDir(elem, args.At(0), param)
	return fit{ok: ef.ok, lossy: ef.lossy, unknown: ef.unknown}
}

// structField is one column-bearing field of a result struct, embedded structs flattened.
type structField struct {
	v    *types.Var
	name string   // Go name, dotted through embedded structs (Base.ID)
	col  string   // result column it binds to
	opts []string // tag options after the name (notnull)
}

// structFields lists the exported, non-skipped fields of st in declaration order. An
// embedded struct (anonymous field without a col / db tag) is flattened: its fields count
// as the parent's, the same way the runtime mapper scans them. A named struct field is a
// nested row instead. dups lists the columns two fields bind to ("A and B both bind to x").
// Kept in sync with the runtime's flatFields.
func structFields(st *types.Struct) (fields []structField, dups []string) {
	seen := map[string]string{}
	var walk func(st *types.Struct, prefix string)
	walk = func(st *types.Struct, prefix string) {
		for i := 0; i < st.NumFields(); i++ {
			fv := st.Field(i)
			if fv.Embedded() {
				if inner := embeddedStruct(fv, st.Tag(i)); inner != nil {
					walk(inner, prefix+fv.Name()+".")
					continue
				}
			}
			if !fv.Exported() {
				continue
			}
			col, opts := columnName(fv, st.Tag(i))
			if col == "-" {
				continue
			}
			if prev, dup := seen[col]; dup {
				dups = append(dups, fmt.Sprintf("%s and %s both bind to column %q", prev, prefix+fv.Name(), col))
				continue
			}
			seen[col] = prefix + fv.Name()
			fields = append(fields, structField{v: fv, name: prefix + fv.Name(), col: col, opts: opts})
		}
	}
	walk(st, "")
	return fields, dups
}

// embeddedStruct returns the struct an embedded field flattens into, or nil when it is a
// leaf: not a struct, time.Time, a Scanner, or tagged with a column name.
func embeddedStruct(fv *types.Var, tag string) *types.Struct {
	if _, ok := lookupTag(tag, "col"); ok {
		return nil
	}
	if _, ok := lookupTag(tag, "db"); ok {
		return nil
	}
	t := fv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil {
		switch p := n.Obj().Pkg().Path(); {
		case p == "time", p == "net/netip", strings.HasSuffix(p, "jackc/pgx/v5/pgtype"):
			return nil // scalars pgx decodes itself, as the runtime's scalarStruct
		}
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	if types.NewMethodSet(types.NewPointer(t)).Lookup(nil, "Scan") != nil {
		return nil
	}
	return st
}

// columnName derives the result column a struct field binds to: `col:"name"` / `db:"name"`
// tag first, else snake_case of the field name.
func columnName(f *types.Var, tag string) (name string, opts []string) {
	for _, key := range []string{"col", "db"} {
		if v, ok := lookupTag(tag, key); ok {
			parts := strings.Split(v, ",")
			if parts[0] == "" {
				parts[0] = snake(f.Name())
			}
			return parts[0], parts[1:]
		}
	}
	return snake(f.Name()), nil
}

func lookupTag(tag, key string) (string, bool) {
	// minimal reflect.StructTag.Lookup
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		val := tag[1:i]
		tag = tag[i+1:]
		if name == key {
			return val, true
		}
	}
	return "", false
}

func snake(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// paramFit is match in the Go → PG direction: the Go value must fit the PG parameter type,
// so the lossy cases are the ones where Go is wider than PG. matchValue already picks the
// param-direction note (see the switch in matchValue), scalar or per array element, so
// there is nothing left to add here.
func (c *checker) paramFit(pg schema.TypeRef, t types.Type) fit {
	return c.matchDir(pg, t, true)
}
