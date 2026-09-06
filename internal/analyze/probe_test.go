package analyze

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/oracle"
)

// TestProbe: PROBE_SCHEMA=file PROBE_SQL=file [PROBE_ORACLE=1] go test -run TestProbe
func TestProbe(t *testing.T) {
	sf, qf := os.Getenv("PROBE_SCHEMA"), os.Getenv("PROBE_SQL")
	if sf == "" || qf == "" {
		t.Skip()
	}
	schemaSQL, _ := os.ReadFile(sf)
	sqlb, _ := os.ReadFile(qf)
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Logf("schema problem: %s", p)
	}
	t.Logf("analyzer:\n%s", renderAnalyzerSafe(s, string(sqlb)))
	if os.Getenv("PROBE_ORACLE") != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		o, err := oracle.Start(ctx, string(schemaSQL))
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close()
		t.Logf("oracle:\n%s", renderOracle(ctx, o, string(sqlb)))
	}
}
