package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// Lane: template (3rd adversarial round, PostgreSQL). Each `// want` in
// testdata/src/adv_template3/*.go records the diagnostic vet *should* emit and was
// confirmed missing (or, for the control cases in b.go/d.go, confirmed present) by
// actually running analysistest against the current implementation:
//
//   - a.go: sqlshape.Query[R,P] assigned to a var and called through the var is never
//     recognized as a query call (isQueryCall / calledFunc only unwrap an IndexExpr /
//     IndexListExpr directly on the call's own Fun), so a syntactically bad, constant SQL
//     string goes completely unanalyzed -- not even -coverage's "unchecked" counter sees
//     it. b.go is the control case: the identical bad SQL, called directly, is flagged.
//   - c.go: x/expand's control() does not recurse into a condition's nested function-call
//     arguments (e.g. `{{if gt (len .Xz) 0}}`), so a typo'd field referenced only that way
//     is never checked against the parameter type. d.go is the control case: the same
//     typo as a bare `{{if .Xz}}` condition is flagged.
//
// This test therefore fails against the current implementation until isQueryCall widens to
// recognize the var-indirection call shape and control() recurses into nested pipelines.
func TestAdvTemplate3(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "adv_template3_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "adv_template3")
}
