package vet

import (
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"strings"

	pgdialect "github.com/kr9ly/sqlshape/check/postgres/v2/dialect"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// Declared type bindings. A Go type can name the PostgreSQL type it carries in its doc
// comment:
//
//	// sqlshape: type money_amount
//	type Money struct{ Amount decimal.Decimal; Currency string }
//
// The checker then accepts Money exactly where SQL has money_amount (and []Money for
// money_amount[]) and reports it anywhere else; how the value crosses the wire is the
// type's own business, through sql.Scanner / driver.Valuer, so the type is not opened
// up as a nested row and its fields are not matched to columns. This is the escape hatch
// for types the table in gotypes.go cannot describe: a decimal wrapper for money, a
// struct for a composite with its own encoding, a named type for an extension type.
// The binding travels with the type as a fact, so other packages using it are checked too.

// TypeBindingFact records the PG type a Go type declares it carries.
type TypeBindingFact struct{ PG string }

func (*TypeBindingFact) AFact()           {}
func (f *TypeBindingFact) String() string { return "carries " + f.PG }

func init() {
	Analyzer.FactTypes = append(Analyzer.FactTypes, (*TypeBindingFact)(nil))
}

type declaredType struct {
	pg    string
	named string // the type's canonical name (dialect.Type.Named); "" when it does not exist (reported at the declaration)
}

var typeDirective = regexp.MustCompile(`(?m)^\s*sqlshape:\s*type\s+(\S+)\s*$`)

// collectDeclaredTypes reads `// sqlshape: type X` doc comments on this package's type
// declarations, validates X against the schema and exports the bindings as facts.
func (c *checker) collectDeclaredTypes() {
	c.declared = map[*types.TypeName]declaredType{}
	for _, f := range c.pass.Files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts := sp.(*ast.TypeSpec)
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				if doc == nil {
					continue
				}
				m := typeDirective.FindStringSubmatch(doc.Text())
				if m == nil {
					continue
				}
				obj, ok := c.pass.TypesInfo.Defs[ts.Name].(*types.TypeName)
				if !ok {
					continue
				}
				dt := declaredType{pg: m[1]}
				sch, name := "", m[1]
				if i := strings.LastIndex(name, "."); i >= 0 {
					sch, name = name[:i], name[i+1:]
				}
				if c.s != nil {
					if t := c.s.Types.Lookup(sch, name); t != nil {
						dt.named = pgdialect.NamedOf(c.s, t.OID)
					} else {
						c.pass.Reportf(ts.Name.Pos(), "sqlshape: type %s: PostgreSQL type %q does not exist in the schema", obj.Name(), m[1])
					}
				}
				c.declared[obj] = dt
				c.pass.ExportObjectFact(obj, &TypeBindingFact{PG: m[1]})
			}
		}
	}
}

// declaredOf is the declared binding of a Go type: this package's, or an imported fact.
func (c *checker) declaredOf(t types.Type) (declaredType, bool) {
	named, ok := t.(*types.Named)
	if !ok {
		return declaredType{}, false
	}
	if dt, ok := c.declared[named.Obj()]; ok {
		return dt, true
	}
	var f TypeBindingFact
	if !c.pass.ImportObjectFact(named.Obj(), &f) {
		return declaredType{}, false
	}
	dt := declaredType{pg: f.PG}
	sch, name := "", f.PG
	if i := strings.LastIndex(name, "."); i >= 0 {
		sch, name = name[:i], name[i+1:]
	}
	if pt := c.s.Types.Lookup(sch, name); pt != nil {
		dt.named = pgdialect.NamedOf(c.s, pt.OID)
	}
	c.declared[named.Obj()] = dt
	return dt, true
}

// matchDeclared decides a declared type against a dialect type: the same named type (a
// domain over it included), and, unless the type is a composite fed by the struct's
// fields, the Go type must do its own decoding / encoding.
func (c *checker) matchDeclared(dt declaredType, t types.Type, dtype dialect.Type, param bool) fit {
	if dt.named == "" {
		return fit{}
	}
	var matched *dialect.Type
	for cur := &dtype; cur != nil; cur = cur.Base {
		if cur.Named == dt.named {
			matched = cur
			break
		}
	}
	if matched == nil {
		return fit{}
	}
	if _, isStruct := t.Underlying().(*types.Struct); isStruct && matched.Kind != dialect.Composite {
		if !param && !implementsScanner(t) {
			return fit{ok: true, lossy: t.String() + " carries " + dt.pg + " but does not implement sql.Scanner: pgx cannot decode into it"}
		}
		if param && !implementsValuer(t) {
			return fit{ok: true, lossy: t.String() + " carries " + dt.pg + " but does not implement driver.Valuer: pgx cannot encode it"}
		}
	}
	return fit{ok: true}
}

func implementsScanner(t types.Type) bool {
	ms := types.NewMethodSet(types.NewPointer(t))
	sel := ms.Lookup(nil, "Scan")
	if sel == nil {
		return false
	}
	sig, ok := sel.Type().(*types.Signature)
	return ok && sig.Params().Len() == 1 && sig.Results().Len() == 1
}

func implementsValuer(t types.Type) bool {
	ms := types.NewMethodSet(t)
	sel := ms.Lookup(nil, "Value")
	if sel == nil {
		return false
	}
	sig, ok := sel.Type().(*types.Signature)
	return ok && sig.Params().Len() == 0 && sig.Results().Len() == 2
}
