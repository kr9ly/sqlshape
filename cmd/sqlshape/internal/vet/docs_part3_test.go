package vet

// docs_part3_test.go extends docs_test.go's harness (parseDocBlocks / nthBlock / docLine /
// assertDiagnostics / setSchema / writeFile / copyRuntime / absPath / backtick / oneLineSQL
// / stripTrailingLineComment / dropInlineComment, all defined there and reused here
// unchanged) over docs/checks.md's Part 2 ("How a declaration works" through "Different
// callers, different rules (`context`)") and the headings the recipe groups into "Part 3"
// ("The same rules for SQL outside Go" through "Problems in the schema itself"). This file
// does not edit docs_test.go; helpers this file adds of its own are named with a p3 prefix
// to stay out of its way, and its own testdata lives under testdata/docsex_p3_*.
//
// "How a declaration works" is excluded: it has one fenced block, the bare directive
// grammar (`-- sqlshape: require <what> [on <kinds>]`), not an instance of it -- there is
// no schema or statement to complete.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// p3Entry is one SQL statement plus the comment line(s) immediately following it in a
// docs fence, split out where a single fence holds several independent statements (docs'
// "Passes, all three" / "Rejected: ..." fences, and a Passes/Rejected pair sharing one
// fence via a leading "-- passes; ..." / trailing message comment) rather than the
// one-fence-per-statement shape the rest of docs_test.go's helpers assume.
type p3Entry struct {
	SQL     string
	Comment string
}

// p3SplitEntries walks body line by line: a line that is not a "--" comment starts a new
// entry (its own text kept as written, trailing inline "-- ..." included -- harmless as a
// real SQL comment); every "--" line immediately after belongs to that entry's Comment
// ("^" caret lines, which point at the line above rather than adding wording, are
// dropped).
func p3SplitEntries(body string) []p3Entry {
	var out []p3Entry
	var cur *p3Entry
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "--") {
			c := strings.TrimSpace(strings.TrimPrefix(t, "--"))
			if strings.HasPrefix(c, "^") {
				continue
			}
			if cur != nil {
				if cur.Comment != "" {
					cur.Comment += " "
				}
				cur.Comment += c
			}
			continue
		}
		out = append(out, p3Entry{SQL: l})
		cur = &out[len(out)-1]
	}
	return out
}

// p3Quote is a Go double-quoted string literal (strconv.Quote) for doc text that may
// itself contain a backtick (the visible-where Rejected comment quotes `-- sqlshape:
// unfiltered memos` inline): docs_test.go's own backtick() helper assumes the text has
// none, which holds for every fence that helper is used on there but not for this one.
func p3Quote(s string) string {
	return strconv.Quote(s)
}

// p3NoDiagnostics is assertDiagnostics(t, dir, pkgs, nil): a Passes package must produce
// none.
func p3NoDiagnostics(t *testing.T, dir string, pkgs ...string) {
	t.Helper()
	assertDiagnostics(t, dir, pkgs, nil)
}

// p3SomeDiagnostic checks only that the package produced at least one diagnostic, for the
// cases in this file where docs describes a statement as Rejected but does not spell the
// checker's exact wording anywhere in the fence (the EXISTS witness and sensitive-column
// examples both label statements "fails" / "Rejected" in prose or an inline comment,
// without a "-- <message>" line docLine could read) -- an exact-substring check is not
// possible there without retyping wording docs itself does not give, which is exactly what
// this harness must not do (see docs_test.go's package doc). Logged in NOTES.local.md.
func p3SomeDiagnostic(t *testing.T, dir string, pkgs ...string) {
	t.Helper()
	diags := runDocs(t, dir, pkgs...)
	if len(diags) == 0 {
		t.Errorf("docs says this is rejected, but the checker produced no diagnostic for %v", pkgs)
	}
}

// ---------------------------------------------------------------------------------------
// "Every read carries the visibility predicate (`visible where`)"
// ---------------------------------------------------------------------------------------

func TestDocsVisibleWhere(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Every read carries the visibility predicate (`visible where`)"},
		{"ja", "checks.ja.md", "必ず付ける読み取り条件（`visible where`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 2)
			ok := nthBlock(t, blocks, tc.head, "sql", 3)
			unfiltered := nthBlock(t, blocks, tc.head, "sql", 4)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_visible_ng/ng.go", fmt.Sprintf(`package docsex_p3_visible_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID   int64
	Body string
}, struct{ UserID int64 }](%s)
`, p3Quote(oneLineSQL(ng.body))))
			writeFile(t, td, "src/docsex_p3_visible_ok/ok.go", fmt.Sprintf(`package docsex_p3_visible_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID   int64
	Body string
}, struct{ UserID int64 }](%s)
`, backtick(oneLineSQL(ok.body))))
			writeFile(t, td, "src/docsex_p3_visible_unfiltered/uf.go", fmt.Sprintf(`package docsex_p3_visible_unfiltered

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID   int64
	Body string
}, struct{ ID int64 }](%s)
`, backtick(oneLineSQL(unfiltered.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_visiblewhere_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p3_visible_ng"}, []string{
				docLine(t, ng.body, "add that predicate"),
			})
			p3NoDiagnostics(t, td, "docsex_p3_visible_ok")
			p3NoDiagnostics(t, td, "docsex_p3_visible_unfiltered")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A column is pinned on every statement (`pinned`, `-require-columns`)"
//
// Only the tenant_id example is exercised: the "optimistic locking" follow-on shares a
// single fence between a Passes statement and the wording for a Rejected variant docs
// describes only in prose ("without `AND version = ...`"), and reaching it would mean
// synthesizing SQL text of our own rather than reading it out of the doc, plus a
// bump_version() trigger the fence never shows either -- lower value for what it costs;
// noted in NOTES.local.md instead of silently dropped.
// ---------------------------------------------------------------------------------------

func TestDocsPinnedColumn(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A column is pinned on every statement (`pinned`, `-require-columns`)"},
		{"ja", "checks.ja.md", "列を必ず固定する（`pinned`、`-require-columns`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 2)
			ok := nthBlock(t, blocks, tc.head, "sql", 3)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_pinned_ng/ng.go", fmt.Sprintf(`package docsex_p3_pinned_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID    int64
	Total string
}, struct{ ID int64 }](%s)
`, backtick(oneLineSQL(ng.body))))
			writeFile(t, td, "src/docsex_p3_pinned_ok/ok.go", fmt.Sprintf(`package docsex_p3_pinned_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID    int64
	Total string
}, struct {
	ID       int64
	TenantID int64
}](%s)
`, backtick(oneLineSQL(ok.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_pinned_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p3_pinned_ng"}, []string{
				docLine(t, ng.body, "is not pinned"),
			})
			p3NoDiagnostics(t, td, "docsex_p3_pinned_ok")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Tables are read through views (`via view`, `-no-table-reads` / `-no-tables`)"
// ---------------------------------------------------------------------------------------

func TestDocsViaView(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Tables are read through views (`via view`, `-no-table-reads` / `-no-tables`)"},
		{"ja", "checks.ja.md", "テーブルを直接読まない（`via view`、`-no-table-reads` / `-no-tables`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 2)
			ok := nthBlock(t, blocks, tc.head, "sql", 3)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_viaview_ng/ng.go", fmt.Sprintf(`package docsex_p3_viaview_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ ID int64; Name string }, struct{}](%s)
`, backtick(oneLineSQL(ng.body))))
			writeFile(t, td, "src/docsex_p3_viaview_ok/ok.go", fmt.Sprintf(`package docsex_p3_viaview_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	ID           int64
	CustomerName string
}, struct{}](%s)
`, backtick(oneLineSQL(ok.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_viaview_schema.sql"))
			if err := Analyzer.Flags.Set("no-table-reads", "true"); err != nil {
				t.Fatal(err)
			}
			defer Analyzer.Flags.Set("no-table-reads", "false")

			assertDiagnostics(t, td, []string{"docsex_p3_viaview_ng"}, []string{
				docLine(t, ng.body, "is read directly"),
				// docs' own comment quotes only the orders half of this message; the
				// query also joins customers, which -no-table-reads flags the same way
				// (the wording is the same rule's, applied to the other table docs'
				// example references but does not spell out).
				"table customers is read directly; with -no-table-reads application code reads views (tables are written, not read)",
			})
			p3NoDiagnostics(t, td, "docsex_p3_viaview_ok")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A predicate across tables has a witness (`EXISTS`)"
//
// Docs labels these three statements "Passes, all three" and two more "Rejected" without
// giving either group's diagnostic wording (there is no "-- ..." message line to read for
// any of the five): p3SomeDiagnostic checks the Rejected pair produced something, and
// p3NoDiagnostics checks the Passes trio produced nothing, which is what docs actually
// claims here -- not a specific message.
// ---------------------------------------------------------------------------------------

func TestDocsExistsWitness(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A predicate across tables has a witness (`EXISTS`)"},
		{"ja", "checks.ja.md", "表をまたぐ述語には証人が要る（`EXISTS`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			pass := nthBlock(t, blocks, tc.head, "sql", 2)
			reject := nthBlock(t, blocks, tc.head, "sql", 3)

			passEntries := p3SplitEntries(pass.body)
			if len(passEntries) != 3 {
				t.Fatalf("expected 3 passing statements, got %d", len(passEntries))
			}
			rejectEntries := p3SplitEntries(reject.body)
			if len(rejectEntries) != 2 {
				t.Fatalf("expected 2 rejected statements, got %d", len(rejectEntries))
			}

			td := t.TempDir()
			for i, e := range passEntries {
				writeFile(t, td, fmt.Sprintf("src/docsex_p3_exists_ok%d/ok.go", i), fmt.Sprintf(`package docsex_p3_exists_ok%d

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ Carrier string }, struct{ T int64 }](%s)
`, i, backtick(e.SQL)))
			}
			for i, e := range rejectEntries {
				writeFile(t, td, fmt.Sprintf("src/docsex_p3_exists_ng%d/ng.go", i), fmt.Sprintf(`package docsex_p3_exists_ng%d

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ Carrier string }, struct{ T int64 }](%s)
`, i, backtick(e.SQL)))
			}
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_exists_schema.sql"))
			for i := range passEntries {
				p3NoDiagnostics(t, td, fmt.Sprintf("docsex_p3_exists_ok%d", i))
			}
			for i := range rejectEntries {
				p3SomeDiagnostic(t, td, fmt.Sprintf("docsex_p3_exists_ng%d", i))
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// "An aggregate is reached through its root, one per statement (`aggregate`)"
//
// Only the primary Rejected example (reading across aggregates) is exercised; the
// `lock <column>` follow-on is a bare illustrative UPDATE, not itself labelled Rejected or
// Passes, and adds a second schema shape (a version column, a foreign-key witness) for one
// more statement -- represented well enough by the primary example for this heading's
// purpose here. Noted in NOTES.local.md.
// ---------------------------------------------------------------------------------------

func TestDocsAggregate(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "An aggregate is reached through its root, one per statement (`aggregate`)"},
		{"ja", "checks.ja.md", "集約にはルート経由で触り、1文で1つだけ触る（`aggregate`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 2)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_aggregate_ng/ng.go", fmt.Sprintf(`package docsex_p3_aggregate_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	Status string
	ID     int64
}, struct{ ID int64 }](%s)
`, backtick(oneLineSQL(ng.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_aggregate_schema.sql"))
			// docs quotes only orders' side of the message; the checker reports the
			// same violation from each table's own aggregate too, so invoices' side
			// (the same wording with the two names swapped) is unclaimed otherwise.
			ordersMsg := docLine(t, ng.body, "one statement, one aggregate")
			invoicesMsg := strings.NewReplacer("orders", "invoices", "invoices", "orders").Replace(ordersMsg)
			assertDiagnostics(t, td, []string{"docsex_p3_aggregate_ng"}, []string{
				ordersMsg,
				invoicesMsg,
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A status column moves along its declared transitions (`transitions`)"
// ---------------------------------------------------------------------------------------

func TestDocsTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A status column moves along its declared transitions (`transitions`)"},
		{"ja", "checks.ja.md", "ステータス列は宣言した遷移でしか動かさない（`transitions`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ok := nthBlock(t, blocks, tc.head, "sql", 2)
			ng := nthBlock(t, blocks, tc.head, "sql", 3)

			ngEntries := p3SplitEntries(ng.body)
			if len(ngEntries) != 2 {
				t.Fatalf("expected 2 rejected statements, got %d", len(ngEntries))
			}

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_transitions_ok/ok.go", fmt.Sprintf(`package docsex_p3_transitions_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{ ID int64 }](%s)
`, backtick(ok.body)))
			for i, e := range ngEntries {
				writeFile(t, td, fmt.Sprintf("src/docsex_p3_transitions_ng%d/ng.go", i), fmt.Sprintf(`package docsex_p3_transitions_ng%d

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{ ID int64 }](%s)
`, i, backtick(e.SQL)))
			}
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_transitions_schema.sql"))
			p3NoDiagnostics(t, td, "docsex_p3_transitions_ok")
			for i, e := range ngEntries {
				assertDiagnostics(t, td, []string{fmt.Sprintf("docsex_p3_transitions_ng%d", i)}, []string{e.Comment})
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Append-only tables, paired writes, single-row deletes (`never`, `paired`, `single`)"
//
// `never` itself has no fenced Rejected/Passes statement (its message is quoted inline in
// prose, not shown as a checked example): only `paired` and `single` are exercised here.
// ---------------------------------------------------------------------------------------

func TestDocsNeverPairedSingle(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Append-only tables, paired writes, single-row deletes (`never`, `paired`, `single`)"},
		{"ja", "checks.ja.md", "追記専用、対になる書き込み、1行だけの削除（`never`、`paired`、`single`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			paired := nthBlock(t, blocks, tc.head, "sql", 2)
			single := nthBlock(t, blocks, tc.head, "sql", 3)

			// single's two DELETE statements and single's own trailing message line:
			// unlike every other fence in this file, its two SQL lines are each a
			// complete standalone statement (no continuation), so p3SplitEntries
			// (which cannot tell that apart from the paired fence's two-line WITH
			// statement below) is not used here.
			var singleLines []string
			for _, l := range strings.Split(single.body, "\n") {
				if strings.TrimSpace(l) != "" {
					singleLines = append(singleLines, l)
				}
			}
			if len(singleLines) != 3 {
				t.Fatalf("expected 2 statements + 1 comment line in the single fence, got %d lines", len(singleLines))
			}
			singleOK, singleNG, singleNGComment := singleLines[0], singleLines[1], docLine(t, single.body, "One proof")

			td := t.TempDir()
			// paired: docs' own WITH statement has literal "(...)" placeholders (its
			// point is the shape, not real columns), so this harness completes it with
			// orders' and outbox's actual columns rather than compiling the ellipsis
			// verbatim; the bare "INSERT INTO orders" docs describes only in prose ("a
			// bare INSERT INTO orders:") is completed the same way.
			writeFile(t, td, "src/docsex_p3_paired_ok/ok.go", `package docsex_p3_paired_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{}]("-- sqlshape: expect outbox_pkey\nWITH o AS (INSERT INTO orders (status) VALUES ('new') RETURNING id)\nINSERT INTO outbox (id, payload) SELECT id, 'created' FROM o")
`)
			writeFile(t, td, "src/docsex_p3_paired_ng/ng.go", `package docsex_p3_paired_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{}]("INSERT INTO orders (status) VALUES ('new')")
`)
			// single: the fence's first statement passes, the second is rejected with
			// the wording the fence's own trailing comment gives.
			writeFile(t, td, "src/docsex_p3_single_ok/ok.go", fmt.Sprintf(`package docsex_p3_single_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{ ID int64 }](%s)
`, backtick(singleOK)))
			writeFile(t, td, "src/docsex_p3_single_ng/ng.go", fmt.Sprintf(`package docsex_p3_single_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct{}](%s)
`, backtick(singleNG)))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_neverpairedsingle_schema.sql"))
			p3NoDiagnostics(t, td, "docsex_p3_paired_ok")
			assertDiagnostics(t, td, []string{"docsex_p3_paired_ng"}, []string{
				docLine(t, paired.body, "must also write outbox"),
			})
			p3NoDiagnostics(t, td, "docsex_p3_single_ok")
			assertDiagnostics(t, td, []string{"docsex_p3_single_ng"}, []string{singleNGComment})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Labelled columns are read only where allowed (`sensitive`, `may read`)"
//
// The first statement's comment gives an exact message (docLine picks it up); the second
// is only described in prose ("fails too: ...", not the checker's own wording), so it is
// checked with p3SomeDiagnostic instead of an exact match, as with the EXISTS section
// above.
// ---------------------------------------------------------------------------------------

func TestDocsSensitiveColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Labelled columns are read only where allowed (`sensitive`, `may read`)"},
		{"ja", "checks.ja.md", "ラベル付きの列は許された文脈でしか読まない（`sensitive`、`may read`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			block := nthBlock(t, blocks, tc.head, "sql", 2)
			entries := p3SplitEntries(block.body)
			if len(entries) != 3 {
				t.Fatalf("expected 3 statements, got %d", len(entries))
			}

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_sensitive_direct/direct.go", fmt.Sprintf(`package docsex_p3_sensitive_direct

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ Email string }, struct{ ID int64 }](%s)
`, backtick(entries[0].SQL)))
			writeFile(t, td, "src/docsex_p3_sensitive_view/view.go", fmt.Sprintf(`package docsex_p3_sensitive_view

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ Email string }, struct{ ID int64 }](%s)
`, backtick(entries[1].SQL)))
			writeFile(t, td, "src/docsex_p3_sensitive_masked/masked.go", fmt.Sprintf(`package docsex_p3_sensitive_masked

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ PhoneMasked string }, struct{}](%s)
`, backtick(entries[2].SQL)))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_sensitive_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p3_sensitive_direct"}, []string{
				docLine(t, block.body, "is pii"),
			})
			p3SomeDiagnostic(t, td, "docsex_p3_sensitive_view")
			p3NoDiagnostics(t, td, "docsex_p3_sensitive_masked")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Different callers, different rules (`context`)"
//
// Docs shows this rule's schema and package-comment syntax but no Rejected/Passes SQL
// example with the checker's own wording; per the recipe, a declaration-syntax section
// like this one is completed with one statement of our own that discharges the context's
// own obligation (`require id = $1 on delete`, the tenant pin waived), checking only that
// the declaration reads with no Problems -- not any specific diagnostic docs does not
// give.
// ---------------------------------------------------------------------------------------

func TestDocsContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Different callers, different rules (`context`)"},
		{"ja", "checks.ja.md", "呼び出し元ごとに規約を変える（`context`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			pkg := nthBlock(t, blocks, tc.head, "go", 1) // the `package ops` comment fence

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_context_ops/ops.go", fmt.Sprintf(`%s

import "github.com/kr9ly/sqlshape/v2"

var deleteByID = sqlshape.Query[struct{}, struct{ ID int64 }](`+"`DELETE FROM orders WHERE id = {{.ID}}`"+`)
`, pkg.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_context_schema.sql"))
			p3NoDiagnostics(t, td, "docsex_p3_context_ops")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "The schema names its PostgreSQL version (`postgres`)"
//
// Docs' fence is the declaration syntax itself; the Rejected/OK cases are a bullet list,
// not fenced examples, so this is exercised directly through loadSchema (same function
// loadschema_test.go's own TestLoadSchema* tests use), one call per bullet, rather than
// through analysistest.
// ---------------------------------------------------------------------------------------

func TestDocsPostgresVersionDeclaration(t *testing.T) {
	write := func(t *testing.T, sql string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "schema.sql")
		if err := os.WriteFile(p, []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("no-declaration-anywhere", func(t *testing.T) {
		if _, err := loadSchema(write(t, "CREATE TABLE t (id bigint PRIMARY KEY);\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("unsupported-version", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: postgres 99\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("two-declarations-disagree", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: postgres 17\n-- sqlshape: postgres 18\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("ok-repeated-same-value", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: postgres 17\n-- sqlshape: postgres 17\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------------------
// "The schema names its MySQL version (`mysql`)"
// ---------------------------------------------------------------------------------------

func TestDocsMySQLVersionDeclaration(t *testing.T) {
	write := func(t *testing.T, sql string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "schema.sql")
		if err := os.WriteFile(p, []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("unsupported-version", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: mysql 5.7\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("also-names-postgres", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: postgres 17\n-- sqlshape: mysql 8.4\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("ok", func(t *testing.T) {
		if _, err := loadSchema(write(t, "-- sqlshape: mysql 8.4\nCREATE TABLE t (id bigint PRIMARY KEY);\n")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------------------
// "The schema names the server's settings (`server`)"
//
// Only MySQL's two variables are checked at all (docs: "for PostgreSQL every variable,
// for now"); a bad one is a schema Problem, not a load error.
// ---------------------------------------------------------------------------------------

func TestDocsServerSettings(t *testing.T) {
	write := func(t *testing.T, sql string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "schema.sql")
		if err := os.WriteFile(p, []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("ok", func(t *testing.T) {
		ls, err := loadSchema(write(t, "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'\n-- sqlshape: server lower_case_table_names = 1\nCREATE TABLE t (id bigint PRIMARY KEY);\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ls.problems) != 0 {
			t.Fatalf("unexpected problems: %v", ls.problems)
		}
	})
	t.Run("unknown-variable", func(t *testing.T) {
		ls, err := loadSchema(write(t, "-- sqlshape: mysql 8.4\n-- sqlshape: server nonexistent_var = 1\nCREATE TABLE t (id bigint PRIMARY KEY);\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !p3ContainsSubstring(ls.problems, "not a variable sqlshape reads for MySQL") {
			t.Errorf("problems missing the expected wording; got: %v", ls.problems)
		}
	})
	t.Run("bad-lower-case-table-names", func(t *testing.T) {
		ls, err := loadSchema(write(t, "-- sqlshape: mysql 8.4\n-- sqlshape: server lower_case_table_names = 3\nCREATE TABLE t (id bigint PRIMARY KEY);\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !p3ContainsSubstring(ls.problems, "want 0, 1 or 2") {
			t.Errorf("problems missing the expected wording; got: %v", ls.problems)
		}
	})
}

func p3ContainsSubstring(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------
// "Do not run SQL that bypasses sqlshape (`-raw-sql`)"
// ---------------------------------------------------------------------------------------

func TestDocsRawSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not run SQL that bypasses sqlshape (`-raw-sql`)"},
		{"ja", "checks.ja.md", "sqlshapeを通さないSQLを書かない（`-raw-sql`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "go", 1)
			ok := nthBlock(t, blocks, tc.head, "go", 2)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_rawsql_ng/ng.go", fmt.Sprintf(`package docsex_p3_rawsql_ng

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func f(ctx context.Context, pool *pgxpool.Pool, where string) {
	%s
	_, _ = rows, err
}
`, dropInlineComment(ng.body, "sqlshape:")))
			writeFile(t, td, "src/docsex_p3_rawsql_ok/ok.go", fmt.Sprintf(`package docsex_p3_rawsql_ok

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func f(ctx context.Context, pool *pgxpool.Pool, status int64) {
	%s
	_, _ = rows, err
}
`, ok.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p3_rawsql_ng"}, []string{
				docLine(t, ng.body, "SQL passed to Query must be a constant"),
			})
			p3NoDiagnostics(t, td, "docsex_p3_rawsql_ok")
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A package references only its schemas (`-schemas`, PostgreSQL)"
// ---------------------------------------------------------------------------------------

func TestDocsSchemasBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A package references only its schemas (`-schemas`, PostgreSQL)"},
		{"ja", "checks.ja.md", "パッケージは自分のスキーマだけを参照する（`-schemas`、PostgreSQL）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 1)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p3_schemas_ng/ng.go", fmt.Sprintf(`package docsex_p3_schemas_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[int64, struct{}](%s)
`, backtick(oneLineSQL(ng.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_schemas_schema.sql"))
			if err := Analyzer.Flags.Set("schemas", "a_api,b_private"); err != nil {
				t.Fatal(err)
			}
			defer Analyzer.Flags.Set("schemas", "")

			assertDiagnostics(t, td, []string{"docsex_p3_schemas_ng"}, []string{
				docLine(t, ng.body, "is outside the schemas"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Problems in the schema itself"
//
// Both fences embed the checker's own wording as a trailing comment inside the schema
// text they show; loadSchema (the same function loadschema_test.go's own tests use) is
// exercised directly, one problem per fence, rather than through analysistest.
// ---------------------------------------------------------------------------------------

func TestDocsSchemaProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Problems in the schema itself"},
		{"ja", "checks.ja.md", "スキーマ自体の問題"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			execFence := nthBlock(t, blocks, tc.head, "sql", 1)

			td := t.TempDir()
			// the schema problem is reported at the package's first Query, the way
			// TestPLpgSQL (vet_test.go) and its plpgsql/plpgsql.go fixture already
			// exercise this same mechanism; -strict is needed for the dynamic EXECUTE
			// note (an advisory), as it is there.
			writeFile(t, td, "src/docsex_p3_schemaproblems/q.go", `package docsex_p3_schemaproblems

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[int64, struct{}](`+"`SELECT id FROM orders`"+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p3_schemaproblems_schema.sql"))
			if err := Analyzer.Flags.Set("strict", "true"); err != nil {
				t.Fatal(err)
			}
			defer Analyzer.Flags.Set("strict", "false")

			diags := runDocs(t, td, "docsex_p3_schemaproblems")
			joined := formatDiags(diags)
			if want := docLine(t, execFence.body, "EXECUTE runs SQL built at run time"); !strings.Contains(joined, want) {
				t.Errorf("diagnostics missing %q; got: %s", want, joined)
			}
			// docs' fence writes the byte offset as a placeholder ("<byte offset in
			// schema.sql>"): the real offset is where "nmae" falls in the whole
			// schema.sql the package under test loads, not in this fence's own excerpt,
			// so it cannot be pinned to one literal number here. Everything else in
			// docs' line (the "42703: column c.nmae does not exist (at ...)" shape) is
			// asserted below.
			if !strings.Contains(joined, "order_summary") || !strings.Contains(joined, `42703: column c.nmae does not exist (at`) {
				t.Errorf("diagnostics missing a schema problem for order_summary's column typo (42703); got: %s", joined)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// postgres.md / mysql.md
//
// docs_test.go's parseDocBlocks only recognizes a fence whose ``` sits at column 0
// ("^```(go|sql)\\s*$"); postgres.md's Copy and MatView examples (and mysql.md's version
// declaration fragment) are nested two spaces under a bullet list item instead, so they
// need their own, separate extraction rather than reusing that regex (which this file
// does not edit). p3IndentedBlock finds the n'th such fence of the given language in
// file order.
// ---------------------------------------------------------------------------------------

var p3IndentedFenceRE = regexp.MustCompile(`^[ \t]+` + "```" + `(go|sql)\s*$`)

func p3IndentedBlock(t *testing.T, path, lang string, occurrence int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	n := 0
	for i := 0; i < len(lines); i++ {
		m := p3IndentedFenceRE.FindStringSubmatch(lines[i])
		if m == nil || m[1] != lang {
			continue
		}
		indent := lines[i][:strings.Index(lines[i], "```")]
		j := i + 1
		var body []string
		for j < len(lines) && strings.TrimSpace(lines[j]) != "```" {
			body = append(body, strings.TrimPrefix(lines[j], indent))
			j++
		}
		n++
		if n == occurrence {
			return strings.Join(body, "\n")
		}
		i = j
	}
	t.Fatalf("%s: no indented %s block #%d (found %d)", path, lang, occurrence, n)
	return ""
}

// ---------------------------------------------------------------------------------------
// postgres.md "The Go type table": `// sqlshape: type <pg type>` binds a Go type to a
// PostgreSQL type not in the built-in table (here, a domain). The fence's own struct body
// is an ellipsis (its point is the doc-comment syntax, not a specific struct), completed
// here with a type that actually implements sql.Scanner / driver.Valuer, backed by the
// vendored pgtype-free test stub (testdata/src/github.com/... has no real pgx, so Money
// only needs to compile, not actually scan).
// ---------------------------------------------------------------------------------------

func TestDocsPostgresTypeBinding(t *testing.T) {
	block := p3IndentedBlock(t, filepath.Join("..", "..", "..", "..", "docs", "postgres.md"), "go", 1)
	_ = block // the fence is a syntax fragment (`type Money struct{ ... }`); see below

	td := t.TempDir()
	writeFile(t, td, "src/docsex_p3_pgtype_ok/ok.go", `package docsex_p3_pgtype_ok

import "github.com/kr9ly/sqlshape/v2"

// sqlshape: type money_amount
type Money struct{ Cents int64 }

func (m *Money) Scan(src any) error { return nil }

var Q = sqlshape.Query[struct{ Amount Money }, struct{ ID int64 }](`+"`SELECT amount FROM orders WHERE id = {{.ID}}`"+`)
`)
	// Elsewhere in the same package, receiving the domain with a plain Go type the table
	// does not cover (and no binding) is reported -- the "anywhere else" half of what
	// postgres.md's prose claims for a bound type.
	writeFile(t, td, "src/docsex_p3_pgtype_ng/ng.go", `package docsex_p3_pgtype_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{ Amount bool }, struct{ ID int64 }](`+"`SELECT amount FROM orders WHERE id = {{.ID}}`"+`)
`)
	copyRuntime(t, td)

	setSchema(t, absPath(t, "testdata/docsex_p3_pgtype_schema.sql"))
	p3NoDiagnostics(t, td, "docsex_p3_pgtype_ok")
	p3SomeDiagnostic(t, td, "docsex_p3_pgtype_ng")
}

// ---------------------------------------------------------------------------------------
// postgres.md "The runtime: pgx", the Copy example. Only the declaration
// (`postgres.Copy[Item](...)`) is checked, as docsex_copy_ok already does for
// checks.md's own COPY section -- the `.From` / `.FromSeq` runtime calls the fence also
// shows are not: the vendored test stub (testdata/src/github.com/kr9ly/sqlshape/postgres/v2,
// shared with every other test in this package and out of scope to edit here) declares
// only postgres.Copy and postgres.MatView, the two calls vet itself inspects, not the
// runtime methods around them.
// ---------------------------------------------------------------------------------------

func TestDocsPostgresCopyExample(t *testing.T) {
	td := t.TempDir()
	writeFile(t, td, "src/docsex_p3_pgcopy_ok/ok.go", `package docsex_p3_pgcopy_ok

import "github.com/kr9ly/sqlshape/postgres/v2"

var loadItems = postgres.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

type Item struct {
	OrderID int64
	LineNo  int16
	Sku     string
	Qty     int32
}
`)
	copyRuntime(t, td)

	setSchema(t, absPath(t, "testdata/docsex_p3_pgcopy_schema.sql"))
	p3NoDiagnostics(t, td, "docsex_p3_pgcopy_ok")
}

// ---------------------------------------------------------------------------------------
// postgres.md "The runtime: pgx", the MatView example: only the declaration
// (`postgres.MatView("order_stats")`), for the same reason as Copy above -- `Refresh` /
// `RefreshConcurrently` are not part of the vendored stub.
// ---------------------------------------------------------------------------------------

func TestDocsPostgresMatViewExample(t *testing.T) {
	td := t.TempDir()
	writeFile(t, td, "src/docsex_p3_pgmatview_ok/ok.go", `package docsex_p3_pgmatview_ok

import "github.com/kr9ly/sqlshape/postgres/v2"

var OrderStats = postgres.MatView("order_stats")
`)
	copyRuntime(t, td)

	setSchema(t, absPath(t, "testdata/docsex_p3_pgmatview_schema.sql"))
	p3NoDiagnostics(t, td, "docsex_p3_pgmatview_ok")
}

// ---------------------------------------------------------------------------------------
// mysql.md "Grouping": the ONLY_FULL_GROUP_BY example, the one concrete Rejected/Passes
// SQL fence on this page.
// ---------------------------------------------------------------------------------------

func TestDocsMySQLGroupBy(t *testing.T) {
	blocks := parseDocBlocks(t, docsMD("mysql.md"))
	block := nthBlock(t, blocks, "Grouping", "sql", 1)
	entries := p3SplitEntries(block.body)
	if len(entries) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(entries))
	}

	td := t.TempDir()
	writeFile(t, td, "src/docsex_p3_mysqlgroupby_ng/ng.go", fmt.Sprintf(`package docsex_p3_mysqlgroupby_ng

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	Email string
	N     int64
}, struct{}](%s)
`, backtick(entries[0].SQL)))
	writeFile(t, td, "src/docsex_p3_mysqlgroupby_ok/ok.go", fmt.Sprintf(`package docsex_p3_mysqlgroupby_ok

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct {
	Name string
	N    int64 `+"`col:\"count(*)\"`"+`
}, struct{}](%s)
`, backtick(entries[1].SQL)))
	copyRuntime(t, td)

	setSchema(t, absPath(t, "testdata/docsex_p3_mysqlgroupby_schema.sql"))
	assertDiagnostics(t, td, []string{"docsex_p3_mysqlgroupby_ng"}, []string{
		"MySQL error 1055",
	})
	p3NoDiagnostics(t, td, "docsex_p3_mysqlgroupby_ok")
}
