package vet

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
)

// Error names. A schema's `-- sqlshape: error <code> = <Name>` annotation (a trigger's
// or routine's own raised failure mode) gets a Go name by:
//
//	var OrderTooLarge = sqlshape.Error("30001")
//
// vet checks both directions: the declaration's own identifier must be the schema's Name
// for that code, and every code the schema declares must have a Go declaration for it
// somewhere in the program. The binding travels across packages the way a declared type's
// PG name does (typebind.go): a Fact on the var's object, exported here and imported by
// whatever other package refers to the var (typically a Violates(err, OrderTooLarge)
// call) -- so a "no Go declaration" diagnostic is only caught where a reference reaches
// this package, the same reach a declared type's binding has. A single-package program
// (every example in this repository, and vet's own testdata) is unaffected: every
// declaration and every reference to it are in the one package this analysis sees.

// ErrorDeclFact records a sqlshape.Error(code) declaration's code, for another package
// that refers to the var to check its own schema against.
type ErrorDeclFact struct{ Code string }

func (*ErrorDeclFact) AFact()           {}
func (f *ErrorDeclFact) String() string { return "sqlshape.Error(" + f.Code + ")" }

func init() {
	Analyzer.FactTypes = append(Analyzer.FactTypes, (*ErrorDeclFact)(nil))
}

// errorDecl is one `var X = sqlshape.Error(code)` declaration in this package.
type errorDecl struct {
	name string
	code string
	pos  token.Pos
}

// collectErrorDecls finds this package's own sqlshape.Error(code) declarations
// (package-level `var X = sqlshape.Error("code")`; a const is not recognized, since
// sqlshape.Error is not constant), exports each as a fact on its var object, and, when a
// schema is loaded, validates it: the code must be one the schema declares, under the
// schema's own Name for it, and no two declarations may claim the same code.
func (c *checker) collectErrorDecls() {
	c.errorDecls = map[*types.Var]errorDecl{}
	byCode := map[string]errorDecl{}
	for _, f := range c.pass.Files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				if len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				code, ok := c.errorCallCode(vs.Values[0])
				if !ok {
					continue
				}
				name := vs.Names[0]
				obj, ok := c.pass.TypesInfo.Defs[name].(*types.Var)
				if !ok {
					continue
				}
				decl := errorDecl{name: name.Name, code: code, pos: name.Pos()}
				if prev, dup := byCode[code]; dup {
					c.pass.Reportf(decl.pos, "sqlshape: %s and %s both declare sqlshape.Error(%q)", prev.name, decl.name, code)
				} else {
					byCode[code] = decl
				}
				c.errorDecls[obj] = decl
				c.pass.ExportObjectFact(obj, &ErrorDeclFact{Code: code})
				if c.sch != nil {
					c.checkErrorDecl(decl)
				}
			}
		}
	}
}

// errorCallCode recognizes sqlshape.Error("code") and returns the constant string
// argument.
func (c *checker) errorCallCode(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	fn, ok := c.pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != sqlshapePkg || fn.Name() != "Error" {
		return "", false
	}
	tv, ok := c.pass.TypesInfo.Types[call.Args[0]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// checkErrorDecl validates one declaration against the schema's own Errors().
func (c *checker) checkErrorDecl(decl errorDecl) {
	for _, e := range c.sch.Errors() {
		if e.Code != decl.code {
			continue
		}
		if e.Name != decl.name {
			c.pass.Reportf(decl.pos, "sqlshape: schema names %s's error %s %q, not %q", e.Subject, e.Code, e.Name, decl.name)
		}
		return
	}
	c.pass.Reportf(decl.pos, "sqlshape: the schema declares no error %q", decl.code)
}

// checkErrorDeclsCovered reports a named violation possible in this package (Name != "",
// so it is a `-- sqlshape: error` annotation, not a plain constraint) with no Go
// declaration this analysis can find: neither this package's own (collectErrorDecls) nor
// one declared elsewhere and referred to somewhere in this package (an ErrorDeclFact
// imported off any identifier this package resolves to another package's var). A code the
// schema declares that no statement in this package can ever raise is not checked -- the
// way an enum label nothing binds is not (binding.go) -- so a schema shared with unrelated
// packages (a test fixture, say) does not force every one of them to declare every error.
// Reported at an expect line that names the code or its Name, when this package has one;
// the fallback position otherwise (there is nothing better to point at without one).
func (c *checker) checkErrorDeclsCovered(fallback token.Pos) {
	if len(c.possibleErrors) == 0 {
		return
	}
	covered := map[string]bool{}
	for _, d := range c.errorDecls {
		covered[d.code] = true
	}
	for _, f := range c.pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			v, ok := c.pass.TypesInfo.Uses[id].(*types.Var)
			if !ok || v.Pkg() == nil || v.Pkg() == c.pass.Pkg {
				return true
			}
			var fct ErrorDeclFact
			if c.pass.ImportObjectFact(v, &fct) {
				covered[fct.Code] = true
			}
			return true
		})
	}
	var codes []string
	for k := range c.possibleErrors {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	for _, k := range codes {
		if covered[k] {
			continue
		}
		v := c.possibleErrors[k]
		pos := fallback
		if p, ok := c.expectPos[k]; ok {
			pos = p
		} else if p, ok := c.expectPos[v.Name]; ok {
			pos = p
		}
		c.pass.Reportf(pos, "sqlshape: %s (%s) has no `var %s = sqlshape.Error(%q)` declared in this program", k, v.Detail, v.Name, k)
	}
}
