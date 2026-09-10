package oracle

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "regenerate testdata/queries/*.golden from the live oracle")

// TestGolden describes every testdata/queries/*.sql against the real PG and
// compares with the .golden next to it. The goldens are the contract the
// pure-Go analyzer must reproduce; regenerate with `go test ./internal/oracle -update`.
func TestGolden(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	schema, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer o.Close()

	files, _ := filepath.Glob("testdata/queries/*.sql")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		t.Run(name, func(t *testing.T) {
			sql, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got := render(o, ctx, string(sql))
			golden := strings.TrimSuffix(f, ".sql") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if string(want) != got {
				t.Errorf("mismatch\n--- want\n%s--- got\n%s", want, got)
			}
		})
	}
}

func render(o *Oracle, ctx context.Context, sql string) string {
	d, err := o.Describe(ctx, sql)
	if err != nil {
		var pgErr *PgError
		if errors.As(err, &pgErr) {
			return "error: " + pgErr.Error() + "\n"
		}
		return "internal error: " + err.Error() + "\n"
	}
	return d.String()
}

// TestDescribeIsStateless checks the anonymous statement leaves nothing behind
// and that describing a bad statement does not poison the connection.
func TestDescribeIsStateless(t *testing.T) {
	ctx := context.Background()
	o, err := Start(ctx, "CREATE TABLE t (a int)")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer o.Close()

	if _, err := o.Describe(ctx, "SELECT nope FROM t"); err == nil {
		t.Fatal("expected error")
	}
	d, err := o.Describe(ctx, "SELECT a FROM t WHERE a = $1")
	if err != nil {
		t.Fatalf("after error: %v", err)
	}
	if len(d.Params) != 1 || d.Params[0].Name != "integer" {
		t.Errorf("params = %+v", d.Params)
	}
	if len(d.Columns) != 1 || d.Columns[0].Source == nil || d.Columns[0].Source.Table != "t" {
		t.Errorf("columns = %+v", d.Columns)
	}
}
