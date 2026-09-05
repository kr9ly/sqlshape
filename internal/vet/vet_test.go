package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "a")
}
