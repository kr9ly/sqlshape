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

func TestStrict(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("strict", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("strict", "false")
	analysistest.Run(t, td, Analyzer, "strict")
}

func TestNoTables(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("no-tables", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("no-tables", "false")
	analysistest.Run(t, td, Analyzer, "policy")
}
