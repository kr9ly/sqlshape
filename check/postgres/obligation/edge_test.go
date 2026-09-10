package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/obligation"
)

const edgeSchema = `
CREATE SCHEMA app;
-- sqlshape: require pinned(tenant_id)
-- sqlshape: require nope = 1 on read
-- sqlshape: require status = 'open' on update
-- sqlshape: require created_at < now() on delete
CREATE TABLE app.tickets (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, created_at timestamptz NOT NULL);
-- sqlshape: transitions level: 1 -> 2, 2 -> 3
CREATE TABLE ranks (id bigint PRIMARY KEY, level int NOT NULL);
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, amount int NOT NULL);
-- sqlshape: require EXISTS (SELECT 1 FROM app.tickets t WHERE t.id = ticket_id AND t.tenant_id = $1)
CREATE TABLE replies (id bigint PRIMARY KEY, ticket_id bigint NOT NULL REFERENCES app.tickets(id), body text);
`

// The corners: schema-qualified subjects, a declared predicate that does not parse, literal
// mismatches, non-string states, a write inside WITH, and subqueries that are not witnesses.
func TestEdges(t *testing.T) {
	s, err := analyze.Load(edgeSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var lines []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			line := d.Leaf.Table + " " + shortSpec(d.Obligation) + " " + pathName(d.Path)
			if d.Failed() {
				line += ": " + d.Message
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct{ sql, want string }{
		// a schema-qualified subject; a predicate naming a column the table lacks fails at judgment time with the parse error
		{`SELECT id FROM app.tickets WHERE tenant_id = $1`, `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)`},
		// a literal in the declaration matches that literal only
		{`UPDATE app.tickets SET status = 'closed' WHERE id = $1 AND tenant_id = $2 AND status = 'open'`, `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)
app.tickets status = 'open' statement`},
		{`UPDATE app.tickets SET status = 'closed' WHERE id = $1 AND tenant_id = $2 AND status = 'pending'`, `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)
app.tickets status = 'open' FAIL: tickets requires status = 'open' here: add that predicate for tickets, or opt out with ` + "`-- sqlshape: unfiltered tickets`"},
		// an opaque conjunct matches by text
		{`DELETE FROM app.tickets WHERE id = $1 AND tenant_id = $2 AND created_at < now()`, `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)
app.tickets created_at < now() statement`},
		// integer states
		{`UPDATE ranks SET level = 2 WHERE id = $1 AND level = 1`, "ranks transitions level statement"},
		{`UPDATE ranks SET level = 3 WHERE id = $1 AND level = 1`,
			"ranks transitions level FAIL: ranks.level: 1 -> 3 is not a declared transition (3 comes from 2)"},
		// a write inside WITH is a write
		{`WITH d AS (DELETE FROM ledger WHERE id = $1 RETURNING amount) SELECT amount FROM d`,
			"ledger never FAIL: ledger is declared `require never on update, delete`: no statement may do this to it"},
		// subqueries that are not witnesses: NOT IN, a row comparison, a non-equality ANY
		{`SELECT body FROM replies WHERE ticket_id NOT IN (SELECT id FROM app.tickets WHERE tenant_id = $1)`, `
replies exists FAIL: rows of replies are visible where EXISTS (SELECT 1 FROM app.tickets t WHERE t.id = ticket_id AND t.tenant_id = $1): add that predicate for replies, or opt out with ` + "`-- sqlshape: unfiltered replies`" + `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)`},
		{`SELECT body FROM replies WHERE ticket_id < ANY (SELECT id FROM app.tickets WHERE tenant_id = $1)`, `
replies exists FAIL: rows of replies are visible where EXISTS (SELECT 1 FROM app.tickets t WHERE t.id = ticket_id AND t.tenant_id = $1): add that predicate for replies, or opt out with ` + "`-- sqlshape: unfiltered replies`" + `
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)`},
		// ... and one that is: a literal on the outer side of IN
		{`SELECT body FROM replies r WHERE r.ticket_id IN (SELECT id FROM app.tickets WHERE tenant_id = $1 AND id = r.ticket_id)`, `
replies exists statement
app.tickets pinned(tenant_id) statement
app.tickets nope = 1 FAIL: app.tickets: ` + "`nope = 1`" + ` does not parse against app.tickets: 42703: column "nope" does not exist (at 1)`},
	}
	for _, c := range cases {
		if got, want := run(c.sql), strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("%s\n--- got ---\n%s\n--- want ---\n%s", c.sql, got, want)
		}
	}
}

func TestParseKindsErrors(t *testing.T) {
	for _, in := range []string{"require x = 1 on select,", "require x = 1 on ", "require x = 1 on select, sometimes"} {
		if _, ok, err := obligation.Parse("t", in); !ok || err == nil {
			t.Errorf("%q: ok=%v err=%v", in, ok, err)
		}
	}
	o, _, err := obligation.Parse("t", "require x = 1 on write")
	if err != nil || o.Kinds != obligation.OnWrite {
		t.Errorf("on write: %+v %v", o, err)
	}
}
