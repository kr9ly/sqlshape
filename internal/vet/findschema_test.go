package vet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// passAt builds a minimal analysis.Pass whose one file lives at dir/pkg.go, for exercising
// findSchema's directory walk without going through analysistest (which always resolves
// packages under a testdata/src tree that itself sits under testdata/schema.sql, so the
// "not found" search would never actually miss).
func passAt(t *testing.T, dir string) *analysis.Pass {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "pkg.go")
	if err := os.WriteFile(file, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &analysis.Pass{Fset: fset, Files: []*ast.File{f}}
}

func withEmptySchemaFlag(t *testing.T) {
	t.Helper()
	prev := schemaPath
	if err := Analyzer.Flags.Set("schema", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Analyzer.Flags.Set("schema", prev); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFindSchemaFile(t *testing.T) {
	withEmptySchemaFlag(t)
	root := t.TempDir()
	// schema.sql sits above the package directory, not in it.
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("-- schema\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "sub", "pkg")
	pass := passAt(t, pkgDir)
	got, err := findSchema(pass)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "schema.sql")
	if got != want {
		t.Errorf("findSchema() = %q, want %q", got, want)
	}
}

func TestFindSchemaDirectory(t *testing.T) {
	withEmptySchemaFlag(t)
	root := t.TempDir()
	schemaDir := filepath.Join(root, "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "01_init.sql"), []byte("-- init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "cmd", "pkg")
	pass := passAt(t, pkgDir)
	got, err := findSchema(pass)
	if err != nil {
		t.Fatal(err)
	}
	want := schemaDir
	if got != want {
		t.Errorf("findSchema() = %q, want %q", got, want)
	}
}

func TestFindSchemaNotFound(t *testing.T) {
	withEmptySchemaFlag(t)
	// An isolated directory tree with neither schema.sql nor schema/ anywhere above it
	// (up to the filesystem root): the search must fail with the documented message.
	root := t.TempDir()
	pkgDir := filepath.Join(root, "sub", "pkg")
	pass := passAt(t, pkgDir)
	_, err := findSchema(pass)
	if err == nil {
		t.Fatal("expected an error")
	}
	wantMsg := "schema.sql (or a schema/ directory) not found above " + pkgDir + " (use -schema)"
	if err.Error() != wantMsg {
		t.Errorf("findSchema() error = %q, want %q", err.Error(), wantMsg)
	}
}
