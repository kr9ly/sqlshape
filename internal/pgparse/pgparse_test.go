package pgparse

import (
	"strings"
	"sync"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"github.com/pganalyze/pg_query_go/v6/parser"
	"google.golang.org/protobuf/proto"
)

var statements = []string{
	"SELECT 1",
	"SELECT a FROM t WHERE b = $1 AND c IN (1,2)",
	"INSERT INTO t (a) VALUES (1) ON CONFLICT (a) DO UPDATE SET a = excluded.a RETURNING *",
	"WITH d AS (DELETE FROM ledger WHERE id = $1 RETURNING amount) SELECT amount FROM d",
	"MERGE INTO o USING s ON o.id = s.id WHEN MATCHED THEN UPDATE SET x = s.x WHEN NOT MATCHED THEN INSERT (id, x) VALUES (s.id, s.x)",
	"SELECT * FROM json_table(j, '$[*]' COLUMNS (a int PATH '$.a')) AS jt",
	"SELECT json_table(j) FROM t", // rejected by both
	"CREATE TABLE t (id bigint PRIMARY KEY, s text NOT NULL CHECK (s IN ('a','b')), FOREIGN KEY (id) REFERENCES p (id))",
	"SELECT * FROM t WHERE x = 'it''s' AND y = E'\\n' AND z = $$dollar$$",
	"SELECT 'ünïcödé', 'あ' FROM t; SELECT 2",
}

// TestParseMatchesCgo: the wasm parser's protobuf bytes are the cgo parser's.
func TestParseMatchesCgo(t *testing.T) {
	for _, sql := range statements {
		want, cgoErr := parser.ParseToProtobuf(sql)
		got, err := pg17.parseProtobuf(sql)
		if cgoErr != nil || err != nil {
			if err == nil || cgoErr == nil || err.Error() != cgoErr.Error() {
				t.Errorf("%q: wasm %v, cgo %v", sql, err, cgoErr)
			}
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%q: protobuf differs (%d vs %d bytes)", sql, len(got), len(want))
		}
	}
}

// TestParseError: a syntax error (the longjmp path) is reported with cgo's message and
// position, and the instance keeps working afterwards.
func TestParseError(t *testing.T) {
	for i := 0; i < 3; i++ {
		_, err := Parse("SELECT id FROM WHERE")
		e, ok := err.(*Error)
		if !ok {
			t.Fatalf("got %v, want *Error", err)
		}
		_, cgoErr := pg_query.Parse("SELECT id FROM WHERE")
		if e.Message != cgoErr.Error() {
			t.Errorf("message %q, cgo %q", e.Message, cgoErr)
		}
		if e.Cursorpos != cgoErr.(*parser.Error).Cursorpos {
			t.Errorf("cursor %d, cgo %d", e.Cursorpos, cgoErr.(*parser.Error).Cursorpos)
		}
		if _, err := Parse("SELECT 1"); err != nil {
			t.Fatalf("after an error: %v", err)
		}
	}
}

func TestDeparse(t *testing.T) {
	tree, err := Parse("SELECT a, b FROM t WHERE c = 1 ORDER BY a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Deparse(tree)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pg_query.Deparse(tree)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParsePlPgSqlToJSON(t *testing.T) {
	src := "CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ DECLARE x int := 1; BEGIN RETURN x + 1; END $$"
	got, err := ParsePlPgSqlToJSON(src)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pg_query.ParsePlPgSqlToJSON(src)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := ParsePlPgSqlToJSON("CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETRUN 1; END $$"); err == nil {
		t.Error("want an error for a PL/pgSQL syntax error")
	}
}

func TestSplitWithScanner(t *testing.T) {
	src := "SELECT 1;\n-- c\nSELECT FROM WHERE 'あ;';\n\nSELECT 3"
	for _, trim := range []bool{false, true} {
		got, err := SplitWithScanner(src, trim)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := pg_query.SplitWithScanner(src, trim)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("trim=%v: got %q, want %q", trim, got, want)
		}
	}
	if _, err := SplitWithScanner("SELECT 'unterminated", false); err == nil {
		t.Error("want an error for an unterminated literal")
	}
}

// TestConcurrent: parallel callers each get a working instance.
func TestConcurrent(t *testing.T) {
	want, _ := parser.ParseToProtobuf(statements[4])
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got, err := pg17.parseProtobuf(statements[4])
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
	for i := 0; i < b.N; i++ {
		pg17.parseProtobuf(statements[4])
	}
}

func BenchmarkParseCgo(b *testing.B) {
	for i := 0; i < b.N; i++ {
		parser.ParseToProtobuf(statements[4])
	}
}

var _ = proto.Marshal
