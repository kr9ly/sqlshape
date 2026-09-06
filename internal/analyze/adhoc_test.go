package analyze

import (
	"os"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestAdhoc: PROBE_SCHEMA (DDL text) + PROBE_SQL → analyzer output. Throwaway.
func TestAdhoc(t *testing.T) {
	sql := os.Getenv("PROBE_SQL")
	if sql == "" {
		t.Skip()
	}
	s, err := schema.Load(os.Getenv("PROBE_SCHEMA"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Logf("schema problem: %s", p)
	}
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Logf("error: %v", aerr)
		return
	}
	t.Logf("\n%s", r.String(s.Types))
	for _, n := range r.Notes {
		t.Logf("note %s: %s (at %d)", n.Code, n.Message, n.Position)
	}
}
