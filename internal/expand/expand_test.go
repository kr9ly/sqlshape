package expand

import (
	"fmt"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	res, err := Expand(`SELECT id FROM o WHERE true
{{if .Status}} AND status = {{.Status}}{{end}}
{{if .UserIDs}} AND user_id = ANY({{.UserIDs}}){{end}}
ORDER BY {{if eq .Sort "total"}} total {{else}} created_at {{end}}
{{range .Tags}} AND {{.}} = ANY(tags){{end}}
{{with .Page}} LIMIT {{.Limit}} OFFSET {{.Offset}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	// 2 * 2 * 2 * 3 * 2 = 48
	if len(res.Expansions) != 48 {
		t.Fatalf("expansions = %d", len(res.Expansions))
	}
	var full *Expansion
	for i := range res.Expansions {
		e := &res.Expansions[i]
		if strings.Count(e.Branch, ":then") == 4 && strings.Contains(e.Branch, ":x2") {
			full = e
		}
	}
	if full == nil {
		t.Fatal("full expansion not found")
	}
	wantSQL := "SELECT id FROM o WHERE true\n AND status = $1\n AND user_id = ANY($2)\nORDER BY  total \n AND $3 = ANY(tags) AND $4 = ANY(tags)\n LIMIT $5 OFFSET $6"
	if full.SQL != wantSQL {
		t.Errorf("sql:\n%s", full.SQL)
	}
	var paths []string
	for _, p := range full.Params {
		paths = append(paths, p.Path.String())
	}
	if got := strings.Join(paths, " "); got != ".Status .UserIDs .Tags[] .Tags[] .Page.Limit .Page.Offset" {
		t.Errorf("paths: %s", got)
	}
	// position mapping: "$1" maps back to the {{.Status}} action
	off := strings.Index(full.SQL, "$1")
	if tp := full.TemplatePos(off); tp < 34 || tp > 70 {
		t.Errorf("template pos of $1 = %d", tp)
	}
	var controls []string
	for _, c := range res.Controls {
		controls = append(controls, c.String())
	}
	if got := strings.Join(controls, " "); !strings.Contains(got, ".Sort") || !strings.Contains(got, ".Page") {
		t.Errorf("controls: %s", got)
	}
}

func TestSameFieldTwiceIsOneParam(t *testing.T) {
	res, err := Expand(`SELECT {{.A}}, {{.A}}, {{.B}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Expansions[0].SQL; got != "SELECT $1, $1, $2" {
		t.Errorf("sql = %q", got)
	}
}

func TestRejectsPipelines(t *testing.T) {
	if _, err := Expand(`SELECT {{.A | printf "%d"}}`); err == nil {
		t.Error("expected error")
	}
	if _, err := Expand(`SELECT {{template "x"}}`); err == nil {
		t.Error("expected error")
	}
}

// TestSparse: beyond MaxExpansions the expander falls back to all-off / all-on / each-alone.
func TestSparse(t *testing.T) {
	var b strings.Builder
	b.WriteString("SELECT id FROM t WHERE true")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, " {{if .C%d}} AND c%d = {{.C%d}} {{end}}", i, i, i)
	}
	b.WriteString(" {{range .IDs}} AND id <> {{.}} {{end}}")
	res, err := Expand(b.String())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sparse || res.Combinations <= MaxExpansions {
		t.Fatalf("expected sparse fallback: sparse=%v combos=%d", res.Sparse, res.Combinations)
	}
	// 1 all-off + 1 all-on + 11 alone
	if len(res.Expansions) != 13 {
		t.Fatalf("expansions: %d", len(res.Expansions))
	}
	if strings.Contains(res.Expansions[0].SQL, "c0 =") {
		t.Errorf("all-off should have no predicates: %s", res.Expansions[0].SQL)
	}
	if !strings.Contains(res.Expansions[1].SQL, "c9 = $10") || !strings.Contains(res.Expansions[1].SQL, "id <> $12") {
		t.Errorf("all-on should have every predicate and two iterations: %s", res.Expansions[1].SQL)
	}
	if got := res.Expansions[2].SQL; !strings.Contains(got, "c0 = $1") || strings.Contains(got, "c1 =") {
		t.Errorf("first alone form: %s", got)
	}
}
