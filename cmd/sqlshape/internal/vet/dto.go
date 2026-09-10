package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/analysis"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	pgdialect "github.com/kr9ly/sqlshape/check/postgres/v2/dialect"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/expand"
)

// Declaring R and P from the query. Every diagnostic about the result struct not fitting
// the query's columns, or the parameter struct not fitting the template's `{{.X}}` paths,
// carries a suggested fix that rewrites the struct from the query: the columns of every
// expansion (a column some branch leaves out becomes a pointer), typed by the reverse of
// the type table (nullable → pointer, enums and value sets → the Go type already bound to
// them, records → nested structs), field names kept where a column already has a field,
// doc comments taken from COMMENT ON. Editors surface it as a quick fix, so a query's DTO
// is written by typing `type Row struct{}` (every column is then a missing field) and
// accepting.

// dtoColumn is one result column seen across the expansions of a call.
type dtoColumn struct {
	col   dialect.Column
	count int // expansions that select it
}

// dtoParam is one parameter path seen across the expansions.
type dtoParam struct {
	path expand.Path
	typ  dialect.Type
	src  *dialect.Source
}

// heldDiag is a diagnostic waiting for its quick fix.
type heldDiag struct {
	pos token.Pos
	msg string
}

// dto collects, over the expansions of one call, what R and P would have to be, and the
// diagnostics about them.
type dto struct {
	cols     map[string]*dtoColumn
	colOrder []string
	params   map[string]*dtoParam
	prmOrder []string
	analyzed int
	rDiags   []heldDiag // about R's fit: emitted with the R rewrite attached
	pDiags   []heldDiag // about P's fit: with the P rewrite
}

func newDTO() *dto {
	return &dto{cols: map[string]*dtoColumn{}, params: map[string]*dtoParam{}}
}

// addResult records the columns of one analyzed expansion.
func (d *dto) addResult(c *checker, r *analyze.Result) {
	d.analyzed++
	for _, ac := range r.Columns {
		col := pgdialect.ColumnOf(c.s, ac)
		if col.Name == "" || col.Type.Kind == dialect.Void {
			continue
		}
		if dc, ok := d.cols[col.Name]; ok {
			dc.count++
			continue
		}
		d.cols[col.Name] = &dtoColumn{col: col, count: 1}
		d.colOrder = append(d.colOrder, col.Name)
	}
}

// addParams records the parameters of one expansion.
func (d *dto) addParams(c *checker, e *expand.Expansion, r *analyze.Result) {
	for _, p := range e.Params {
		if p.N-1 >= len(r.Params) {
			continue
		}
		key := p.Path.String()
		if _, ok := d.params[key]; ok {
			continue
		}
		prm := pgdialect.ParamOf(c.s, r.Params[p.N-1], r.ParamSources[p.N-1])
		d.params[key] = &dtoParam{path: p.Path, typ: prm.Type, src: prm.Source}
		d.prmOrder = append(d.prmOrder, key)
	}
}

// emitHeld reports the held diagnostics about R and P, each with the rewrite of its
// struct as a suggested fix when one can be made (a struct literal or a struct type
// declared in this package, with columns / top-level paths to write).
func (c *checker) emitHeld(call *ast.CallExpr, d *dto, res *expand.Result, rType, pType types.Type) {
	rExpr, pExpr := typeArgExprs(call)
	var rFix, pFix []analysis.SuggestedFix
	if rExpr != nil && len(d.rDiags) > 0 && len(d.cols) > 0 {
		if st, ok := rType.Underlying().(*types.Struct); ok && !isNamed(rType, "time", "Time") {
			if target := c.structNode(rExpr); target != nil {
				rFix = c.structFix(target, rType, c.resultStruct(d, st, target))
			}
		}
	}
	if pExpr != nil && len(d.pDiags) > 0 {
		if st, ok := pType.Underlying().(*types.Struct); ok {
			if target := c.structNode(pExpr); target != nil {
				if g := c.paramStruct(d, res, st, target); g != nil {
					pFix = c.structFix(target, pType, g)
				}
			}
		}
	}
	for _, h := range d.rDiags {
		c.pass.Report(analysis.Diagnostic{Pos: h.pos, Message: "sqlshape: " + h.msg, SuggestedFixes: rFix})
	}
	for _, h := range d.pDiags {
		c.pass.Report(analysis.Diagnostic{Pos: h.pos, Message: "sqlshape: " + h.msg, SuggestedFixes: pFix})
	}
}

// typeArgExprs returns the R and P type expressions of a Query[R, P] / One[R, P] call.
func typeArgExprs(call *ast.CallExpr) (r, p ast.Expr) {
	if ix, ok := call.Fun.(*ast.IndexListExpr); ok && len(ix.Indices) == 2 {
		return ix.Indices[0], ix.Indices[1]
	}
	return nil, nil
}

// structNode is the struct type literal a type argument stands for: the literal itself,
// or the body of a `type X struct{...}` declared in this package. Nil when it is neither
// (an imported type, an alias of something else).
func (c *checker) structNode(expr ast.Expr) *ast.StructType {
	switch e := expr.(type) {
	case *ast.StructType:
		return e
	case *ast.Ident:
		obj, ok := c.pass.TypesInfo.Uses[e].(*types.TypeName)
		if !ok || obj.Pkg() != c.pass.Pkg {
			return nil
		}
		spec, _ := c.typeSpec(obj.Pos())
		if spec == nil {
			return nil
		}
		st, _ := spec.Type.(*ast.StructType)
		return st
	}
	return nil
}

// generated is a struct body with the imports its field types need.
type generated struct {
	fields  []string // rendered field lines, without indentation
	imports map[string]bool
}

func (g *generated) need(path string) {
	if g.imports == nil {
		g.imports = map[string]bool{}
	}
	g.imports[path] = true
}

// structFix is the rewrite of a struct literal as a suggested fix, imports included.
func (c *checker) structFix(target *ast.StructType, t types.Type, g *generated) []analysis.SuggestedFix {
	file := c.fileOf(target.Pos())
	if file == nil {
		return nil
	}
	pos := c.pass.Fset.Position(target.Pos())
	indent := strings.Repeat("\t", max(pos.Column-1, 0))
	var b strings.Builder
	b.WriteString("struct {\n")
	for _, f := range g.fields {
		for _, line := range strings.Split(f, "\n") {
			b.WriteString(indent + "\t" + line + "\n")
		}
	}
	b.WriteString(indent + "}")
	edits := []analysis.TextEdit{{Pos: target.Pos(), End: target.End(), NewText: []byte(b.String())}}
	edits = append(edits, importEdits(file, g.imports)...)
	return []analysis.SuggestedFix{{
		Message:   fmt.Sprintf("Declare %s from the query", typeName(t)),
		TextEdits: edits,
	}}
}

// fileOf finds the file containing pos.
func (c *checker) fileOf(pos token.Pos) *ast.File {
	for _, f := range c.pass.Files {
		if f.Pos() <= pos && pos <= f.End() {
			return f
		}
	}
	return nil
}

// importEdits adds the imports the file lacks.
func importEdits(file *ast.File, paths map[string]bool) []analysis.TextEdit {
	var missing []string
	for p := range paths {
		found := false
		for _, im := range file.Imports {
			if strings.Trim(im.Path.Value, `"`) == p {
				found = true
			}
		}
		if !found {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var b strings.Builder
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if gd.Lparen.IsValid() {
			for _, p := range missing {
				b.WriteString("\t\"" + p + "\"\n")
			}
			return []analysis.TextEdit{{Pos: gd.Rparen, End: gd.Rparen, NewText: []byte(b.String())}}
		}
		// a single unparenthesized import becomes a block
		b.WriteString("import (\n")
		for _, im := range gd.Specs {
			b.WriteString("\t" + types.ExprString(im.(*ast.ImportSpec).Path) + "\n")
		}
		for _, p := range missing {
			b.WriteString("\t\"" + p + "\"\n")
		}
		b.WriteString(")")
		return []analysis.TextEdit{{Pos: gd.Pos(), End: gd.End(), NewText: []byte(b.String())}}
	}
	b.WriteString("\n\nimport (\n")
	for _, p := range missing {
		b.WriteString("\t\"" + p + "\"\n")
	}
	b.WriteString(")")
	at := file.Name.End()
	return []analysis.TextEdit{{Pos: at, End: at, NewText: []byte(b.String())}}
}

// resultStruct renders R from the collected columns. A field the current struct already
// has for a column keeps its name, its doc comment and its tag, and its type too when
// that type fits the column (a type the user chose from the table's alternatives stays).
func (c *checker) resultStruct(d *dto, cur *types.Struct, node *ast.StructType) *generated {
	g := &generated{}
	type existingField struct {
		node *ast.Field
		v    *types.Var
	}
	existing := map[string]existingField{}
	flat, _ := structFields(cur)
	for _, f := range flat {
		existing[f.col] = existingField{c.fieldNode(node, f.name), f.v}
	}
	for _, name := range d.colOrder {
		dc := d.cols[name]
		nullable := dc.col.Nullable || dc.count < d.analyzed
		fname := exportedName(name)
		var doc, typ, tag string
		if f := existing[name]; f.node != nil {
			if len(f.node.Names) == 1 {
				fname = f.node.Names[0].Name
			}
			if f.node.Doc != nil {
				doc = strings.TrimSpace(f.node.Doc.Text())
			}
			if fit := c.fitPG(dc.col.Type, f.v.Type(), false); fit.ok && fit.lossy == "" && !fit.unknown && (!nullable || fit.nullable) {
				typ = types.ExprString(f.node.Type)
				if f.node.Tag != nil {
					tag = " " + f.node.Tag.Value
				}
			}
		}
		if typ == "" {
			typ = c.goType(g, dc.col, nullable, false)
			if snake(fname) != name {
				tag = fmt.Sprintf(" `col:%q`", name)
			}
		}
		if doc == "" && dc.col.Source != nil {
			doc = dc.col.Source.Comment
		}
		g.fields = append(g.fields, withDoc(doc, fname+" "+typ+tag))
	}
	return g
}

// paramStruct renders P from the parameter and control paths, nested: `.Filter.Name`
// makes a Filter struct field, `{{range .Items}}{{.Sku}}{{end}}` an Items slice of structs,
// `{{range .Tags}}{{.}}{{end}}` a Tags slice of the element type. A top-level field the
// current struct already has keeps its type when every path under it resolves and fits.
func (c *checker) paramStruct(d *dto, res *expand.Result, cur *types.Struct, node *ast.StructType) *generated {
	root := &pathNode{}
	for _, key := range d.prmOrder {
		p := d.params[key]
		if n := root.at(p.path); n != nil {
			n.leaf = p
		}
	}
	for _, ctl := range res.Controls {
		if n := root.at(ctl); n != nil {
			n.control = true
		}
	}
	g := &generated{}
	for _, name := range root.order {
		child := root.fields[name]
		typ := ""
		if f := c.fieldNode(node, name); f != nil && c.pathsFit(cur, child, expand.Path{name}) {
			typ = types.ExprString(f.Type)
		}
		if typ == "" {
			typ = c.pathType(g, child, false)
		}
		g.fields = append(g.fields, withDoc(c.fieldDoc(node, name), name+" "+typ))
	}
	return g
}

// pathNode is one step of the parameter paths: a struct (fields), a slice (elem), a value
// (leaf), a condition (control), or several of these at once.
type pathNode struct {
	fields  map[string]*pathNode
	order   []string
	elem    *pathNode
	leaf    *dtoParam
	control bool
}

// at walks (creating) the node a path names; nil for a path the tree cannot hold.
func (n *pathNode) at(p expand.Path) *pathNode {
	cur := n
	for _, el := range p {
		switch el {
		case "#index":
			return nil
		case "[]":
			if cur.elem == nil {
				cur.elem = &pathNode{}
			}
			cur = cur.elem
		default:
			if cur.fields == nil {
				cur.fields = map[string]*pathNode{}
			}
			if cur.fields[el] == nil {
				cur.fields[el] = &pathNode{}
				cur.order = append(cur.order, el)
			}
			cur = cur.fields[el]
		}
	}
	return cur
}

// pathType renders the Go type of a node. A node read both as a value and as a condition
// (`{{if .X}} ... {{.X}} ...`) is optional: a pointer unless the type is nil-able itself.
func (c *checker) pathType(g *generated, n *pathNode, param bool) string {
	switch {
	case n.elem != nil:
		return "[]" + c.pathType(g, n.elem, param)
	case len(n.fields) > 0:
		var b strings.Builder
		b.WriteString("struct {\n")
		for _, name := range n.order {
			b.WriteString("\t" + name + " " + strings.ReplaceAll(c.pathType(g, n.fields[name], param), "\n", "\n\t") + "\n")
		}
		b.WriteString("}")
		return b.String()
	case n.leaf != nil:
		return c.goType(g, dialect.Column{Type: n.leaf.typ, Source: n.leaf.src}, n.control, true)
	default:
		return "bool"
	}
}

// pathsFit reports whether every leaf under n resolves on t along its path and fits its
// parameter type (so the declared type can stay).
func (c *checker) pathsFit(t types.Type, n *pathNode, prefix expand.Path) bool {
	if n.leaf != nil || (n.control && n.elem == nil && len(n.fields) == 0) {
		gt, err := c.resolvePath(t, prefix)
		if err != nil {
			return false
		}
		if n.leaf != nil {
			fit := c.fitPG(n.leaf.typ, gt, true)
			if !fit.ok || fit.lossy != "" || fit.unknown || (n.control && !fit.nullable) {
				return false
			}
		}
	}
	if n.elem != nil && !c.pathsFit(t, n.elem, append(append(expand.Path{}, prefix...), "[]")) {
		return false
	}
	for _, name := range n.order {
		if !c.pathsFit(t, n.fields[name], append(append(expand.Path{}, prefix...), name)) {
			return false
		}
	}
	return true
}

func withDoc(doc, line string) string {
	if doc == "" {
		return line
	}
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(doc), "\n") {
		b.WriteString("// " + strings.TrimSpace(l) + "\n")
	}
	return b.String() + line
}

// fieldNode finds the field named name in a struct literal.
func (c *checker) fieldNode(node *ast.StructType, name string) *ast.Field {
	for _, f := range node.Fields.List {
		for _, n := range f.Names {
			if n.Name == name {
				return f
			}
		}
	}
	return nil
}

func (c *checker) fieldDoc(node *ast.StructType, name string) string {
	if f := c.fieldNode(node, name); f != nil && f.Doc != nil {
		return strings.TrimSpace(f.Doc.Text())
	}
	return ""
}

// exportedName turns a column name into a Go field name: snake_case → CamelCase, with
// the usual initialisms upper-cased.
func exportedName(col string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(col, func(r rune) bool { return r == '_' || r == ' ' || r == '-' }) {
		switch strings.ToLower(part) {
		case "id", "uid", "url", "uri", "ip", "json", "sql", "uuid", "html", "api", "db":
			b.WriteString(strings.ToUpper(part))
		default:
			rs := []rune(part)
			rs[0] = unicode.ToUpper(rs[0])
			b.WriteString(string(rs))
		}
	}
	if b.Len() == 0 {
		return "Field"
	}
	name := b.String()
	if !unicode.IsLetter([]rune(name)[0]) {
		name = "F" + name
	}
	return name
}

// goType renders the Go type for a column: the dialect's first choice, a bound type where
// the column carries a nominal type. nullable wraps in a pointer unless the type carries
// NULL itself (slices, maps, pgtype.*).
func (c *checker) goType(g *generated, col dialect.Column, nullable, param bool) string {
	typ, selfNullable := c.goTypeValue(g, col, param)
	if nullable && !selfNullable {
		return "*" + typ
	}
	return typ
}

func (c *checker) goTypeValue(g *generated, col dialect.Column, param bool) (typ string, selfNullable bool) {
	// a Go type already bound to this enum / value set / domain / key is the one to use
	if n, ok := nominalOf(col.Type, col.Source); ok && n.kind != 'k' {
		if name := c.boundTypeName(g, n); name != "" {
			return name, false
		}
	}
	return c.renderType(g, col.Type, param)
}

// renderType renders the Go type the dialect lists first for the type — or, among the
// listed options, the one whose package the program already imports (a decimal type, a
// uuid type), so that a generated struct follows the program's own choices.
func (c *checker) renderType(g *generated, dt dialect.Type, param bool) (string, bool) {
	// the value's canonical Go type is the result list's first, in either direction (the
	// parameter list is ordered by what fits, narrow types first)
	list := dt.Result
	if len(list) == 0 {
		list = dt.Param
	}
	if len(list) == 0 {
		return "any /* " + dt.Name + ": no known mapping */", true
	}
	chosen := list[0].Go
	for _, opt := range list {
		if path := spellingPkg(opt.Go); path != "" && strings.Contains(path, ".") && !strings.HasSuffix(path, "pgtype") && c.imports(path) {
			chosen = opt.Go
			break
		}
	}
	return c.renderSpelling(g, chosen, dt, param)
}

// renderSpelling writes one GoSpelling as Go source, importing what it needs; the second
// result says whether the type carries NULL itself (a slice, a map, a Valid-bearing value).
func (c *checker) renderSpelling(g *generated, spell string, dt dialect.Type, param bool) (string, bool) {
	switch {
	case spell == "struct":
		return c.rowStruct(g, dt.Fields, param), false
	case spell == "json":
		return "any", true
	case spell == "[]byte":
		return "[]byte", true
	case strings.HasPrefix(spell, "map["):
		return spell, true
	case spell == "[]$elem":
		if dt.Elem == nil {
			return "[]any", true
		}
		et, _ := c.renderType(g, *dt.Elem, param)
		return "[]" + et, true
	case strings.HasSuffix(spell, "[$elem]"):
		base, _ := c.renderSpelling(g, strings.TrimSuffix(spell, "[$elem]"), dt, param)
		et := "any"
		if dt.Elem != nil {
			et, _ = c.renderType(g, *dt.Elem, param)
		}
		return base + "[" + et + "]", true
	case strings.HasPrefix(spell, "[") && strings.HasSuffix(spell, "]byte"):
		return spell, false
	}
	if path := spellingPkg(spell); path != "" {
		g.need(path)
		name := spell[strings.LastIndexByte(spell, '.')+1:]
		pkg := path[strings.LastIndexByte(path, '/')+1:]
		self := strings.HasSuffix(path, "pgtype") || spell == "encoding/json.RawMessage" || spell == "net.HardwareAddr"
		return pkg + "." + name, self
	}
	return spell, false
}

// spellingPkg is the package path of a named-type spelling ("net/netip.Addr" → "net/netip"),
// "" for anything else.
func spellingPkg(spell string) string {
	if strings.HasPrefix(spell, "[") || strings.HasPrefix(spell, "map[") || !strings.ContainsAny(spell, "./") {
		return ""
	}
	return spell[:strings.LastIndexByte(spell, '.')]
}

// rowStruct renders a record / composite column as a nested struct of its fields.
func (c *checker) rowStruct(g *generated, fields []dialect.Column, param bool) string {
	if len(fields) == 0 {
		return "struct{}"
	}
	var b strings.Builder
	b.WriteString("struct {\n")
	for _, f := range fields {
		b.WriteString("\t" + exportedName(f.Name) + " " + c.goType(g, f, f.Nullable, param) + "\n")
	}
	b.WriteString("}")
	return b.String()
}

// boundTypeName is the Go type bound to a nominal type, as this package spells it
// (qualified and imported when it belongs to another package); "" when none is.
func (c *checker) boundTypeName(g *generated, n nominal) string {
	var names []string
	for tn, b := range c.bindings {
		if b.kind != n.kind || b.key != n.key {
			continue
		}
		if tn.Pkg() == c.pass.Pkg {
			names = append(names, tn.Name())
		} else if tn.Pkg() != nil {
			g.need(tn.Pkg().Path())
			names = append(names, tn.Pkg().Name()+"."+tn.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// imports reports whether any file of the package imports path.
func (c *checker) imports(path string) bool {
	for _, f := range c.pass.Files {
		for _, im := range f.Imports {
			if strings.Trim(im.Path.Value, `"`) == path {
				return true
			}
		}
	}
	return false
}
