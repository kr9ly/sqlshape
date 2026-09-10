package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAdvParam is an adversarial-testing lane test (see
// /tmp/.../scratchpad/adv/lane-param.md). It uses its own schema
// (testdata/adv_param_schema.sql) so it does not depend on the shared schema.sql other
// lanes may also be adjusting.
func TestAdvParam(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "adv_param_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "adv_param")
}
