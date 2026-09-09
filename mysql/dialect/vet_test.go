package dialect

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/kr9ly/sqlshape/internal/vet"
)

// TestVet runs the checker end to end over a MySQL schema: the dialect line selects this
// package's analyzer, and the diagnostics are the checker's dialect path over its Result.
func TestVet(t *testing.T) {
	td := analysistest.TestData()
	if err := vet.Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, vet.Analyzer, "app")
}
