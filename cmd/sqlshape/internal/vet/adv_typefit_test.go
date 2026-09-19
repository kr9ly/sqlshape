package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAdvTypefit is an adversarial-testing lane test for lane "typefit" (Go type
// acceptance: x/dialect.Type / GoFit / gofit.go / typebind.go / check/postgres/dialect/
// types.go, against what pgx actually scans / encodes). Each case is reproduced against
// a running PostgreSQL in pgtest/runtime_adv_typefit_test.go; this test only records the
// diagnostic the checker should give. It uses its own schema
// (testdata/adv_typefit_schema.sql) so it does not depend on schema.sql other lanes may
// also be adjusting.
//
// Per the ruling in scratchpad/adv-pg3-fix-brief.md (decision 1), the array-null-element
// finding is a standing note (Advice / Lossy) in both modes, and additionally a
// rejection under -strict; testdata/src/adv_typefit checks the default mode,
// testdata/src/adv_typefit_strict the same three cases under -strict, since the two
// modes report a different diagnostic set for them (and, unrelated to this lane, for the
// binding.go "carries key" advisory too).
func TestAdvTypefit(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "adv_typefit_schema.sql")); err != nil {
		t.Fatal(err)
	}
	t.Run("default", func(t *testing.T) {
		analysistest.Run(t, td, Analyzer, "adv_typefit")
	})
	if err := Analyzer.Flags.Set("strict", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("strict", "false")
	t.Run("strict", func(t *testing.T) {
		analysistest.Run(t, td, Analyzer, "adv_typefit_strict")
	})
}
