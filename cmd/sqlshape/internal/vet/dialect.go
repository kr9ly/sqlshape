package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/cardinality"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/expand"
)

// The dialect path: a schema that declares a dialect other than PostgreSQL is judged
// through internal/dialect. It checks what every dialect's Result can say — the statement
// analyzes, its result columns fit R by name, type and nullability, its parameters fit P —
// and, over its Facts, the One proof (x/cardinality). Obligations, violations, bindings and
// the quick fixes stay on the PostgreSQL path until the Result carries what they need.

// runDialect is run's tail for a non-PostgreSQL schema.
func (c *checker) runDialect(calls, matviews []*ast.CallExpr) {
	for _, call := range calls[:len(calls)-len(matviews)] {
		c.checkCallDialect(call)
	}
	for _, call := range matviews {
		what := "MatView"
		if isCopyCall(c.pass, call) {
			what = "Copy"
		}
		c.pass.Reportf(call.Pos(), "sqlshape: %s is not supported for %s", what, c.ls.dialectName)
	}
	if coverageFlag {
		n := len(calls) - len(matviews)
		c.pass.Reportf(calls[0].Pos(), "sqlshape: coverage: %d of %d statements checked, %d unchecked (non-constant templates)", n-c.unchecked, n, c.unchecked)
	}
}

func (c *checker) checkCallDialect(call *ast.CallExpr) {
	pass := c.pass
	cs, ok := c.template(call)
	if !ok {
		return
	}
	rType, pType, lit, res := cs.rType, cs.pType, cs.lit, cs.res
	if c.strict {
		c.reportUnusedParams(pType, res, call.Pos())
	}
	dd := &deduper{seen: map[string]int{}, expansions: len(res.Expansions)}
	var held []heldDiag
	report := func(pos token.Pos, format string, args ...any) {
		if msg, ok := dd.add(fmt.Sprintf(format, args...)); ok {
			held = append(held, heldDiag{pos, msg})
		}
	}
	defer func() {
		for _, h := range held {
			pass.Reportf(h.pos, "sqlshape: %s", dd.finish(h.msg))
		}
	}()
	multi := len(res.Expansions) > 1
	if res.Sparse && c.strict {
		pass.Reportf(lit.pos(0), "sqlshape: %d branch combinations exceed %d: checked sparsely (all branches off, all on, each on alone); the runtime cannot compare renderings with the checked set", res.Combinations, expand.MaxExpansions)
	}
	for _, ctl := range res.Controls {
		if _, err := c.resolvePath(pType, ctl); err != nil {
			report(lit.pos(0), "%v", err)
		}
	}
	analyzed := 0
	missing := map[string]int{}
	missingType := map[string]types.Type{}
	missingBranch := map[string]string{}
	for i := range res.Expansions {
		e := &res.Expansions[i]
		where := ""
		if multi {
			where = " [" + e.Branch + "]"
		}
		checkActionPlacement(e, lit, report, where)
		r, err := c.ls.dialect.Analyze(e.SQL)
		if err != nil {
			if de, ok := err.(*dialect.Error); ok {
				tp := 0
				if de.Position >= 0 {
					tp = e.TemplatePos(de.Position)
				}
				report(lit.pos(tp), "%v%s", de, where)
			} else {
				report(lit.pos(0), "%v%s", err, where)
			}
			continue
		}
		if cs.single {
			if ok, why := cardinality.AtMostOne(r.Facts); !ok {
				report(lit.pos(0), "One: cannot prove at most one row: %s%s", why, where)
			}
		}
		c.checkParamsDialect(e, r, pType, lit, report, where)
		for name, t := range c.checkResultDialect(call.Pos(), r, rType, lit, report, where) {
			missing[name]++
			missingType[name] = t
			if _, ok := missingBranch[name]; !ok {
				missingBranch[name] = where
			}
		}
		analyzed++
	}
	// optional projection, as on the PostgreSQL path: a field some branches do not select
	// must be nullable; a field no branch selects is a mistake
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if missing[name] == analyzed {
			report(call.Pos(), "field %s.%s has no result column", typeName(rType), name)
			continue
		}
		if _, nullable := unwrapNullable(missingType[name]); !nullable {
			if _, isSlice := missingType[name].Underlying().(*types.Slice); !isSlice {
				report(call.Pos(), "field %s.%s is not selected in every branch%s: make it a pointer so those branches leave it nil", typeName(rType), name, missingBranch[name])
			}
		}
	}
}

// checkParamsDialect matches each $n's Go origin against the dialect's parameter type.
func (c *checker) checkParamsDialect(e *expand.Expansion, r *dialect.Result, pType types.Type, lit literal, report func(token.Pos, string, ...any), where string) {
	for _, p := range e.Params {
		gt, err := c.resolvePath(pType, p.Path)
		if err != nil {
			report(lit.pos(p.Pos), "%v", err)
			continue
		}
		if p.N-1 >= len(r.Params) {
			continue
		}
		dt := r.Params[p.N-1].Type
		f := fitType(dt, gt, true, c.ls.dialect.Traits())
		switch {
		case !f.ok:
			report(lit.pos(p.Pos), "parameter %s is %s but SQL expects %s%s", p.Path, gt, dt.Name, where)
		case f.unknown:
			report(lit.pos(p.Pos), "parameter %s: no known Go mapping for %s, not checked%s", p.Path, dt.Name, where)
		case f.lossy != "" && c.strict:
			report(lit.pos(p.Pos), "parameter %s: %s%s", p.Path, f.lossy, where)
		}
	}
}

// checkResultDialect matches result columns against R and returns the struct fields no
// column of this expansion fed (name → type), like checkResult.
func (c *checker) checkResultDialect(at token.Pos, r *dialect.Result, rType types.Type, lit literal, report func(token.Pos, string, ...any), where string) map[string]types.Type {
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
		if len(r.Columns) != 1 {
			report(at, "R is %s but the query returns %d columns%s", rType, len(r.Columns), where)
			return nil
		}
		col := r.Columns[0]
		reportFitDialect(report, at, "column "+col.Name, col, rType, fitType(col.Type, rType, false, c.ls.dialect.Traits()), where)
		return nil
	}
	byName := map[string]int{}
	for i, col := range r.Columns {
		if col.Name == "" {
			report(at, "result column %d has no name: give it an alias (... AS name) so it can bind to a field of %s%s", i+1, rType, where)
			continue
		}
		if j, dup := byName[col.Name]; dup {
			report(at, "result columns %d and %d are both named %q: alias one of them (... AS other_name)%s", j+1, i+1, col.Name, where)
			continue
		}
		byName[col.Name] = i
	}
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
			for name, f := range fields {
				if strings.EqualFold(name, col.Name) {
					fv, ok = f, true
					col.Name = name
					break
				}
			}
		}
		if !ok {
			if col.Name != "" {
				report(at, "result column %q has no field in %s%s", col.Name, rType, where)
			}
			continue
		}
		matched[col.Name] = true
		if notnull[col.Name] {
			col.Nullable = false
		}
		reportFitDialect(report, at, "field "+fieldName[col.Name], col, fv.Type(), fitType(col.Type, fv.Type(), false, c.ls.dialect.Traits()), where)
	}
	missing := map[string]types.Type{}
	for _, name := range order {
		if !matched[name] {
			missing[fieldName[name]] = fields[name].Type()
		}
	}
	return missing
}

func reportFitDialect(report func(token.Pos, string, ...any), at token.Pos, what string, col dialect.Column, gt types.Type, f fit, where string) {
	switch {
	case !f.ok:
		report(at, "%s is %s but column %q is %s%s", what, gt, col.Name, col.Type.Name, where)
	case f.unknown:
		report(at, "%s: no known Go mapping for %s, not checked%s", what, col.Type.Name, where)
	}
	if f.ok && !f.unknown && col.Nullable && !f.nullable {
		report(at, "%s is %s but column %q may be NULL (use a pointer, or tag it `col:\",notnull\"` if you know better)%s", what, gt, col.Name, where)
	}
}
