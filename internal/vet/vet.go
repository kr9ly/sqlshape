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
	"reflect"
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
	"github.com/kr9ly/sqlshape/internal/consumers"
	"github.com/kr9ly/sqlshape/internal/expand"
	"github.com/kr9ly/sqlshape/internal/schema"
)

const sqlshapePkg = "github.com/kr9ly/sqlshape"

// Analyzer is the sqlshape checker.
var Analyzer = &analysis.Analyzer{
	Name:       "sqlshape",
	Doc:        "checks every expansion of sqlshape.Query templates against schema.sql and the Go result / parameter types",
	Run:        run,
	Requires:   []*analysis.Analyzer{inspect.Analyzer},
	ResultType: reflect.TypeOf((*consumers.Index)(nil)),
}

var (
	schemaPath   string
	strictFlag   bool
	noTables     bool
	noTableReads bool
	rawSQLFlag   string
	rawSQLAllow  string
	schemasFlag  string
	requireCols  string
	coverageFlag bool
	syncComments bool
)

func init() {
	Analyzer.Flags.StringVar(&schemaPath, "schema", "", "path to schema.sql, or to a directory whose *.sql files apply in name order (default: the nearest schema.sql or schema/ above the package directory)")
	Analyzer.Flags.BoolVar(&noTables, "no-tables", false, "forbid direct table references: application code may only read views and call functions (tables are the database's private side)")
	Analyzer.Flags.BoolVar(&noTableReads, "no-table-reads", false, "forbid reading tables: SELECTs (and the reading parts of writes) go through views; a table may still be the target of INSERT / UPDATE / DELETE / MERGE")
	Analyzer.Flags.StringVar(&rawSQLFlag, "raw-sql", "constant", "driver calls (pgx / database/sql Query, Exec, ...) outside sqlshape: constant requires their SQL to be a constant string, forbid rejects them, allow ignores them")
	Analyzer.Flags.StringVar(&rawSQLAllow, "raw-sql-allow", "", "comma-separated package paths (or prefixes ending in /...) where -raw-sql=forbid does not apply")
	Analyzer.Flags.StringVar(&schemasFlag, "schemas", "", "comma-separated schemas this code may reference (service boundary), e.g. a_api,b_private; empty allows all")
	Analyzer.Flags.StringVar(&requireCols, "require-columns", "", "comma-separated columns (e.g. tenant_id) every statement must pin by equality on each table that has them (row ownership); INSERTs must assign them")
	Analyzer.Flags.BoolVar(&coverageFlag, "coverage", false, "report per package how many Query / One declarations were checked and how many could not be (non-constant templates)")
	Analyzer.Flags.BoolVar(&syncComments, "sync-comments", false, "suggest doc comments for result struct fields and types from the schema's COMMENT ON (apply with sqlshape -fix)")
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
	// fnRefs are the relations each analyzable function body references (adviseSchema:
	// SECURITY DEFINER functions past row-level security)
	fnRefs map[*schema.Function][]analyze.RelationRef
	// fnAdvice are the advisory notes of function bodies (a PL/pgSQL EXECUTE of a string
	// built at run time), reported with -strict
	fnAdvice map[*schema.Function][]analyze.Note
}

func loadSchema(path string) (*loadedSchema, error) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if s, ok := schemaCache[path]; ok {
		return s, nil
	}
	src, err := schema.ReadSource(path)
	if err != nil {
		return nil, err
	}
	s, err := analyze.Load(src)
	if err != nil {
		return nil, err
	}
	ls := &loadedSchema{s: s, fnRefs: map[*schema.Function][]analyze.RelationRef{}, fnAdvice: map[*schema.Function][]analyze.Note{}}
	for _, p := range s.Problems {
		ls.problems = append(ls.problems, p.String())
	}
	// function bodies (LANGUAGE sql and plpgsql) are checked like PG does at CREATE time
	for _, fn := range s.Functions {
		fr, err := analyze.AnalyzeFunction(s, fn)
		if err != nil {
			ls.problems = append(ls.problems, fmt.Sprintf("function %s: %v", fn.Name, err))
			continue
		}
		ls.fnRefs[fn] = fr.Relations
		for _, n := range fr.Notes {
			if n.Advisory() {
				ls.fnAdvice[fn] = append(ls.fnAdvice[fn], n)
			} else {
				ls.problems = append(ls.problems, fmt.Sprintf("function %s: %s", fn.Name, n.Message))
			}
		}
	}
	// row-level security policies: predicates type-checked like CREATE POLICY does, and
	// policies on a table whose row security is off do not apply at all
	for _, rel := range s.Relations {
		for _, pol := range rel.Policies {
			notes, err := analyze.AnalyzePolicy(s, rel, pol)
			if err != nil {
				ls.problems = append(ls.problems, fmt.Sprintf("policy %s on %s: %v", pol.Name, rel.FullName(), err))
			}
			for _, n := range notes {
				if !n.Advisory() {
					ls.problems = append(ls.problems, fmt.Sprintf("policy %s on %s: %s", pol.Name, rel.FullName(), n.Message))
				}
			}
		}
		if len(rel.Policies) > 0 && !rel.RowSecurity {
			ls.problems = append(ls.problems, fmt.Sprintf("%s has policies but row level security is not enabled, so they do not apply: ALTER TABLE %s ENABLE ROW LEVEL SECURITY", rel.FullName(), rel.FullName()))
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
		// a schema.sql file, or a schema/ directory of *.sql files applied in name order
		for _, name := range []string{"schema.sql", "schema"} {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && (fi.IsDir() == (name == "schema")) {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("schema.sql (or a schema/ directory) not found above %s (use -schema)", filepath.Dir(pass.Fset.File(pass.Files[0].Pos()).Name()))
		}
		dir = parent
	}
}

type checker struct {
	pass     *analysis.Pass
	s        *schema.Schema
	ls       *loadedSchema
	strict   bool
	bindings map[*types.TypeName]*binding
	// constDecls: this package's constant declarations, for mapping concatenated templates back to source
	constDecls map[*types.Const]ast.Expr
	// declared: Go types that name the PG type they carry (`// sqlshape: type X`), local and imported
	declared map[*types.TypeName]declaredType
	// unchecked counts Query / One calls whose template is not a constant (-coverage)
	unchecked int
	// index collects the relation columns this package's statements depend on (the
	// analyzer's result); owners names the declaration each call sits in
	index  *consumers.Index
	owners map[*ast.CallExpr]string
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	index := consumers.New()
	owners := map[*ast.CallExpr]string{}
	var calls, matviews, copies, all []*ast.CallExpr
	insp.WithStack([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push {
			return false
		}
		call := n.(*ast.CallExpr)
		all = append(all, call)
		switch {
		case isQueryCall(pass, call):
			calls = append(calls, call)
		case isMatViewConversion(pass, call):
			matviews = append(matviews, call)
		case isCopyCall(pass, call):
			copies = append(copies, call)
		default:
			return true
		}
		owners[call] = owner(pass, stack)
		return true
	})
	matviews = append(matviews, copies...) // checked after the statements, like matviews
	calls = append(calls, matviews...)
	checkRawSQL(pass, all)
	if len(calls) == 0 {
		// still export constant sets and declared type bindings so packages that use these types in queries can check them
		c := &checker{pass: pass, bindings: map[*types.TypeName]*binding{}}
		c.exportConstSets()
		c.collectDeclaredTypes()
		return index, nil
	}
	path, err := findSchema(pass)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: %v", err)
		return index, nil
	}
	ls, err := loadSchema(path)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: load %s: %v", path, err)
		return index, nil
	}
	s := ls.s
	c := &checker{pass: pass, s: s, ls: ls, strict: strictFlag, bindings: map[*types.TypeName]*binding{}, index: index, owners: owners}
	c.collectDeclaredTypes()
	for _, p := range ls.problems {
		pass.Reportf(calls[0].Pos(), "sqlshape: schema %s: %s", path, p)
	}
	if strictFlag {
		c.adviseSchema(calls[0].Pos())
	}
	for _, call := range calls[:len(calls)-len(matviews)] {
		c.checkCall(call)
	}
	if coverageFlag {
		n := len(calls) - len(matviews)
		pass.Reportf(calls[0].Pos(), "sqlshape: coverage: %d of %d statements checked, %d unchecked (non-constant templates)", n-c.unchecked, n, c.unchecked)
	}
	for _, call := range matviews {
		if isCopyCall(pass, call) {
			c.checkCopy(call)
		} else {
			c.checkMatView(call)
		}
	}
	c.finishBindings()
	return index, nil
}

// owner names the declaration a call sits in: "pkg.Func", "pkg.(*T).Method", "pkg.var";
// the package path alone at the top level of an expression outside any declaration.
func owner(pass *analysis.Pass, stack []ast.Node) string {
	pkg := pass.Pkg.Path()
	for i := len(stack) - 1; i >= 0; i-- {
		switch d := stack[i].(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 {
				return pkg + "." + types.ExprString(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			return pkg + "." + d.Name.Name
		case *ast.ValueSpec:
			if len(d.Names) > 0 {
				return pkg + "." + d.Names[0].Name
			}
		}
	}
	return pkg
}

// site is the consumer index entry for a position in the SQL of call.
func (c *checker) site(call *ast.CallExpr, at token.Pos) consumers.Site {
	return consumers.Site{Pos: c.pass.Fset.Position(at), Owner: c.owners[call]}
}

// record adds one expansion's relation and column uses to the index.
func (c *checker) record(call *ast.CallExpr, e *expand.Expansion, r *analyze.Result, lit literal) {
	for _, ref := range r.Relations {
		name := ref.Name
		if ref.Schema != "public" {
			name = ref.Schema + "." + name
		}
		c.index.AddRelation(name, c.site(call, lit.pos(e.TemplatePos(int(ref.Position)-1))))
	}
	for _, u := range r.Uses {
		c.index.AddColumn(u.Table, u.Column, c.site(call, lit.pos(e.TemplatePos(int(u.Position)-1))))
	}
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

// literal is where diagnostics about the template text land. A template may be one
// string literal or a constant expression concatenating literals and named constants
// (shared SQL fragments); each piece is a segment mapping a range of template offsets
// back to its literal, so a diagnostic lands in the fragment it is about.
type literal struct {
	text     string
	segs     []segment
	fallback token.Pos
}

// segment is one piece of the template text: [start, end) offsets and their source.
type segment struct {
	start, end int
	lit        *ast.BasicLit // nil: a constant without a literal in this package
	raw        bool          // raw string: template offsets map 1:1 onto file positions
	fallback   token.Pos
}

func (l literal) pos(tmplOff int) token.Pos {
	if tmplOff < 0 || tmplOff > len(l.text) {
		return l.fallback
	}
	for i, sg := range l.segs {
		if tmplOff < sg.end || (i == len(l.segs)-1 && tmplOff == sg.end) {
			if sg.lit == nil {
				return sg.fallback
			}
			return litPos(sg.lit, sg.raw, tmplOff-sg.start)
		}
	}
	return l.fallback
}

// litPos maps an offset into a literal's decoded text onto the literal's source.
func litPos(lit *ast.BasicLit, raw bool, off int) token.Pos {
	if raw {
		return lit.Pos() + token.Pos(1+off)
	}
	// interpreted string: walk the source, decoding escapes, until the template offset
	src := lit.Value
	decoded := 0
	for i := 1; i < len(src)-1; {
		if decoded >= off {
			return lit.Pos() + token.Pos(i)
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
	return lit.Pos() + token.Pos(len(src)-1)
}

// segments maps a constant string expression onto its literals: literals directly,
// concatenations piecewise, named constants through their declaration in this package
// (constants from other packages become one segment landing on the reference).
func (c *checker) segments(e ast.Expr, start int) []segment {
	tv, ok := c.pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return nil
	}
	n := len(constant.StringVal(tv.Value))
	switch x := e.(type) {
	case *ast.ParenExpr:
		return c.segments(x.X, start)
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			return []segment{{start: start, end: start + n, lit: x, raw: strings.HasPrefix(x.Value, "`")}}
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			left := c.segments(x.X, start)
			if len(left) == 0 {
				break
			}
			right := c.segments(x.Y, left[len(left)-1].end)
			if len(right) == 0 {
				break
			}
			return append(left, right...)
		}
	case *ast.Ident, *ast.SelectorExpr:
		var id *ast.Ident
		if sel, ok := x.(*ast.SelectorExpr); ok {
			id = sel.Sel
		} else {
			id = x.(*ast.Ident)
		}
		if k, ok := c.pass.TypesInfo.Uses[id].(*types.Const); ok {
			if decl := c.constDecl(k); decl != nil {
				if segs := c.segments(decl, start); len(segs) > 0 {
					return segs
				}
			}
		}
	}
	return []segment{{start: start, end: start + n, fallback: e.Pos()}}
}

// constDecl is the value expression declaring k in this package, or nil.
func (c *checker) constDecl(k *types.Const) ast.Expr {
	if c.constDecls == nil {
		c.constDecls = map[*types.Const]ast.Expr{}
		for _, f := range c.pass.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					if len(vs.Values) != len(vs.Names) {
						continue
					}
					for i, name := range vs.Names {
						if obj, ok := c.pass.TypesInfo.Defs[name].(*types.Const); ok {
							c.constDecls[obj] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	return c.constDecls[k]
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
		c.unchecked++
		pass.Reportf(call.Args[0].Pos(), "sqlshape: query template must be a string constant")
		return
	}
	lit := literal{text: constant.StringVal(tv.Value), fallback: call.Args[0].Pos(), segs: c.segments(call.Args[0], 0)}

	res, err := expand.Expand(lit.text)
	if err != nil {
		if te, ok := err.(*expand.Error); ok {
			pass.Reportf(lit.pos(te.Pos), "sqlshape: template: %s", te.Msg)
		} else {
			pass.Reportf(lit.pos(0), "sqlshape: template: %v", err)
		}
		return
	}
	if c.strict {
		c.reportUnusedParams(pType, res, call.Pos())
	}
	checkActionPlacement(lit.text, lit, func(pos token.Pos, format string, args ...any) {
		pass.Reportf(pos, "sqlshape: "+format, args...)
	})

	// One diagnostic per distinct message; the branch suffix (after " [") does not count
	// towards distinctness, so a problem shared by many expansions is reported once.
	seen := map[string]bool{}
	dedupe := func(msg string) (string, bool) {
		key := msg
		if i := strings.LastIndex(msg, " ["); i >= 0 && strings.HasSuffix(msg, "]") {
			key = msg[:i]
		}
		if seen[key] {
			return "", false
		}
		seen[key] = true
		return msg, true
	}
	report := func(pos token.Pos, format string, args ...any) {
		if msg, ok := dedupe(fmt.Sprintf(format, args...)); ok {
			pass.Reportf(pos, "sqlshape: %s", msg)
		}
	}
	multi := len(res.Expansions) > 1
	if res.Sparse && c.strict {
		pass.Reportf(lit.pos(0), "sqlshape: %d branch combinations exceed %d: checked sparsely (all branches off, all on, each on alone); the runtime cannot compare renderings with the checked set", res.Combinations, expand.MaxExpansions)
	}
	possible := map[string]analyze.Violation{}
	branch := map[string]string{}
	analyzedAll := true
	analyzed := 0
	missing := map[string]int{}
	missingType := map[string]types.Type{}
	missingBranch := map[string]string{}
	d := newDTO()
	// diagnostics about R's fit and P's fit are held back and emitted at the end with the
	// rewrite of the struct (dto.go) attached as their quick fix
	reportR := func(pos token.Pos, format string, args ...any) {
		if msg, ok := dedupe(fmt.Sprintf(format, args...)); ok {
			d.rDiags = append(d.rDiags, heldDiag{pos, msg})
		}
	}
	reportP := func(pos token.Pos, format string, args ...any) {
		if msg, ok := dedupe(fmt.Sprintf(format, args...)); ok {
			d.pDiags = append(d.pDiags, heldDiag{pos, msg})
		}
	}
	// control paths must exist on P
	for _, ctl := range res.Controls {
		if _, err := c.resolvePath(pType, ctl); err != nil {
			reportP(lit.pos(0), "%v", err)
		}
	}

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
		c.record(call, e, r, lit)
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
		c.checkParams(e, r, pType, lit, reportP, where)
		checkBareOrderBy(e, lit, report, where)
		d.addParams(e, r)
		d.addResult(r)
		for name, t := range c.checkResult(call.Pos(), r, rType, lit, reportR, where) {
			missing[name]++
			missingType[name] = t
			if _, ok := missingBranch[name]; !ok {
				missingBranch[name] = where
			}
		}
		analyzed++
	}
	// optional projection: a field some branches do not select must be nullable; a field no
	// branch selects is a mistake
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if missing[name] == analyzed {
			reportR(call.Pos(), "field %s.%s has no result column", typeName(rType), name)
			continue
		}
		if _, nullable := unwrapNullable(missingType[name]); !nullable {
			if _, isSlice := missingType[name].Underlying().(*types.Slice); !isSlice {
				reportR(call.Pos(), "field %s.%s is not selected in every branch%s: make it a pointer so those branches leave it nil", typeName(rType), name, missingBranch[name])
			}
		}
	}
	c.emitHeld(call, d, res, rType, pType)
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
		if f.ok {
			// a composite (or composite[]) parameter: the struct's fields must line up with the type's columns
			if col := c.paramColumn(pg); col != nil {
				c.checkNested(*col, gt, lit.pos(p.Pos), "parameter "+p.Path.String(), report, where, true)
			}
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

// checkResult matches result columns against R. It returns the struct fields that had
// no result column in this expansion (name → type); checkCall decides whether that is
// an error (missing everywhere) or an optional projection (missing in some branches,
// allowed for nullable fields).
func (c *checker) checkResult(callPos token.Pos, r *analyze.Result, rType types.Type, lit literal, report func(token.Pos, string, ...any), where string) map[string]types.Type {
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
	// a void column (SELECT some_procedure_like_function(...)) carries nothing: it binds to no field
	cols := r.Columns[:0:0]
	for _, col := range r.Columns {
		if col.Type.OID != catalog.Void {
			cols = append(cols, col)
		}
	}
	r.Columns = cols
	st, isStruct := rType.Underlying().(*types.Struct)
	if !isStruct || isNamed(rType, "time", "Time") {
		// scalar R: exactly one column
		if len(r.Columns) != 1 {
			report(at, "R is %s but the query returns %d columns%s", rType, len(r.Columns), where)
			return nil
		}
		col := r.Columns[0]
		c.meet(rType, col.Type, col.Source, at, "R")
		f := c.match(col.Type, rType)
		c.reportFit(report, at, "column "+col.Name, col, rType, f, where)
		if f.ok {
			c.checkNested(col, rType, at, "R", report, where, false)
		}
		return nil
	}
	if syncComments {
		c.suggestTypeComment(rType, r)
	}
	// struct R: every column needs a distinct name to bind to a field
	byName := map[string]int{}
	for i, col := range r.Columns {
		if col.Name == "?column?" {
			report(at, "result column %d has no name: give it an alias (... AS name) so it can bind to a field of %s%s", i+1, rType, where)
			continue
		}
		if j, dup := byName[col.Name]; dup {
			report(at, "result columns %d and %d are both named %q: alias one of them (... AS other_name)%s", j+1, i+1, col.Name, where)
			continue
		}
		byName[col.Name] = i
	}
	// fields ↔ columns both ways (embedded structs flattened)
	flat, dups := structFields(st)
	for _, d := range dups {
		report(at, "%s: fields %s%s", rType, d, where)
	}
	fields := map[string]*types.Var{}
	fieldName := map[string]string{}
	notnull := map[string]bool{}
	order := []string{}
	for _, f := range flat {
		fields[f.col] = f.v
		fieldName[f.col] = f.name
		order = append(order, f.col)
		for _, o := range f.opts {
			if o == "notnull" {
				notnull[f.col] = true
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
			if col.Name != "?column?" {
				report(at, "result column %q has no field in %s%s", col.Name, rType, where)
			}
			continue
		}
		matched[col.Name] = true
		if syncComments {
			c.suggestFieldComment(fv, col)
		}
		fname := fieldName[col.Name]
		c.meet(fv.Type(), col.Type, col.Source, at, "field "+fname)
		f := c.match(col.Type, fv.Type())
		if notnull[col.Name] {
			col.Nullable = false
		}
		c.reportFit(report, at, "field "+fname, col, fv.Type(), f, where)
		if f.ok {
			c.checkNested(col, fv.Type(), at, "field "+fname, report, where, false)
			if msg := c.fidelity(col.Type, fv.Type()); msg != "" && c.strict {
				report(at, "field %s: %s%s", fname, msg, where)
			}
		}
	}
	missing := map[string]types.Type{}
	for _, name := range order {
		if !matched[name] {
			missing[fieldName[name]] = fields[name].Type()
		}
	}
	return missing
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
		if _, ok := cur.Underlying().(*types.Struct); !ok {
			return nil, fmt.Errorf("%s is %s, not a struct; cannot select .%s", p[:i].String(), cur, el)
		}
		// promoted fields of embedded structs resolve too, as text/template (and the runtime) do
		obj, _, _ := types.LookupFieldOrMethod(cur, true, nil, el)
		found, _ := obj.(*types.Var)
		if found == nil || !found.IsField() {
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
		if noTableReads && !noTables && ref.Kind == 'r' && !ref.Target {
			report(at, "table %s is read directly; with -no-table-reads application code reads views (tables are written, not read)%s", name, where)
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
		if fixed {
			continue
		}
		// a row-level security policy that fixes the column pins it for every statement;
		// unless forced, though, not for the table's owner
		if pol := policyPins(rel, col); pol != nil {
			if c.strict && !rel.ForceRowSecurity {
				report(at, "%s.%s is pinned by policy %s for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner%s", rel.Name, col, pol.Name, where)
			}
			continue
		}
		report(at, "%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)%s", rel.Name, col, rel.Name, col, where)
	}
}

// adviseSchema reports advisory findings about the schema itself (-strict).
func (c *checker) adviseSchema(at token.Pos) {
	c.adviseRowSecurity(at)
	for _, fn := range c.s.Functions {
		for _, n := range c.ls.fnAdvice[fn] {
			c.pass.Reportf(at, "sqlshape: schema: function %s: %s", fn.Name, n.Message)
		}
	}
	for _, rel := range c.s.Relations {
		if rel.Kind == schema.Table {
			// a value set kept as an enum cannot lose or reorder a label without the type
			// being rebuilt under every column (see migrate); a seeded lookup table changes
			// with a MERGE, its rows can carry a label and an order, and the checker reads
			// it just as well
			for _, col := range rel.Columns {
				if t := c.s.Types.ByOID(col.Type.OID); t != nil && t.Kind == 'e' && !rel.Temp {
					c.pass.Reportf(at, "sqlshape: schema: %s.%s is enum %s: a seeded lookup table (rows in schema.sql, referenced by a foreign key) is easier to change — an enum cannot drop or reorder a label without being recreated under every column — and is checked the same way", rel.FullName(), col.Name, t.Name)
				}
			}
		}
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

// adviseRowSecurity: the ways a row-level security setup does less than it looks like.
func (c *checker) adviseRowSecurity(at token.Pos) {
	for _, rel := range c.s.Relations {
		if rel.Kind != schema.Table || !rel.RowSecurity {
			continue
		}
		if len(rel.Policies) == 0 {
			c.pass.Reportf(at, "sqlshape: schema: %s has row level security enabled and no policy: every role but the owner sees no rows", rel.FullName())
		}
		for _, pol := range rel.Policies {
			for _, name := range analyze.SettingReads(pol) {
				c.pass.Reportf(at, "sqlshape: schema: policy %s on %s reads current_setting(%q, true): a session that never set it gets NULL, so the predicate hides every row silently; without missing_ok the session fails loudly instead", pol.Name, rel.FullName(), name)
			}
		}
		if rel.ForceRowSecurity {
			continue
		}
		// a SECURITY DEFINER function runs as its owner, whom the policies do not bind
		// unless the table forces them
		for fn, refs := range c.ls.fnRefs {
			if !fn.SecurityDefiner {
				continue
			}
			for _, ref := range refs {
				if ref.Schema == rel.Schema && ref.Name == rel.Name {
					c.pass.Reportf(at, "sqlshape: schema: function %s is SECURITY DEFINER and reaches %s, whose policies do not bind the owner: rows are unrestricted inside it (ALTER TABLE %s FORCE ROW LEVEL SECURITY applies them)", fn.Name, rel.FullName(), rel.FullName())
					break
				}
			}
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
	default:
		c.index.AddRelation(rel.FullName(), c.site(call, call.Args[0].Pos()))
	}
}

// reportUnusedParams (advisory) names the fields of P no expansion reads, as a value
// action or in a condition: a parameter the SQL never sees is a dead field or a typo.
func (c *checker) reportUnusedParams(pType types.Type, res *expand.Result, at token.Pos) {
	inner, _ := unwrapNullable(pType)
	if inner == nil {
		return
	}
	st, ok := inner.Underlying().(*types.Struct)
	if !ok {
		return
	}
	used := map[string]bool{}
	mark := func(p expand.Path) {
		for _, el := range p {
			if el != "" && el != "[]" && el != "#index" && el[0] != '$' {
				used[el] = true
				return
			}
		}
	}
	for _, e := range res.Expansions {
		for _, p := range e.Params {
			mark(p.Path)
		}
	}
	for _, p := range res.Controls {
		mark(p)
	}
	var walk func(st *types.Struct)
	walk = func(st *types.Struct) {
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if f.Embedded() {
				if inner := embeddedStruct(f, st.Tag(i)); inner != nil {
					walk(inner) // promoted fields are referenced by their own name
					continue
				}
			}
			if f.Exported() && !used[f.Name()] {
				c.pass.Reportf(at, "sqlshape: parameter field %s is never used by the template", f.Name())
			}
		}
	}
	walk(st)
}
