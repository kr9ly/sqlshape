package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// check judges statements outside Go: every judgment is printed, a failure is exit 1,
// and the `-- sqlshape:` lines above a statement belong to it.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	schema := `
-- sqlshape: visible where deleted_at IS NULL
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, deleted_at timestamptz);
CREATE TABLE audit (id bigint PRIMARY KEY, note text);
`
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(pg17Decl+schema), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := `-- nightly cleanup
UPDATE orders SET status = 'archived'
 WHERE tenant_id = 7 AND deleted_at IS NULL AND status = 'closed';

-- sqlshape: waive orders pinned(tenant_id)
SELECT count(*) FROM orders WHERE deleted_at IS NULL;

DELETE FROM orders WHERE id = 42;
INSERT INTO audit (id, note) VALUES (1, 'done');
`
	sqlPath := filepath.Join(dir, "ops.sql")
	if err := os.WriteFile(sqlPath, []byte(ops), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), sqlPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, errs)
	}
	want := []string{
		sqlPath + ":3: ok orders: visible where deleted_at IS NULL",
		sqlPath + ":2: ok orders: require pinned(tenant_id)",
		sqlPath + ":6: ok orders: visible where deleted_at IS NULL",
		sqlPath + ":6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement",
		sqlPath + ":8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where deleted_at IS NULL: add that predicate for orders, or opt out with `-- sqlshape: unfiltered orders`",
		sqlPath + ":8: FAIL orders: require pinned(tenant_id): orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)",
	}
	if got := strings.TrimRight(out, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if !strings.Contains(errs, "2 finding(s)") {
		t.Errorf("stderr: %s", errs)
	}

	// -quiet: failures only; a clean file is exit 0 and silent
	code, out, _ = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), "-quiet", sqlPath)
	if code != 1 || strings.Count(out, "\n") != 2 {
		t.Errorf("quiet: exit %d\n%s", code, out)
	}
	clean := filepath.Join(dir, "clean.sql")
	os.WriteFile(clean, []byte("SELECT id FROM orders WHERE tenant_id = 1 AND deleted_at IS NULL"), 0o644)
	if code, out, _ = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), "-quiet", clean); code != 0 || out != "" {
		t.Errorf("clean: exit %d\n%s", code, out)
	}
	// -context selects a declared context: ops lifts the tenant pin
	if code, out, _ = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), "-context", "ops", "-quiet", sqlPath); code != 1 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "visible where") {
		t.Errorf("context ops: exit %d\n%s", code, out)
	}
	// stdin, a missing file, and a schema whose declarations do not parse
	r, w, _ := os.Pipe()
	w.WriteString("SELECT id FROM orders WHERE tenant_id = 1 AND deleted_at IS NULL")
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	code, out, _ = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"))
	os.Stdin = saved
	if code != 0 || !strings.Contains(out, "stdin:1: ok") {
		t.Errorf("stdin: exit %d\n%s", code, out)
	}
	if code, _, errs = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), filepath.Join(dir, "missing.sql")); code != 2 || !strings.Contains(errs, "missing.sql") {
		t.Errorf("missing: exit %d %s", code, errs)
	}
	badSchema := filepath.Join(dir, "bad_schema.sql")
	os.WriteFile(badSchema, []byte(pg17Decl+"-- sqlshape: require pinned()\nCREATE TABLE t (id int);"), 0o644)
	if code, _, errs = run(t, "check", "-schema", badSchema, clean); code != 2 || !strings.Contains(errs, "pinned needs a column") {
		t.Errorf("bad schema: exit %d %s", code, errs)
	}
	// the audit shows the discharge path: a policy, a composite foreign key, an EXISTS witness
	audit := filepath.Join(dir, "audit")
	os.MkdirAll(audit, 0o755)
	os.WriteFile(filepath.Join(audit, "schema.sql"), []byte(pg17Decl+`
-- sqlshape: require pinned(tenant_id)
CREATE TABLE docs (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, body text, UNIQUE (id, tenant_id));
ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
CREATE POLICY docs_tenant ON docs USING (tenant_id = current_setting('app.tenant', true)::bigint);
-- sqlshape: require pinned(tenant_id)
-- sqlshape: require EXISTS (SELECT 1 FROM docs d WHERE d.id = doc_id AND d.body IS NOT NULL) on read
CREATE TABLE doc_tags (doc_id bigint NOT NULL, tenant_id bigint NOT NULL, tag text NOT NULL,
  PRIMARY KEY (doc_id, tag), FOREIGN KEY (doc_id, tenant_id) REFERENCES docs (id, tenant_id));
`), 0o644)
	auditSQL := filepath.Join(audit, "q.sql")
	os.WriteFile(auditSQL, []byte(`SELECT body FROM docs WHERE id = 1;
SELECT t.tag FROM docs d JOIN doc_tags t ON t.doc_id = d.id WHERE d.tenant_id = 7 AND d.body IS NOT NULL;
`), 0o644)
	code, out, _ = run(t, "check", "-schema", filepath.Join(audit, "schema.sql"), auditSQL)
	wantAudit := []string{
		auditSQL + ":1: ok(policy) docs: require pinned(tenant_id): docs.tenant_id is pinned by policy docs_tenant for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner",
		auditSQL + ":2: ok docs: require pinned(tenant_id)",
		auditSQL + ":2: ok(fk) doc_tags: require pinned(tenant_id)",
		auditSQL + ":2: ok doc_tags: require EXISTS (SELECT 1 FROM docs d WHERE d.id = doc_id AND d.body IS NOT NULL) on read",
	}
	if code != 0 || strings.TrimRight(out, "\n") != strings.Join(wantAudit, "\n") {
		t.Errorf("audit: exit %d\n%s\nwant:\n%s", code, out, strings.Join(wantAudit, "\n"))
	}

	// a statement that does not analyze is a failure too
	bad := filepath.Join(dir, "bad.sql")
	os.WriteFile(bad, []byte("SELECT nope FROM orders"), 0o644)
	if code, out, _ = run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), bad); code != 1 || !strings.Contains(out, `FAIL 42703: column "nope" does not exist`) {
		t.Errorf("bad: exit %d\n%s", code, out)
	}
}
