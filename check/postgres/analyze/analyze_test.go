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

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
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
	s, err := Load(string(schemaSQL))
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
			// goldens exercise PG parity; sqlshape's own findings are covered by TestDomainNotes
			if r, err := Analyze(s, sql); err == nil {
				for _, n := range r.Notes {
					if !n.Advisory() {
						t.Errorf("unexpected note: %v", n)
					}
				}
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

// TestUtilityErrors: PG only parses utility statements when it prepares them, so a
// missing relation surfaces at execution; the analyzer reports it up front (no golden).
func TestUtilityErrors(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"TRUNCATE nope":                    "42P01",
		"LOCK TABLE nope":                  "42P01",
		"REFRESH MATERIALIZED VIEW orders": "42809",
	}
	for sql, code := range cases {
		_, err := Analyze(s, sql)
		var aerr *Error
		if !errors.As(err, &aerr) || aerr.Code != code {
			t.Errorf("%s: want %s, got %v", sql, code, err)
		}
	}
}

// TestGroupingSetsNullable: a column grouped by GROUPING SETS is NULL in the sets that
// leave it out, so it is nullable even when the table column is NOT NULL; count(*) is not.
func TestGroupingSetsNullable(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, "SELECT user_id, count(*) AS n, sum(total) AS t FROM orders GROUP BY ROLLUP (user_id)")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Columns[0].Nullable || r.Columns[1].Nullable || !r.Columns[2].Nullable {
		t.Errorf("nullability: user_id %v, n %v, t %v", r.Columns[0].Nullable, r.Columns[1].Nullable, r.Columns[2].Nullable)
	}
	r, err = Analyze(s, "SELECT user_id, count(*) AS n FROM orders GROUP BY user_id")
	if err != nil {
		t.Fatal(err)
	}
	if r.Columns[0].Nullable {
		t.Error("plain GROUP BY keeps NOT NULL")
	}
}
