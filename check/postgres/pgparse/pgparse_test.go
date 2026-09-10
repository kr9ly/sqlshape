package pgparse

import (
	"strings"
	"sync"
	"testing"
)

// Equivalence with pg_query_go (cgo) was established when this package replaced it: the
// protobuf bytes of a corpus, error messages and cursor positions, deparse output and
// PL/pgSQL JSON were identical. From here the regress probe and the golden tests guard the
// parser; these tests cover the wasm boundary itself.

func TestParse(t *testing.T) {
	tree, err := Parse("SELECT a FROM t WHERE b = $1 AND c IN (1,2); SELECT 'あ'")
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(tree.Stmts))
	}
	sel := tree.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil || len(sel.TargetList) != 1 || sel.WhereClause == nil {
		t.Errorf("unexpected tree: %v", tree.Stmts[0])
	}
	if got := tree.Stmts[1].StmtLocation; got != 44 {
		t.Errorf("second statement location %d, want 44", got)
	}
}

// TestParseError: a syntax error (the longjmp path) is reported with PostgreSQL's message
// and position, and the instance keeps working afterwards.
func TestParseError(t *testing.T) {
	for range 3 {
		_, err := Parse("SELECT id FROM WHERE")
		e, ok := err.(*Error)
		if !ok {
			t.Fatalf("got %v, want *Error", err)
		}
		if e.Message != `syntax error at or near "WHERE"` || e.Cursorpos != 16 {
			t.Errorf("got %q at %d", e.Message, e.Cursorpos)
		}
		if _, err := Parse("SELECT 1"); err != nil {
			t.Fatalf("after an error: %v", err)
		}
	}
}

func TestDeparse(t *testing.T) {
	tree, err := Parse("select a,b from t where c=1 order by a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Deparse(tree)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT a, b FROM t WHERE c = 1 ORDER BY a"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := Deparse(&ParseResult{Stmts: []*RawStmt{{Stmt: &Node{}}}}); err == nil {
		t.Error("want an error for an empty node")
	}
}

func TestParsePlPgSqlToJSON(t *testing.T) {
	got, err := ParsePlPgSqlToJSON("CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ DECLARE x int := 1; BEGIN RETURN x + 1; END $$")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"PLpgSQL_stmt_return"`) || !strings.Contains(got, `"x"`) {
		t.Errorf("unexpected JSON: %s", got)
	}
	_, err = ParsePlPgSqlToJSON("CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETRUN 1; END $$")
	if e, ok := err.(*Error); !ok || !strings.HasPrefix(e.Message, "syntax error") {
		t.Errorf("got %v, want a syntax error", err)
	}
}

func TestSplitWithScanner(t *testing.T) {
	src := "SELECT 1;\n-- c\nSELECT FROM WHERE 'あ;';\n\nSELECT 3"
	got, err := SplitWithScanner(src, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT 1|\n-- c\nSELECT FROM WHERE 'あ;'|\n\nSELECT 3"; strings.Join(got, "|") != want {
		t.Errorf("got %q", got)
	}
	got, _ = SplitWithScanner(src, true)
	if want := "SELECT 1|-- c\nSELECT FROM WHERE 'あ;'|SELECT 3"; strings.Join(got, "|") != want {
		t.Errorf("trimmed: got %q", got)
	}
	if _, err := SplitWithScanner("SELECT 'unterminated", false); err == nil {
		t.Error("want an error for an unterminated literal")
	}
}

// TestConcurrent: parallel callers each get a working instance.
func TestConcurrent(t *testing.T) {
	const sql = "MERGE INTO o USING s ON o.id = s.id WHEN MATCHED THEN UPDATE SET x = s.x WHEN NOT MATCHED THEN INSERT (id, x) VALUES (s.id, s.x)"
	want, err := PG17.module().parseProtobuf(sql)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				got, err := PG17.module().parseProtobuf(sql)
				if err != nil || string(got) != string(want) {
					t.Errorf("concurrent parse: err=%v same=%v", err, string(got) == string(want))
					return
				}
				if _, err := Parse("SELECT FROM WHERE"); err == nil {
					t.Error("want a syntax error")
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkParse(b *testing.B) {
	for range b.N {
		PG17.module().parseProtobuf("SELECT o.id, o.total, c.name FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.tenant_id = $1 AND o.status IN ('open','paid') ORDER BY o.created_at DESC LIMIT 20")
	}
}
