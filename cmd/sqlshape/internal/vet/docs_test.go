package vet

// docs_test.go makes sure that what docs/*.md promises the checker will say, the checker
// actually says. Each Rejected/Passes fence in those files is prose first: most fences
// show only the half of a query that the point is about (the R struct, say, but not the
// P it is paired with, or vice versa), and lean on tables (orders, customers, ...) that
// are named across many sections but never declared once, in full, in one place. So a
// fence cannot be dropped into a Go file and run unmodified -- something has to complete
// it into a compiling program: an R or P this fence doesn't show, a schema.sql these
// tables didn't get.
//
// That completion lives here as ordinary Go source under testdata/src/docsex_*, one
// package per Rejected or Passes example, following the same testdata/src/<pkg> +
// analysistest shape every other test in this package already uses. What must NOT be
// hand-copied is the SQL/Go text the fence shows and the diagnostic wording its trailing
// comment (`// field ... has no result column`, `-- ^ ...`, `-- sqlshape: ...`) claims the
// checker will produce: both are read out of the .md file at test time by
// parseDocBlocks/docLine below, so a docs edit changes what this test compiles and
// expects without anyone updating a second copy.
//
// The comparison itself does not use analysistest's own "// want" mechanism: "// want"
// matches a regexp against the single line a diagnostic's position falls on, and several
// of these diagnostics (the ones that map into a `-- sqlshape: expect` line inside a
// multi-line template, in particular) do not share a line with anything a Go comment could
// sit on. Instead, assertDiagnostics below runs the analyzer through analysistest with a
// Testing stub that swallows its own pass/fail verdict, reads every analysis.Diagnostic
// out of the result, and checks -- regardless of position -- that each string docs claims
// is a literal substring (not a regexp: regexp.QuoteMeta is not involved) of exactly one
// diagnostic, with none left over on either side. That is what "the docs' words are the
// diagnostic" means operationally, and it is the same check the recipe asked for: a docs
// rewrite that drifts from the checker's actual wording fails here, on either side --
// fix whichever is wrong.
//
// Coverage: docs/checks.md (114 go/sql fences) is overwhelmingly Part 1 (statement-level
// checks); this harness currently exercises a representative slice of it -- the section
// that prompted this task (naming a trigger's error, both dialects) plus several
// self-contained Part 1 examples -- not the full 114. NOTES.local.md lists what is not
// wired up yet and why (mostly: Part 2/3 fences are schema-declaration syntax with `...`
// ellipses, not complete SQL, or CLI/runtime-only snippets -- see the exclusion notes in
// each Test function below for the ones adjacent to what is covered).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"

	// registers the MySQL dialect with x/dialect for this test binary, the way
	// dialects_test.go does for PostgreSQL; docs' MySQL examples need it.
	_ "github.com/kr9ly/sqlshape/check/mysql/v2/dialect"
)

// docBlock is one ```go or ```sql fenced code block lifted from a docs/*.md file.
type docBlock struct {
	lang    string // "go" or "sql"
	heading string // the nearest preceding ## / ### / #### heading text
	body    string // the fence's content, unindented, fences themselves excluded
	line    int    // 1-based line number of the opening fence, for error messages
}

var (
	docHeadingRE = regexp.MustCompile(`^(#{2,4})\s+(.*)`)
	docFenceRE   = regexp.MustCompile("^```(go|sql)\\s*$")
)

// parseDocBlocks reads path (a docs/*.md file) and returns every fenced go/sql block in
// file order, each tagged with the heading text of the section it appears under.
func parseDocBlocks(t *testing.T, path string) []docBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var blocks []docBlock
	heading := ""
	for i := 0; i < len(lines); i++ {
		if m := docHeadingRE.FindStringSubmatch(lines[i]); m != nil {
			heading = m[2]
			continue
		}
		if m := docFenceRE.FindStringSubmatch(lines[i]); m != nil {
			lang := m[1]
			j := i + 1
			var body []string
			for j < len(lines) && strings.TrimSpace(lines[j]) != "```" {
				body = append(body, lines[j])
				j++
			}
			blocks = append(blocks, docBlock{lang: lang, heading: heading, body: strings.Join(body, "\n"), line: i + 1})
			i = j
		}
	}
	return blocks
}

// nthBlock returns the occurrence'th (1-based, in file order) block of the given
// language directly under heading. It fails the test loudly if the doc no longer has
// that many -- a section being edited out from under this harness is a finding, not a
// silent skip.
func nthBlock(t *testing.T, blocks []docBlock, heading, lang string, occurrence int) docBlock {
	t.Helper()
	n := 0
	for _, b := range blocks {
		if b.heading == heading && b.lang == lang {
			n++
			if n == occurrence {
				return b
			}
		}
	}
	t.Fatalf("heading %q: no %s block #%d (found %d)", heading, lang, occurrence, n)
	return docBlock{}
}

// docLine returns the text of the line in body containing marker, with its comment
// syntax (a leading Go "//" or SQL "--", and a "^" caret pointing up at the line above)
// trimmed off -- the diagnostic wording the docs use, verbatim, not retyped here.
func docLine(t *testing.T, body, marker string) string {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if !strings.Contains(l, marker) {
			continue
		}
		s := l
		// take only the comment portion: a struct field's diagnostic is a trailing
		// "// text" after real code, not the whole line.
		if i := strings.Index(s, "//"); i >= 0 {
			s = s[i+2:]
		} else if i := strings.Index(s, "--"); i >= 0 {
			s = s[i+2:]
		}
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "^")
		return strings.TrimSpace(s)
	}
	t.Fatalf("no line containing %q in:\n%s", marker, body)
	return ""
}

// quietT is an analysistest.Testing that records rather than fails: analysistest.Run
// otherwise reports every diagnostic as "unexpected" the moment a file has no "// want"
// comments at all, which is always, here (see the package doc above for why this harness
// does not use "// want"). assertDiagnostics reads the real diagnostics out of the
// Result instead and ignores q entirely.
type quietT struct{ errs []string }

func (q *quietT) Errorf(format string, args ...any) {
	q.errs = append(q.errs, fmt.Sprintf(format, args...))
}

// runDocs runs Analyzer over dir/pkgs and returns every diagnostic it produced, in the
// order the packages were given. Each package's own name, as a qualifier
// ("<pkg>.TypeName"), is stripped from messages first: docs write the bare "TypeName"
// (the snippet has no package of its own to speak of), while go/types always prints
// a Named type's diagnostics package-qualified.
func runDocs(t *testing.T, dir string, pkgs ...string) []analysis.Diagnostic {
	t.Helper()
	var q quietT
	res := analysistest.Run(&q, dir, Analyzer, pkgs...)
	var diags []analysis.Diagnostic
	for _, r := range res {
		diags = append(diags, r.Diagnostics...)
	}
	for i := range diags {
		for _, pkg := range pkgs {
			diags[i].Message = strings.ReplaceAll(diags[i].Message, pkg+".", "")
		}
	}
	return diags
}

// assertDiagnostics checks that the analyzer's diagnostics over dir/pkgs are exactly what
// docs claims: each string in expect is a literal substring (strings.Contains, not a
// regexp) of exactly one diagnostic message, and no diagnostic is left unclaimed. Passing
// examples call this with expect == nil: any diagnostic at all is then a failure.
func assertDiagnostics(t *testing.T, dir string, pkgs []string, expect []string) {
	t.Helper()
	diags := runDocs(t, dir, pkgs...)
	used := make([]bool, len(diags))
	for _, e := range expect {
		found := false
		for i, d := range diags {
			if used[i] {
				continue
			}
			if strings.Contains(d.Message, e) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs promises a diagnostic containing %q, but the checker did not produce one; got: %s", e, formatDiags(diags))
		}
	}
	for i, d := range diags {
		if !used[i] {
			t.Errorf("checker produced a diagnostic docs does not claim: %s", d.Message)
		}
	}
}

func formatDiags(diags []analysis.Diagnostic) string {
	if len(diags) == 0 {
		return "(none)"
	}
	var parts []string
	for _, d := range diags {
		parts = append(parts, d.Message)
	}
	return strings.Join(parts, " | ")
}

func setSchema(t *testing.T, path string) {
	t.Helper()
	if err := Analyzer.Flags.Set("schema", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Analyzer.Flags.Set("schema", "") })
}

func docsMD(name string) string {
	return filepath.Join("..", "..", "..", "..", "docs", name)
}

// ---------------------------------------------------------------------------------------
// "Result columns bind to fields by name"
// ---------------------------------------------------------------------------------------

// TestDocsColumnBindByName runs docs/checks.md's and docs/checks.ja.md's "Result columns
// bind to fields by name" example: an unaliased join column with no matching field
// (Rejected), and the same query with the column aliased and the field tagged (Passes).
func TestDocsColumnBindByName(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Result columns bind to fields by name"},
		{"ja", "checks.ja.md", "結果列とフィールドは名前で対応する"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			ngSQL := nthBlock(t, blocks, tc.head, "sql", 1)
			okSQL := nthBlock(t, blocks, tc.head, "sql", 2) // the column aliased
			okGo := nthBlock(t, blocks, tc.head, "go", 2)   // the field tagged instead

			// docs shows two independent fixes ("alias the column, or name the column
			// in the tag"), not one aligned pair: okSQL (aliased) is meant to pair with
			// the original, untagged struct (ngGo minus its Rejected comment), and okGo
			// (tagged `col:"name"`) is meant to pair with the original, unaliased query
			// (ngSQL). Pairing okSQL with okGo instead (aliased column + a tag for the
			// unaliased name) would not bind at all.
			plainGo := dropInlineComment(ngGo.body, "has no result column")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_colbind_ng/ng.go", fmt.Sprintf(`package docsex_colbind_ng

import "github.com/kr9ly/sqlshape/v2"

%s

var Q = sqlshape.Query[Order, struct{}](%s)
`, ngGo.body, backtick(oneLineSQL(ngSQL.body))))
			writeFile(t, td, "src/docsex_colbind_ok_alias/ok.go", fmt.Sprintf(`package docsex_colbind_ok_alias

import "github.com/kr9ly/sqlshape/v2"

%s

var Q = sqlshape.Query[Order, struct{}](%s)
`, plainGo, backtick(oneLineSQL(okSQL.body))))
			writeFile(t, td, "src/docsex_colbind_ok_tag/ok.go", fmt.Sprintf(`package docsex_colbind_ok_tag

import "github.com/kr9ly/sqlshape/v2"

%s

var Q = sqlshape.Query[Order, struct{}](%s)
`, okGo.body, backtick(oneLineSQL(ngSQL.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_colbind_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_colbind_ng"}, []string{
				docLine(t, ngGo.body, "has no result column"),
				docLine(t, ngSQL.body, "has no field in Order"),
			})
			assertDiagnostics(t, td, []string{"docsex_colbind_ok_alias"}, nil)
			assertDiagnostics(t, td, []string{"docsex_colbind_ok_tag"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A column that may be NULL needs a field that can hold NULL"
// ---------------------------------------------------------------------------------------

func TestDocsNullableColumn(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A column that may be NULL needs a field that can hold NULL"},
		{"ja", "checks.ja.md", "NULLになりうる列はNULLを受けられる型で受ける"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)
			okGo := nthBlock(t, blocks, tc.head, "go", 2)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_null_ng/ng.go", fmt.Sprintf(`package docsex_null_ng

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

%s

var Q = sqlshape.Query[User, struct{}](%s)
`, ngGo.body, backtick(oneLineSQL(sql.body))))
			writeFile(t, td, "src/docsex_null_ok/ok.go", fmt.Sprintf(`package docsex_null_ok

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

%s

var Q = sqlshape.Query[User, struct{}](%s)
`, okGo.body, backtick(oneLineSQL(sql.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_null_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_null_ng"}, []string{
				docLine(t, ngGo.body, "may be NULL"),
			})
			assertDiagnostics(t, td, []string{"docsex_null_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Fix a unique key by equality" (the `One` proof)
// ---------------------------------------------------------------------------------------

func TestDocsOneProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Fix a unique key by equality"},
		{"ja", "checks.ja.md", "一意キーを等値で固定する"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "go", 1)
			ok := nthBlock(t, blocks, tc.head, "go", 2)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_one_ng/ng.go", fmt.Sprintf(`package docsex_one_ng

import "github.com/kr9ly/sqlshape/v2"

type User struct {
	ID    int64
	Email string
}

%s
`, stripTrailingLineComment(ng.body, "One: cannot prove")))
			writeFile(t, td, "src/docsex_one_ok/ok.go", fmt.Sprintf(`package docsex_one_ok

import "github.com/kr9ly/sqlshape/v2"

type User struct {
	ID    int64
	Email string
}

%s
`, ok.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_oneproof_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_one_ng"}, []string{
				docLine(t, ng.body, "One: cannot prove"),
			})
			assertDiagnostics(t, td, []string{"docsex_one_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Bulk loading with COPY (PostgreSQL)"
// ---------------------------------------------------------------------------------------

// TestDocsCopy reuses the shared testdata/schema.sql, whose order_items table already
// matches this example exactly (order_id, line_no NOT NULL with no default, sku): no
// docs-specific schema needed.
func TestDocsCopy(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Bulk loading with COPY (PostgreSQL)"},
		{"ja", "checks.ja.md", "COPYで一括ロードする（PostgreSQL）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "go", 1)
			ok := nthBlock(t, blocks, tc.head, "go", 2)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_copy_ng/ng.go", fmt.Sprintf(`package docsex_copy_ng

import "github.com/kr9ly/sqlshape/postgres/v2"

%s
`, ng.body))
			writeFile(t, td, "src/docsex_copy_ok/ok.go", fmt.Sprintf(`package docsex_copy_ok

import "github.com/kr9ly/sqlshape/postgres/v2"

%s
`, ok.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_copy_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_copy_ng"}, []string{
				docLine(t, ng.body, "is not copied"),
			})
			assertDiagnostics(t, td, []string{"docsex_copy_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Name the errors a trigger raises" -- the section that prompted this task: docs'
// `-- sqlshape: expect OrderTooLarge` (naming an error by the Name a schema's
// `-- sqlshape: error <code> = <Name>` line gives it) must be exactly what the checker
// accepts, in both directions (a correct declaration, a wrong one, a duplicate, an
// unknown code, a missing declaration) and both dialects (PostgreSQL RAISE ... ERRCODE,
// MySQL SIGNAL ... MYSQL_ERRNO).
// ---------------------------------------------------------------------------------------

func TestDocsErrorNames(t *testing.T) {
	blocks := parseDocBlocks(t, docsMD("checks.md"))
	head := "Name the errors a trigger raises"
	declOK := nthBlock(t, blocks, head, "go", 1)   // var OrderTooLarge = sqlshape.Error("P0401")
	okSQL := nthBlock(t, blocks, head, "sql", 2)   // -- sqlshape: expect OrderTooLarge, orders_customer_id_fkey
	wrong := nthBlock(t, blocks, head, "go", 2)    // var Wrong = ...
	dup := nthBlock(t, blocks, head, "go", 3)      // var A, B = ...
	unknown := nthBlock(t, blocks, head, "go", 4)  // var Ghost = ...
	missing := nthBlock(t, blocks, head, "go", 5)  // no var at all; comment only
	mysqlOK := nthBlock(t, blocks, head, "sql", 4) // -- sqlshape: expect OrderTooLarge, fk_orders_customer

	td := t.TempDir()
	writeFile(t, td, "src/docsex_errname_ok/ok.go", fmt.Sprintf(`package docsex_errname_ok

import "github.com/kr9ly/sqlshape/v2"

%s

var Q = sqlshape.Query[struct{}, struct {
	CustomerID int64
	Total      string
}](%s)
`, stripTrailingLineComment(declOK.body, ""), backtick(oneLineSQL(okSQL.body))))

	// wrong/dup/unknown each need at least one sqlshape.Query call in the package for
	// vet to run its error-declaration checks at all (see errnamewrong/errnamedup/
	// errnameunknown in testdata/src for the same requirement); a plain read is enough
	// and is not itself part of what the doc's comment claims.
	writeFile(t, td, "src/docsex_errname_wrong/wrong.go", fmt.Sprintf(`package docsex_errname_wrong

import "github.com/kr9ly/sqlshape/v2"

%s

var read = sqlshape.Query[int64, struct{}]("SELECT id FROM orders")
`, wrong.body))

	writeFile(t, td, "src/docsex_errname_dup/dup.go", fmt.Sprintf(`package docsex_errname_dup

import "github.com/kr9ly/sqlshape/v2"

%s

var read = sqlshape.Query[int64, struct{}]("SELECT id FROM orders")
`, dup.body))

	writeFile(t, td, "src/docsex_errname_unknown/unknown.go", fmt.Sprintf(`package docsex_errname_unknown

import "github.com/kr9ly/sqlshape/v2"

%s

var read = sqlshape.Query[int64, struct{}]("SELECT id FROM orders")
`, unknown.body))

	// missing: docs shows no code at all for this case (only the comment describing
	// what the checker says), so the query is this harness's own completion -- the
	// same expect line as the ok/mysql examples, in a package that declares no var at
	// all for P0401.
	writeFile(t, td, "src/docsex_errname_missing/missing.go", `package docsex_errname_missing

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct {
	CustomerID int64
	Total      string
}]("-- sqlshape: expect OrderTooLarge, orders_customer_id_fkey\nINSERT INTO orders (customer_id, total) VALUES ({{.CustomerID}}, {{.Total}})")
`)

	writeFile(t, td, "src/docsex_errname_mysql_ok/ok.go", fmt.Sprintf(`package docsex_errname_mysql_ok

import "github.com/kr9ly/sqlshape/v2"

var OrderTooLarge = sqlshape.Error("30001")

var Q = sqlshape.Query[struct{}, struct {
	CustomerID int64
	Total      string
}](%s)
`, backtick(oneLineSQL(mysqlOK.body))))
	copyRuntime(t, td)

	t.Run("pg-ok", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_errname_ok"}, nil)
	})
	t.Run("pg-wrong-name", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_errname_wrong"}, []string{
			docLine(t, wrong.body, "schema names"),
		})
	})
	t.Run("pg-duplicate", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_schema.sql"))
		// docs' A/B example is a duplicate-declaration illustration only: neither A
		// nor B is the schema's own name for P0401 ("OrderTooLarge"), so a real
		// compile also raises the wrong-name diagnostic for each -- a true side
		// effect of the names docs chose to illustrate "both declare", not a claim
		// docs makes or a mismatch to fix. wrongNameLine (from the "wrong" example,
		// same schema) gives the message's shape; substituting the name it blames
		// keeps this literal rather than hand-typed.
		wrongNameLine := docLine(t, wrong.body, "schema names")
		assertDiagnostics(t, td, []string{"docsex_errname_dup"}, []string{
			docLine(t, dup.body, "both declare"),
			strings.Replace(wrongNameLine, `"Wrong"`, `"A"`, 1),
			strings.Replace(wrongNameLine, `"Wrong"`, `"B"`, 1),
		})
	})
	t.Run("pg-unknown-code", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_errname_unknown"}, []string{
			docLine(t, unknown.body, "declares no error"),
		})
	})
	t.Run("pg-missing-declaration", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_errname_missing"}, []string{
			docLine(t, missing.body, "has no `var"),
		})
	})
	t.Run("mysql-ok", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_errname_mysql_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_errname_mysql_ok"}, nil)
	})
}

// ---------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------

// oneLineSQL joins a fenced sql block into as few physical lines as possible: a "--"
// documentation caret (`-- ^ ...`, pointing at the line above) is dropped entirely, a
// real SQL comment or `-- sqlshape:` directive is kept on its own line (a SQL line
// comment runs to end of line, so it cannot be joined with what follows), and
// consecutive plain SQL lines between comments are joined with a space.
func oneLineSQL(body string) string {
	var out []string
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, " "))
			buf = nil
		}
	}
	for _, l := range strings.Split(body, "\n") {
		s := strings.TrimSpace(l)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "--") {
			if strings.Contains(s, "^") {
				continue
			}
			flush()
			out = append(out, s)
			continue
		}
		buf = append(buf, s)
	}
	flush()
	return strings.Join(out, "\n")
}

// stripTrailingLineComment drops the line containing marker entirely (used where a
// docs snippet's diagnostic-bearing comment is on a line of its own, appended after
// real code, and must not appear in the compiled source). marker == "" strips nothing.
func stripTrailingLineComment(body, marker string) string {
	if marker == "" {
		return body
	}
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, marker) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// dropInlineComment finds the line in body containing marker and truncates it at the
// start of its trailing "// ..." comment, keeping the code before it (used where the
// comment sits on the same line as real code, e.g. a struct field, rather than as a
// line of its own).
func dropInlineComment(body, marker string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, marker) {
			if i := strings.Index(l, "//"); i >= 0 {
				l = strings.TrimRight(l[:i], " \t")
			}
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func backtick(s string) string {
	return "`" + s + "`"
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyRuntime symlinks the vet package's own vendored sqlshape/postgres runtime stubs
// (testdata/src/github.com/...) into td/src, the same GOPATH layout analysistest.TestData
// already uses, so packages built under td can import them.
func copyRuntime(t *testing.T, td string) {
	t.Helper()
	src := absPath(t, "testdata/src/github.com")
	if err := os.MkdirAll(filepath.Join(td, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(td, "src", "github.com")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}

func absPath(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
