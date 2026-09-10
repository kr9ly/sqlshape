package pgparse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
)

// TestUpgrade17: a 17 tree is read into the newest node types, its renamed fields moved.
func TestUpgrade17(t *testing.T) {
	tree, err := PG17.Parse("INSERT INTO t (a) VALUES (1) RETURNING a, b")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Version/10000 != 17 {
		t.Errorf("version %d", tree.Version)
	}
	ins := tree.Stmts[0].Stmt.GetInsertStmt()
	if got := len(ins.GetReturningClause().GetExprs()); got != 2 {
		t.Errorf("RETURNING has %d expressions, want 2", got)
	}
	tree18, err := PG18.Parse("INSERT INTO t (a) VALUES (1) RETURNING a, b")
	if err != nil {
		t.Fatal(err)
	}
	tree18.Version = tree.Version
	if !proto.Equal(tree, tree18) {
		t.Error("the upgraded 17 tree differs from 18's own")
	}
	// a statement without any renamed field skips the rewrite and still decodes
	if _, err := PG17.Parse("SELECT 1 WHERE (1,2) < (3,4)"); err != nil {
		t.Error(err)
	}
	// 17's constraints are enforced and its generated columns stored, as 18 says explicitly
	ddl := "CREATE TABLE t (a int CHECK (a > 0), b int GENERATED ALWAYS AS (a * 2) STORED, FOREIGN KEY (a) REFERENCES p (id))"
	tree, err = PG17.Parse(ddl)
	if err != nil {
		t.Fatal(err)
	}
	tree18, err = PG18.Parse(ddl)
	if err != nil {
		t.Fatal(err)
	}
	tree18.Version = tree.Version
	if !proto.Equal(tree, tree18) {
		t.Errorf("17 DDL differs from 18's:\n17: %v\n18: %v", tree, tree18)
	}
}

func TestVersionGrammar(t *testing.T) {
	// RETURNING OLD/NEW is 18 syntax
	const sql = "UPDATE t SET a = 1 RETURNING old.a, new.a"
	if _, err := PG18.Parse(sql); err != nil {
		t.Errorf("18 rejects %q: %v", sql, err)
	}
	if _, err := PG17.Parse(sql); err != nil {
		t.Errorf("17 parses %q as column references, expected no error: %v", sql, err)
	}
	const sql18 = "UPDATE t SET a = 1 RETURNING WITH (OLD AS o) o.a"
	if _, err := PG18.Parse(sql18); err != nil {
		t.Errorf("18 rejects %q: %v", sql18, err)
	}
	if _, err := PG17.Parse(sql18); err == nil {
		t.Errorf("17 accepts %q", sql18)
	}
}

// TestCorpus runs the regress corpus (when present) through both versions: every statement
// 17 accepts must decode through the 17 upgrade (protojson rejects a field the upgrade
// missed), and for the newest version the JSON path must give the protobuf path's tree.
func TestCorpus(t *testing.T) {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "sqlshape", "regress-17", "src", "test", "regress", "sql")
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	if len(files) == 0 {
		t.Skip("no regress corpus at " + dir)
	}
	var stmts, upgraded, same, rejected18 int
	var failures []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := PG17.module().splitWithScanner(string(src))
		if err != nil {
			continue
		}
		for _, sql := range parts {
			if _, err := PG17.module().parseProtobuf(sql); err != nil || !utf8.ValidString(sql) {
				continue // 17 rejects it, or invalid UTF-8 (a proto3 string may not carry it)
			}
			stmts++
			if _, err := PG17.Parse(sql); err != nil {
				if len(failures) < 5 {
					failures = append(failures, filepath.Base(f)+": "+truncate(sql, 100)+": "+err.Error())
				}
				continue
			}
			upgraded++
			pb18, err := PG18.module().parseProtobuf(sql)
			if err != nil {
				rejected18++
				continue
			}
			want := &ParseResult{}
			if err := proto.Unmarshal(pb18, want); err != nil {
				continue
			}
			got, err := PG18.Parse(sql)
			if err != nil {
				t.Errorf("18 JSON path: %v", err)
				continue
			}
			if proto.Equal(got, want) {
				same++
			} else if len(failures) < 5 {
				failures = append(failures, "18 JSON differs: "+truncate(sql, 100))
			}
		}
	}
	for _, f := range failures {
		t.Error(f)
	}
	if upgraded != stmts {
		t.Errorf("%d of %d statements 17 accepts did not decode through the upgrade", stmts-upgraded, stmts)
	}
	t.Logf("17 accepts %d statements; 18 rejects %d of them; 18 JSON == protobuf for %d", stmts, rejected18, same)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
