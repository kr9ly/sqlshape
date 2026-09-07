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

func TestPathString(t *testing.T) {
	if got, want := (Path{}).String(), "."; got != want {
		t.Errorf("empty path = %q, want %q", got, want)
	}
	if got, want := (Path{"A", "[]", "B"}).String(), ".A[].B"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Pos: 12, Msg: "boom"}
	if got, want := e.Error(), "template: boom (at 12)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseError(t *testing.T) {
	if _, err := Expand(`SELECT {{if .A}}`); err == nil {
		t.Fatal("expected parse error")
	} else if _, ok := err.(*Error); !ok {
		t.Errorf("expected *Error, got %T", err)
	}
}

func TestTemplatePosFallback(t *testing.T) {
	res, err := Expand(`SELECT {{.A}}`)
	if err != nil {
		t.Fatal(err)
	}
	e := &res.Expansions[0]
	// offset past the end of every segment: falls back to the end of the last segment
	if got := e.TemplatePos(1000); got == 0 {
		t.Errorf("TemplatePos past end = %d, want > 0", got)
	}
	// an Expansion with no segments at all falls back to 0
	empty := &Expansion{}
	if got := empty.TemplatePos(5); got != 0 {
		t.Errorf("TemplatePos on empty expansion = %d, want 0", got)
	}
}

func TestUndefinedVariable(t *testing.T) {
	if _, err := Expand(`SELECT {{$x}}`); err == nil {
		t.Error("expected error for undefined variable")
	} else if !strings.Contains(err.Error(), "undefined variable") {
		t.Errorf("got %v", err)
	}
}

// {{with $y := .Foo}} binds $y to .Foo inside the with-body, like range binds its
// loop variables.
func TestWithDeclBindsVariable(t *testing.T) {
	res, err := Expand(`SELECT {{with $y := .Foo}}{{$y}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range res.Expansions {
		if e.SQL == "SELECT $1" {
			seen = true
			if len(e.Params) != 1 || e.Params[0].Path.String() != ".Foo" {
				t.Errorf("params = %#v, want one bound to .Foo", e.Params)
			}
		}
	}
	if !seen {
		t.Errorf("no expansion took the with branch: %#v", res.Expansions)
	}
}

func TestVariableDeclAndUse(t *testing.T) {
	res, err := Expand(`SELECT {{$x := .A}}{{$x}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Expansions[0].SQL, "SELECT $1"; got != want {
		t.Errorf("sql = %q, want %q", got, want)
	}
}

func TestDotAction(t *testing.T) {
	res, err := Expand(`{{range .IDs}}{{.}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	// the "two iterations" expansion is the last one
	last := res.Expansions[len(res.Expansions)-1]
	if last.SQL != "$1$2" {
		t.Errorf("sql = %q", last.SQL)
	}
}

func TestRangeWithIndexAndValue(t *testing.T) {
	res, err := Expand(`{{range $i, $v := .IDs}}{{$v}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	last := res.Expansions[len(res.Expansions)-1]
	if last.SQL != "$1$2" {
		t.Errorf("sql = %q", last.SQL)
	}
}

func TestWithElse(t *testing.T) {
	res, err := Expand(`{{with .A}}yes{{.}}{{else}}no{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	var sqls []string
	for _, e := range res.Expansions {
		sqls = append(sqls, e.SQL)
	}
	got := strings.Join(sqls, "|")
	if !strings.Contains(got, "no") {
		t.Errorf("expected an else-branch expansion, got %s", got)
	}
}

func TestRejectsBadValueAction(t *testing.T) {
	// more than one command in the pipe
	if _, err := Expand(`SELECT {{.A .B}}`); err == nil {
		t.Error("expected error for multi-arg pipe")
	}
	// a non-field / non-dot / non-variable argument
	if _, err := Expand(`SELECT {{"literal"}}`); err == nil {
		t.Error("expected error for a non-field value action")
	}
}

// TestSparseErrorInUntakenBranch: in sparse mode, an error inside a branch only turns up
// when that branch is actually taken by one of the sparse policies (all-off, all-on,
// alone) - not on the first policy tried. With >MaxExpansions combinations, the full
// expansion (which would take every branch and so catch any error immediately) is
// skipped entirely, so an error confined to one rarely-chosen branch is only found once
// the sparse walk reaches the policy that turns it on. (A bare undefined `$var` can't
// serve as the error here: Go's own template parser rejects that at parse time, before
// expand's branch machinery ever runs - see expand's own checks: {{template}} is rejected by expand, not by the parser.)
func TestSparseErrorInUntakenBranch(t *testing.T) {
	var b strings.Builder
	b.WriteString("SELECT id FROM t WHERE true")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, " {{if .C%d}} AND c%d = {{.C%d}} {{end}}", i, i, i)
	}
	// this branch is never on in the "all-off" policy (the first tried), only in
	// "all-on" and its own "alone" policy
	b.WriteString(` {{if .Bad}}{{template "x"}}{{end}}`)
	if _, err := Expand(b.String()); err == nil {
		t.Fatal("expected an error surfaced from a later sparse policy")
	} else if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v", err)
	}
}

func TestControlDotCondition(t *testing.T) {
	res, err := Expand(`{{range .IDs}}{{if .}}yes{{end}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Controls) == 0 {
		t.Fatal("expected controls to be recorded")
	}
}

// TestUnsupportedNode reaches node()'s default case (a node type the switch does not
// name at all). `{{block}}` looked like a candidate but Go's parser desugars it into a
// TemplateNode ("{{template}} is not supported", already covered by TestRejectsPipelines)
// - {{break}}/{{continue}} (Go 1.18+) are BreakNode/ContinueNode, which really do fall
// through to the default.
func TestUnsupportedNode(t *testing.T) {
	if _, err := Expand(`{{range .X}}{{break}}{{end}}`); err == nil {
		t.Error("expected an error for an unsupported node")
	} else if !strings.Contains(err.Error(), "unsupported template node") {
		t.Errorf("got %v", err)
	}
}

// TestCommentNode exercises `{{/* ... */}}` end to end (it is simply stripped from the
// output). It does not actually reach node()'s own *parse.CommentNode case, though: this
// package's parse.Parse call uses the default parser mode, which never emits
// CommentNode at all (that requires the ParseComments mode, enabled through a *Template's
// Option, which parse.Parse's package-level function has no way to set) - the comment
// is dropped by the lexer/parser before node() ever sees it. That switch case looks like
// dead code as currently wired.
func TestCommentNode(t *testing.T) {
	res, err := Expand(`SELECT 1 {{/* a comment */}} FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Expansions[0].SQL, "SELECT 1  FROM t"; got != want {
		t.Errorf("sql = %q, want %q", got, want)
	}
}

func TestBadDeclPipeline(t *testing.T) {
	if _, err := Expand(`{{$x := .A .B}}{{$x}}`); err == nil {
		t.Error("expected an error for a bad decl pipeline")
	}
}

func TestBadWithPipeline(t *testing.T) {
	if _, err := Expand(`{{with .A .B}}x{{end}}`); err == nil {
		t.Error("expected an error for a bad with pipeline")
	}
}

func TestBadRangePipeline(t *testing.T) {
	if _, err := Expand(`{{range .A .B}}x{{end}}`); err == nil {
		t.Error("expected an error for a bad range pipeline")
	}
}

func TestRangeSingleVarDecl(t *testing.T) {
	res, err := Expand(`{{range $v := .IDs}}{{$v}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	last := res.Expansions[len(res.Expansions)-1]
	if last.SQL != "$1$2" {
		t.Errorf("sql = %q", last.SQL)
	}
}

func TestRangeErrorInBody(t *testing.T) {
	if _, err := Expand(`{{range .IDs}}{{.A .B}}{{end}}`); err == nil {
		t.Error("expected an error from the range body")
	}
}

func TestRangeErrorInElse(t *testing.T) {
	if _, err := Expand(`{{range .IDs}}x{{else}}{{.A .B}}{{end}}`); err == nil {
		t.Error("expected an error from the range else-body")
	}
}

func TestIfErrorInElse(t *testing.T) {
	if _, err := Expand(`{{if .A}}x{{else}}{{.C .D}}{{end}}`); err == nil {
		t.Error("expected an error from the if's else-body")
	}
}
