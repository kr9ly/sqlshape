package vet

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/cmd/sqlshape/internal/consumers"
	"golang.org/x/tools/go/analysis/analysistest"
)

// The analyzer's result is the package's consumer index: which declarations depend on
// which relation columns.
func TestConsumers(t *testing.T) {
	td := analysistest.TestData()
	if err := Analyzer.Flags.Set("schema", filepath.Join(td, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	results := analysistest.Run(t, td, Analyzer, "a")
	if len(results) != 1 {
		t.Fatalf("%d results", len(results))
	}
	index := results[0].Result.(*consumers.Index)
	sites := func(table, column string) string {
		var out []string
		for _, s := range index.Column(table, column) {
			out = append(out, fmt.Sprintf("%s@%d:%d", s.Owner, s.Pos.Line, s.Pos.Column))
		}
		return strings.Join(out, " ")
	}
	// a column read by a template: the declaration and the position inside the SQL
	if got := sites("orders", "note"); !strings.Contains(got, "a.listOrders@34:") {
		t.Errorf("orders.note consumers: %s", got)
	}
	// a view is consumed as itself; its base table's readers are not the statement's
	if got := sites("live_memos", "id"); got != "a.viaLiveView@244:68" {
		t.Errorf("live_memos.id consumers: %s", got)
	}
	if got := sites("memos", "deleted_at"); strings.Contains(got, "viaLiveView") {
		t.Errorf("memos.deleted_at reached through the view: %s", got)
	}
	// COPY names its columns
	if got := sites("hosts", "mac"); got != "a.hosts@351:52" {
		t.Errorf("hosts.mac consumers: %s", got)
	}
	// relation-level: every statement touching the table
	if n := len(index.Relation("orders")); n < 10 {
		t.Errorf("orders has %d consumers", n)
	}
	if !strings.Contains(index.String(), "orders.status\n") {
		t.Errorf("String lacks orders.status:\n%s", index.String())
	}
}
