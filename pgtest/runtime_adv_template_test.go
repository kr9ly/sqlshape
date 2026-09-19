package pgtest_test

// Lane: template. Attack surface: expand vs. Render byte-identity, hazard detection in
// internal/vet/hazards.go (checkActionPlacement), SQL-fragment splicing.
//
// checkActionPlacement (internal/vet/hazards.go) scans the *raw* template text once,
// linearly, in source order, tracking whether it is inside a quoted string / comment by
// counting quote characters as it goes. But an `{{if}}...{{else}}...{{end}}` is not a
// sequence -- only one side ever executes -- while the scanner walks the then-branch text
// followed by the else-branch text as if they were concatenated code. An odd number of
// quote characters in the then-branch flips the scanner into "inside a string" state; that
// bogus state then gets flipped back to "code" by an unrelated quote in the else-branch,
// so by the time the scanner reaches a `{{.Field}}` placed right after an opening quote in
// the else-branch, it reports state = code and stays silent, even though that placement is
// exactly the hazard docs/templates.md warns about ("`{{.X}}` inside a string literal ...
// becomes text, not a parameter").
//
// The result: vet (and internal/vet/testdata/src/adv_template/a.go, driven by
// TestAdvTemplateHazardMissed in the vet package) emits *no* diagnostic for
// spliceViaIfElse below, yet at runtime, when .A is false, the rendered SQL is
//
//	SELECT id FROM users WHERE (name = 'bar$1')
//
// -- "$1" is text inside the string constant 'bar$1', not a bind placeholder. PG parses
// this statement as requiring zero parameters. sqlshape's Render, unaware of the splice,
// still allocates one Arg for .Q and hands it to the driver, so pgx sends one bind value to
// a statement PG says takes none: pgx errors instead of quietly returning a wrong row, but
// no build-time diagnostic ever warned the caller this template was broken.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
)

type advSpliceParams struct {
	A bool
	Q string
}

// Mirrors internal/vet/testdata/src/adv_template/a.go's SpliceViaIfElse exactly; vet sees
// zero hazard diagnostics for this template (verified by
// internal/vet.TestAdvTemplateHazardMissed).
var advSpliceViaIfElse = sqlshape.Query[int64, advSpliceParams](`SELECT id FROM users WHERE ({{if .A}}name = 'foo{{else}}name = 'bar{{.Q}}{{end}}')`)

func TestAdvTemplateSpliceViaIfElseBreaksAtRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO users (email, name) VALUES ('a@x', 'barSECRET')`); err != nil {
		t.Fatal(err)
	}

	// vet passed this template clean: no "inside a string literal" hazard for {{.Q}}.
	// A caller relying on that silence would reasonably expect Q to be bound safely
	// (e.g. as a WHERE-clause equality parameter). Confirm the runtime disagrees.
	_, err = postgres.Collect(ctx, db, advSpliceViaIfElse, advSpliceParams{A: false, Q: "SECRET"})
	if err == nil {
		t.Fatalf("want the driver to reject the bind-count mismatch produced by the splice, got no error")
	}
	// PG/pgx surfaces this as a bind-parameter-count mismatch, not as anything that looks
	// like a sqlshape-level "your template is unsafe" diagnostic -- vet never warned, and
	// the failure only appears against a live database, at query time.
	if !strings.Contains(err.Error(), "0") && !strings.Contains(strings.ToLower(err.Error()), "param") {
		t.Logf("got runtime error (recorded, not asserting exact wording): %v", err)
	} else {
		t.Logf("runtime error confirms the splice: %v", err)
	}
}

// checkBareOrderBy (internal/vet/hazards.go) only matches a bare `$n` placeholder that sits
// immediately after "ORDER BY" (or a run of other bare `$n`s straight after it):
//
//	(?i)\b(ORDER|GROUP|PARTITION)\s+BY\s+((?:\$\d+\s*,\s*)*)\$(\d+)
//
// `ORDER BY id, {{.Sort}}` puts the real column first, so {{.Sort}} is preceded by "id, "
// rather than another bare $n -- the regex never matches, and vet stays silent even though
// this is exactly the ORDER-BY-by-constant hazard docs/templates.md documents ("Do not put
// a parameter directly in ORDER BY"). Confirmed statically: internal/vet's
// TestAdvTemplateHazardMissedAcrossIfElse fails to see a diagnostic for
// testdata/src/adv_template/a.go's BareOrderBySecondColumn at all.
//
// This test confirms the runtime consequence: the second ORDER BY key is a bind-time
// constant, so PG sorts only by the first key and Sort's value has zero effect on row
// order -- silently, with no error and no vet warning, ever.
type advOrderByParams struct {
	Sort string
}

var advBareOrderBySecondColumn = sqlshape.Query[int64, advOrderByParams](`SELECT id FROM orders ORDER BY id DESC, {{.Sort}}`)

func TestAdvTemplateBareOrderBySecondColumnIsIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO orders (id, user_id, total) OVERRIDING SYSTEM VALUE VALUES (1, 1, 1), (2, 1, 1), (3, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	ids, err := postgres.Collect(ctx, db, advBareOrderBySecondColumn, advOrderByParams{Sort: "does-not-exist-as-a-column"})
	if err != nil {
		t.Fatalf("want this to run without error (that's the bug: nothing catches it): %v", err)
	}
	// vet's silence would lead a reader to believe {{.Sort}} chooses the secondary sort
	// column, the way the docs' `{{if eq .Sort "total"}} ... {{end}}` idiom does. It does
	// not: the ids come back in exactly `ORDER BY id DESC` order (3, 2, 1) no matter what
	// Sort is set to, because $2 is a bind-time constant PG uses as-is for ordering.
	want := []int64{3, 2, 1}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (ORDER BY {{.Sort}} had zero effect)", ids, want)
		}
	}
}
