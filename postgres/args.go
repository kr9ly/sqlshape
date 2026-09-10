package postgres

import (
	"database/sql/driver"
	"reflect"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kr9ly/sqlshape"
)

// normalizeArg turns a template value into something pgx encodes for any target OID: what
// sqlshape.Normalize does (named strings to string, nil pointers to nil), and a struct that
// receives a composite (sqlshape.IsRow) becomes pgtype.CompositeFields in the order the
// checker verified (embedded structs flattened, `col:"-"` skipped), a slice of them a
// slice of those.
func normalizeArg(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return normalizeArg(v.Elem())
	}
	t := v.Type()
	switch {
	case t.Kind() == reflect.Struct && sqlshape.IsRow(t) && !t.Implements(valuerType):
		return compositeArg(v)
	case t.Kind() == reflect.Slice && sqlshape.IsRow(t) && !t.Elem().Implements(valuerType):
		if v.IsNil() {
			return nil
		}
		out := make([]pgtype.CompositeFields, v.Len())
		for i := range out {
			out[i] = compositeArg(v.Index(i))
		}
		return out
	}
	return sqlshape.Normalize(v.Interface())
}

var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// compositeArg lays a struct (or *struct) out as the fields of a composite value.
func compositeArg(v reflect.Value) pgtype.CompositeFields {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	flat, _ := sqlshape.Fields(v.Type())
	out := make(pgtype.CompositeFields, len(flat))
	for i, f := range flat {
		out[i] = normalizeArg(v.FieldByIndex(f.Index))
	}
	return out
}

// pgArgs applies normalizeArg to rendered arguments (the root's Render normalizes what it
// knows; composites are this runtime's).
func pgArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = normalizeArg(reflect.ValueOf(a))
	}
	return out
}
