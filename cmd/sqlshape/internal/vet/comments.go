package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/kr9ly/sqlshape/check/postgres/analyze"
)

// COMMENT ON in schema.sql is the documentation of a column or table. With -sync-comments
// the checker suggests it as the doc comment of the Go field / type that receives it, as
// a quick fix gopls can apply, so "what does this column mean" is answered by the schema
// and shows up on hover in the editor.

// suggestFieldComment proposes the column's COMMENT as the doc comment of an undocumented field.
func (c *checker) suggestFieldComment(fv *types.Var, col analyze.Column) {
	if col.Source == nil {
		return
	}
	comment := c.s.Comments[col.Source.Table+"."+col.Source.Column]
	if comment == "" {
		return
	}
	field, _ := c.fieldDecl(fv.Pos())
	if field == nil || field.Doc != nil {
		return
	}
	c.suggestDoc(field.Pos(), fmt.Sprintf("field %s", fv.Name()), comment)
}

// suggestTypeComment proposes the table's COMMENT for an undocumented result type whose
// rows all come from one relation.
func (c *checker) suggestTypeComment(rType types.Type, r *analyze.Result) {
	named, ok := rType.(*types.Named)
	if !ok || named.Obj().Pkg() != c.pass.Pkg {
		return
	}
	table := ""
	for _, col := range r.Columns {
		if col.Source == nil || (table != "" && col.Source.Table != table) {
			return
		}
		table = col.Source.Table
	}
	comment := c.s.Comments[table]
	if table == "" || comment == "" {
		return
	}
	spec, decl := c.typeSpec(named.Obj().Pos())
	if spec == nil || spec.Doc != nil || (decl != nil && decl.Doc != nil) {
		return
	}
	at := spec.Pos()
	if decl != nil && len(decl.Specs) == 1 {
		at = decl.Pos()
	}
	c.suggestDoc(at, "type "+named.Obj().Name(), comment)
}

// suggestDoc emits the diagnostic with the insertion as a suggested fix.
func (c *checker) suggestDoc(at token.Pos, what, comment string) {
	pos := c.pass.Fset.Position(at)
	indent := strings.Repeat("\t", max(pos.Column-1, 0))
	var text strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(comment), "\n") {
		text.WriteString("// " + strings.TrimSpace(line) + "\n" + indent)
	}
	c.pass.Report(analysis.Diagnostic{
		Pos:     at,
		Message: fmt.Sprintf("sqlshape: %s has no doc comment; the schema says: %s", what, strings.TrimSpace(comment)),
		SuggestedFixes: []analysis.SuggestedFix{{
			Message:   "Add the schema's COMMENT as doc comment",
			TextEdits: []analysis.TextEdit{{Pos: at, End: at, NewText: []byte(text.String())}},
		}},
	})
}

// fieldDecl finds the struct field declared at pos.
func (c *checker) fieldDecl(pos token.Pos) (*ast.Field, *ast.File) {
	for _, f := range c.pass.Files {
		if f.Pos() > pos || pos > f.End() {
			continue
		}
		var found *ast.Field
		ast.Inspect(f, func(n ast.Node) bool {
			if fd, ok := n.(*ast.Field); ok {
				for _, name := range fd.Names {
					if name.Pos() == pos {
						found = fd
					}
				}
			}
			return found == nil
		})
		return found, f
	}
	return nil, nil
}

// typeSpec finds the type declaration whose name is at pos.
func (c *checker) typeSpec(pos token.Pos) (*ast.TypeSpec, *ast.GenDecl) {
	for _, f := range c.pass.Files {
		if f.Pos() > pos || pos > f.End() {
			continue
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok && ts.Name.Pos() == pos {
					return ts, gd
				}
			}
		}
	}
	return nil, nil
}
