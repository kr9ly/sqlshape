package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// The migration commands on a MySQL schema: diff against a live server (canonicalized in a
// scratch database on it), verify-schema before and after, apply with -dry-run and for real,
// a re-apply refused, and diff -from two texts (canonicalized on a local mysqld).
func TestMySQLDiffApplyVerify(t *testing.T) {
	ctx := context.Background()
	base := example(t, "5-mysql")
	db, err := mysqltest.Start(ctx, base)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	edit := base + `
ALTER TABLE customers ADD COLUMN nickname VARCHAR(50) DEFAULT 'anon';
CREATE TABLE order_kinds (code VARCHAR(20) PRIMARY KEY, label VARCHAR(50) NOT NULL);
CREATE VIEW paid_orders AS SELECT id, total FROM orders WHERE status = 'paid';
`
	if err := os.WriteFile(schemaPath, []byte(edit), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := db.DSN()

	code, out, errs := run(t, "diff", "-db", dsn, "-schema", schemaPath)
	if code != 0 || !strings.Contains(out, "ADD COLUMN `nickname`") || !strings.Contains(out, "CREATE TABLE `order_kinds`") || !strings.Contains(out, "VIEW `paid_orders`") {
		t.Fatalf("diff: code %d\n%s%s", code, out, errs)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "verify-schema", "-db", dsn, "-schema", schemaPath); code != 1 || !strings.Contains(errs, "+ table order_kinds") || !strings.Contains(errs, "+ view paid_orders") {
		t.Fatalf("verify-schema before apply: code %d\n%s", code, errs)
	}
	if code, out, _ := run(t, "apply", "-db", dsn, "-schema", schemaPath, "-dry-run", ddlPath); code != 0 || !strings.Contains(out, "dry run") {
		t.Fatalf("apply -dry-run: code %d\n%s", code, out)
	}
	if code, _, _ := run(t, "verify-schema", "-db", dsn, "-schema", schemaPath); code != 1 {
		t.Fatalf("dry run must not apply: code %d", code)
	}
	if code, out, errs := run(t, "apply", "-db", dsn, "-schema", schemaPath, ddlPath); code != 0 || !strings.Contains(out, "applied") {
		t.Fatalf("apply: code %d\n%s%s", code, out, errs)
	}
	if code, out, errs := run(t, "verify-schema", "-db", dsn, "-schema", schemaPath); code != 0 || !strings.Contains(out, "ok:") {
		t.Fatalf("verify-schema after apply: code %d\n%s%s", code, out, errs)
	}
	// the same DDL again is refused: its end state is no longer the target
	if code, _, errs := run(t, "apply", "-db", dsn, "-schema", schemaPath, ddlPath); code != 1 || !strings.Contains(errs, "does not apply") {
		t.Fatalf("re-apply: code %d\n%s", code, errs)
	}
	// an undeclared drop is a finding, the DDL still printed
	if err := os.WriteFile(schemaPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run(t, "diff", "-db", dsn, "-schema", schemaPath); code != 1 || !strings.Contains(errs, "column customers.nickname is dropped, which no @migrate declares") || !strings.Contains(out, "DROP COLUMN `nickname`") {
		t.Fatalf("diff back: code %d\n%s%s", code, out, errs)
	}
	// -from compares two schema texts without a database
	if code, out, errs := run(t, "diff", "-from", "../../../../examples/5-mysql/schema.sql", "-schema", ddlEditPath(t, dir, edit)); code != 0 || !strings.Contains(out, "ADD COLUMN `nickname`") {
		t.Fatalf("diff -from: code %d\n%s%s", code, out, errs)
	}
	// no scratch database is left on the server
	var n int
	if err := db.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'sqlshape_scratch_%'").Scan(&n); err != nil || n != 0 {
		t.Errorf("scratch databases left: %d %v", n, err)
	}
}

func ddlEditPath(t *testing.T, dir, text string) string {
	t.Helper()
	p := filepath.Join(dir, "edited.sql")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
