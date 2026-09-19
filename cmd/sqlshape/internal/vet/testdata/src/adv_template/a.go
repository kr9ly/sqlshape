package adv_template

import "github.com/kr9ly/sqlshape/v2"

// P1 exercises an if/else where the then-branch leaves an odd number of quotes in the
// raw template text (closed only by text shared after {{end}}); checkActionPlacement scans
// the raw template text once, linearly, treating then+else as if they were concatenated
// code rather than alternatives. The then-branch's stray quote flips the scanner into
// "inside a string" state that then gets closed again by the else-branch's own quote,
// so by the time the scanner reaches {{.Q}} in the else-branch it reports state=code and
// stays silent -- even though the else-branch, taken alone, splices {{.Q}} directly inside
// the string literal 'bar$1'.
type P1 struct {
	A bool
	Q string
}

// Expected (correct) behavior: vet should flag {{.Q}} as spliced inside the else-branch's
// string literal 'bar{{.Q}} -- exactly the hazard docs/templates.md documents for
// '%{{.Q}}%'. It currently does not (checkActionPlacement's raw linear scan bleeds quote
// state across the if/else alternative, see internal/vet/hazards.go); it instead reports a
// spurious, unrelated diagnostic about {{else}} itself. The want-comment below records the
// diagnostic vet ought to produce and is expected to FAIL against the current implementation.
var SpliceViaIfElse = sqlshape.Query[int64, P1](`SELECT id FROM users WHERE ({{if .A}}name = 'foo{{else}}name = 'bar{{.Q}}{{end}}')`) // want `\{\{\.Q\}\} is inside a string literal: it becomes text, not a parameter`

// P2: checkBareOrderBy's regex (internal/vet/hazards.go) only matches a bare `$n` that
// immediately follows "ORDER BY" (or a run of other bare `$n`s straight after it):
// `(?i)\b(ORDER|GROUP|PARTITION)\s+BY\s+((?:\$\d+\s*,\s*)*)\$(\d+)`. A bare parameter that
// is not the first ORDER BY item -- because a real column comes first -- never matches,
// even though it is exactly the same hazard docs/templates.md warns about ("Do not put a
// parameter directly in ORDER BY"): {{.Sort}} sorts by the constant value of Sort, not by
// any column, silently and identically to the documented-as-rejected single-column case.
type P2 struct {
	Sort string
}

// Expected (correct) behavior: vet should flag {{.Sort}} the same way it flags a bare
// leading ORDER BY parameter. It does not, because {{.Sort}} is the second ORDER BY item,
// preceded by the literal column "id" (not another `$n`), which checkBareOrderBy's regex
// does not recognize as a still-bare position.
var BareOrderBySecondColumn = sqlshape.Query[int64, P2](`SELECT id FROM orders ORDER BY id, {{.Sort}}`) // want `ORDER BY \{\{\.Sort\}\} sorts by a constant`
