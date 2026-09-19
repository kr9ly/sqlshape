package main

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/kr9ly/sqlshape/cmd/sqlshape/v2/internal/vet"
)

// TestMySQLVet runs the checker end to end over a MySQL schema: the dialect line selects
// the MySQL analyzer (registered by this binary's blank import), and the diagnostics are
// the checker's dialect path over its Result.
func TestMySQLVet(t *testing.T) {
	td := filepath.Join(analysistest.TestData(), "mysql")
	if err := vet.Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, vet.Analyzer, "app")
}
