package vet

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// stubDialect answers for a fixed table things(id, name, price, at) without parsing
// anything: the test is about the checker's dialect path, not about an analyzer.
type stubDialect struct{}

var stubColumns = map[string]dialect.Column{
	"id":    {Name: "id", Type: dialect.Type{Name: "bigint", Go: []string{"int64"}}},
	"name":  {Name: "name", Type: dialect.Type{Name: "varchar(20)", Go: []string{"string"}}},
	"price": {Name: "price", Type: dialect.Type{Name: "decimal(10,2)", Go: []string{"string"}}, Nullable: true},
	"at":    {Name: "at", Type: dialect.Type{Name: "datetime", Go: []string{"time.Time"}}},
}

func (stubDialect) Problems() []string { return []string{"3: a problem with the schema"} }

func (stubDialect) Analyze(sql string) (*dialect.Result, error) {
	if strings.HasPrefix(sql, "SELECT boom") {
		return nil, &dialect.Error{Message: "no such thing as boom", Code: "TD001", Position: 7}
	}
	r := &dialect.Result{}
	rest := strings.TrimPrefix(sql, "SELECT ")
	list, _, _ := strings.Cut(rest, " FROM ")
	for _, name := range strings.Split(list, ",") {
		r.Columns = append(r.Columns, stubColumns[strings.TrimSpace(name)])
	}
	if i := strings.Index(sql, "WHERE id = $1"); i >= 0 {
		r.Params = []dialect.Type{stubColumns["id"].Type}
	}
	return r, nil
}

func init() {
	dialect.Register("testdb", func(string) (dialect.Analyzer, error) { return stubDialect{}, nil })
}

func TestDialectPath(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "dialect_schema.sql")); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, td, Analyzer, "dialect")
}
