package adv_template3

import "github.com/kr9ly/sqlshape/v2"

// isQueryCall (cmd/sqlshape/internal/vet/vet.go) recognizes a statement only when the
// call expression's Fun is (after unwrapping a generic instantiation's IndexExpr /
// IndexListExpr) a *ast.SelectorExpr, i.e. literally `sqlshape.Query[...](...)` or
// `sqlshape.One[...](...)` written at the call site. Assigning the instantiated generic
// function to a package-level var first, then calling the var, makes call.Fun an
// *ast.Ident -- calledFunc's type switch has no case for that shape, so markerOf never
// matches and the call is skipped entirely: it is never added to `calls` in run(), so the
// SQL text is never analyzed, in any mode, with or without -coverage. This is a genuine
// escape route, not the documented "non-constant template" gap (-coverage's "unchecked"
// counter): the template argument here is a constant string literal, and the SQL is
// syntactically wrong (a column that does not exist) -- the same statement written as
// `sqlshape.Query[int64, struct{}](...)` directly is correctly flagged (see b.go). Bound
// through a var, sqlshape has nothing to say about it at all: no diagnostic, and no
// -coverage counter change either (unchecked is only incremented inside checkCall, which
// this call never reaches).
var runBadQuery = sqlshape.Query[int64, struct{}]

func UseIt() {
	_ = runBadQuery(`SELECT nope FROM widgets`) // want `sqlshape: column "nope" does not exist`
}
