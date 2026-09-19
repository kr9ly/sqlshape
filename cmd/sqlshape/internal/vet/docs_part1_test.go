package vet

// docs_part1_test.go covers the rest of docs/checks.md's ("Part 1 — Checks on every
// statement") Result-columns and Passing-parameters groups that docs_test.go's own
// harness does not already exercise: see that file's package doc for the general
// approach (parseDocBlocks/nthBlock/docLine read the .md files at test time; a docs edit
// changes what this test compiles and expects without a second copy to keep in sync).
// This file reuses every helper docs_test.go defines (same package) and defines none of
// its own with the same name; identifiers introduced here that are not test functions
// carry a p1 prefix to stay clear of docs_part2_test.go / docs_part3_test.go.
//
// Sections covered (docs/checks.md heading, in file order):
//   - Unnamed and duplicate columns need an alias
//   - A column only some branches select needs a field that can hold NULL
//   - Column and field types follow the table below
//   - Nested rows are received by structs
//   - A single-column statement can be received by a scalar
//   - Embedded structs are flattened
//   - The Go type table                              (excluded: prose only, no fences)
//   - A parameter's type follows where it is used
//   - A parameter that may be NULL is a pointer (-strict)
//   - Nested paths and `range`
//   - Composite parameters are structs
//   - Do not leave unused fields in `P` (-strict)
//   - Do not always send a value into a column with a DEFAULT (-strict)
//
// The "Nested rows" Passes example and both -strict examples that touch an enum column
// each get one diagnostic beyond the one docs' prose leads with; docs itself now says so
// (the array-element NULL note has no way to be proven absent yet; the enum-column
// advisory is a separate, correct finding about the column rather than the parameter),
// so each expect list below names the extra diagnostic explicitly rather than treating it
// as an unexplained mismatch.

import (
	"strings"
	"testing"
)

// p1SetStrict turns -strict on for the calling test and restores it on cleanup, the way
// vet_test.go's TestStrict does; docs_test.go's own tests never need -strict, so it adds
// no such helper.
func p1SetStrict(t *testing.T) {
	t.Helper()
	if err := Analyzer.Flags.Set("strict", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Analyzer.Flags.Set("strict", "false") })
}

// p1DropBracket cuts s at the first " [" (a branch-location marker such as
// "[if@11:else]"): the byte offset docs' own comment bakes in is the offset within its
// own copy of the template text, which need not match this harness's -- it is not part
// of what the doc is actually promising (the wording around it is), so a caller matches
// only the text on one side of it.
func p1DropBracket(s string) string {
	if i := strings.Index(s, " ["); i >= 0 {
		return s[:i]
	}
	return s
}

// p1Paragraphs splits a fenced sql block's body on blank lines: docs sometimes shows two
// independent statements (each with its own trailing "-- ^ ..." comment) in one fence,
// separated by a blank line, rather than as two fences.
func p1Paragraphs(body string) []string {
	var out []string
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, "\n"))
			buf = nil
		}
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		buf = append(buf, l)
	}
	flush()
	return out
}

// ---------------------------------------------------------------------------------------
// "Unnamed and duplicate columns need an alias"
// ---------------------------------------------------------------------------------------

func TestDocsP1Alias(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Unnamed and duplicate columns need an alias"},
		{"ja", "checks.ja.md", "名前の無い列と同名の列には別名を付ける"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 1)
			ok := nthBlock(t, blocks, tc.head, "sql", 2)

			ngStmts := p1Paragraphs(ng.body)
			if len(ngStmts) != 2 {
				t.Fatalf("expected 2 Rejected statements, got %d:\n%s", len(ngStmts), ng.body)
			}
			okLines := strings.Split(strings.TrimSpace(ok.body), "\n")
			if len(okLines) != 2 {
				t.Fatalf("expected 2 Passes statements, got %d:\n%s", len(okLines), ok.body)
			}

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_alias_ng_group/ng.go", `package docsex_p1_alias_ng_group

import "github.com/kr9ly/sqlshape/v2"

type OrderCount struct {
	ID int64
}

var Q1 = sqlshape.Query[OrderCount, struct{}](`+backtick(oneLineSQL(ngStmts[0]))+`)

type OrderCustomer struct {
	ID int64
}

var Q2 = sqlshape.Query[OrderCustomer, struct{}](`+backtick(oneLineSQL(ngStmts[1]))+`)
`)
			writeFile(t, td, "src/docsex_p1_alias_ok_group/ok.go", `package docsex_p1_alias_ok_group

import "github.com/kr9ly/sqlshape/v2"

type OrderCount struct {
	ID int64
	N  int64
}

var Q1 = sqlshape.Query[OrderCount, struct{}](`+backtick(okLines[0])+`)

type OrderCustomer struct {
	ID         int64
	CustomerID int64
}

var Q2 = sqlshape.Query[OrderCustomer, struct{}](`+backtick(okLines[1])+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_colbind_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_alias_ng_group"}, []string{
				docLine(t, ngStmts[0], "has no name"),
				docLine(t, ngStmts[1], "both named"),
			})
			assertDiagnostics(t, td, []string{"docsex_p1_alias_ok_group"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A column only some branches select needs a field that can hold NULL"
// ---------------------------------------------------------------------------------------

func TestDocsP1BranchNullable(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A column only some branches select needs a field that can hold NULL"},
		{"ja", "checks.ja.md", "一部の分岐だけが選ぶ列はNULLを受けられる型で受ける"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)
			okGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := dropInlineComment(ngGo.body, "is not selected in every branch")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_branch_ng/ng.go", `package docsex_p1_branch_ng

import "github.com/kr9ly/sqlshape/v2"

`+plainNG+`

type Params struct {
	WithTotal bool
}

var Q = sqlshape.Query[Order, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_branch_ok/ok.go", `package docsex_p1_branch_ok

import "github.com/kr9ly/sqlshape/v2"

`+okGo.body+`

type Params struct {
	WithTotal bool
}

var Q = sqlshape.Query[Order, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			// the shared schema.sql's orders.total (numeric(12,2) NOT NULL) already
			// matches what this example needs.
			setSchema(t, absPath(t, "testdata/schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_branch_ng"}, []string{
				// dropping the "[if@N:else]" branch-location suffix: see p1DropBracket.
				p1DropBracket(docLine(t, ngGo.body, "is not selected in every branch")),
			})
			assertDiagnostics(t, td, []string{"docsex_p1_branch_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Column and field types follow the table below"
// ---------------------------------------------------------------------------------------

// A numeric(12,2) column received into a float64 field is accepted with a precision-loss
// note, not rejected: float64 is a lossy but permitted receiver for numeric, not an
// incompatible one (see the type table in docs/postgres.md).
func TestDocsP1ColumnTypeTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Column and field types follow the table below"},
		{"ja", "checks.ja.md", "列の型とフィールドの型は下の表に従う"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			lossyGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)
			silentGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainLossy := dropInlineComment(lossyGo.body, "loses precision")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_coltype_lossy/lossy.go", `package docsex_p1_coltype_lossy

import "github.com/kr9ly/sqlshape/v2"

`+plainLossy+`

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_coltype_silent/silent.go", `package docsex_p1_coltype_silent

import "github.com/kr9ly/sqlshape/v2"

`+silentGo.body+`

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_coltype_lossy"}, []string{
				docLine(t, lossyGo.body, "loses precision"),
			})
			assertDiagnostics(t, td, []string{"docsex_p1_coltype_silent"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Nested rows are received by structs"
//
// Docs now name both of the diagnostics the checker actually raises here, so the expect
// lists below are the documented behaviour, not an unexplained mismatch:
//
//  1. Swapping two adjacent fields (docs' Rejected Item{Qty, Sku}) puts *both* fields at
//     the wrong position, not one: checkNested (nested.go) compares struct field i
//     against row column i for every i, so both "Qty is at position 1, row column 1 is
//     sku" and "Sku is at position 2, row column 2 is qty" fire.
//  2. docs' Passes example receives a composite array_agg column with a plain
//     (non-pointer) struct slice, []Item, and carries no "may contain a NULL element"
//     note: array_agg's own argument here, (i.sku, i.qty)::order_item, is a row
//     constructor, never NULL itself regardless of its fields' own nullability, and
//     dialect.Type.ElemNotNull now lets the checker say so (unlike a plain scalar
//     array_agg, which still carries the note -- see docs/postgres.md's `T[]` row).
// ---------------------------------------------------------------------------------------

func TestDocsP1NestedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Nested rows are received by structs"},
		{"ja", "checks.ja.md", "ネストした行は構造体で受ける"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 2) // sql#1 is the "-- schema.sql" CREATE TYPE excerpt
			okGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := dropInlineComment(ngGo.body, "is at position 1")

			// docs' okGo only redeclares Item (Sku before Qty); Order (with []Item)
			// carries over from the Rejected example.
			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_nested_ng/ng.go", `package docsex_p1_nested_ng

import "github.com/kr9ly/sqlshape/v2"

`+plainNG+`

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_nested_ok/ok.go", `package docsex_p1_nested_ok

import "github.com/kr9ly/sqlshape/v2"

`+okGo.body+`

type Order struct {
	ID    int64
	Items []Item
}

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_nested_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_nested_ng"}, []string{
				docLine(t, ngGo.body, "is at position 1"),
				// real behaviour, not docs' own words: the same swap also misplaces
				// Sku (see the mismatch note above).
				`field Items.Sku is at position 2 but the row type's column 2 is "qty" (fields are scanned in order)`,
			})
			assertDiagnostics(t, td, []string{"docsex_p1_nested_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A single-column statement can be received by a scalar"
// ---------------------------------------------------------------------------------------

func TestDocsP1ScalarResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A single-column statement can be received by a scalar"},
		{"ja", "checks.ja.md", "1列だけ返すSQLはスカラーで受けられる"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ok := nthBlock(t, blocks, tc.head, "go", 1)
			ng := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := stripTrailingLineComment(ng.body, "R is int64")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_scalar_ok/ok.go", `package docsex_p1_scalar_ok

import "github.com/kr9ly/sqlshape/v2"

`+ok.body+`
`)
			writeFile(t, td, "src/docsex_p1_scalar_ng/ng.go", `package docsex_p1_scalar_ng

import "github.com/kr9ly/sqlshape/v2"

`+plainNG+`
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_scalar_ok"}, nil)
			assertDiagnostics(t, td, []string{"docsex_p1_scalar_ng"}, []string{
				docLine(t, ng.body, "R is int64"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Embedded structs are flattened"
// ---------------------------------------------------------------------------------------

func TestDocsP1EmbeddedFlatten(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Embedded structs are flattened"},
		{"ja", "checks.ja.md", "埋め込み構造体は平坦化される"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			okGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)
			ngGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := dropInlineComment(ngGo.body, "both bind to column")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_embed_ok/ok.go", `package docsex_p1_embed_ok

import "time"
import "github.com/kr9ly/sqlshape/v2"

`+okGo.body+`

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_embed_ng/ng.go", `package docsex_p1_embed_ng

import "time"
import "github.com/kr9ly/sqlshape/v2"

type Base struct {
	ID        int64
	CreatedAt time.Time
}

`+plainNG+`

var Q = sqlshape.Query[Order, struct{}](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_embed_ok"}, nil)
			assertDiagnostics(t, td, []string{"docsex_p1_embed_ng"}, []string{
				docLine(t, ngGo.body, "both bind to column"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "The Go type table" -- prose only (a cross-reference to postgres.md / mysql.md's own
// tables), no fenced go/sql example of its own to run.
// ---------------------------------------------------------------------------------------

func TestDocsP1GoTypeTableExcluded(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "The Go type table"},
		{"ja", "checks.ja.md", "Go型の表"},
	} {
		blocks := parseDocBlocks(t, docsMD(tc.doc))
		n := 0
		for _, b := range blocks {
			if b.heading == tc.head {
				n++
			}
		}
		if n != 0 {
			t.Errorf("%s: %q now has %d fenced block(s); this test's exclusion is stale, wire it up", tc.name, tc.head, n)
		}
		t.Logf("excluded %s %q: prose only, cross-references postgres.md/mysql.md's own Go type tables, no runnable fence", tc.name, tc.head)
	}
}

// ---------------------------------------------------------------------------------------
// "A parameter's type follows where it is used"
// ---------------------------------------------------------------------------------------

func TestDocsP1ParamTypeUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A parameter's type follows where it is used"},
		{"ja", "checks.ja.md", "パラメータの型は使われる場所の型に合わせる"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)
			okGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := dropInlineComment(ngGo.body, "SQL expects uuid")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_paramtype_ng/ng.go", `package docsex_p1_paramtype_ng

import (
	"github.com/google/uuid"
	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID    uuid.UUID
	Email string
}

`+plainNG+`

var Q = sqlshape.Query[User, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_paramtype_ok/ok.go", `package docsex_p1_paramtype_ok

import (
	"github.com/google/uuid"
	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID    uuid.UUID
	Email string
}

`+okGo.body+`

var Q = sqlshape.Query[User, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_paramtype_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_paramtype_ng"}, []string{
				docLine(t, ngGo.body, "SQL expects uuid"),
			})
			assertDiagnostics(t, td, []string{"docsex_p1_paramtype_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A parameter that may be NULL is a pointer" (-strict)
//
// Mismatch found: this schema's status column is an enum, and -strict's schema.Advice()
// (check/postgres/dialect/schema.go) always reports "<table>.<col> is enum <name>: a
// seeded lookup table ... is easier to change" for every enum column in every table,
// once per package under -strict, regardless of whether any query touches it. Docs'
// Passes example is therefore not actually diagnostic-free under -strict either. Filed
// as a docs suggestion; both expect lists below name the advisory explicitly.
// ---------------------------------------------------------------------------------------

func TestDocsP1ParamNullPointer(t *testing.T) {
	p1SetStrict(t)
	enumAdvice := "orders.status is enum order_status: a seeded lookup table"

	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "A parameter that may be NULL is a pointer"},
		{"ja", "checks.ja.md", "NULLを渡しうるパラメータはポインタにする"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			okGo := nthBlock(t, blocks, tc.head, "go", 2)

			plainNG := dropInlineComment(ngGo.body, "zero value")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_paramnull_ng/ng.go", `package docsex_p1_paramnull_ng

import "github.com/kr9ly/sqlshape/v2"

type OrderStatus string

`+plainNG+`

var Q = sqlshape.Query[int64, Params](`+backtick("SELECT count(*) FROM orders WHERE status = {{.Status}}")+`)
`)
			writeFile(t, td, "src/docsex_p1_paramnull_ok/ok.go", `package docsex_p1_paramnull_ok

import "github.com/kr9ly/sqlshape/v2"

type OrderStatus string

`+okGo.body+`

var Q = sqlshape.Query[int64, Params](`+backtick("SELECT count(*) FROM orders WHERE status = {{.Status}}")+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_paramnull_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_paramnull_ng"}, []string{
				docLine(t, ngGo.body, "zero value"),
				enumAdvice,
			})
			assertDiagnostics(t, td, []string{"docsex_p1_paramnull_ok"}, []string{
				enumAdvice,
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Nested paths and `range`"
// ---------------------------------------------------------------------------------------

func TestDocsP1RangePaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Nested paths and `range`"},
		{"ja", "checks.ja.md", "ネストしたパスと`range`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			okGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_rangepath_ok/ok.go", `package docsex_p1_rangepath_ok

import "github.com/kr9ly/sqlshape/v2"

`+okGo.body+`

var Q = sqlshape.Query[struct{ ID int64 }, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_rangepath_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_rangepath_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Composite parameters are structs"
// ---------------------------------------------------------------------------------------

func TestDocsP1CompositeParams(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Composite parameters are structs"},
		{"ja", "checks.ja.md", "複合型のパラメータは構造体で渡す"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			okGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 2) // sql#1 is the "-- schema.sql" excerpt

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_compositeparam_ok/ok.go", `package docsex_p1_compositeparam_ok

import "github.com/kr9ly/sqlshape/v2"

`+okGo.body+`

var Q = sqlshape.Query[int64, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_compositeparam_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_compositeparam_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not leave unused fields in `P` (-strict)"
// ---------------------------------------------------------------------------------------

func TestDocsP1UnusedParamField(t *testing.T) {
	p1SetStrict(t)
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not leave unused fields in `P` (`-strict`)"},
		{"ja", "checks.ja.md", "使っていないフィールドを`P`に残さない（`-strict`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			sql := nthBlock(t, blocks, tc.head, "sql", 1)

			plainNG := dropInlineComment(ngGo.body, "never used by the template")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_unusedfield_ng/ng.go", `package docsex_p1_unusedfield_ng

import "github.com/kr9ly/sqlshape/v2"

`+plainNG+`

var Q = sqlshape.Query[struct{ ID int64 }, Params](`+backtick(oneLineSQL(sql.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_unusedfield_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_unusedfield_ng"}, []string{
				docLine(t, ngGo.body, "never used by the template"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not always send a value into a column with a DEFAULT (-strict)"
//
// Mismatch found: NewOrder.Status here is the same OrderStatus/order_status pair as "A
// parameter that may be NULL is a pointer", so both that section's non-pointer-enum
// note and this schema's unconditional enum-lookup-table advice (see that test's
// comment) accompany the DEFAULT diagnostic docs' comment names -- three diagnostics,
// not one, for the Rejected example; the Passes example gets the enum advice alone, not
// zero. Filed as a docs suggestion; expect lists name both extra diagnostics explicitly.
// ---------------------------------------------------------------------------------------

func TestDocsP1ParamDefault(t *testing.T) {
	p1SetStrict(t)
	enumAdvice := "orders.status is enum order_status: a seeded lookup table"

	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not always send a value into a column with a DEFAULT (`-strict`)"},
		{"ja", "checks.ja.md", "既定値のある列に常に値を送らない（`-strict`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)
			ngSQL := nthBlock(t, blocks, tc.head, "sql", 1)
			okSQL := nthBlock(t, blocks, tc.head, "sql", 2)

			plainNG := dropInlineComment(ngGo.body, "always sends a value")
			nonPointerEnumNote := "parameter .Status is a non-pointer OrderStatus: its zero value \"\" is not a label of enum order_status and fails at runtime (SQLSTATE 22P02) when unset"

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p1_paramdefault_ng/ng.go", `package docsex_p1_paramdefault_ng

import "github.com/kr9ly/sqlshape/v2"

type OrderStatus string

`+plainNG+`

var Q = sqlshape.Query[struct{}, NewOrder](`+backtick(oneLineSQL(ngSQL.body))+`)
`)
			writeFile(t, td, "src/docsex_p1_paramdefault_ok/ok.go", `package docsex_p1_paramdefault_ok

import "github.com/kr9ly/sqlshape/v2"

type OrderStatus string

type NewOrder struct {
	CustomerID int64
	Status     *OrderStatus
}

var Q = sqlshape.Query[struct{}, NewOrder](`+backtick(oneLineSQL(okSQL.body))+`)
`)
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p1_paramdefault_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p1_paramdefault_ng"}, []string{
				docLine(t, ngGo.body, "always sends a value"),
				nonPointerEnumNote,
				enumAdvice,
			})

			// Docs' Passes SQL guards the same {{if .Status}} test twice, once in the
			// column list and once in the VALUES list; branch-state exploration now
			// follows an if/with/range on an already-decided plain path within the
			// same lineage, so the two occurrences are treated as one decision, not
			// as four independently-variable combinations, and no spurious
			// syntax-error diagnostic is produced.
			assertDiagnostics(t, td, []string{"docsex_p1_paramdefault_ok"}, []string{
				enumAdvice,
			})
		})
	}
}
