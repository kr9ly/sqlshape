package vet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// loadSchema is exercised end to end by every analysistest package through schema.sql,
// but its own error paths (a schema that fails to read or parse, and a function / policy /
// view body that fails to analyze outright, or whose notes are not advisory) need a schema
// built to trigger them directly, isolated from every other test's shared schema.sql.

func TestLoadSchemaReadError(t *testing.T) {
	_, err := loadSchema(filepath.Join(t.TempDir(), "does-not-exist.sql"))
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadSchemaParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE (;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSchema(path); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadSchemaBodyProblems(t *testing.T) {
	path := filepath.Join(analysistest.TestData(), "loaderrors_schema.sql")
	ls, err := loadSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ls.problems, "\n")
	for _, want := range []string{
		`relation "dup_table" already exists`, // a schema-level Problem (s.Problems)
		"function bad_fn",                     // AnalyzeFunction hard error
		"function note_fn: domain mismatch",   // non-advisory function note
		"policy p_bad on t",                   // AnalyzePolicy hard error
		"policy p_note on t: domain mismatch", // non-advisory policy note
		"view bad_view",                       // AnalyzeView hard error
		"view note_view: domain mismatch",     // non-advisory view note
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q; got:\n%s", want, joined)
		}
	}
}
