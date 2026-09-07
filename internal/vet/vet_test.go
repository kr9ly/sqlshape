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

func TestRequireColumns(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("require-columns", "user_id"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("require-columns", "")
	analysistest.Run(t, td, Analyzer, "owner")
}

func TestSyncComments(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("sync-comments", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("sync-comments", "false")
	analysistest.Run(t, td, Analyzer, "docs")
}

func TestCoverage(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("coverage", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("coverage", "false")
	analysistest.Run(t, td, Analyzer, "cov")
}

func TestRawSQL(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "raw")
}

func TestRawSQLForbid(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("raw-sql", "forbid"); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("raw-sql-allow", "rawok/..."); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("raw-sql", "constant")
	defer Analyzer.Flags.Set("raw-sql-allow", "")
	analysistest.Run(t, td, Analyzer, "rawforbid", "rawok")
}

func TestDTOFixes(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.RunWithSuggestedFixes(t, td, Analyzer, "dto")
}

func TestRowSecurity(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "rls_schema.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Analyzer.Flags.Set("strict", "true"); err != nil {
		t.Fatal(err)
	}
	defer Analyzer.Flags.Set("strict", "false")
	analysistest.Run(t, td, Analyzer, "rls")
}

func TestRowSecurityPins(t *testing.T) {
	td := analysistest.TestData()
	for k, v := range map[string]string{"schema": filepath.Join(td, "rls_schema.sql"), "strict": "true", "require-columns": "tenant_id"} {
		if err := Analyzer.Flags.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	defer Analyzer.Flags.Set("strict", "false")
	defer Analyzer.Flags.Set("require-columns", "")
	analysistest.Run(t, td, Analyzer, "rlspin")
}
