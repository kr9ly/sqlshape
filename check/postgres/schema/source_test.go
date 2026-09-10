package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSourceDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "020_tables.sql"), []byte("CREATE TABLE t (id int, m mood);"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("not sql"), 0o644)
	// no file declares the version yet
	if _, err := ReadSource(dir); err == nil || !strings.Contains(err.Error(), "declare the PostgreSQL version") {
		t.Errorf("without a declaration: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "010_types.sql"), []byte("-- sqlshape: postgres 17\nCREATE TYPE mood AS ENUM ('a');\n"), 0o644)
	src, err := ReadSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(src)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v\n%s", s.Problems, src)
	}
	if s.Relation("public", "t") == nil {
		t.Errorf("table t missing")
	}
	if _, err := ReadSource(filepath.Join(dir, "sub")); err == nil {
		t.Error("missing path should fail")
	}
}
