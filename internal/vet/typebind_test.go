package vet

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// implementsValuer is exercised end-to-end through analysistest in testdata/src/a (a
// declared type used as a parameter), but paramFit (gotypes.go) unconditionally clears
// fit.lossy after calling matchDir, so the "does not implement driver.Valuer" diagnostic
// that matchDeclared computes from it never reaches a reported diagnostic; only the
// success path (implements Valuer, so no lossy note at all) is visible there. This test
// checks implementsValuer itself directly, both ways.
func TestImplementsValuer(t *testing.T) {
	const src = `package p

type Good struct{}
func (Good) Value() (interface{}, error) { return nil, nil }

type GoodPtr struct{}
func (*GoodPtr) Value() (interface{}, error) { return nil, nil }

type NoMethod struct{}

type WrongResults struct{}
func (WrongResults) Value() error { return nil }

type WrongParams struct{}
func (WrongParams) Value(x int) (interface{}, error) { return nil, nil }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	conf := types.Config{Importer: importer.Default()}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	pkg, err := conf.Check("p", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	typeOf := func(name string) types.Type {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("no such type %s", name)
		}
		return obj.Type()
	}

	if !implementsValuer(typeOf("Good")) {
		t.Error("Good: value-receiver Value() (interface{}, error) should implement driver.Valuer's shape")
	}
	// GoodPtr's Value method has a pointer receiver: the value type itself does not carry it.
	if implementsValuer(typeOf("GoodPtr")) {
		t.Error("GoodPtr (value type): pointer-receiver Value should not count")
	}
	if implementsValuer(typeOf("NoMethod")) {
		t.Error("NoMethod should not implement driver.Valuer")
	}
	if implementsValuer(typeOf("WrongResults")) {
		t.Error("WrongResults: Value() error (one result) should not count")
	}
	if implementsValuer(typeOf("WrongParams")) {
		t.Error("WrongParams: Value(x int) (...) (one param) should not count")
	}
}
