package verify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/oracle"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		name, want, got string
		match           bool
	}{
		{"equal", "columns: id integer\n", "columns: id integer\n", true},
		{"different", "columns: id integer\n", "columns: id bigint\n", false},
		{"same error class", "error: 42703: column \"x\" does not exist (at 5)\n", "error: 42703: column \"y\" does not exist (at 9)\n", true},
		{"different error class", "error: 42703: column \"x\" does not exist\n", "error: 42P01: relation \"t\" does not exist\n", false},
		{"one side an error", "error: 42703: column \"x\" does not exist\n", "columns: id integer\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(c.want, c.got); got != c.match {
				t.Errorf("Match(%q, %q) = %v, want %v", c.want, c.got, got, c.match)
			}
		})
	}
}

func TestMismatchError(t *testing.T) {
	m := &Mismatch{Template: "SELECT {{.A}}", Branch: "if@1:then", SQL: "  SELECT $1  ", Oracle: "error: 42P01: x\n", Analyzer: "columns: a integer\n"}
	got := m.Error()
	if !strings.Contains(got, "sqlshape: the analyzer disagrees with PostgreSQL [if@1:then]") {
		t.Errorf("missing header: %s", got)
	}
	if !strings.Contains(got, "--- sql\nSELECT $1\n--- postgres\nerror: 42P01: x\n--- analyzer\ncolumns: a integer\n") {
		t.Errorf("missing body: %s", got)
	}
	// no branch: the "[...]" suffix is left out
	m2 := &Mismatch{SQL: "SELECT 1", Oracle: "o\n", Analyzer: "a\n"}
	if got2 := m2.Error(); strings.Contains(got2, "[") {
		t.Errorf("did not want a branch marker: %s", got2)
	}
}

func TestRenderOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, "CREATE TABLE t (id int, name text);")
	if err != nil {
		t.Fatalf("oracle.Start: %v", err)
	}
	defer o.Close()

	if got := RenderOracle(ctx, o, "SELECT id, name FROM t"); !strings.Contains(got, "id") {
		t.Errorf("RenderOracle success = %q", got)
	}
	if got := RenderOracle(ctx, o, "SELECT nope FROM t"); !strings.HasPrefix(got, "error: 42703") {
		t.Errorf("RenderOracle pg error = %q, want a 42703 error", got)
	}
	// an internal error (not a PgError) also renders, with its own prefix; a canceled
	// context on Describe is a convenient way to produce one without a wire-level PG error.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if got := RenderOracle(cctx, o, "SELECT 1"); !strings.HasPrefix(got, "internal error:") {
		t.Errorf("RenderOracle internal error = %q, want an internal error prefix", got)
	}
}

func TestRenderAnalyzer(t *testing.T) {
	s, err := analyze.Load("CREATE TABLE t (id int, name text);")
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderAnalyzer(s, "SELECT id, name FROM t"); !strings.Contains(got, "id") {
		t.Errorf("RenderAnalyzer success = %q", got)
	}
	if got := RenderAnalyzer(s, "SELECT nope FROM t"); !strings.HasPrefix(got, "error:") {
		t.Errorf("RenderAnalyzer analyze error = %q, want an error prefix", got)
	}
	// analyze.Analyze's error is not always an *analyze.Error: more than one statement
	// is rejected with a plain fmt.Errorf, which exercises RenderAnalyzer's "internal
	// error" fallback (a syntax error, by contrast, is itself an *analyze.Error).
	if got := RenderAnalyzer(s, "SELECT 1; SELECT 2;"); !strings.HasPrefix(got, "internal error:") {
		t.Errorf("RenderAnalyzer multi-statement = %q, want an internal error prefix", got)
	}
}

// TestTemplateAgrees expands a template with no branches and checks the analyzer and
// PostgreSQL agree on every (here, the only) expansion: an empty Mismatch slice.
func TestTemplateAgrees(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schemaSQL := "CREATE TABLE t (id int, name text);"
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatalf("oracle.Start: %v", err)
	}
	defer o.Close()
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	mismatches, err := Template(ctx, o, s, "SELECT id, name FROM t WHERE id = {{.ID}}")
	if err != nil {
		t.Fatalf("Template: %v", err)
	}
	if len(mismatches) != 0 {
		t.Errorf("mismatches = %v, want none", mismatches)
	}
}

// TestTemplateDisagrees forces a genuine disagreement between the analyzer's schema and
// PostgreSQL's real one (the analyzer is told `id` is bigint; PostgreSQL's actual table
// has it as int) so Template must report a Mismatch rather than silently agreeing.
func TestTemplateDisagrees(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, "CREATE TABLE t (id int, name text);")
	if err != nil {
		t.Fatalf("oracle.Start: %v", err)
	}
	defer o.Close()
	s, err := analyze.Load("CREATE TABLE t (id bigint, name text);")
	if err != nil {
		t.Fatal(err)
	}
	mismatches, err := Template(ctx, o, s, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("Template: %v", err)
	}
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %d, want 1", len(mismatches))
	}
	m := mismatches[0]
	if m.SQL != "SELECT id FROM t" {
		t.Errorf("SQL = %q", m.SQL)
	}
	if m.Oracle == m.Analyzer {
		t.Errorf("oracle and analyzer descriptions should differ:\n%s\n%s", m.Oracle, m.Analyzer)
	}
	// Error() should render without panicking and mention both descriptions
	errStr := m.Error()
	if !strings.Contains(errStr, m.Oracle) || !strings.Contains(errStr, m.Analyzer) {
		t.Errorf("Error() missing a rendering: %s", errStr)
	}
}

// TestTemplateExpandError covers Template's early return when the template itself
// cannot be expanded.
func TestTemplateExpandError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, "CREATE TABLE t (id int);")
	if err != nil {
		t.Fatalf("oracle.Start: %v", err)
	}
	defer o.Close()
	s, err := analyze.Load("CREATE TABLE t (id int);")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Template(ctx, o, s, "SELECT {{template \"x\"}}"); err == nil {
		t.Error("expected an expansion error")
	}
}
