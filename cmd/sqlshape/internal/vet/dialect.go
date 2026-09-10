package vet

import (
	"go/ast"
)

// The dialect path: a schema that declares a dialect other than PostgreSQL is judged
// through internal/dialect. It checks what every dialect's Result can say — the statement
// analyzes, its result columns fit R by name, type and nullability, its parameters fit P —
// and, over its Facts, the One proof (x/cardinality). Obligations, violations, bindings and
// the quick fixes stay on the PostgreSQL path until the Result carries what they need.

// runDialect is run's tail for a schema whose dialect gives the checker no schema contract
// yet: the statements are checked like any other, MatView and Copy are not available.
func (c *checker) runDialect(calls, matviews []*ast.CallExpr) {
	for _, call := range calls[:len(calls)-len(matviews)] {
		c.checkCall(call)
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
