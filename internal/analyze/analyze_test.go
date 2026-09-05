package analyze

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/schema"
)

var update = flag.Bool("update", false, "regenerate testdata/queries/*.golden from the live oracle")

// TestGolden runs every testdata/queries/*.sql through the analyzer and compares
// with the oracle-generated .golden. With -update the goldens are regenerated
// from a real PG (needs the embedded binary); without it no PG is started.
func TestGolden(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	files, _ := filepath.Glob("testdata/queries/*.sql")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}

	var o *oracle.Oracle
	ctx := context.Background()
	if *update {
		ctx2, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		ctx = ctx2
		o, err = oracle.Start(ctx, string(schemaSQL))
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		defer o.Close()
	}

	pass, fail := 0, 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		t.Run(name, func(t *testing.T) {
			sqlb, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			sql := strings.TrimSpace(string(sqlb))
			golden := strings.TrimSuffix(f, ".sql") + ".golden"
			if *update {
				out := renderOracle(ctx, o, sql)
				if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			got := renderAnalyzer(s, sql)
			if !match(string(want), got) {
				fail++
				t.Errorf("mismatch\n--- oracle\n%s--- analyzer\n%s", want, got)
			} else {
				pass++
			}
		})
	}
	t.Logf("%d pass, %d fail", pass, fail)
}

// match compares oracle and analyzer renderings. Error lines only need the same
// SQLSTATE class prefix (first 5 chars) to count as agreeing; messages are informative.
func match(want, got string) bool {
	if strings.HasPrefix(want, "error:") && strings.HasPrefix(got, "error:") {
		return strings.HasPrefix(got, want[:len("error: 42XXX")])
	}
	return want == got
}

func renderOracle(ctx context.Context, o *oracle.Oracle, sql string) string {
	d, err := o.Describe(ctx, sql)
	if err != nil {
		var pgErr *oracle.PgError
		if errors.As(err, &pgErr) {
			return "error: " + pgErr.Error() + "\n"
		}
		return "internal error: " + err.Error() + "\n"
	}
	return d.String()
}

func renderAnalyzer(s *schema.Schema, sql string) string {
	r, err := Analyze(s, sql)
	if err != nil {
		var aerr *Error
		if errors.As(err, &aerr) {
			return "error: " + aerr.Error() + "\n"
		}
		return "internal error: " + err.Error() + "\n"
	}
	return r.String(s.Types)
}
