package sqlshape

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Nested rows at runtime.
//
// A record / composite column is scanned positionally into a Go struct (and record[]
// into a slice of structs): the checker has verified that the struct's exported fields
// line up with the row type's columns, so the runtime only has to hand pgx one scan
// target per position. pgx decodes composites field by field from the binary format,
// which carries each field's type OID; user-defined types (enums, composites, their
// arrays) must be registered in the connection's type map for that, so Run loads any
// result type the connection does not know yet (once per connection) before scanning,
// and LoadUserTypes does it wholesale for pool AfterConnect hooks.

// isNested reports whether a Go type receives a row (struct) or rows (slice of structs).
func isNested(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return t.Kind() == reflect.Struct && t != reflect.TypeOf(time.Time{}) &&
		!reflect.PointerTo(t).Implements(reflect.TypeOf((*interface{ Scan(any) error })(nil)).Elem())
}

// nestedDest returns the scan target for a struct / slice-of-struct field.
func nestedDest(v reflect.Value) any {
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		return &optionalDest{v: v}
	}
	if t.Kind() == reflect.Slice {
		return &rowsDest{v: v}
	}
	return &rowDest{v: v}
}

// rowDest scans one record into a struct, field by field in order.
type rowDest struct{ v reflect.Value }

func (d *rowDest) ScanNull() error {
	d.v.Set(reflect.Zero(d.v.Type()))
	return nil
}

func (d *rowDest) ScanIndex(i int) any {
	fields := scanFields(d.v.Type())
	if i >= len(fields) {
		return nil
	}
	fv := d.v.Field(fields[i])
	if isNested(fv.Type()) {
		return nestedDest(fv)
	}
	return fv.Addr().Interface()
}

// optionalDest scans a record into *T: NULL leaves the pointer nil.
type optionalDest struct{ v reflect.Value }

func (d *optionalDest) ScanNull() error {
	d.v.Set(reflect.Zero(d.v.Type()))
	return nil
}

func (d *optionalDest) ScanIndex(i int) any {
	if d.v.IsNil() {
		d.v.Set(reflect.New(d.v.Type().Elem()))
	}
	return (&rowDest{v: d.v.Elem()}).ScanIndex(i)
}

// rowsDest scans an array of records into a slice of structs.
type rowsDest struct{ v reflect.Value }

func (d *rowsDest) SetDimensions(dims []pgtype.ArrayDimension) error {
	if dims == nil {
		d.v.Set(reflect.Zero(d.v.Type()))
		return nil
	}
	n := 1
	for _, dim := range dims {
		n *= int(dim.Length)
	}
	d.v.Set(reflect.MakeSlice(d.v.Type(), n, n))
	return nil
}

func (d *rowsDest) ScanIndex(i int) any {
	return nestedDest(d.v.Index(i))
}

func (d *rowsDest) ScanIndexType() any {
	return nestedDest(reflect.New(d.v.Type().Elem()).Elem())
}

var scanFieldsCache sync.Map // reflect.Type → []int

// scanFields lists the exported, non-skipped struct fields in declaration order.
func scanFields(t reflect.Type) []int {
	if f, ok := scanFieldsCache.Load(t); ok {
		return f.([]int)
	}
	var out []int
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && fieldColumn(f) != "-" {
			out = append(out, i)
		}
	}
	scanFieldsCache.Store(t, out)
	return out
}

// --- type registration -----------------------------------------------------

// unknownTypes lists the result column type OIDs the connection cannot decode yet.
func unknownTypes(conn *pgx.Conn, fds []pgconn.FieldDescription) []uint32 {
	if conn == nil {
		return nil
	}
	var out []uint32
	for _, fd := range fds {
		if _, ok := conn.TypeMap().TypeForOID(fd.DataTypeOID); !ok {
			out = append(out, fd.DataTypeOID)
		}
	}
	return out
}

// loadTypes registers the named-by-OID types (and what they depend on) with conn.
func loadTypes(ctx context.Context, conn *pgx.Conn, oids []uint32) error {
	rows, err := conn.Query(ctx, `SELECT n.nspname, t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE t.oid = ANY($1)`, oids)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var nsp, name string
		if err := rows.Scan(&nsp, &name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, qualify(nsp, name))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return registerTypes(ctx, conn, names)
}

func qualify(nsp, name string) string {
	if nsp == "public" || nsp == "pg_catalog" {
		return name
	}
	return nsp + "." + name
}

func registerTypes(ctx context.Context, conn *pgx.Conn, names []string) error {
	if len(names) == 0 {
		return nil
	}
	types, err := conn.LoadTypes(ctx, names)
	if err != nil {
		return fmt.Errorf("sqlshape: load types %v: %w", names, err)
	}
	conn.TypeMap().RegisterTypes(types)
	return nil
}

// LoadUserTypes registers every user-defined enum, composite and domain type of the
// database (and their array types) with conn, so records containing them decode.
// Use it from pgxpool's AfterConnect; Run also loads result types lazily, but cannot
// see types nested inside an anonymous record before scanning it.
func LoadUserTypes(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, `
SELECT n.nspname, t.typname
  FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE t.typtype IN ('e', 'c', 'd')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
   AND (t.typrelid = 0 OR EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = t.typrelid AND c.relkind IN ('c', 'r', 'v', 'm', 'p')))
 ORDER BY t.oid`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var nsp, name string
		if err := rows.Scan(&nsp, &name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, qualify(nsp, name), qualify(nsp, "_"+name))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return registerTypes(ctx, conn, names)
}
