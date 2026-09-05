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
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"

	"github.com/kr9ly/sqlshape/internal/analyze"
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

var schemaPath string

func init() {
	Analyzer.Flags.StringVar(&schemaPath, "schema", "", "path to schema.sql (default: nearest schema.sql above the package directory)")
}

var (
	schemaMu    sync.Mutex
	schemaCache = map[string]*schema.Schema{}
)

func loadSchema(path string) (*schema.Schema, error) {
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
	schemaCache[path] = s
	return s, nil
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
	pass *analysis.Pass
	s    *schema.Schema
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	var calls []*ast.CallExpr
	insp.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if isQueryCall(pass, call) {
			calls = append(calls, call)
		}
	})
	if len(calls) == 0 {
		return nil, nil
	}
	path, err := findSchema(pass)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: %v", err)
		return nil, nil
	}
	s, err := loadSchema(path)
	if err != nil {
		pass.Reportf(calls[0].Pos(), "sqlshape: load %s: %v", path, err)
		return nil, nil
	}
	c := &checker{pass: pass, s: s}
	for _, p := range s.Problems {
		pass.Reportf(calls[0].Pos(), "sqlshape: schema %s: %s", path, p)
	}
	for _, call := range calls {
		c.checkCall(call)
	}
	return nil, nil
}

// isQueryCall recognizes sqlshape.Query[R, P](...).
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
	return obj.Pkg().Path() == sqlshapePkg && obj.Name() == "Query"
}

// literal is where diagnostics about the template text land.
type literal struct {
	lit      *ast.BasicLit
	raw      bool // raw string: template offsets map 1:1 onto file positions
	text     string
	fallback token.Pos
}

func (l literal) pos(tmplOff int) token.Pos {
	if l.lit != nil && l.raw && tmplOff >= 0 && tmplOff <= len(l.text) {
		return l.lit.Pos() + token.Pos(1+tmplOff)
	}
	return l.fallback
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
	inst, ok := pass.TypesInfo.Instances[ident]
	if !ok || inst.TypeArgs.Len() != 2 {
		pass.Reportf(call.Pos(), "sqlshape: Query must be instantiated as Query[R, P]")
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

	for i := range res.Expansions {
		e := &res.Expansions[i]
		where := ""
		if multi {
			where = " [" + e.Branch + "]"
		}
		r, err := analyze.Analyze(c.s, e.SQL)
		if err != nil {
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
		c.checkParams(e, r, pType, lit, report, where)
		c.checkResult(call.Pos(), r, rType, report, where)
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
		f := c.paramFit(pg, gt)
		switch {
		case !f.ok:
			report(lit.pos(p.Pos), "parameter %s is %s but SQL expects %s%s", p.Path, gt, c.s.Types.Format(pg), where)
		case f.lossy != "":
			report(lit.pos(p.Pos), "parameter %s: %s%s", p.Path, f.lossy, where)
		case f.unknown:
			report(lit.pos(p.Pos), "parameter %s: no known Go mapping for %s, not checked%s", p.Path, c.s.Types.Format(pg), where)
		}
	}
}

// checkResult matches result columns against R.
func (c *checker) checkResult(callPos token.Pos, r *analyze.Result, rType types.Type, report func(token.Pos, string, ...any), where string) {
	at := callPos
	st, isStruct := rType.Underlying().(*types.Struct)
	if !isStruct || isNamed(rType, "time", "Time") {
		// scalar R: exactly one column
		if len(r.Columns) != 1 {
			report(at, "R is %s but the query returns %d columns%s", rType, len(r.Columns), where)
			return
		}
		col := r.Columns[0]
		f := c.match(col.Type, rType)
		c.reportFit(report, at, "column "+col.Name, col, rType, f, where)
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
		f := c.match(col.Type, fv.Type())
		if notnull[col.Name] {
			col.Nullable = false
		}
		c.reportFit(report, at, "field "+fv.Name(), col, fv.Type(), f, where)
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
