package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// TestAdvNeverOnConflictUpdate: an INSERT ... ON CONFLICT DO UPDATE against a table
// declared `require never on update` performs an update on an existing row, which is
// exactly what `never` promises cannot happen -- but does the checker see it?
func TestAdvNeverOnConflictUpdate(t *testing.T) {
	schemaSQL := `
CREATE TABLE ledger (
  id bigint PRIMARY KEY,
  amount int NOT NULL
);
-- sqlshape: require never on update
CREATE TABLE ledger2 (
  id bigint PRIMARY KEY,
  amount int NOT NULL
);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `INSERT INTO ledger2 (id, amount) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET amount = $2`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var found bool
	var lines []string
	for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
		lines = append(lines, d.Obligation.Source+" "+pathName(d.Path))
		if d.Obligation.Body.Never {
			found = true
		}
	}
	if !found {
		t.Errorf("expected: the `never on update` obligation on ledger2 to be judged (and to FAIL) for `INSERT ... ON CONFLICT DO UPDATE`\nactual: no judgment at all -- lines:\n%s", strings.Join(lines, "\n"))
	}
}

// TestAdvTransitionsOnConflictUpdate: same gap, for `transitions` -- ON CONFLICT DO
// UPDATE setting the state column should still owe the compare-and-set check.
func TestAdvTransitionsOnConflictUpdate(t *testing.T) {
	schemaSQL := `
-- sqlshape: transitions status: draft -> submitted, submitted -> paid
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  status text NOT NULL
);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	// draft -> paid directly is not a declared transition; if the ON CONFLICT DO UPDATE
	// branch is judged at all, this must FAIL.
	sql := `INSERT INTO orders (id, status) VALUES ($1, 'paid') ON CONFLICT (id) DO UPDATE SET status = 'paid'`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var found bool
	var lines []string
	for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
		lines = append(lines, d.Obligation.Source+" "+pathName(d.Path)+" "+d.Message)
		if d.Obligation.Body.Transitions != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("expected: the `transitions status` obligation on orders to be judged for the ON CONFLICT DO UPDATE branch, and to FAIL (draft -> paid is not declared)\nactual: no judgment at all -- lines:\n%s", strings.Join(lines, "\n"))
	}
}

// A MERGE that only inserts (WHEN NOT MATCHED THEN INSERT, no WHEN MATCHED at all)
// against a table declared `require never on update, delete` (append-only, INSERT
// allowed) passes: it never updates or deletes anything, so `never on update, delete`
// is judged against the branches this MERGE actually has, not against every kind a
// MERGE can in general perform.
func TestAdvMergeInsertOnlyPassesNeverOnUpdate(t *testing.T) {
	schemaSQL := `
CREATE TABLE src (id bigint PRIMARY KEY, amount int NOT NULL);
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, amount int NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	sql := `MERGE INTO ledger l USING src s ON l.id = s.id WHEN NOT MATCHED THEN INSERT (id, amount) VALUES (s.id, s.amount)`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var lines []string
	var fail bool
	for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
		lines = append(lines, d.Obligation.Source+" "+pathName(d.Path)+" "+d.Message)
		if d.Obligation.Body.Never && d.Failed() {
			fail = true
		}
	}
	if fail {
		t.Errorf("expected PASS -- the MERGE has no WHEN MATCHED branch, so it never updates or deletes ledger -- got:\n%s", strings.Join(lines, "\n"))
	}
}

// When a MERGE's INSERT branch and UPDATE branch each assign the same column name
// (status) to a different literal, each branch's Write keeps its own assignment: the
// transitions check judges the WHEN MATCHED branch's `status = 'paid'` against its own
// branch, not against a value pooled in from the WHEN NOT MATCHED branch.
func TestAdvMergeAssignedStaysWithItsBranch(t *testing.T) {
	schemaSQL := `
CREATE TABLE src (id bigint PRIMARY KEY, status text NOT NULL);
-- sqlshape: transitions status: draft -> submitted, submitted -> paid
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	// WHEN NOT MATCHED inserts a fresh 'draft' order; WHEN MATCHED illegally jumps the
	// existing row straight to 'paid' with no CAS precondition at all (should FAIL: no
	// current-state predicate, and draft -> paid is not even a declared transition).
	sql := `MERGE INTO orders o USING src s ON o.id = s.id
WHEN NOT MATCHED THEN INSERT (id, status) VALUES (s.id, 'draft')
WHEN MATCHED THEN UPDATE SET status = 'paid'`
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var lines []string
	var sawPaidFail bool
	for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
		line := d.Obligation.Source + " " + pathName(d.Path) + " " + d.Message
		lines = append(lines, line)
		if strings.Contains(d.Message, "paid") {
			sawPaidFail = true
		}
	}
	if !sawPaidFail {
		t.Errorf("expected a FAIL message about the WHEN MATCHED branch's `status = 'paid'` (no CAS precondition, not a declared transition from an unfixed state); got:\n%s", strings.Join(lines, "\n"))
	}
}

// sensSchema: sensitive pii on users.email, plus a view / matview / function that
// might carry (or drop) the label.
const sensSchema = `
-- sqlshape: sensitive pii: email, phone
CREATE TABLE users (id bigint PRIMARY KEY, name text NOT NULL, email text NOT NULL, phone text NOT NULL);
CREATE VIEW user_contacts AS SELECT id, email AS contact, phone FROM users;
CREATE MATERIALIZED VIEW user_contacts_mv AS SELECT id, email, phone FROM users;
CREATE FUNCTION users_of(gid bigint) RETURNS SETOF users LANGUAGE sql AS $$ SELECT * FROM users $$;
`

func sensLines(t *testing.T, s *schema.Schema, decls []obligation.Obligation, sql string) (string, bool) {
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var lines []string
	var anyFail bool
	for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
		if d.Obligation.Body.Sensitive == nil {
			continue
		}
		line := pathName(d.Path)
		if d.Failed() {
			line += " " + d.Message
			anyFail = true
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), anyFail
}

func TestAdvSensitive(t *testing.T) {
	s, err := analyze.Load(sensSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	cases := []struct {
		name     string
		sql      string
		wantLeak bool // true: expected to FAIL (email/phone is read) -- if it does not, that's a hole
	}{
		{"select-star", `SELECT * FROM users WHERE id = $1`, true},
		{"table-star", `SELECT u.* FROM users u WHERE u.id = $1`, true},
		{"whole-row", `SELECT u FROM users u WHERE u.id = $1`, true},
		{"row-to-json", `SELECT row_to_json(u) FROM users u WHERE u.id = $1`, true},
		{"to-jsonb", `SELECT to_jsonb(u) FROM users u WHERE u.id = $1`, true},
		{"json-build-object", `SELECT json_build_object('e', email) FROM users WHERE id = $1`, true},
		{"where-only", `SELECT id FROM users WHERE email = $1`, true},
		{"is-not-null", `SELECT id FROM users WHERE email IS NOT NULL`, true},
		{"count", `SELECT count(email) FROM users`, true},
		{"order-by", `SELECT id FROM users ORDER BY email`, true},
		{"group-by", `SELECT email, count(*) FROM users GROUP BY email`, true},
		{"view-passthrough", `SELECT contact FROM user_contacts WHERE id = $1`, true},
		{"matview", `SELECT email FROM user_contacts_mv WHERE id = $1`, true},
		{"func-returns-setof", `SELECT email FROM users_of($1)`, true},
	}
	for _, c := range cases {
		got, fail := sensLines(t, s, decls, c.sql)
		if fail != c.wantLeak {
			t.Errorf("%s (%s): wantLeak=%v gotFail=%v\nlines:\n%s", c.name, c.sql, c.wantLeak, fail, got)
		}
	}
}

// TestAdvTransitionsShapes: forms of SET that should be refused (non-literal target),
// and one that should be accepted as usual, checking the actual message against the
// declared contract in docs/checks.md ("The target must be a literal naming a declared
// state; `SET status = {{.S}}` is refused").
func TestAdvTransitionsShapes(t *testing.T) {
	schemaSQL := `
CREATE TABLE other (id bigint PRIMARY KEY, next text NOT NULL);
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL, other_id bigint REFERENCES other(id));
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			return "ANALYZE ERROR: " + err.Error()
		}
		var lines []string
		for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
			if d.Obligation.Body.Transitions == nil {
				continue
			}
			line := pathName(d.Path)
			if d.Failed() {
				line += " " + d.Message
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct{ name, sql string }{
		{"case-expr", `UPDATE orders SET status = CASE WHEN other_id IS NULL THEN 'submitted' ELSE 'paid' END WHERE id = $1 AND status = 'draft'`},
		{"set-default", `UPDATE orders SET status = DEFAULT WHERE id = $1`},
		{"cast", `UPDATE orders SET status = 'paid'::text WHERE id = $1 AND status = 'submitted'`},
		{"from-other", `UPDATE orders o SET status = t.next FROM other t WHERE o.other_id = t.id AND o.id = $1`},
		{"self-assign", `UPDATE orders SET status = status WHERE id = $1`},
		{"tuple-set", `UPDATE orders SET (status, other_id) = ('paid', $2) WHERE id = $1 AND status = 'submitted'`},
	}
	for _, c := range cases {
		t.Logf("%s (%s):\n%s", c.name, c.sql, run(c.sql))
	}
}

// TestAdvSingleAndPairedAndNeverShapes: a batch of the remaining shapes from the lane
// brief for single / paired / never, logged for manual inspection against the contract.
func TestAdvSingleAndPairedAndNeverShapes(t *testing.T) {
	schemaSQL := `
CREATE TABLE outbox (id bigint PRIMARY KEY, payload text NOT NULL);
-- sqlshape: require paired(outbox) on insert
-- sqlshape: require single on delete
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  tag text NOT NULL,
  region_id bigint NOT NULL,
  seq int NOT NULL,
  UNIQUE (tag),
  UNIQUE (region_id, seq)
);
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, amount int NOT NULL);
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	run := func(sql string) string {
		r, err := analyze.Analyze(s, sql)
		if err != nil {
			return "ANALYZE ERROR: " + err.Error()
		}
		var lines []string
		for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
			if d.Obligation.Body.Single && d.Obligation.Subject != "orders" && d.Obligation.Subject != "app.orders" {
				continue
			}
			line := d.Obligation.Source + " " + pathName(d.Path)
			if d.Failed() {
				line += " " + d.Message
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cases := []struct{ name, sql string }{
		{"single-by-unique-tag", `DELETE FROM orders WHERE tag = $1`},
		{"single-by-composite-partial", `DELETE FROM orders WHERE region_id = $1`},
		{"single-by-composite-full", `DELETE FROM orders WHERE region_id = $1 AND seq = $2`},
		{"single-with-returning", `DELETE FROM orders WHERE tag = $1 RETURNING id`},
		{"single-using-join", `DELETE FROM orders o USING ledger l WHERE l.id = o.id AND o.tag = $1`},
		{"single-using-join-no-key", `DELETE FROM orders o USING ledger l WHERE l.id = o.region_id`},
		{"paired-with-with", `WITH o AS (INSERT INTO orders (id, tag, region_id, seq) VALUES ($1, $2, $3, $4) RETURNING id) INSERT INTO outbox (id, payload) SELECT id, 'created' FROM o`},
		{"paired-unrelated-values", `WITH o AS (INSERT INTO orders (id, tag, region_id, seq) VALUES ($1, $2, $3, $4) RETURNING id) INSERT INTO outbox (id, payload) VALUES (999, 'unrelated')`},
		{"paired-outbox-only", `INSERT INTO outbox (id, payload) VALUES ($1, $2)`},
		{"never-select-for-update", `SELECT amount FROM ledger WHERE id = $1 FOR UPDATE`},
	}
	for _, c := range cases {
		t.Logf("%s (%s):\n%s", c.name, c.sql, run(c.sql))
	}
}

// Function bodies (a trigger's included) and view bodies are judged against the
// obligations when the schema is loaded by vet and by `sqlshape check`: the entry points
// run analyze.AnalyzeFunction / AnalyzeView and Check over each statement's facts. This
// is that path over a trigger function updating an append-only table and a SQL function
// reading past a visibility predicate.
func TestAdvFunctionBodiesAreJudged(t *testing.T) {
	schemaSQL := `
-- sqlshape: require never on update, delete
CREATE TABLE ledger (id bigint PRIMARY KEY, amount int NOT NULL);
CREATE TABLE orders (id bigint PRIMARY KEY);
CREATE FUNCTION bump_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE ledger SET amount = amount + 1 WHERE id = NEW.id;
  RETURN NEW;
END;
$$;
CREATE TRIGGER orders_bump AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION bump_ledger();
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (id bigint PRIMARY KEY, body text NOT NULL, deleted_at timestamptz);
CREATE FUNCTION all_memos() RETURNS SETOF memos LANGUAGE sql AS $$ SELECT * FROM memos $$;
`
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	var failed []string
	for _, fn := range s.Functions {
		fr, err := analyze.AnalyzeFunction(s, fn)
		if err != nil {
			t.Fatalf("%s: %v", fn.Name, err)
		}
		for _, st := range fr.Statements {
			for _, d := range obligation.Check(s.Contract(), decls, st.Facts, lowerer{s}) {
				if d.Failed() {
					failed = append(failed, fn.Name+": "+d.Obligation.Source)
				}
			}
		}
	}
	want := "bump_ledger: require never on update, delete\nall_memos: visible where deleted_at IS NULL"
	if got := strings.Join(failed, "\n"); got != want {
		t.Errorf("--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
