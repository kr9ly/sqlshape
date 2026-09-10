package vet

import (
	"go/token"
	"go/types"
	"strconv"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// Nested rows. A record / composite column (or an array of them) is received by a Go
// struct (or slice of structs). The runtime scans such values positionally, so the
// struct's exported fields must line up with the row type's columns one to one, in
// order: for a named composite the names must agree too; for an anonymous row(...)
// only the position exists (PG calls the fields f1, f2, ...).

// checkNested matches the fields of a record column against the Go struct receiving it
// (param = false), or of a composite parameter against the struct passed for it (param =
// true: the runtime encodes the struct's fields positionally as pgtype.CompositeFields).
func (c *checker) checkNested(col dialect.Column, gt types.Type, at token.Pos, what string, report func(token.Pos, string, ...any), where string, param bool) {
	rowFields, named := nestedFields(col.Type)
	if len(rowFields) == 0 {
		return
	}
	inner, _ := unwrapNullable(gt)
	if inner == nil {
		return
	}
	switch u := inner.Underlying().(type) {
	case *types.Slice:
		inner = u.Elem()
	case *types.Array:
		inner = u.Elem()
	}
	inner, _ = unwrapNullable(inner)
	if inner == nil {
		return
	}
	st, ok := inner.Underlying().(*types.Struct)
	if !ok {
		return // matchValue already reported the non-struct
	}
	if _, declared := c.declaredOf(inner); declared || implementsScanner(inner) {
		return // the type decodes the value itself; its fields are not columns
	}
	flat, dups := structFields(st)
	for _, d := range dups {
		report(at, "%s: %s: fields %s%s", what, inner, d, where)
	}
	fields := make([]*types.Var, len(flat))
	names := make([]string, len(flat))
	goNames := make([]string, len(flat))
	notnull := make([]bool, len(flat))
	for i, f := range flat {
		fields[i], names[i], goNames[i] = f.v, f.col, f.name
		for _, o := range f.opts {
			if o == "notnull" {
				notnull[i] = true // `col:",notnull"`: the author knows better than the analyzer
			}
		}
	}
	if len(fields) != len(rowFields) {
		report(at, "%s: %s has %d fields but the row type has %d (%s)%s", what, inner, len(fields), len(rowFields), rowShape(rowFields), where)
		return
	}
	for i, f := range rowFields {
		fv := fields[i]
		sub := what + "." + goNames[i]
		if named && names[i] != f.Name {
			report(at, "%s is at position %d but the row type's column %d is %q (fields are scanned in order)%s", sub, i+1, i+1, f.Name, where)
			continue
		}
		c.meet(fv.Type(), f.Type, f.Source, at, sub)
		fit := c.fitPG(f.Type, fv.Type(), param)
		if notnull[i] {
			f.Nullable = false
		}
		c.reportFit(report, at, sub, f, fv.Type(), fit, where)
		c.checkNested(f, fv.Type(), at, sub, report, where, param)
	}
}

// nestedFields are the columns of a row-typed value (a composite, a record, or an array
// of either; a domain over one), and whether the row type is named (a composite: the
// field names must agree, not only the positions).
func nestedFields(t dialect.Type) ([]dialect.Column, bool) {
	for t.Kind == dialect.Domain && t.Base != nil {
		t = *t.Base
	}
	if t.Kind == dialect.Array && t.Elem != nil {
		t = *t.Elem
	}
	return t.Fields, t.Kind == dialect.Composite
}

// rowShape spells a row type's columns for a diagnostic.
func rowShape(fields []dialect.Column) string {
	s := ""
	for i, f := range fields {
		if i > 0 {
			s += ", "
		}
		s += f.Name + " " + f.Type.Name
	}
	return strconv.Quote(s)
}
