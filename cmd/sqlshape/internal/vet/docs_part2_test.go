package vet

// docs_part2_test.go covers the milestone's own slice of docs/checks.md (and its ja
// translation): "Giving types a meaning" through "Returning one row" minus the sections
// docs_test.go already owns ("Fix a unique key by equality", "Bulk loading with COPY",
// "Name the errors a trigger raises"). It reuses every helper docs_test.go already
// defines (parseDocBlocks, nthBlock, docLine, assertDiagnostics, setSchema, docsMD,
// writeFile, copyRuntime, absPath, backtick, oneLineSQL, dropInlineComment,
// stripTrailingLineComment) and adds only what is missing here, under a p2 prefix.
//
// Exclusions (sections in this milestone's scope with nothing to compile):
//   - "Constraint names": prose plus two cross-doc links, no go/sql fence at all.
//   - "PL/pgSQL statements that fail on their own (PostgreSQL)": a bullet list describing
//     SQLSTATEs (P0002/P0003/20000/P0004) and how RAISE ... USING ERRCODE resolves
//     statically; no fence.
//   - Under "Declare the constraints a write can violate", the third fence
//     (`_, err := postgres.First(ctx, db, CreateCustomer, p); if postgres.Violates(...) {
//     ... }`) is a runtime call-site illustration: ctx/db/p/CreateCustomer are all
//     undefined and the snippet asserts nothing a diagnostic could check. The two sql
//     fences either side of it (the bare INSERT and the same INSERT with its expect line)
//     are exercised on their own below.
//
// A few fences in this milestone's sections are prose demonstrations rather than
// complete, standalone programs; each such case is called out at its own Test function
// with what was supplied to make it compile and why that supply does not change the
// diagnostic wording being checked.
import (
	"fmt"
	"strings"
	"testing"
)

// setStrictP2 turns on the -strict flag for the duration of the subtest, mirroring the
// Set/defer-Set("false") shape vet_test.go's TestStrict already uses (Analyzer.Flags is
// package-level state, not scoped to a single subtest on its own).
func setStrictP2(t *testing.T) {
	t.Helper()
	if err := Analyzer.Flags.Set("strict", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Analyzer.Flags.Set("strict", "false") })
}

// p2Rename replaces the first occurrence of from with to in body. It exists for the two
// fences in this milestone (see TestDocsP2MeaningFromUse and TestDocsP2NotAnotherTablesID)
// that write `var User = sqlshape.One[User, ...]`: naming the var the same as the type
// parameter it instantiates is fine in isolation (the doc never declares `type User` in
// the same fence, so there is no real clash there), but this harness must supply that
// missing `type User` itself to let the snippet compile at all, and doing so turns the
// doc's own var name into a genuine redeclaration. Renaming only the var side leaves the
// diagnostic wording (which never mentions the var, only the type UserID and the columns
// it meets) untouched.
func p2Rename(body, from, to string) string {
	return strings.Replace(body, from, to, 1)
}

// ---------------------------------------------------------------------------------------
// "Meaning comes from use, not from a registry"
// ---------------------------------------------------------------------------------------

// TestDocsP2MeaningFromUse runs the section's own two-statement illustration (UserID binds
// to users.id on the first meeting, then is checked -- and found wanting -- on the
// second). The doc never declares `type User`; that struct belongs to an earlier,
// unrelated section of the same page (a plain users(id, email) shape matches the SQL
// here), so this harness supplies it, and renames the doc's `var User` to `var ByID` to
// avoid redeclaring the type it just added (see p2Rename).
func TestDocsP2MeaningFromUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Meaning comes from use, not from a registry"},
		{"ja", "checks.ja.md", "意味は登録ではなく使用箇所から決まる"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			typeDecl := nthBlock(t, blocks, tc.head, "go", 1) // type UserID int64
			twoStmts := nthBlock(t, blocks, tc.head, "go", 2) // User query, then Total query

			body := stripTrailingLineComment(twoStmts.body, "elsewhere, but here meets")
			body = p2Rename(body, "var User =", "var ByID =")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_meaning/meaning.go", fmt.Sprintf(`package docsex_p2_meaning

import "github.com/kr9ly/sqlshape/v2"

%s

type User struct {
	ID    UserID
	Email string
}

%s
`, typeDecl.body, body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_meaning_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_meaning"}, []string{
				docLine(t, twoStmts.body, "elsewhere, but here meets"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Enum and lookup values agree with the named type's constants"
// ---------------------------------------------------------------------------------------

func TestDocsP2EnumConstants(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Enum and lookup values agree with the named type's constants"},
		{"ja", "checks.ja.md", "enumやlookupテーブルの値はnamed typeの定数と一致させる"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			bind := nthBlock(t, blocks, tc.head, "go", 1) // var ByStatus = sqlshape.Query[Order, struct{ Status OrderStatus }](...)
			ng := nthBlock(t, blocks, tc.head, "go", 2)   // type OrderStatus string; const (... Canceled ...)
			ok := nthBlock(t, blocks, tc.head, "go", 3)   // const (... Shipped ...)

			ngConsts := dropInlineComment(ng.body, "is not a label of value set")
			ngConsts = stripTrailingLineComment(ngConsts, "has no constant for it")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_enum_ng/ng.go", fmt.Sprintf(`package docsex_p2_enum_ng

import "github.com/kr9ly/sqlshape/v2"

type Order struct {
	ID    int64
	Total string
}

%s

%s
`, ngConsts, bind.body))
			writeFile(t, td, "src/docsex_p2_enum_ok/ok.go", fmt.Sprintf(`package docsex_p2_enum_ok

import "github.com/kr9ly/sqlshape/v2"

type OrderStatus string

type Order struct {
	ID    int64
	Total string
}

%s

%s
`, ok.body, bind.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_enum_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_enum_ng"}, []string{
				docLine(t, ng.body, "is not a label of value set"),
				docLine(t, ng.body, "has no constant for it"),
			})
			assertDiagnostics(t, td, []string{"docsex_p2_enum_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not pass another table's ID"
// ---------------------------------------------------------------------------------------

// TestDocsP2NotAnotherTablesID runs the Rejected/Passes Params pair, primed by the
// section's own opening illustration (UserID bound to users.id). As in
// TestDocsP2MeaningFromUse, that opening fence writes `var User = sqlshape.One[User,
// ...]` without ever declaring `type User` in the fence itself; this harness supplies the
// type and renames the var (p2Rename) so both meanings of "User" do not collide.
func TestDocsP2NotAnotherTablesID(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not pass another table's ID"},
		{"ja", "checks.ja.md", "別のテーブルのIDを渡さない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			prime := nthBlock(t, blocks, tc.head, "go", 1)  // type UserID/OrderID int64; var User = ...
			ngGo := nthBlock(t, blocks, tc.head, "go", 2)   // type Params struct{ ID UserID // ... }
			ngSQL := nthBlock(t, blocks, tc.head, "sql", 1) // SELECT total FROM orders WHERE id = {{.ID}}
			okGo := nthBlock(t, blocks, tc.head, "go", 3)   // type Params struct{ ID OrderID }

			primeBody := p2Rename(prime.body, "var User =", "var ByID =")
			ngParams := dropInlineComment(ngGo.body, "elsewhere, but here meets")

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_tableid_ng/ng.go", fmt.Sprintf(`package docsex_p2_tableid_ng

import "github.com/kr9ly/sqlshape/v2"

%s

type User struct {
	ID    UserID
	Email string
}

%s

var Q = sqlshape.Query[struct{ Total string }, Params](%s)
`, primeBody, ngParams, backtick(oneLineSQL(ngSQL.body))))
			writeFile(t, td, "src/docsex_p2_tableid_ok/ok.go", fmt.Sprintf(`package docsex_p2_tableid_ok

import "github.com/kr9ly/sqlshape/v2"

%s

type User struct {
	ID    UserID
	Email string
}

%s

var Q = sqlshape.Query[struct{ Total string }, Params](%s)
`, primeBody, okGo.body, backtick(oneLineSQL(ngSQL.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_tableid_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_tableid_ng"}, []string{
				docLine(t, ngGo.body, "elsewhere, but here meets"),
			})
			assertDiagnostics(t, td, []string{"docsex_p2_tableid_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not mix domains of different units (PostgreSQL)"
// ---------------------------------------------------------------------------------------

// TestDocsP2DomainMismatch splits the section's two independent illustrations: a plain
// SQL expression mixing two domains (no parameter involved), and a -strict advisory about
// a parameter carrying a domain through a plain int64 versus a named type. A parameter
// compared against a domain column is resolved by PostgreSQL to the domain, so both the
// plain-SQL and the parameter advisory fire, as docs claim.
func TestDocsP2DomainMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not mix domains of different units (PostgreSQL)"},
		{"ja", "checks.ja.md", "単位の違うドメインを混ぜない（PostgreSQL）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngSQL := nthBlock(t, blocks, tc.head, "sql", 2) // SELECT id FROM products WHERE price + weight > 1000
			ngGo := nthBlock(t, blocks, tc.head, "go", 1)   // Params{ Max int64 // ... }
			okGo := nthBlock(t, blocks, tc.head, "go", 2)   // type Yen int64; Params{ Max Yen }
			okSQL := nthBlock(t, blocks, tc.head, "sql", 3) // SELECT id FROM products WHERE price > {{.Max}}

			td := t.TempDir()
			// R is a named ProductID, not a bare int64: products.id is itself a key
			// column, and under -strict a bare int64 receiving it would add its own
			// "carries key products.id as a plain int64" advisory alongside the domain
			// one this test is actually about.
			writeFile(t, td, "src/docsex_p2_domainsql_ng/ng.go", fmt.Sprintf(`package docsex_p2_domainsql_ng

import "github.com/kr9ly/sqlshape/v2"

type ProductID int64

var Q = sqlshape.Query[struct{ ID ProductID }, struct{}](%s)
`, backtick(oneLineSQL(ngSQL.body))))
			copyRuntime(t, td)

			ngParams := dropInlineComment(ngGo.body, "declare a named type to have it checked")

			writeFile(t, td, "src/docsex_p2_domainparam_ng/ng.go", fmt.Sprintf(`package docsex_p2_domainparam_ng

import "github.com/kr9ly/sqlshape/v2"

type ProductID int64

%s

var Q = sqlshape.Query[struct{ ID ProductID }, Params](%s)
`, ngParams, backtick(oneLineSQL(okSQL.body))))
			writeFile(t, td, "src/docsex_p2_domainparam_ok/ok.go", fmt.Sprintf(`package docsex_p2_domainparam_ok

import "github.com/kr9ly/sqlshape/v2"

type ProductID int64

%s

var Q = sqlshape.Query[struct{ ID ProductID }, Params](%s)
`, okGo.body, backtick(oneLineSQL(okSQL.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_domain_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_domainsql_ng"}, []string{
				"domain mismatch: yen + gram: operands must share the domain (cast to the base type to drop it)",
			})

			setStrictP2(t)
			assertDiagnostics(t, td, []string{"docsex_p2_domainparam_ng"}, []string{
				"parameter .Max carries domain yen as a plain int64; declare a named type to have it checked",
			})
			assertDiagnostics(t, td, []string{"docsex_p2_domainparam_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not receive with a type that loses information (`-strict`)"
// ---------------------------------------------------------------------------------------

// TestDocsP2LossyReceive runs the section's one struct verbatim; the doc shows no SELECT
// for it (the point is made on the fields alone), so this harness supplies a query whose
// result columns match At/Day exactly ("SELECT at, day FROM events" against an
// events(at timestamp, day date) table, both NOT NULL so neither field's other advisory,
// "may be NULL", can appear and crowd out the ones the doc claims).
func TestDocsP2LossyReceive(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not receive with a type that loses information (`-strict`)"},
		{"ja", "checks.ja.md", "情報が落ちる型で受けない（`-strict`）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			event := nthBlock(t, blocks, tc.head, "go", 1)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_event/event.go", fmt.Sprintf(`package docsex_p2_event

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

%s

var Q = sqlshape.Query[Event, struct{}](`+"`SELECT at, day FROM events`"+`)
`, event.body))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_event_schema.sql"))
			setStrictP2(t)
			assertDiagnostics(t, td, []string{"docsex_p2_event"}, []string{
				docLine(t, event.body, "which zone the value is in"),
				docLine(t, event.body, "a zone conversion can move the day"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Declare the constraints a write can violate"
// ---------------------------------------------------------------------------------------

func TestDocsP2DeclareViolations(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Declare the constraints a write can violate"},
		{"ja", "checks.ja.md", "違反しうる制約はexpect行に宣言する"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ngSQL := nthBlock(t, blocks, tc.head, "sql", 1) // INSERT ... RETURNING id, no expect line
			okSQL := nthBlock(t, blocks, tc.head, "sql", 2) // same, with expect customers_email_key
			// The third fence under this heading (postgres.First/Violates call-site
			// illustration) is a runtime usage snippet, not a diagnostic to check --
			// see the file-level exclusion note.

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_writefail_ng/ng.go", fmt.Sprintf(`package docsex_p2_writefail_ng

import "github.com/kr9ly/sqlshape/v2"

var CreateCustomer = sqlshape.Query[int64, struct {
	Email string
	Name  string
}](%s)
`, backtick(oneLineSQL(ngSQL.body))))
			writeFile(t, td, "src/docsex_p2_writefail_ok/ok.go", fmt.Sprintf(`package docsex_p2_writefail_ok

import "github.com/kr9ly/sqlshape/v2"

var CreateCustomer = sqlshape.Query[int64, struct {
	Email string
	Name  string
}](%s)
`, backtick(oneLineSQL(okSQL.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_writefail_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_writefail_ng"}, []string{
				docLine(t, ngSQL.body, "may violate customers_email_key"),
			})
			assertDiagnostics(t, td, []string{"docsex_p2_writefail_ok"}, nil)
		})
	}
}

// ---------------------------------------------------------------------------------------
// "Do not declare a violation that cannot happen"
// ---------------------------------------------------------------------------------------

func TestDocsP2OverdeclaredExpect(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Do not declare a violation that cannot happen"},
		{"ja", "checks.ja.md", "起こりえない違反を宣言しない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "sql", 1)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_overdeclare/ng.go", fmt.Sprintf(`package docsex_p2_overdeclare

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[struct{}, struct {
	Note string
	ID   int64
}](%s)
`, backtick(oneLineSQL(ng.body))))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_overdeclare_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_overdeclare"}, []string{
				docLine(t, ng.body, "expects orders_total_check but no expansion can violate it"),
			})
		})
	}
}

// ---------------------------------------------------------------------------------------
// "A statement that calls a function declares the function's failure modes too"
// ---------------------------------------------------------------------------------------

// TestDocsP2CallDeclaresFailureModes runs both dialects' Rejected call. Neither shows a
// schema for the PostgreSQL half (it assumes the customers/orders pair from elsewhere on
// the page); the MySQL half's schema fence declares place_order() itself but likewise
// assumes customers/orders already exist. Both are supplied in docsex_p2_call_pg_schema.sql
// / docsex_p2_call_mysql_schema.sql (the latter carries the doc's function fence
// verbatim). This section has no ja run: it is PostgreSQL/MySQL-specific prose that
// checks.ja.md mirrors under the same heading translated, so the same two dialect cases
// cover it; running the loop would just duplicate assertions against the same fences
// under a second heading string.
func TestDocsP2CallDeclaresFailureModes(t *testing.T) {
	blocks := parseDocBlocks(t, docsMD("checks.md"))
	head := "A statement that calls a function declares the function's failure modes too"
	pgCall := nthBlock(t, blocks, head, "sql", 1)    // SELECT place_order({{.CustomerID}}, {{.Note}})
	mysqlCall := nthBlock(t, blocks, head, "sql", 3) // SELECT place_order({{.CustomerID}}, {{.Total}})

	td := t.TempDir()
	writeFile(t, td, "src/docsex_p2_call_pg/pg.go", fmt.Sprintf(`package docsex_p2_call_pg

import "github.com/kr9ly/sqlshape/v2"

var Q = sqlshape.Query[int64, struct {
	CustomerID int64
	Note       string
}](%s)
`, backtick(oneLineSQL(pgCall.body))))
	// mysqlCall's caret comment (unlike every other doc example in this milestone) spans
	// two "--" lines ("^ may violate ..." then a continuation "add `-- sqlshape: expect
	// ...` ..."); oneLineSQL only recognizes a caret line by "^" on that same line, so it
	// keeps the continuation as a real SQL comment -- and that continuation's own
	// backtick-quoted directive breaks out of the Go raw string this harness wraps it in.
	// The statement itself is a single line, so this harness takes just that line instead.
	mysqlCallSQL := strings.SplitN(mysqlCall.body, "\n", 2)[0]
	writeFile(t, td, "src/docsex_p2_call_mysql/mysql.go", fmt.Sprintf(`package docsex_p2_call_mysql

import "github.com/kr9ly/sqlshape/v2"

// declared so the "no var declares P0401/30001 anywhere this program can reach" finding
// ("Name the errors a trigger raises") does not crowd out the one this test is about.
var OrderTooLarge = sqlshape.Error("30001")

var Q = sqlshape.Query[int64, struct {
	CustomerID uint64
	Total      string
}](%s)
`, backtick(mysqlCallSQL)))
	copyRuntime(t, td)

	t.Run("pg", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_p2_call_pg_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_p2_call_pg"}, []string{
			docLine(t, pgCall.body, "may violate orders_customer_id_fkey"),
		})
	})
	t.Run("mysql", func(t *testing.T) {
		setSchema(t, absPath(t, "testdata/docsex_p2_call_mysql_schema.sql"))
		assertDiagnostics(t, td, []string{"docsex_p2_call_mysql"}, []string{
			docLine(t, mysqlCall.body, "may violate 30001"),
		})
	})
}

// ---------------------------------------------------------------------------------------
// "Every branch must be provable"
// ---------------------------------------------------------------------------------------

func TestDocsP2EveryBranchProvable(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		head string
	}{
		{"en", "checks.md", "Every branch must be provable"},
		{"ja", "checks.ja.md", "すべての分岐で証明できなければならない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseDocBlocks(t, docsMD(tc.doc))
			ng := nthBlock(t, blocks, tc.head, "go", 1)

			td := t.TempDir()
			writeFile(t, td, "src/docsex_p2_branch/branch.go", fmt.Sprintf(`package docsex_p2_branch

import "github.com/kr9ly/sqlshape/v2"

type User struct {
	ID    int64
	Email string
}

%s
`, stripTrailingLineComment(ng.body, "One: cannot prove")))
			copyRuntime(t, td)

			setSchema(t, absPath(t, "testdata/docsex_p2_branch_schema.sql"))
			assertDiagnostics(t, td, []string{"docsex_p2_branch"}, []string{
				docLine(t, ng.body, "One: cannot prove"),
			})
		})
	}
}
