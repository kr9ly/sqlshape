// Package vet is the go/analysis analyzer: it finds sqlshape.Query[R, P](literal)
// calls, expands each template, checks every expansion against schema.sql and
// matches result columns / parameters against R / P.
package vet

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/expand"
	"github.com/kr9ly/sqlshape/internal/schema"
)

const sqlshapePkg = "github.com/kr9ly/sqlshape"

// Analyzer is the sqlshape checker.
var Analyzer = &analysis.Analyzer{
	Name:     "sqlshape",
	Doc:      "checks every expansion of sqlshape.Query templates against schema.sql and the Go result / parameter types",
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

var (
	schemaPath  string
	strictFlag  bool
	noTables    bool
	schemasFlag string
	requireCols string
)

func init() {
	Analyzer.Flags.StringVar(&schemaPath, "schema", "", "path to schema.sql (default: nearest schema.sql above the package directory)")
	Analyzer.Flags.BoolVar(&noTables, "no-tables", false, "forbid direct table references: application code may only read views and call functions (tables are the database's private side)")
	Analyzer.Flags.StringVar(&schemasFlag, "schemas", "", "comma-separated schemas this code may reference (service boundary), e.g. a_api,b_private; empty allows all")
	Analyzer.Flags.StringVar(&requireCols, "require-columns", "", "comma-separated columns (e.g. tenant_id) every statement must pin by equality on each table that has them (row ownership); INSERTs must assign them")
	Analyzer.Flags.BoolVar(&strictFlag, "strict", false, "also report advisory findings: enum / domain / key columns carried by unnamed Go types, timestamp / date received as time.Time, non-pointer enum parameters (zero value is no label), parameters that always override a column DEFAULT, LIMIT without ORDER BY, enum ordering")
}

var (
	schemaMu    sync.Mutex
	schemaCache = map[string]*loadedSchema{}
)

// loadedSchema is a schema plus the findings about the schema itself, reported once per package.
type loadedSchema struct {
	s        *schema.Schema
	problems []string
}

func loadSchema(path string) (*loadedSchema, error) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if s, ok := schemaCache[path]; ok {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := schema.Load(string(b))
	if err != nil {
		return nil, err
	}
	ls := &loadedSchema{s: s}
	for _, p := range s.Problems {
		ls.problems = append(ls.problems, p.String())
	}
	// SQL function bodies are checked like PG does at CREATE time
	for _, fn := range s.Functions {
		if _, err := analyze.AnalyzeFunction(s, fn); err != nil {
			ls.problems = append(ls.problems, fmt.Sprintf("function %s: %v", fn.Name, err))
		}
	}
	// view bodies: sqlshape's own findings (policies, domains) are the view's
	for _, rel := range s.Relations {
		if rel.Kind != schema.View && rel.Kind != schema.MatView {
			continue
		}
		r, err := analyze.AnalyzeView(s, rel)
		if err != nil {
			ls.problems = append(ls.problems, fmt.Sprintf("view %s: %v", rel.Name, err))
			continue
		}
		for _, n := range r.Notes {
			if !n.Advisory() {
				ls.problems = append(ls.problems, fmt.Sprintf("view %s: %s", rel.Name, n.Message))
			}
		}
	}
	schemaCache[path] = ls
	return ls, nil
}

func findSchema(pass *analysis.Pass) (string, error) {
	if schemaPath != "" {
		return schemaPath, nil
	}
	if len(pass.Files) == 0 {
		return "", fmt.Errorf("no files")
	}
	dir := filepath.Dir(pass.Fset.File(pass.Files[0].Pos()).Name())
	for {
		p := filepath.Join(dir, "schema.sql")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("schema.sql not found above %s (use -schema)", filepath.Dir(pass.Fset.File(pass.Files[0].Pos()).Name()))
		}
		dir = parent
	}
}

type checker struct {
	pass     *analysis.Pass
	s        *schema.Schema
	strict   bool
	bindings map[*types.TypeName]*binding
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	var calls, matviews []*ast.CallExpr
	insp.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if isQueryCall(pass, call) {
			calls = append(calls, call)
		} else if isMatViewConversion(pass, call) {
			matviews = append(matviews, call)
		}
	})
	calls = append(calls, matviews...)
	if len(calls) == 0 {
		// still export constant sets so packages that use these types in queries can diff them
		(&checker{pass: pass, bindings: map[*types.TypeName]*binding{}}).exportConstSets()
		return nil, nil
	}
	path, err := findSchema(pass)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: %v", err)
		return nil, nil
	}
	ls, err := loadSchema(path)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: load %s: %v", path, err)
		return nil, nil
	}
	s := ls.s
	c := &checker{pass: pass, s: s, strict: strictFlag, bindings: map[*types.TypeName]*binding{}}
	for _, p := range ls.problems {
		pass.Reportf(calls[0].Pos(), "sqlshape: schema %s: %s", path, p)
	}
	if strictFlag {
		c.adviseSchema(calls[0].Pos())
	}
	for _, call := range calls[:len(calls)-len(matviews)] {
		c.checkCall(call)
	}
	for _, call := range matviews {
		c.checkMatView(call)
	}
	c.finishBindings()
	return nil, nil
}

// isQueryCall recognizes sqlshape.Query[R, P](...) and sqlshape.One[R, P](...).
func isQueryCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	var fun ast.Expr = call.Fun
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	obj, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == sqlshapePkg && (obj.Name() == "Query" || obj.Name() == "One")
}

// literal is where diagnostics about the template text land.
type literal struct {
	lit      *ast.BasicLit
	raw      bool // raw string: template offsets map 1:1 onto file positions
	text     string
	fallback token.Pos
}

func (l literal) pos(tmplOff int) token.Pos {
	if l.lit == nil || tmplOff < 0 || tmplOff > len(l.text) {
		return l.fallback
	}
	if l.raw {
		return l.lit.Pos() + token.Pos(1+tmplOff)
	}
	// interpreted string: walk the source, decoding escapes, until the template offset
	src := l.lit.Value
	decoded := 0
	for i := 1; i < len(src)-1; {
		if decoded >= tmplOff {
			return l.lit.Pos() + token.Pos(i)
		}
		if src[i] != '\\' {
			_, size := utf8.DecodeRuneInString(src[i:])
			decoded += size
			i += size
			continue
		}
		// an escape sequence: find its source length and decoded length
		var n, d int
		switch src[i+1] {
		case 'x':
			n, d = 4, 1
		case 'u':
			n, d = 6, utf8.RuneLen(runeOfHex(src[i+2:i+6]))
		case 'U':
			n, d = 10, utf8.RuneLen(runeOfHex(src[i+2:i+10]))
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n, d = 4, 1
		default:
			n, d = 2, 1
		}
		decoded += d
		i += n
	}
	return l.lit.Pos() + token.Pos(len(src)-1)
}

func runeOfHex(s string) rune {
	r, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return utf8.RuneError
	}
	return rune(r)
}

func (c *checker) checkCall(call *ast.CallExpr) {
	pass := c.pass
	// type arguments R, P
	var fun ast.Expr = call.Fun
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	ident := fun.(*ast.SelectorExpr).Sel
	single := ident.Name == "One"
	inst, ok := pass.TypesInfo.Instances[ident]
	if !ok || inst.TypeArgs.Len() != 2 {
		pass.Reportf(call.Pos(), "sqlshape: %s must be instantiated as %s[R, P]", ident.Name, ident.Name)
		return
	}
	rType, pType := inst.TypeArgs.At(0), inst.TypeArgs.At(1)

	if len(call.Args) != 1 {
		return
	}
	tv, ok := pass.TypesInfo.Types[call.Args[0]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		pass.Reportf(call.Args[0].Pos(), "sqlshape: query template must be a string constant")
		return
	}
	lit := literal{text: constant.StringVal(tv.Value), fallback: call.Args[0].Pos()}
	if bl, ok := call.Args[0].(*ast.BasicLit); ok && bl.Kind == token.STRING {
		lit.lit = bl
		lit.raw = strings.HasPrefix(bl.Value, "`")
	}

	res, err := expand.Expand(lit.text)
	if err != nil {
		if te, ok := err.(*expand.Error); ok {
			pass.Reportf(lit.pos(te.Pos), "sqlshape: template: %s", te.Msg)
		} else {
			pass.Reportf(lit.pos(0), "sqlshape: template: %v", err)
		}
		return
	}
	// control paths must exist on P
	for _, ctl := range res.Controls {
		if _, err := c.resolvePath(pType, ctl); err != nil {
			pass.Reportf(lit.pos(0), "sqlshape: %v", err)
		}
	}

	// One diagnostic per distinct message; the branch suffix (after " [") does not count
	// towards distinctness, so a problem shared by many expansions is reported once.
	seen := map[string]bool{}
	report := func(pos token.Pos, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		key := msg
		if i := strings.LastIndex(msg, " ["); i >= 0 && strings.HasSuffix(msg, "]") {
			key = msg[:i]
		}
		if seen[key] {
			return
		}
		seen[key] = true
		pass.Reportf(pos, "sqlshape: %s", msg)
	}
	multi := len(res.Expansions) > 1
	possible := map[string]analyze.Violation{}
	branch := map[string]string{}
	analyzedAll := true

	for i := range res.Expansions {
		e := &res.Expansions[i]
		where := ""
		if multi {
			where = " [" + e.Branch + "]"
		}
		r, err := analyze.Analyze(c.s, e.SQL)
		if err != nil {
			analyzedAll = false
			if ae, ok := err.(*analyze.Error); ok {
				tp := 0
				if ae.Position > 0 {
					tp = e.TemplatePos(int(ae.Position) - 1)
				}
				report(lit.pos(tp), "%s (SQLSTATE %s)%s", ae.Message, ae.Code, where)
			} else {
				report(lit.pos(0), "%v%s", err, where)
			}
			continue
		}
		c.checkReferences(e, r, lit, report, where)
		for _, v := range c.possibleViolations(e, r, pType) {
			if _, seen := possible[v.Key()]; !seen {
				possible[v.Key()] = v
				branch[v.Key()] = where
			}
		}
		for _, n := range r.Notes {
			if n.Advisory() && !c.strict {
				continue
			}
			tp := 0
			if n.Position > 0 {
				tp = e.TemplatePos(int(n.Position) - 1)
			}
			report(lit.pos(tp), "%s%s", n.Message, where)
		}
		if single && !r.AtMostOne {
			report(lit.pos(0), "One: cannot prove at most one row: %s%s", r.ManyRowsWhy, where)
		}
		c.checkParams(e, r, pType, lit, report, where)
		c.checkResult(call.Pos(), r, rType, lit, report, where)
	}
	if analyzedAll {
		c.checkExpectations(lit, possible, branch, report)
	}
}

// checkParams matches each $n's Go origin against the inferred PG parameter type.
func (c *checker) checkParams(e *expand.Expansion, r *analyze.Result, pType types.Type, lit literal, report func(token.Pos, string, ...any), where string) {
	for _, p := range e.Params {
		gt, err := c.resolvePath(pType, p.Path)
		if err != nil {
			report(lit.pos(p.Pos), "%v", err)
			continue
		}
		if p.N-1 >= len(r.Params) {
			continue
		}
		pg := r.Params[p.N-1]
		c.meet(gt, pg, r.ParamSources[p.N-1], lit.pos(p.Pos), "parameter "+p.Path.String())
		f := c.paramFit(pg, gt)
		switch {
		case !f.ok:
			report(lit.pos(p.Pos), "parameter %s is %s but SQL expects %s%s", p.Path, gt, c.s.Types.Format(pg), where)
		case f.lossy != "":
			report(lit.pos(p.Pos), "parameter %s: %s%s", p.Path, f.lossy, where)
		case f.unknown:
			report(lit.pos(p.Pos), "parameter %s: no known Go mapping for %s, not checked%s", p.Path, c.s.Types.Format(pg), where)
		}
		if c.strict && f.ok {
			c.adviseParam(p, gt, pg, r.ParamSources[p.N-1], lit, report, where)
		}
	}
}

// adviseParam reports advisory findings about a parameter (-strict).
func (c *checker) adviseParam(p expand.Param, gt types.Type, pg schema.TypeRef, src *analyze.Source, lit literal, report func(token.Pos, string, ...any), where string) {
	inner, nullable := unwrapNullable(gt)
	if msg := c.fidelity(pg, gt); msg != "" {
		report(lit.pos(p.Pos), "parameter %s: %s%s", p.Path, msg, where)
	}
	if t := c.s.Types.ByOID(c.s.Types.BaseOf(pg).OID); t != nil && t.Kind == 'e' && !nullable && inner != nil {
		report(lit.pos(p.Pos), "parameter %s is a non-pointer %s: its zero value \"\" is not a label of enum %s and fails at runtime (SQLSTATE 22P02) when unset%s", p.Path, gt, t.Name, where)
	}
	if src != nil && src.Assigned && !nullable {
		if rel := c.relByFullName(src.Table); rel != nil {
			if col := rel.Column(src.Column); col != nil {
				switch {
				case col.Default != nil:
					report(lit.pos(p.Pos), "parameter %s always sends a value into %s.%s, so its DEFAULT never applies: decide which side owns the default (make the column conditional with {{if}} to use the database's)%s", p.Path, rel.Name, col.Name, where)
				case col.Identity != 0:
					report(lit.pos(p.Pos), "parameter %s sends a value into %s.%s, which the database generates%s", p.Path, rel.Name, col.Name, where)
				}
			}
		}
	}
}

// fidelity says where a Go type receives a PG type faithfully but with an implicit
// interpretation the application then owns (advisory).
func (c *checker) fidelity(pg schema.TypeRef, gt types.Type) string {
	inner, _ := unwrapNullable(gt)
	if inner == nil || !isNamed(inner, "time", "Time") {
		return ""
	}
	switch c.s.Types.BaseOf(pg).OID {
	case catalog.Timestamp:
		return "timestamp without time zone into time.Time: which zone the value is in becomes the application's implicit choice (prefer timestamptz)"
	case catalog.Date:
		return "date into time.Time: a zone conversion can move the day (keep it at UTC midnight or use a civil date type)"
	}
	return ""
}

// checkResult matches result columns against R.
func (c *checker) checkResult(callPos token.Pos, r *analyze.Result, rType types.Type, lit literal, report func(token.Pos, string, ...any), where string) {
	at := callPos
	// `-- sqlshape: not null a, b` in the template overrides the analyzer's nullability
	overrides, _ := notNullOverrides(lit.text)
	for name, off := range overrides {
		found := false
		for i := range r.Columns {
			if r.Columns[i].Name == name {
				r.Columns[i].Nullable = false
				found = true
			}
		}
		if !found {
			report(lit.pos(off), "not null: the query has no result column %q%s", name, where)
		}
	}
	st, isStruct := rType.Underlying().(*types.Struct)
	if !isStruct || isNamed(rType, "time", "Time") {
		// scalar R: exactly one column
		if len(r.Columns) != 1 {
			report(at, "R is %s but the query returns %d columns%s", rType, len(r.Columns), where)
			return
		}
		col := r.Columns[0]
		c.meet(rType, col.Type, col.Source, at, "R")
		f := c.match(col.Type, rType)
		c.reportFit(report, at, "column "+col.Name, col, rType, f, where)
		if f.ok {
			c.checkNested(col, rType, at, "R", report, where)
		}
		return
	}
	// struct R: fields ↔ columns both ways
	fields := map[string]*types.Var{}
	notnull := map[string]bool{}
	order := []string{}
	for i := 0; i < st.NumFields(); i++ {
		fv := st.Field(i)
		if !fv.Exported() {
			continue
		}
		name, opts := columnName(fv, st.Tag(i))
		if name == "-" {
			continue
		}
		fields[name] = fv
		order = append(order, name)
		for _, o := range opts {
			if o == "notnull" {
				notnull[name] = true
			}
		}
	}
	matched := map[string]bool{}
	for _, col := range r.Columns {
		fv, ok := fields[col.Name]
		if !ok {
			// case-insensitive fallback
			for name, f := range fields {
				if strings.EqualFold(name, col.Name) {
					fv, ok = f, true
					col.Name = name
					break
				}
			}
		}
		if !ok {
			report(at, "result column %q has no field in %s%s", col.Name, rType, where)
			continue
		}
		matched[col.Name] = true
		c.meet(fv.Type(), col.Type, col.Source, at, "field "+fv.Name())
		f := c.match(col.Type, fv.Type())
		if notnull[col.Name] {
			col.Nullable = false
		}
		c.reportFit(report, at, "field "+fv.Name(), col, fv.Type(), f, where)
		if f.ok {
			c.checkNested(col, fv.Type(), at, "field "+fv.Name(), report, where)
			if msg := c.fidelity(col.Type, fv.Type()); msg != "" && c.strict {
				report(at, "field %s: %s%s", fv.Name(), msg, where)
			}
		}
	}
	missing := []string{}
	for _, name := range order {
		if !matched[name] {
			missing = append(missing, fields[name].Name())
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		report(at, "field %s.%s has no result column%s", typeName(rType), m, where)
	}
}

func (c *checker) reportFit(report func(token.Pos, string, ...any), at token.Pos, what string, col analyze.Column, gt types.Type, f fit, where string) {
	pgName := c.s.Types.Format(col.Type)
	switch {
	case !f.ok:
		report(at, "%s is %s but column %q is %s%s", what, gt, col.Name, pgName, where)
	case f.lossy != "":
		report(at, "%s: %s%s", what, f.lossy, where)
	case f.unknown:
		report(at, "%s: no known Go mapping for %s, not checked%s", what, pgName, where)
	}
	if f.ok && col.Nullable && !f.nullable {
		report(at, "%s is %s but column %q may be NULL (use a pointer, or tag it `col:\",notnull\"` if you know better)%s", what, gt, col.Name, where)
	}
}

func typeName(t types.Type) string {
	if n, ok := t.(*types.Named); ok {
		return n.Obj().Name()
	}
	return t.String()
}

// resolvePath walks a field path on P.
func (c *checker) resolvePath(t types.Type, p expand.Path) (types.Type, error) {
	cur := t
	for i, el := range p {
		if el == "#index" {
			return types.Typ[types.Int], nil
		}
		if ptr, ok := cur.(*types.Pointer); ok {
			cur = ptr.Elem()
		}
		if el == "[]" {
			switch u := cur.Underlying().(type) {
			case *types.Slice:
				cur = u.Elem()
			case *types.Array:
				cur = u.Elem()
			case *types.Map:
				cur = u.Elem()
			default:
				return nil, fmt.Errorf("%s is %s, cannot range over it", p[:i].String(), cur)
			}
			continue
		}
		st, ok := cur.Underlying().(*types.Struct)
		if !ok {
			return nil, fmt.Errorf("%s is %s, not a struct; cannot select .%s", p[:i].String(), cur, el)
		}
		var found *types.Var
		for j := 0; j < st.NumFields(); j++ {
			if st.Field(j).Name() == el {
				found = st.Field(j)
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%s has no field %s", cur, el)
		}
		cur = found.Type()
	}
	return cur, nil
}

// checkReferences enforces the reference policy flags on the relations an expansion reads or writes.
func (c *checker) checkReferences(e *expand.Expansion, r *analyze.Result, lit literal, report func(token.Pos, string, ...any), where string) {
	var allowed map[string]bool
	if schemasFlag != "" {
		allowed = map[string]bool{}
		for _, s := range strings.Split(schemasFlag, ",") {
			if s = strings.TrimSpace(s); s != "" {
				allowed[s] = true
			}
		}
	}
	for _, ref := range r.Relations {
		at := lit.pos(e.TemplatePos(int(ref.Position) - 1))
		name := ref.Name
		if ref.Schema != "public" {
			name = ref.Schema + "." + name
		}
		if noTables && ref.Kind == 'r' {
			report(at, "table %s is referenced directly; with -no-tables application code reads views and calls functions only%s", name, where)
		}
		if allowed != nil && !allowed[ref.Schema] {
			report(at, "%s is outside the schemas this code may reference (%s)%s", name, schemasFlag, where)
		}
		if requireCols != "" && ref.Kind == 'r' {
			c.checkRequiredColumns(ref, r, at, report, where)
		}
	}
}

// checkRequiredColumns enforces -require-columns on one referenced table.
func (c *checker) checkRequiredColumns(ref analyze.RelationRef, r *analyze.Result, at token.Pos, report func(token.Pos, string, ...any), where string) {
	rel := c.s.Relation(ref.Schema, ref.Name)
	if rel == nil {
		return
	}
	for _, col := range strings.Split(requireCols, ",") {
		col = strings.TrimSpace(col)
		if col == "" || rel.Column(col) == nil {
			continue
		}
		fixed := false
		for _, f := range r.Fixed {
			if f.Table == rel.FullName() && f.Column == col {
				fixed = true
			}
		}
		if !fixed {
			report(at, "%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)%s", rel.Name, col, rel.Name, col, where)
		}
	}
}

// adviseSchema reports advisory findings about the schema itself (-strict).
func (c *checker) adviseSchema(at token.Pos) {
	for _, rel := range c.s.Relations {
		if rel.Kind != schema.MatView {
			continue
		}
		unique := false
		for _, con := range rel.Constraints {
			if con.Kind == schema.Unique && con.Predicate == nil {
				unique = true
			}
		}
		if !unique {
			c.pass.Reportf(at, "sqlshape: schema: materialized view %s has no unique index, so REFRESH MATERIALIZED VIEW CONCURRENTLY is not possible", rel.FullName())
		}
	}
}

// isMatViewConversion recognizes sqlshape.MatView("name").
func isMatViewConversion(pass *analysis.Pass, call *ast.CallExpr) bool {
	tv, ok := pass.TypesInfo.Types[call.Fun]
	if !ok || !tv.IsType() {
		return false
	}
	return isNamed(tv.Type, sqlshapePkg, "MatView") && len(call.Args) == 1
}

// checkMatView verifies the named materialized view exists.
func (c *checker) checkMatView(call *ast.CallExpr) {
	tv, ok := c.pass.TypesInfo.Types[call.Args[0]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		c.pass.Reportf(call.Args[0].Pos(), "sqlshape: MatView name must be a string constant")
		return
	}
	name := constant.StringVal(tv.Value)
	sch, n := "", name
	if i := strings.LastIndex(name, "."); i >= 0 {
		sch, n = name[:i], name[i+1:]
	}
	rel := c.s.Relation(sch, n)
	switch {
	case rel == nil:
		c.pass.Reportf(call.Args[0].Pos(), "sqlshape: materialized view %q does not exist", name)
	case rel.Kind != schema.MatView:
		c.pass.Reportf(call.Args[0].Pos(), "sqlshape: %q is not a materialized view", name)
	}
}
