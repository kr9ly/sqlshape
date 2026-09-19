package sqlshape

// The row-mapping contract shared by the checker and every runtime: how a Go struct's
// fields bind to result columns (`col:"name"` / `db:"name"`, else snake_case; embedded
// structs flattened; a named struct field is a nested row), and the Labelled contract of
// a Go enum type. A runtime (sqlshape/postgres, sqlshape/mysql) maps rows with these; the
// checker verifies the same binding statically.

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Labelled is implemented by a Go enum type (a named string type bound to a PG enum) to
// say which labels the application knows. When a scanned value answers false the row is
// rejected with an *UnknownLabelError: the database has a label this build predates.
//
//	func (s OrderStatus) Known() bool { switch s { case Pending, Paid: return true }; return false }
type Labelled interface{ Known() bool }

// UnknownLabelError reports a scanned enum value the application does not know.
type UnknownLabelError struct {
	Type  reflect.Type
	Value string
}

var labelledType = reflect.TypeOf((*Labelled)(nil)).Elem()

// HasLabelled reports whether t (or anything it contains) implements Labelled.
func HasLabelled(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if t.Implements(labelledType) {
		return true
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return HasLabelled(t.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() && HasLabelled(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}

// CheckLabels validates every Labelled value inside v.
func CheckLabels(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	if v.Type().Implements(labelledType) && v.Kind() == reflect.String {
		if s := v.String(); s != "" && !v.Interface().(Labelled).Known() {
			return &UnknownLabelError{Type: v.Type(), Value: s}
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return CheckLabels(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := CheckLabels(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if err := CheckLabels(v.Field(i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Field is one scan target of a struct: an exported, non-skipped field, with the
// fields of embedded structs promoted (flattened) into their parent.
type Field struct {
	Index  []int  // for reflect's FieldByIndex
	Name   string // Go name, dotted through embedded structs (Base.ID)
	Column string // result column it binds to
	Type   reflect.Type
}

var fieldsCache sync.Map // reflect.Type → []Field or error

// Fields lists the scan targets of a struct type in declaration order. An embedded
// struct (anonymous field, no col / db tag) is flattened: its fields are the parent's,
// so `type Row struct { Base; Extra string }` receives base columns and extra. A named
// struct field is a nested row instead. Two fields binding the same column is an error.
func Fields(t reflect.Type) ([]Field, error) {
	if v, ok := fieldsCache.Load(t); ok {
		if err, isErr := v.(error); isErr {
			return nil, err
		}
		return v.([]Field), nil
	}
	var out []Field
	seen := map[string]string{}
	var walk func(st reflect.Type, prefix string, index []int) error
	walk = func(st reflect.Type, prefix string, index []int) error {
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			idx := append(append([]int{}, index...), i)
			if f.Anonymous && embeddedStruct(f) != nil {
				if err := walk(embeddedStruct(f), prefix+f.Name+".", idx); err != nil {
					return err
				}
				continue
			}
			if !f.IsExported() {
				continue
			}
			col := fieldColumn(f)
			if col == "-" {
				continue
			}
			if prev, dup := seen[col]; dup {
				return fmt.Errorf("sqlshape: fields %s.%s and %s.%s both bind to column %q", t, prev, t, prefix+f.Name, col)
			}
			seen[col] = prefix + f.Name
			out = append(out, Field{Index: idx, Name: prefix + f.Name, Column: col, Type: f.Type})
		}
		return nil
	}
	if err := walk(t, "", nil); err != nil {
		fieldsCache.Store(t, err)
		return nil, err
	}
	fieldsCache.Store(t, out)
	return out, nil
}

// embeddedStruct returns the struct type an anonymous field flattens into, or nil when
// the field is a leaf: not a struct, time.Time, a Scanner, or tagged with a column name.
func embeddedStruct(f reflect.StructField) reflect.Type {
	if !f.Anonymous {
		return nil
	}
	if _, ok := f.Tag.Lookup("col"); ok {
		return nil
	}
	if _, ok := f.Tag.Lookup("db"); ok {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || !IsRow(t) {
		return nil
	}
	return t
}

// FieldByIndex is reflect.Value.FieldByIndex that allocates nil embedded pointers on the way.
func FieldByIndex(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

// fieldColumn is the column a struct field binds to: `col:"name"` / `db:"name"`, else snake_case.
// (Kept in sync with the analyzer's rule.)
func fieldColumn(f reflect.StructField) string {
	for _, key := range []string{"col", "db"} {
		if v, ok := f.Tag.Lookup(key); ok {
			name := strings.Split(v, ",")[0]
			if name == "" {
				return snake(f.Name)
			}
			return name
		}
	}
	return snake(f.Name)
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

func (e *UnknownLabelError) Error() string {
	return "sqlshape: unknown " + e.Type.String() + " label " + strconv.Quote(e.Value) + " received (the database has a value this build does not know)"
}

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

// IsRow reports whether a Go type receives a row (struct) or rows (slice of structs).
func IsRow(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return t.Kind() == reflect.Struct && !ScalarStruct(t) &&
		!reflect.PointerTo(t).Implements(reflect.TypeOf((*interface{ Scan(any) error })(nil)).Elem())
}

// ScalarStruct reports whether a struct type is a scalar pgx decodes itself rather than a
// row: time.Time, netip.Addr / Prefix, and every pgtype value (Range[T], Bits, Point, ...).
func ScalarStruct(t reflect.Type) bool {
	switch p := t.PkgPath(); {
	case p == "time", p == "net/netip", strings.HasSuffix(p, "jackc/pgx/v5/pgtype"):
		return true
	}
	return false
}
