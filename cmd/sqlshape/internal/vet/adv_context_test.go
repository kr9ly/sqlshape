package vet

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// A package-comment context directive (`sqlshape: context <name>`) that carries a
// trailing note after the name is diagnosed as an unrecognized directive, rather than
// silently falling back to no context at all.
func TestAdvContextTrailingTextIsDiagnosed(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "adv_context_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "adv_context")
}
