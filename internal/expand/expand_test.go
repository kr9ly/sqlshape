package expand

import (
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
