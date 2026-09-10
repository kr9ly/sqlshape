package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// docs/templates.md ("Many branches") promises: "A problem shared by every expansion is
// reported once, without the suffix." testdata/src/adv_obligation/adv_obligation.go's
// query has an {{if .X}} that never touches whether orders.tenant_id is pinned -- both
// branches' WHERE omit it, so `require pinned(tenant_id)` fails identically in every
// expansion, and is reported once, with no "[if@N:branch]" suffix.
func TestAdvObligationBranchSuffixOnUniformFailure(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "adv_obligation_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "adv_obligation")
}
