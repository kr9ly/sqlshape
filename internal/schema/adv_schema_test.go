package schema_test

// Adversarial probes for the schema lane: cases where the constraint / index names
// schema.Load computes for unnamed constraints diverge from what real PostgreSQL
// assigns to the same DDL. Anything downstream that keys off Constraint.Name /
// Index.Name (migrate's ADD CONSTRAINT / DROP CONSTRAINT rendering, diff matching,
// analyze's violation messages) trusts these names to agree with PG's.
//
// Each test below currently FAILS: it asserts the name real PG assigns (via the
// embedded-postgres oracle), which is not what schema.Load currently produces. They
// are meant to stay red until the naming logic in schema.addConstraint / chooseIndexName
// is fixed, at which point they become regression guards.

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// oracleConNames boots a real PG on sql and returns the conname of every constraint
// of the given contype ('c' check, 'u' unique, 'p' primary key, 'f' foreign key)
// on user tables (oid >= 16384, so catalog constraints are excluded).
func oracleConNames(t *testing.T, sql string, contype byte) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, sql)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	rows, err := o.Conn().Query(ctx,
		`SELECT conname FROM pg_constraint WHERE contype = $1 AND conrelid >= 16384 ORDER BY conname`,
		string(contype))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func oracleIndexNames(t *testing.T, sql, tableLike string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, sql)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	rows, err := o.Conn().Query(ctx, `SELECT indexname FROM pg_indexes WHERE tablename LIKE $1 ORDER BY indexname`, tableLike)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

// A1: an unnamed table-level CHECK constraint referencing more than one column is
// named "<table>_check" by PG (no column suffix) once more than one column is
// involved; schema.addConstraint always joins every referenced column into the name.
func TestCheckConstraintName_MultiColumn(t *testing.T) {
	sql := `CREATE TABLE t (a int, b int, CHECK (a > 0 AND b > 0));`

	want := oracleConNames(t, sql, 'c')
	if len(want) != 1 {
		t.Fatalf("oracle: expected 1 check constraint, got %v", want)
	}

	s, err := schema.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("problem: %s", p)
	}
	rel := s.Relation("", "t")
	if len(rel.Constraints) != 1 {
		t.Fatalf("schema: expected 1 constraint, got %+v", rel.Constraints)
	}
	got := rel.Constraints[0].Name
	if got != want[0] {
		t.Errorf("check constraint name: schema.Load gave %q, real PG gives %q", got, want[0])
	}
}

// A2: an unnamed UNIQUE constraint whose auto name (<table>_<cols>_key) would exceed
// NAMEDATALEN-1 (63 bytes) is truncated by PG's ChooseConstraintName (via makeObjectName,
// which shortens the table-name component so the whole name fits). schema.addConstraint
// never truncates, so the computed name is longer than PG will ever actually use for the
// same unnamed constraint, and differs from what a later `dump.Canonical` / introspection
// of the real table would report.
func TestUniqueConstraintName_LongTruncation(t *testing.T) {
	longTable := ""
	for i := 0; i < 58; i++ {
		longTable += "a"
	}
	sql := "CREATE TABLE " + longTable + " (x int UNIQUE);"

	want := oracleConNames(t, sql, 'u')
	if len(want) != 1 {
		t.Fatalf("oracle: expected 1 unique constraint, got %v", want)
	}
	if len(want[0]) > 63 {
		t.Fatalf("oracle name itself exceeds 63 bytes: %q", want[0])
	}

	s, err := schema.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("problem: %s", p)
	}
	rel := s.Relation("", longTable)
	if len(rel.Constraints) != 1 {
		t.Fatalf("schema: expected 1 constraint, got %+v", rel.Constraints)
	}
	got := rel.Constraints[0].Name
	if len(got) > 63 {
		t.Errorf("schema.Load produced a %d-byte constraint name (> NAMEDATALEN-1): %q", len(got), got)
	}
	if got != want[0] {
		t.Errorf("unique constraint name: schema.Load gave %q (%d bytes), real PG gives %q (%d bytes)",
			got, len(got), want[0], len(want[0]))
	}
}

// A3: chooseIndexName / makeObjectName truncate the assembled name with a plain byte
// slice (name1[:n1]), not PG's pg_mbcliplen (which only cuts on a rune boundary). With
// a long enough multibyte table name, sqlshape's computed index name is not even valid
// UTF-8, while PG's own truncation always yields a valid string.
func TestIndexName_MultibyteTruncation(t *testing.T) {
	// 20 "あ" (3 bytes each) + the "t_" prefix is 62 bytes, just inside NAMEDATALEN-1,
	// so the *table* name itself survives identifier truncation intact (schema.Load's
	// own identifier truncation is multibyte-safe - confirmed separately). It is
	// chooseIndexName's byte-oriented shortening of "<table>_<col>_idx" that then
	// has to cut the table name further, and does so without respecting rune
	// boundaries.
	table := ""
	for i := 0; i < 20; i++ {
		table += "あ"
	}
	tableName := "t_" + table
	sql := "CREATE TABLE " + tableName + " (x int); CREATE INDEX ON " + tableName + " (x);"

	want := oracleIndexNames(t, sql, "t\\_%")
	if len(want) != 1 {
		t.Fatalf("oracle: expected 1 index, got %v", want)
	}
	if !utf8.ValidString(want[0]) {
		t.Fatalf("oracle index name itself is invalid UTF-8: %q", want[0])
	}

	s, err := schema.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("problem: %s", p)
	}
	var rel *schema.Relation
	for _, r := range s.Relations {
		if r.Kind == schema.Table {
			rel = r
		}
	}
	if rel == nil {
		t.Fatalf("schema: no table relation loaded (relations: %+v)", s.Relations)
	}
	if len(rel.Indexes) != 1 {
		t.Fatalf("schema: expected 1 index, got %+v", rel.Indexes)
	}
	got := rel.Indexes[0].Name
	if !utf8.ValidString(got) {
		t.Errorf("schema.Load produced an invalid-UTF-8 index name: %q", got)
	}
	if got != want[0] {
		t.Errorf("index name: schema.Load gave %q, real PG gives %q", got, want[0])
	}
}
