package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// Lane: template. See adv_template_test.go at the repo root for the runtime-side
// confirmation (a real PG bind-count mismatch caused by the same template) and
// testdata/src/adv_template/a.go for the template under test.
//
// checkActionPlacement (hazards.go) walks the *raw* template text once, front to back,
// tracking quote state by counting quote characters as it goes. An `{{if}}...{{else}}...
// {{end}}` is not a sequence at runtime -- only one branch ever executes -- but the scanner
// treats the then-branch text and the else-branch text as if they were concatenated code.
// An odd number of quotes in the then-branch (closed only by text shared after {{end}})
// flips the scanner into "inside a string" state; a later, unrelated quote in the
// else-branch flips it back to "code" before the scanner reaches the else-branch's
// `{{.Q}}` -- so the placement hazard for `{{.Q}}` (spliced inside the string literal
// 'bar$1' when .A is false) goes completely unreported. Confirmed by hand with the raw
// scanner before writing this test: it reports exactly one diagnostic, for `{{else}}`
// "being inside a string literal" (a side-effect of the same bug, and not what a user of
// this template needs to hear), and nothing at all for `{{.Q}}`.
//
// testdata/src/adv_template/a.go's `// want` records the diagnostic vet *should* emit for
// {{.Q}}. This test therefore fails against the current implementation -- it becomes a
// regression guard once hazards.go is made branch-aware (e.g. by scanning each expansion's
// rendered SQL, or each branch's text in isolation, instead of the raw template once).
func TestAdvTemplateHazardMissedAcrossIfElse(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "adv_template")
}
