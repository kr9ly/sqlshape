package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A syntax error in one statement of a file is a per-statement failure, not a
// whole-file abort: the other statements are still judged and printed (ok), the bad
// one is reported as a FAIL at its own line, and the file exits 1 -- per docs/checks.md
// ("Every judgment is printed ... so the output doubles as the audit of what a script
// was allowed to do", and "The exit code is 1 when a statement fails an obligation or
// does not analyze").
func TestAdvCheckSyntaxErrorFailsOnlyThatStatement(t *testing.T) {
	dir := t.TempDir()
	schema := "-- sqlshape: require pinned(id)\nCREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL);"
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(pg17Decl+schema), 0o644); err != nil {
		t.Fatal(err)
	}
	sql := "SELECT id FROM orders WHERE id = 1;\n" +
		"SELECT id FROM WHERE;\n" + // a typo: the second statement does not parse
		"SELECT id FROM orders WHERE id = 2;\n"
	path := filepath.Join(dir, "q.sql")
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), path)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (a statement that does not analyze is a finding); stderr=%q", code, errs)
	}
	want := path + ":1: ok orders: require pinned(id)\n" +
		path + ":2: FAIL 42601: at or near \"WHERE\"\n" +
		path + ":3: ok orders: require pinned(id)\n"
	if out != want {
		t.Errorf("the other statements are still judged:\n--- got ---\n%s--- want ---\n%s", out, want)
	}
}

// A UTF-8 byte order mark (files saved by some Windows editors) is not part of the SQL.
func TestAdvCheckBOM(t *testing.T) {
	dir := t.TempDir()
	schema := "-- sqlshape: require pinned(id)\nCREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL);"
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(pg17Decl+schema), 0o644); err != nil {
		t.Fatal(err)
	}
	sql := "\xEF\xBB\xBFSELECT id FROM orders WHERE id = 1;\n"
	path := filepath.Join(dir, "q.sql")
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), path)
	if code != 0 || !strings.Contains(out, path+":1: ok") {
		t.Errorf("a BOM-prefixed file with one ordinary statement is judged; exit=%d out=%q errs=%q", code, out, errs)
	}
}

// The schema's own function and view bodies are judged by check as they are by vet: a
// trigger function updating an append-only table is a finding no statement would show.
func TestAdvCheckSchemaBodies(t *testing.T) {
	dir := t.TempDir()
	schema := `
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
-- sqlshape: visible where amount > 0
CREATE TABLE memos (id bigint PRIMARY KEY, amount int NOT NULL);
CREATE VIEW all_memos AS SELECT id FROM memos;
`
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(pg17Decl+schema), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "q.sql")
	if err := os.WriteFile(path, []byte("SELECT id FROM orders WHERE id = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), path)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{
		"schema.sql: function bump_ledger line 3: FAIL ledger: require never on update, delete: ledger is declared",
		"schema.sql: view all_memos: FAIL memos: visible where amount > 0: rows of memos are visible where amount > 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A self-join of a `pinned` table: o2 fails to pin tenant_id, o1 does not. Like the
// predicate obligations (`visible where`, an arbitrary `require <expr>`), which
// disambiguate a repeated alias by appending the leaf's alias to the message text
// ("add that predicate for orders o1" -- see the passing case in TestCheck), a
// structural obligation's (`pinned`, `immutable`, `via view`, `alone`, `single`,
// `never`, `paired`, `transitions`) failure message names which occurrence failed, so a
// reader of `sqlshape check`'s plain-text audit can tell o1's passing occurrence from
// o2's failing one on the same line.
func TestAdvCheckPinnedMessageNamesSelfJoinOccurrence(t *testing.T) {
	dir := t.TempDir()
	schema := "-- sqlshape: require pinned(tenant_id)\n" +
		"CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, parent_id bigint);\n"
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(pg17Decl+schema), 0o644); err != nil {
		t.Fatal(err)
	}
	sql := "SELECT o1.id FROM orders o1 JOIN orders o2 ON o2.parent_id = o1.id WHERE o1.tenant_id = 1;\n"
	path := filepath.Join(dir, "q.sql")
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 judgments (o1 ok, o2 fail), got:\n%s", out)
	}
	// o1 passes; o2 fails, and its message names o2, not just the bare table -- parity
	// with the predicate obligations' checker.predicate, which appends the leaf's alias
	// ("add that predicate for %s").
	if !strings.Contains(lines[0], "ok orders") {
		t.Errorf("line 1 (o1, pinned): %q", lines[0])
	}
	if !strings.Contains(lines[1], "(the occurrence o2)") {
		t.Errorf("line 2 (o2, pinned) does not name which occurrence failed: %q", lines[1])
	}
}
