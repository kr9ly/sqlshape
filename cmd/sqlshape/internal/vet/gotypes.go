package vet

import (
	"fmt"
	"go/types"
	"strings"
	"unicode"

	pgdialect "github.com/kr9ly/sqlshape/check/postgres/v2/dialect"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// fit is the verdict of matching a Go type against a PostgreSQL type.
type fit struct {
	ok       bool   // the Go type can carry the PG value
	nullable bool   // the Go type can carry NULL (pointer, sql.Null*, pgtype.*)
	lossy    string // non-empty when the mapping loses information (e.g. numeric → float64)
	advice   string // what a faithful mapping still leaves to the application (-strict)
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
// param = true for a Go value encoded as a parameter. The table of what fits is the
// dialect's (check/postgres/dialect.TypeOf spells pgx's); the grammar it is read with is
// gofit.go's. A Go type that declares the PostgreSQL type it carries (`// sqlshape: type
// X`) is judged by that declaration alone.
func (c *checker) matchDir(pg schema.TypeRef, t types.Type, param bool) fit {
	return c.fitPG(pgdialect.TypeOf(c.s, pg), t, param)
}

// fitPG is fitType with this package's declared type bindings applied first.
func (c *checker) fitPG(dt dialect.Type, t types.Type, param bool) fit {
	if inner, nullable := unwrapNullable(t); inner != nil {
		if d, ok := c.declaredOf(inner); ok {
			f := c.matchDeclared(d, inner, dt, param)
			switch inner.Underlying().(type) {
			case *types.Slice, *types.Map:
				nullable = true
			}
			f.nullable = nullable || implementsScanner(inner)
			return f
		}
	}
	return fitType(dt, t, param, c.traits())
}

// traits are the loaded dialect's conventions for Go types.
func (c *checker) traits() dialect.Traits {
	if c.ls != nil && c.ls.dialect != nil {
		return c.ls.dialect.Traits()
	}
	return pgdialect.Traits
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
// so the lossy cases are the ones where Go is wider than PG (the dialect's Param list).
func (c *checker) paramFit(pg schema.TypeRef, t types.Type) fit {
	return c.matchDir(pg, t, true)
}
