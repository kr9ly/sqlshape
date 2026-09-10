package analyze

// Tests that internal/facts.Facts faithfully mirrors the statement's structure --
// docs/obligations.md's contract that obligation-checking rests on.

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

type advLowerer struct{ s *schema.Schema }

func (l advLowerer) Lower(expr string, rel obligation.Relation) ([]facts.Pred, error) {
	return Lower(l.s, expr, l.s.ByFullName(rel.FullName()))
}

func advPathName(p obligation.Path) string {
	switch p {
	case obligation.ByStatement:
		return "statement"
	case obligation.ByView:
		return "view"
	case obligation.ByForeignKey:
		return "fk"
	case obligation.ByPolicy:
		return "policy"
	case obligation.Waived:
		return "waived"
	}
	return "FAIL"
}

func advCheckLines(t *testing.T, s *schema.Schema, decls []obligation.Obligation, f *facts.Facts) string {
	t.Helper()
	var lines []string
	for _, d := range obligation.Check(s.Contract(), decls, f, advLowerer{s}) {
		line := d.Obligation.Source + " " + advPathName(d.Path)
		if d.Message != "" {
			line += " " + d.Message
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

const advFactsSchema = `
CREATE TABLE tenants (id bigint PRIMARY KEY);
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL REFERENCES tenants(id),
  status text NOT NULL,
  version int NOT NULL DEFAULT 0,
  UNIQUE (id, tenant_id)
);
CREATE TABLE staging_orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL,
  status text NOT NULL
);
CREATE TABLE order_items (
  id bigint PRIMARY KEY,
  order_id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  FOREIGN KEY (order_id, tenant_id) REFERENCES orders(id, tenant_id)
);
`

// A MERGE with a WHEN MATCHED and a WHEN NOT MATCHED branch produces one Write per
// branch, each with its own Kind (Update / Insert) and its own Assigned/Values -- per
// internal/facts/facts.go's Write.Kind doc comment ("each WHEN branch also appears as
// its own Write with the branch's kind, so an obligation `on update` sees the UPDATE
// branch"). This lets `on update` / `on insert` obligations see exactly the branch that
// applies, and keeps one branch's assignment to a column from shadowing another
// branch's assignment to the same column name.
func TestAdvMergeWriteIsPerBranch(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	sql := `MERGE INTO orders o USING staging_orders s ON o.id = s.id
WHEN MATCHED THEN UPDATE SET status = 'matched'
WHEN NOT MATCHED THEN INSERT (id, tenant_id, status) VALUES (s.id, s.tenant_id, s.status)`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(r.Facts.Writes) != 2 {
		t.Fatalf("expected one Write per WHEN branch (2), got %d:\n%s", len(r.Facts.Writes), r.Facts.String())
	}
	var sawUpdate, sawInsert bool
	for _, w := range r.Facts.Writes {
		if w.Kind == facts.Update {
			sawUpdate = true
		}
		if w.Kind == facts.Insert {
			sawInsert = true
			var hasStatusCol bool
			for _, c := range w.Assigned {
				if c == "status" {
					hasStatusCol = true
				}
			}
			if !hasStatusCol {
				t.Errorf("INSERT branch's status=s.status is missing from its own Write")
			}
		}
	}
	if !sawUpdate || !sawInsert {
		t.Fatalf("expected a Write with Kind=Update and a Write with Kind=Insert, got:\n%s", r.Facts.String())
	}
}

// `require never on insert` rejects only a MERGE branch that actually inserts: a MERGE
// with just a WHEN MATCHED THEN UPDATE clause (no WHEN NOT MATCHED at all) never
// inserts, so it does not discharge -- and does not fail -- `never on insert`.
func TestAdvMergeNeverOnInsertOnlyFlagsInsertBranch(t *testing.T) {
	schemaSQL := `
CREATE TABLE tenants (id bigint PRIMARY KEY);
-- sqlshape: require never on insert
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL REFERENCES tenants(id),
  status text NOT NULL,
  version int NOT NULL DEFAULT 0
);
CREATE TABLE staging_orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL,
  status text NOT NULL
);
`
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	// no WHEN NOT MATCHED clause at all: this statement can never insert into orders.
	sql := `MERGE INTO orders o USING staging_orders s ON o.id = s.id
WHEN MATCHED THEN UPDATE SET status = s.status`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	got := advCheckLines(t, s, decls, r.Facts)
	if got != "" {
		t.Fatalf("expected no discharge (this MERGE has no WHEN NOT MATCHED / INSERT branch, so it never inserts), got:\n%s", got)
	}
}

// A data-modifying CTE's Write is reported separately from the outer statement's, WITH
// item first, with its own Kind: docs/facts.go's "Writes are the tables the statement
// stores into ... WITH items first, the statement last" holds.
func TestAdvCTEWriteNotHole(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	sql := `WITH removed AS (
  DELETE FROM order_items WHERE order_id = $1 RETURNING id
)
INSERT INTO orders (id, tenant_id, status) SELECT id, $2, 'archived' FROM removed`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(r.Facts.Writes) != 2 {
		t.Fatalf("expected 2 Writes (the CTE's delete, then the statement's insert), got %d:\n%s", len(r.Facts.Writes), r.Facts.String())
	}
	if r.Facts.Writes[0].Kind != facts.Delete || r.Facts.Writes[0].Table != "order_items" {
		t.Errorf("expected Writes[0] = delete order_items, got %s %s", r.Facts.Writes[0].Kind, r.Facts.Writes[0].Table)
	}
	if r.Facts.Writes[1].Kind != facts.Insert || r.Facts.Writes[1].Table != "orders" {
		t.Errorf("expected Writes[1] = insert orders, got %s %s", r.Facts.Writes[1].Kind, r.Facts.Writes[1].Table)
	}
}

// TestAdvCorrelatedDepth2NotHole locks down a correlated subquery nested two levels
// deep: the innermost EXISTS's predicate refers to the *grandparent* scope's column
// (o.tenant_id, not the immediate parent i's), which the analyzer cannot place as an
// Outer term (Term.Outer's contract is one level: "Col indexes the parent's leaves").
// Falling back to an opaque Known term is the documented safe behavior ("追えないもの
// は証明できないとして扱う"), not a hole: the outer scope gains nothing false from it.
func TestAdvCorrelatedDepth2NotHole(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	sql := `SELECT o.id FROM orders o WHERE EXISTS (
  SELECT 1 FROM order_items i WHERE i.order_id = o.id AND EXISTS (
    SELECT 1 FROM staging_orders g WHERE g.id = i.id AND g.tenant_id = o.tenant_id
  )
)`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	// the outer (orders) scope must not pick up any Fixed/NotNull from the doubly-nested
	// correlation -- it can only see one level down.
	if len(r.Facts.Top.Fixed) != 0 || len(r.Facts.Top.NotNull) != 0 {
		t.Errorf("expected the outer scope to gain nothing from the depth-2 correlation, got Fixed=%v NotNull=%v", r.Facts.Top.Fixed, r.Facts.Top.NotNull)
	}
	t.Logf("facts:\n%s", r.Facts.String())
}

// A MERGE whose ON clause pins the target's primary key by equality to a parameter
// proves AtMostOne, exactly as the equivalent UPDATE's WHERE would -- so `require
// single on update` (or `on write`) is satisfiable for a MERGE that targets one row,
// not just for the same statement spelled as an UPDATE.
func TestAdvMergeAtMostOneIsProved(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	sql := `MERGE INTO orders o USING staging_orders s ON o.id = $1 AND s.id = $1
WHEN MATCHED THEN UPDATE SET status = s.status`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if !r.Facts.AtMostOne {
		t.Fatalf("expected AtMostOne (ON pins orders.id by equality to a parameter, same as an equivalent UPDATE's WHERE), got false:\n%s", r.Facts.String())
	}
}

// TestAdvUsesAssignedNotHole locks down three Uses.Assigned edge cases that were
// suspected holes but turned out correct: a SET's self-reference on the right
// (version = version + 1) is a read, not write-only; an ON CONFLICT (col) target column
// is a read (it names which unique index detects the conflict) even though the same
// column is also an INSERT target; and a RETURNING column is a read (it is exposed to
// the caller), never Assigned.
func TestAdvUsesAssignedNotHole(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql          string
		table, col   string
		wantAssigned bool
	}{
		{`UPDATE orders SET version = version + 1 WHERE id = $1`, "orders", "version", false},
		{`INSERT INTO orders (id, tenant_id, status) VALUES ($1, $2, 'new') ON CONFLICT (id) DO UPDATE SET status = 'dup'`, "orders", "id", false},
		{`INSERT INTO orders (id, tenant_id, status) VALUES ($1, $2, 'new') ON CONFLICT (id) DO UPDATE SET status = 'dup'`, "orders", "tenant_id", true},
		{`DELETE FROM orders WHERE id = $1 RETURNING tenant_id`, "orders", "tenant_id", false},
	}
	for _, c := range cases {
		r, aerr := Analyze(s, c.sql)
		if aerr != nil {
			t.Fatalf("%s: %v", c.sql, aerr)
		}
		var found bool
		for _, u := range r.Facts.Uses {
			if u.Table == c.table && u.Column == c.col {
				found = true
				if u.Assigned != c.wantAssigned {
					t.Errorf("%s: Use{%s.%s}.Assigned = %v, want %v", c.sql, c.table, c.col, u.Assigned, c.wantAssigned)
				}
			}
		}
		if !found {
			t.Errorf("%s: expected a Use for %s.%s", c.sql, c.table, c.col)
		}
	}
}

// facts.Use.Position is documented as "the 0-based byte offset of the reference in the
// statement text" (internal/facts/facts.go). A column named only in GROUP BY still gets
// its real byte offset, not a sentinel -- Position feeds x/obligation/check.go's
// sensitive() diagnostics, so a `context X: may read <label>` violation on such a column
// must be reported at the position where it actually appears.
func TestAdvGroupByColumnPositionIsCorrect(t *testing.T) {
	s, err := Load(advFactsSchema)
	if err != nil {
		t.Fatal(err)
	}
	sql := `SELECT count(*) FROM orders GROUP BY tenant_id`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	var found bool
	for _, u := range r.Facts.Uses {
		if u.Table == "orders" && u.Column == "tenant_id" {
			found = true
			if u.Position < 0 {
				t.Errorf("%s: Use{orders.tenant_id}.Position = %d, want the real byte offset (>= 0)", sql, u.Position)
			}
		}
	}
	if !found {
		t.Fatalf("%s: expected a Use for orders.tenant_id, got %+v", sql, r.Facts.Uses)
	}
}

// TestAdvViewOfViewAliasNotHole locks down that a view-of-view read through an alias
// keeps the outer alias on the top leaf, leaves the nested view leaves unaliased (they
// have none in their own defining SQL), and still inherits both views' predicates.
func TestAdvViewOfViewAliasNotHole(t *testing.T) {
	schemaSQL := `
CREATE TABLE tenants (id bigint PRIMARY KEY);
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tenant_id bigint NOT NULL REFERENCES tenants(id),
  status text NOT NULL,
  deleted_at timestamptz
);
CREATE VIEW live_orders AS SELECT id, tenant_id, status FROM orders WHERE deleted_at IS NULL;
CREATE VIEW live_paid AS SELECT id, tenant_id FROM live_orders WHERE status = 'paid';
`
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	sql := `SELECT lp.id FROM live_paid lp WHERE lp.tenant_id = $1`
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatal(aerr)
	}
	want := strings.TrimPrefix(`
select
  leaf 0 view live_paid as lp @18
      leaf 0 view live_orders @-1
          leaf 0 table orders @-1
          pred 0.deleted_at IS NULL
      pred 0.status = const spaid
      fixed 0.status
      notnull 0.status
  pred 0.tenant_id = $1
  fixed 0.tenant_id
  notnull 0.tenant_id
`, "\n")
	if got := r.Facts.String(); got != want {
		t.Errorf("--- got ---\n%s--- want ---\n%s", got, want)
	}
}
