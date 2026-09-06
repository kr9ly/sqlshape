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

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/expand"
	"github.com/kr9ly/sqlshape/internal/schema"
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
	col   analyze.Column
	count int // expansions that select it
}

// dtoParam is one parameter path seen across the expansions.
type dtoParam struct {
	path expand.Path
	pg   schema.TypeRef
	src  *analyze.Source
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
func (d *dto) addResult(r *analyze.Result) {
	d.analyzed++
	for _, col := range r.Columns {
		if col.Name == "?column?" || col.Type.OID == catalog.Void {
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
func (d *dto) addParams(e *expand.Expansion, r *analyze.Result) {
	for _, p := range e.Params {
		if p.N-1 >= len(r.Params) {
			continue
		}
		key := p.Path.String()
		if _, ok := d.params[key]; ok {
			continue
		}
		d.params[key] = &dtoParam{path: p.Path, pg: r.Params[p.N-1], src: r.ParamSources[p.N-1]}
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
// has for a column keeps its name and doc comment.
func (c *checker) resultStruct(d *dto, cur *types.Struct, node *ast.StructType) *generated {
	g := &generated{}
	existing := map[string]*ast.Field{}
	flat, _ := structFields(cur)
	for _, f := range flat {
		existing[f.col] = c.fieldNode(node, f.name)
	}
	for _, name := range d.colOrder {
		dc := d.cols[name]
		nullable := dc.col.Nullable || dc.count < d.analyzed
		typ := c.goType(g, dc.col, nullable, false)
		fname := exportedName(name)
		var doc string
		if f := existing[name]; f != nil {
			if len(f.Names) == 1 {
				fname = f.Names[0].Name
			}
			if f.Doc != nil {
				doc = strings.TrimSpace(f.Doc.Text())
			}
		}
		if doc == "" && dc.col.Source != nil {
			doc = c.s.Comments[dc.col.Source.Table+"."+dc.col.Source.Column]
		}
		line := fname + " " + typ
		if snake(fname) != name {
			line += fmt.Sprintf(" `col:%q`", name)
		}
		g.fields = append(g.fields, withDoc(doc, line))
	}
	return g
}

// paramStruct renders P from the parameter paths. Paths below the top level (`.A.B`,
// ranges) are not generated: nil.
func (c *checker) paramStruct(d *dto, res *expand.Result, cur *types.Struct, node *ast.StructType) *generated {
	optional := map[string]bool{}
	for _, ctl := range res.Controls {
		if len(ctl) == 1 {
			optional[ctl[0]] = true
		}
	}
	g := &generated{}
	seen := map[string]bool{}
	for _, key := range d.prmOrder {
		p := d.params[key]
		if len(p.path) != 1 || p.path[0] == "#index" {
			return nil
		}
		name := p.path[0]
		seen[name] = true
		col := analyze.Column{Name: name, Type: p.pg, Source: p.src}
		if nested := c.paramColumn(p.pg); nested != nil {
			col.Fields = nested.Fields
		}
		typ := c.goType(g, col, optional[name], true)
		g.fields = append(g.fields, withDoc(c.fieldDoc(node, name), name+" "+typ))
	}
	// a control read alone (`{{if .Verbose}}`) is a field too
	for _, ctl := range res.Controls {
		if len(ctl) != 1 || seen[ctl[0]] {
			continue
		}
		seen[ctl[0]] = true
		typ := "bool"
		if f := c.fieldNode(node, ctl[0]); f != nil {
			typ = types.ExprString(f.Type)
		}
		g.fields = append(g.fields, withDoc(c.fieldDoc(node, ctl[0]), ctl[0]+" "+typ))
	}
	return g
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

// goType renders the Go type for a column: the reverse of the type table. nullable
// wraps in a pointer unless the type carries NULL itself (slices, maps, pgtype.*).
func (c *checker) goType(g *generated, col analyze.Column, nullable, param bool) string {
	typ, selfNullable := c.goTypeValue(g, col, param)
	if nullable && !selfNullable {
		return "*" + typ
	}
	return typ
}

const pgtypePkg = "github.com/jackc/pgx/v5/pgtype"

func (c *checker) goTypeValue(g *generated, col analyze.Column, param bool) (typ string, selfNullable bool) {
	pg := col.Type
	// a Go type already bound to this enum / value set / domain / key is the one to use
	if n, ok := c.nominalOf(pg, col.Source); ok && n.kind != 'k' {
		if name := c.boundTypeName(g, n); name != "" {
			return name, false
		}
	}
	base := c.s.Types.BaseOf(pg)
	pt := c.s.Types.ByOID(base.OID)
	if pt == nil {
		return "any", false
	}
	// arrays
	if pt.Elem != 0 && strings.HasPrefix(pt.Name, "_") {
		elem := analyze.Column{Name: col.Name, Type: schema.TypeRef{OID: pt.Elem, Typmod: -1}, Fields: col.Fields}
		et, _ := c.goTypeValue(g, elem, param)
		return "[]" + et, true
	}
	pgt := func(name string) (string, bool) {
		g.need(pgtypePkg)
		return "pgtype." + name, true
	}
	switch base.OID {
	case catalog.Int2:
		return "int16", false
	case catalog.Int4:
		return "int32", false
	case catalog.Int8:
		return "int64", false
	case catalog.OIDType:
		return "uint32", false
	case catalog.Float4:
		return "float32", false
	case catalog.Float8:
		return "float64", false
	case catalog.Numeric:
		if c.imports("github.com/shopspring/decimal") {
			g.need("github.com/shopspring/decimal")
			return "decimal.Decimal", false
		}
		return pgt("Numeric")
	case catalog.Bool:
		return "bool", false
	case catalog.Text, catalog.Varchar, catalog.BPChar, catalog.Name, catalog.Char, catalog.Cstring:
		return "string", false
	case catalog.Bytea:
		return "[]byte", true
	case catalog.UUID:
		if c.imports("github.com/google/uuid") {
			g.need("github.com/google/uuid")
			return "uuid.UUID", false
		}
		return "string", false
	case catalog.Date, catalog.Timestamp, catalog.TimestampTZ, catalog.Time:
		g.need("time")
		return "time.Time", false
	case catalog.TimeTZ:
		return "string", false
	case catalog.Interval:
		g.need("time")
		return "time.Duration", false
	case catalog.JSON, catalog.JSONB:
		g.need("encoding/json")
		return "json.RawMessage", true // a slice: nil for NULL
	case catalog.Record:
		return c.rowStruct(g, col, param), false
	}
	switch pt.Kind {
	case 'e':
		return "string", false
	case 'c':
		return c.rowStruct(g, col, param), false
	case 'r':
		if rng := c.s.Types.RangeOf(base.OID); rng != nil {
			st, _ := c.goTypeValue(g, analyze.Column{Type: schema.TypeRef{OID: rng.Subtype, Typmod: -1}}, param)
			g.need(pgtypePkg)
			return "pgtype.Range[" + st + "]", true
		}
	case 'm':
		if rng := c.s.Types.RangeOfMulti(base.OID); rng != nil {
			st, _ := c.goTypeValue(g, analyze.Column{Type: schema.TypeRef{OID: rng.Subtype, Typmod: -1}}, param)
			g.need(pgtypePkg)
			return "pgtype.Multirange[pgtype.Range[" + st + "]]", true
		}
	}
	if pt.Schema == "" || pt.Schema == "pg_catalog" {
		switch pt.Name {
		case "inet", "cidr":
			g.need("net/netip")
			return "netip.Prefix", false
		case "macaddr", "macaddr8":
			g.need("net")
			return "net.HardwareAddr", true
		case "bit", "varbit":
			return pgt("Bits")
		case "point":
			return pgt("Point")
		case "lseg":
			return pgt("Lseg")
		case "path":
			return pgt("Path")
		case "box":
			return pgt("Box")
		case "polygon":
			return pgt("Polygon")
		case "line":
			return pgt("Line")
		case "circle":
			return pgt("Circle")
		case "tsvector":
			return pgt("TSVector")
		case "xml", "money", "tsquery", "jsonpath", "tid", "pg_lsn", "txid_snapshot", "pg_snapshot", "aclitem", "regclass", "regtype", "regproc", "regprocedure", "regoper", "regoperator", "regnamespace", "regrole", "regconfig", "regdictionary", "regcollation":
			return "string", false
		}
	}
	switch pt.Name {
	case "hstore":
		return "map[string]*string", true
	case "citext", "ltree", "lquery", "ltxtquery":
		return "string", false
	}
	return "any /* " + c.s.Types.Format(pg) + ": no known mapping */", true
}

// rowStruct renders a record / composite column as a nested struct of its fields.
func (c *checker) rowStruct(g *generated, col analyze.Column, param bool) string {
	if len(col.Fields) == 0 {
		return "struct{}"
	}
	var b strings.Builder
	b.WriteString("struct {\n")
	for _, f := range col.Fields {
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
