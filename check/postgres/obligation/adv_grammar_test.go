package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// A directive written above a statement that takes none (ALTER TABLE, COMMENT ON) is
// reported as a schema Problem -- neither silently dropped nor silently attached to
// the wrong relation.
func TestAdvDirectiveAboveOtherStatementIsAProblem(t *testing.T) {
	for _, stmt := range []string{
		"ALTER TABLE orders ADD COLUMN note text;",
		"COMMENT ON TABLE orders IS 'the orders table';",
		"CREATE INDEX orders_tenant ON orders (tenant_id);",
	} {
		s, err := analyze.Load("CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);\n-- sqlshape: require pinned(tenant_id) on select\n" + stmt)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Problems) != 1 || !strings.Contains(s.Problems[0].Message, `directive "require pinned(tenant_id) on select" is written above a statement that takes no directives`) {
			t.Errorf("%s: problems = %+v", stmt, s.Problems)
		}
		if decls, _ := obligation.Declarations(s.Contract()); len(decls) != 0 {
			t.Errorf("%s: the directive attached to something: %+v", stmt, decls)
		}
	}
}

// A `context <name>: <item>; <item>` predicate item containing a literal ";" inside a
// string literal (a status list, an escape marker) is not cut in half: either it parses
// as one whole predicate, or it is reported as a Problem -- never silently split wrong.
func TestAdvContextSemicolonInsideStringLiteralHandledSafely(t *testing.T) {
	schemaSQL := `
-- sqlshape: context ops: require note <> 'a;b' on select
CREATE TABLE orders (id bigint PRIMARY KEY, note text NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	t.Logf("decls=%+v problems=%+v", decls, problems)
	if len(problems) == 0 {
		for _, o := range decls {
			if o.Context == "ops" && o.Body.Predicate == "note <> 'a;b'" {
				return // parsed correctly, as one predicate
			}
		}
		t.Errorf("context item containing `;` inside a string literal was not parsed as one predicate (no Problem either): decls=%+v", decls)
	}
	// else: it failed loudly (a Problem), which is an acceptable fallback.
}

// A child spelled with a quoted, mixed-case schema (`"App".orders`) in an `aggregate`
// child list that cannot be resolved is reported as a Problem, not silently dropped.
func TestAdvAggregateQuotedSchemaChildFailsAsProblem(t *testing.T) {
	schemaSQL := `
CREATE SCHEMA "App";
-- sqlshape: aggregate orders ("App".order_items)
CREATE TABLE "App".orders (id bigint PRIMARY KEY);
CREATE TABLE "App".order_items (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES "App".orders(id));
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %+v", s.Problems)
	}
	_, problems := obligation.Declarations(s.Contract())
	if len(problems) == 0 {
		t.Errorf("aggregate with quoted-schema child %q raised no Problem", `"App".order_items`)
	} else {
		t.Logf("aggregate quoted-schema child: got expected Problem(s): %+v", problems)
	}
}

// TestAdvPinnedQuotedColumnNameFailsAsProblemNotSilently: pinned()/immutable() extract the
// text between parens verbatim, quote characters included. Declaring `pinned("Tenant Id")`
// for an actual column named (quoted) `Tenant Id` does not match hasColumn (which compares
// against the stored, unquoted name) and becomes a Problem -- confirming the mistake is
// caught loudly, per the contract, even though the message doesn't explain *why* (quoting).
func TestAdvPinnedQuotedColumnNameFailsAsProblemNotSilently(t *testing.T) {
	schemaSQL := `
-- sqlshape: require pinned("Tenant Id")
CREATE TABLE orders (id bigint PRIMARY KEY, "Tenant Id" bigint NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, problems := obligation.Declarations(s.Contract())
	if len(problems) == 0 {
		t.Errorf("expected a Problem for a quoted column name inside pinned(), got none (would mean it's silently accepted or silently broken)")
	} else {
		t.Logf("as expected, a Problem: %+v", problems[0].Message)
	}
}

// TestAdvRequireKeywordsCaseInsensitive pins the case-insensitivity of the `REQUIRE` /
// `PINNED` / `VIA VIEW` / `NEVER` keywords (and mixed case) as a passing contract, so a
// future change that breaks it is caught.
func TestAdvRequireKeywordsCaseInsensitive(t *testing.T) {
	cases := []string{
		"REQUIRE PINNED(tenant_id)",
		"Require pinned(Tenant_Id)",
		"require VIA VIEW",
		"REQUIRE NEVER on update, delete",
	}
	for _, in := range cases {
		if in == "Require pinned(Tenant_Id)" {
			// column-name case is a separate axis; skip here, exercised as its own test.
			continue
		}
		o, ok, err := obligation.Parse("t", in)
		if !ok || err != nil {
			t.Errorf("%q: ok=%v err=%v", in, ok, err)
			continue
		}
		if o.Body.Spec() == "" {
			t.Errorf("%q: parsed to an empty body: %+v", in, o)
		}
	}
}

// TestAdvPinnedColumnCaseMismatchBecomesProblem: `pinned(TENANT_ID)` in the directive vs.
// an actual unquoted (hence lower-cased by Postgres) column `tenant_id` is a column-name
// case mismatch. hasColumn compares byte-for-byte, so this should become a Problem -- not
// a silently-always-failing (or worse, silently no-op) obligation.
func TestAdvPinnedColumnCaseMismatchBecomesProblem(t *testing.T) {
	schemaSQL := `
-- sqlshape: require pinned(TENANT_ID)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) == 0 {
		t.Errorf("`pinned(TENANT_ID)` against column `tenant_id` produced no Problem; decls=%+v", decls)
	} else {
		t.Logf("as expected, a Problem: %+v", problems[0].Message)
	}
}

// TestAdvWaiveSpecMismatchIsSilentNoOpNotAHole: a `waive` whose spelling does not match
// the declared body's normalized Spec() exactly (case difference in a predicate) simply
// fails to remove anything from the base set -- the base obligation stays in force. This
// is fail-safe (no false OK), just silent (no diagnostic that the waiver itself typo'd).
// Recorded as "not a hole", per the brief.
func TestAdvWaiveSpecMismatchIsSilentNoOpNotAHole(t *testing.T) {
	schemaSQL := `
-- sqlshape: require status = 'Open' on select
-- sqlshape: context ops: waive status = 'open'
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	inOps := obligation.InContext(all, "ops")
	found := false
	for _, o := range inOps {
		if o.Body.Predicate == "status = 'Open'" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the base obligation `status = 'Open'` to survive under context ops (the waiver's case-mismatched spelling should not remove it); got %+v", inOps)
	}
}

// TestAdvExistsPredicateWithJoinOnAndTrailingKindsParsesCorrectly: a declared EXISTS
// predicate that itself contains a JOIN ... ON inside its parens, followed by a top-level
// `on <kinds>` suffix. lastTopLevel's paren-depth tracking should skip the nested ON and
// find only the trailing one.
func TestAdvExistsPredicateWithJoinOnAndTrailingKindsParsesCorrectly(t *testing.T) {
	body := "EXISTS (SELECT 1 FROM orders o JOIN tenants t ON t.id = o.tenant_id WHERE o.id = shipment_id) on select"
	o, ok, err := obligation.Parse("shipments", "require "+body)
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := "EXISTS (SELECT 1 FROM orders o JOIN tenants t ON t.id = o.tenant_id WHERE o.id = shipment_id)"
	if o.Body.Predicate != want {
		t.Errorf("predicate:\n got  %q\n want %q", o.Body.Predicate, want)
	}
	if o.Kinds != obligation.OnSelect {
		t.Errorf("kinds: got %v want OnSelect", o.Kinds)
	}
}

// TestAdvPredicateContainingLiteralOnWordParsesCorrectly: a predicate whose string
// literal is itself the word "on" (`x = 'on'`), followed by a real `on <kinds>` suffix.
func TestAdvPredicateContainingLiteralOnWordParsesCorrectly(t *testing.T) {
	o, ok, err := obligation.Parse("t", "require x = 'on' on update")
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if o.Body.Predicate != "x = 'on'" {
		t.Errorf("predicate: got %q want %q", o.Body.Predicate, "x = 'on'")
	}
	if o.Kinds != obligation.OnUpdate {
		t.Errorf("kinds: got %v want OnUpdate", o.Kinds)
	}
}

// TestAdvUnquotedSemicolonAtTopLevelOfSinglePredicateStillOneItem is a control for the
// context-semicolon hypothesis: a single `require` item (no `;`) is unaffected.
func TestAdvSingleContextItemNoSemicolonUnaffected(t *testing.T) {
	schemaSQL := `
-- sqlshape: context ops: require note <> 'nope' on select
CREATE TABLE orders (id bigint PRIMARY KEY, note text NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	all, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	found := false
	for _, o := range all {
		if o.Context == "ops" && o.Body.Predicate == "note <> 'nope'" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the single-item context predicate to parse cleanly; got %+v", all)
	}
}
