package vet

import (
	"go/token"
	"go/types"
	"strconv"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// Nested rows. A record / composite column (or an array of them) is received by a Go
// struct (or slice of structs). The runtime scans such values positionally, so the
// struct's exported fields must line up with the row type's columns one to one, in
// order: for a named composite the names must agree too; for an anonymous row(...)
// only the position exists (PG calls the fields f1, f2, ...).

// checkNested matches the fields of a record column against the Go struct receiving it
// (param = false), or of a composite parameter against the struct passed for it (param =
// true: the runtime encodes the struct's fields positionally as pgtype.CompositeFields).
func (c *checker) checkNested(col analyze.Column, gt types.Type, at token.Pos, what string, report func(token.Pos, string, ...any), where string, param bool) {
	if len(col.Fields) == 0 {
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
	et := c.s.Types.ByOID(elemOID(c, col))
	named := et != nil && et.Kind == 'c'
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
	if len(fields) != len(col.Fields) {
		report(at, "%s: %s has %d fields but the row type has %d (%s)%s", what, inner, len(fields), len(col.Fields), rowShape(c, col), where)
		return
	}
	for i, f := range col.Fields {
		fv := fields[i]
		sub := what + "." + goNames[i]
		if named && names[i] != f.Name {
			report(at, "%s is at position %d but the row type's column %d is %q (fields are scanned in order)%s", sub, i+1, i+1, f.Name, where)
			continue
		}
		c.meet(fv.Type(), f.Type, f.Source, at, sub)
		fit := c.matchDir(f.Type, fv.Type(), param)
		if notnull[i] {
			f.Nullable = false
		}
		c.reportFit(report, at, sub, f, fv.Type(), fit, where)
		c.checkNested(f, fv.Type(), at, sub, report, where, param)
	}
}

// paramColumn describes a composite parameter (or an array of composites) like a result
// column, so checkNested can match the struct passed for it: the fields are the declared
// columns of the relation whose row type it is, recursively. Nil for other types.
func (c *checker) paramColumn(pg schema.TypeRef) *analyze.Column {
	oid := c.s.Types.BaseOf(pg).OID
	if t := c.s.Types.ByOID(oid); t != nil && t.IsArray() {
		oid = t.Elem
	}
	rel := c.relByRowType(oid)
	if rel == nil {
		return nil
	}
	col := &analyze.Column{Type: pg}
	for _, rc := range rel.Columns {
		f := analyze.Column{Name: rc.Name, Type: rc.Type}
		if sub := c.paramColumn(rc.Type); sub != nil {
			f.Fields = sub.Fields
		}
		col.Fields = append(col.Fields, f)
	}
	return col
}

func (c *checker) relByRowType(oid catalog.OID) *schema.Relation {
	for _, r := range c.s.Relations {
		if r.RowType == oid {
			return r
		}
	}
	return nil
}

func elemOID(c *checker, col analyze.Column) catalog.OID {
	oid := col.Type.OID
	if t := c.s.Types.ByOID(oid); t != nil && t.IsArray() {
		oid = t.Elem
	}
	return oid
}

func rowShape(c *checker, col analyze.Column) string {
	s := ""
	for i, f := range col.Fields {
		if i > 0 {
			s += ", "
		}
		s += f.Name + " " + c.s.Types.Format(f.Type)
	}
	return strconv.Quote(s)
}
