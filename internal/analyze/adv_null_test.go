package analyze

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/oracle"
)

// checkNullability runs sql against a real PG loaded with schemaSQL and inserts, then
// compares the analyzer's declared nullability for each result column against what the
// real data actually contains. It reports (via t.Errorf) any column the analyzer claims
// is NOT NULL (nullable=false) while the real PG returned an actual NULL for it — that
// is the high-severity hole (vet lets a non-pointer field through, runtime scans a NULL
// into it and panics / silently zeroes it).
func checkNullability(t *testing.T, schemaSQL string, inserts []string, sql string) {
	t.Helper()
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	r, aerr := Analyze(s, sql)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatalf("oracle start: %v", err)
	}
	defer o.Close()
	conn := o.Conn()
	for _, ins := range inserts {
		if _, err := conn.Exec(ctx, ins); err != nil {
			t.Fatalf("insert %q: %v", ins, err)
		}
	}
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	if len(fields) != len(r.Columns) {
		t.Fatalf("column count mismatch: analyzer %d, real %d", len(r.Columns), len(fields))
	}
	actualNull := make([]bool, len(fields))
	nrows := 0
	for rows.Next() {
		nrows++
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("row values: %v", err)
		}
		for i, v := range vals {
			if v == nil {
				actualNull[i] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if nrows == 0 {
		t.Fatalf("query returned no rows; cannot probe nullability")
	}
	for i, col := range r.Columns {
		if !col.Nullable && actualNull[i] {
			t.Errorf("column %d (%s): analyzer says NOT NULL but real PG returned NULL", i, col.Name)
		}
	}
}

func TestAdvNullTxidCurrentIfAssigned(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	checkNullability(t, string(schemaSQL), nil, "SELECT txid_current_if_assigned()")
}

func TestAdvNullJsonbArrowOperator(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	inserts := []string{
		`INSERT INTO users (id, email, name) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x.com', 'A')`,
		`INSERT INTO orders (user_id, total, meta) VALUES (1, 10, '{"other":1}'::jsonb)`,
	}
	checkNullability(t, string(schemaSQL), inserts, `
		SELECT meta ->> 'missing_key' FROM orders WHERE meta IS NOT NULL`)
}

func TestAdvNullHstoreArrowOperator(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	inserts := []string{
		`INSERT INTO users (id, email, name, attrs) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x.com', 'A', 'k=>v'::hstore)`,
	}
	checkNullability(t, string(schemaSQL), inserts, `
		SELECT attrs -> 'missing_key' FROM users WHERE attrs IS NOT NULL`)
}

func TestAdvNullViewFrozenAfterDropNotNull(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, body text NOT NULL);
CREATE VIEW v AS SELECT id, body FROM t;
ALTER TABLE t ALTER COLUMN body DROP NOT NULL;
`
	inserts := []string{
		`INSERT INTO t (id, body) VALUES (1, NULL)`,
	}
	checkNullability(t, schemaSQL, inserts, `SELECT body FROM v`)
}

// TestAdvNullViewWhereUnrelatedColumnDynamic is A5's DROP NOT NULL case again, but this
// time the view has a WHERE clause -- on a column other than the one being asked about.
// The first fix attempt only re-derived nullability for a view with no WHERE / JOIN at
// all (viewIsSimplePassthrough), which meant this case stayed frozen at nullable=false
// forever: too narrow a heuristic, since the WHERE clause here has nothing to do with
// "body" at all. The DDL-triggered refreeze (analyze.go's NotNullHook /
// refreezeDependentNullability) re-analyzes the view's whole query fresh instead of
// guessing from its shape, so it does not need this restriction.
func TestAdvNullViewWhereUnrelatedColumnDynamic(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, body text NOT NULL, other int);
CREATE VIEW v AS SELECT id, body FROM t WHERE other = 1;
ALTER TABLE t ALTER COLUMN body DROP NOT NULL;
`
	inserts := []string{
		`INSERT INTO t (id, body, other) VALUES (1, NULL, 1)`,
	}
	checkNullability(t, schemaSQL, inserts, `SELECT body FROM v`)
}

// TestAdvNullViewOfViewDynamic is A5 through a view-of-view: the DROP NOT NULL reaches
// v2 by way of v1, both re-frozen in declaration order (DependentViews).
func TestAdvNullViewOfViewDynamic(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, body text NOT NULL);
CREATE VIEW v1 AS SELECT id, body FROM t;
CREATE VIEW v2 AS SELECT id, body FROM v1;
ALTER TABLE t ALTER COLUMN body DROP NOT NULL;
`
	inserts := []string{
		`INSERT INTO t (id, body) VALUES (1, NULL)`,
	}
	checkNullability(t, schemaSQL, inserts, `SELECT body FROM v2`)
}

// TestAdvNullViewWhereRejectionStaysNotNull is the flip side of A5: a column a WHERE
// clause null-rejects (`WHERE body IS NOT NULL`) must stay non-nullable even after the
// base column's own NOT NULL is dropped, since the predicate itself guarantees no NULL
// ever survives it -- regardless of what the underlying column now allows. checkNullability
// alone cannot tell a correct nullable=false from a wrongly-relaxed nullable=true here
// (the real query never returns a NULL either way, so there is nothing for it to catch),
// so this asserts the analyzer's own Nullable field directly.
func TestAdvNullViewWhereRejectionStaysNotNull(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, body text NOT NULL);
CREATE VIEW v AS SELECT id, body FROM t WHERE body IS NOT NULL;
ALTER TABLE t ALTER COLUMN body DROP NOT NULL;
`
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema problem: %s", p)
	}
	r, aerr := Analyze(s, `SELECT body FROM v`)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	if r.Columns[0].Nullable {
		t.Errorf("body: nullable=true, want false (WHERE body IS NOT NULL keeps it non-nullable regardless of the base column's own NOT NULL)")
	}
}

func TestAdvNullJsonExistsOnErrorUnknown(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	inserts := []string{
		`INSERT INTO users (id, email, name) OVERRIDING SYSTEM VALUE VALUES (1, 'a@x.com', 'A')`,
		`INSERT INTO orders (user_id, total, meta) VALUES (1, 10, '"just a string"'::jsonb)`,
	}
	checkNullability(t, string(schemaSQL), inserts, `
		SELECT JSON_EXISTS(meta, 'strict $.a' UNKNOWN ON ERROR) FROM orders`)
}

// TestAdvNullViewAliasedColumnsDynamic: a view declared with a column list (CREATE VIEW v
// (x) AS ...) freezes the alias as the column name; the refreeze must compare against the
// alias, or it silently skips every aliased view.
func TestAdvNullViewAliasedColumnsDynamic(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, body text NOT NULL);
CREATE VIEW v (k, txt) AS SELECT id, body FROM t;
ALTER TABLE t ALTER COLUMN body DROP NOT NULL;
`
	inserts := []string{
		`INSERT INTO t (id, body) VALUES (1, NULL)`,
	}
	checkNullability(t, schemaSQL, inserts, `SELECT txt FROM v`)
}
