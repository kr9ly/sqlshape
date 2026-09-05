package vet

import (
	"go/ast"
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// sqlshape.Copy[R](table, cols...) declares a bulk load. The checker verifies the table
// and its columns against the schema and each column's type against the field of R that
// feeds it (by column name, embedded structs flattened, as the runtime lays rows out),
// and that the columns left out can be left out: NOT NULL without a default is a load
// that fails on the first row.

// isCopyCall recognizes sqlshape.Copy[R](...).
func isCopyCall(pass *analysis.Pass, call *ast.CallExpr) bool {
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
	return ok && obj.Pkg() != nil && obj.Pkg().Path() == sqlshapePkg && obj.Name() == "Copy"
}

func (c *checker) checkCopy(call *ast.CallExpr) {
	report := func(format string, args ...any) {
		c.pass.Reportf(call.Pos(), "sqlshape: "+format, args...)
	}
	// R
	tv, ok := c.pass.TypesInfo.Types[call.Fun]
	if !ok {
		return
	}
	sig, ok := tv.Type.(*types.Signature)
	if !ok || sig.Results().Len() != 1 {
		return
	}
	inst, ok := sig.Results().At(0).Type().(*types.Named)
	if !ok || inst.TypeArgs() == nil || inst.TypeArgs().Len() != 1 {
		return
	}
	rType := inst.TypeArgs().At(0)
	// arguments: constant strings
	var strs []string
	for _, a := range call.Args {
		av, ok := c.pass.TypesInfo.Types[a]
		if !ok || av.Value == nil || av.Value.Kind() != constant.String {
			c.pass.Reportf(a.Pos(), "sqlshape: Copy table and column names must be string constants")
			return
		}
		strs = append(strs, constant.StringVal(av.Value))
	}
	if len(strs) == 0 {
		return
	}
	table, cols := strs[0], strs[1:]
	sch, n := "public", table
	if i := strings.LastIndex(table, "."); i >= 0 {
		sch, n = table[:i], table[i+1:]
	}
	rel := c.s.Relation(sch, n)
	if rel == nil {
		report("Copy: table %q does not exist", table)
		return
	}
	if rel.Kind != schema.Table {
		report("Copy: %q is not a table (COPY FROM loads tables)", table)
		return
	}
	if noTables {
		report("table %s is written directly; with -no-tables application code reads views and calls functions only", rel.FullName())
	}
	if schemasFlag != "" {
		allowed := false
		for _, s := range strings.Split(schemasFlag, ",") {
			if strings.TrimSpace(s) == rel.Schema {
				allowed = true
			}
		}
		if !allowed {
			report("%s is outside the schemas this code may reference (%s)", rel.FullName(), schemasFlag)
		}
	}
	// fields of R
	inner, _ := unwrapNullable(rType)
	st, isStruct := inner.Underlying().(*types.Struct)
	if !isStruct || isNamed(inner, "time", "Time") || scalarPkg(inner) {
		// scalar R: exactly one column
		if len(cols) != 1 {
			report("Copy[%s] into %s: a scalar R feeds exactly one column, %d given", rType, table, len(cols))
			return
		}
		col := rel.Column(cols[0])
		if col == nil {
			report("Copy into %s: column %q does not exist", table, cols[0])
			return
		}
		c.copyFit(report, col, rType, "R")
		c.copyOmitted(report, rel, cols)
		return
	}
	flat, dups := structFields(st)
	for _, d := range dups {
		report("%s: fields %s", rType, d)
	}
	byCol := map[string]structField{}
	for _, f := range flat {
		byCol[f.col] = f
	}
	if len(cols) == 0 {
		for _, f := range flat {
			cols = append(cols, f.col)
		}
	}
	fed := map[string]bool{}
	for _, name := range cols {
		col := rel.Column(name)
		if col == nil {
			report("Copy into %s: column %q does not exist", table, name)
			continue
		}
		if col.Generated != nil {
			report("Copy into %s: column %q is generated and cannot be copied into", table, name)
			continue
		}
		f, ok := byCol[name]
		if !ok {
			report("Copy into %s: column %q has no field in %s", table, name, rType)
			continue
		}
		fed[f.col] = true
		c.meet(f.v.Type(), col.Type, &analyze.Source{Table: rel.FullName(), Column: col.Name, NotNull: col.NotNull, Assigned: true}, call.Pos(), "field "+f.name)
		c.copyFit(report, col, f.v.Type(), "field "+f.name)
	}
	for _, f := range flat {
		if !fed[f.col] {
			report("Copy into %s: field %s (column %q) is not copied", table, f.name, f.col)
		}
	}
	c.copyOmitted(report, rel, cols)
}

// copyFit checks one column against the Go type feeding it (parameter direction).
func (c *checker) copyFit(report func(string, ...any), col *schema.Column, gt types.Type, what string) {
	f := c.paramFit(col.Type, gt)
	switch {
	case !f.ok:
		report("Copy: %s is %s but column %q is %s", what, gt, col.Name, c.s.Types.Format(col.Type))
	case f.lossy != "":
		report("Copy: %s: %s", what, f.lossy)
	case f.unknown:
		report("Copy: %s: no known Go mapping for %s, not checked", what, c.s.Types.Format(col.Type))
	}
	if f.ok && col.NotNull && f.nullable {
		report("Copy: %s is %s but column %q is NOT NULL: a nil value fails the load", what, gt, col.Name)
	}
}

// copyOmitted reports the columns the load leaves to their defaults that have none and
// forbid NULL: COPY fills them with NULL and the first row fails.
func (c *checker) copyOmitted(report func(string, ...any), rel *schema.Relation, cols []string) {
	listed := map[string]bool{}
	for _, n := range cols {
		listed[n] = true
	}
	for _, col := range rel.Columns {
		if listed[col.Name] || !col.NotNull || col.Default != nil || col.Identity != 0 || col.Generated != nil {
			continue
		}
		report("Copy into %s: column %q is NOT NULL without a default and is not copied", rel.FullName(), col.Name)
	}
}

// scalarPkg reports the struct types pgx encodes as scalars (netip, pgtype).
func scalarPkg(t types.Type) bool {
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	p := n.Obj().Pkg().Path()
	return p == "net/netip" || strings.HasSuffix(p, "jackc/pgx/v5/pgtype")
}
