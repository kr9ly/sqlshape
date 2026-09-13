package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestGuardedPointer covers m12e-guarded-pointer: a pointer parameter read inside the
// then branch of a plain {{if .X}} / {{with .X}} on itself (or on a path it is nested
// under) is proven non-nil for that expansion, so no NOT NULL violation is reported for
// it there; outside that branch (unconditional, or in the else branch), the ordinary
// nullable-pointer violation still fires. It uses its own schema
// (testdata/guarded_pointer_schema.sql) so it does not depend on the shared schema.sql
// other lanes may also be adjusting.
func TestGuardedPointer(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "guarded_pointer_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "guarded_pointer")
}
