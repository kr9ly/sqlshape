package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
)

// one embedded server for every command of the test binary (each command would boot
// its own); Close is deferred to TestMain
var shared *dump.Server

// defaultNewServer is the package's real newServer implementation, captured before
// TestMain overrides the var with the shared embedded server below, so one test
// (TestNewServerDefault) can exercise the actual dump.NewServer call path.
var defaultNewServer = newServer

func TestMain(m *testing.M) {
	if _, err := exec.LookPath(dump.Binary()); err == nil {
		srv, err := dump.NewServer(context.Background(), pgparse.Default)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		shared = srv
		newServer = func(context.Context, pgparse.Version) (server, error) { return sharedServer{srv}, nil }
	}
	code := m.Run()
	if shared != nil {
		shared.Close()
	}
	os.Exit(code)
}

// sharedServer is the shared server with a Close that does nothing.
type sharedServer struct{ *dump.Server }

func (sharedServer) Close() error { return nil }

func requirePgDump(t *testing.T) {
	t.Helper()
	if shared == nil {
		t.Skipf("%s not found", dump.Binary())
	}
}

// run executes a subcommand and returns its exit code, stdout and stderr.
// pg17Decl is the version declaration every schema.sql must open with.
const pg17Decl = "-- sqlshape: postgres 17\n"

func run(t *testing.T, name string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), name, args, &out, &errb)
	t.Logf("sqlshape %s %s → %d\n%s%s", name, strings.Join(args, " "), code, out.String(), errb.String())
	return code, out.String(), errb.String()
}

func example(t *testing.T, name string) string {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join("../../../../examples", name, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(sql)
}

// A database holding the example schema, a schema.sql that grew (a column, a seeded
// lookup table): diff prints the DDL, verify-schema sees the drift, apply runs the DDL
// after checking it, and verify-schema is quiet afterwards.
func TestDiffApplyVerify(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := example(t, "1-tables")
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	edit := base + `
ALTER TABLE customers ADD COLUMN nickname text DEFAULT 'anon';
CREATE TABLE order_kinds (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO order_kinds (code, label) VALUES ('retail', 'Retail'), ('bulk', 'Bulk');
`
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+edit), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, "diff", "-db", db.ConnString(), "-schema", schemaPath)
	if code != 0 || !strings.Contains(out, "ADD COLUMN") || !strings.Contains(out, "CREATE TABLE") || !strings.Contains(out, "MERGE INTO") {
		t.Fatalf("diff: code %d\n%s", code, out)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, errs := run(t, "verify-schema", "-db", db.ConnString(), "-schema", schemaPath); code != 1 || !strings.Contains(errs, "+ table order_kinds") {
		t.Fatalf("verify-schema before apply: code %d\n%s", code, errs)
	}
	if code, out, _ := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, "-dry-run", ddlPath); code != 0 || !strings.Contains(out, "dry run") {
		t.Fatalf("apply -dry-run: code %d\n%s", code, out)
	}
	if code, _, errs := run(t, "verify-schema", "-db", db.ConnString(), "-schema", schemaPath); code != 1 {
		t.Fatalf("dry run must not apply: code %d\n%s", code, errs)
	}
	if code, out, _ := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath); code != 0 || !strings.Contains(out, "applied") {
		t.Fatalf("apply: code %d\n%s", code, out)
	}
	if code, out, errs := run(t, "verify-schema", "-db", db.ConnString(), "-schema", schemaPath); code != 0 || !strings.Contains(out, "ok:") {
		t.Fatalf("verify-schema after apply: code %d\n%s%s", code, out, errs)
	}
	// the same DDL again is refused: its end state is no longer the target
	if code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath); code != 1 || !strings.Contains(errs, "does not apply") {
		t.Fatalf("re-apply: code %d\n%s", code, errs)
	}
	// -from compares two schema texts without a database
	if code, out, _ := run(t, "diff", "-from", "../../../../examples/1-tables/schema.sql", "-schema", schemaPath); code != 0 || !strings.Contains(out, "ADD COLUMN") {
		t.Fatalf("diff -from: code %d\n%s", code, out)
	}
}

// A DROP no declaration explains is a finding of diff; with -packages the consumers of
// the dropped column are listed, and apply refuses to run while they exist.
func TestConsumers(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := example(t, "1-tables")
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root) // -packages patterns are relative to the module root
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+base+"\nALTER TABLE orders DROP COLUMN note;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "diff", "-db", db.ConnString(), "-schema", schemaPath, "-packages", "./examples/1-tables")
	if code != 1 || !strings.Contains(errs, "needs declarations") || !strings.Contains(out, "DROP COLUMN") {
		t.Fatalf("diff: code %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "-- - column orders.note reaches") || !strings.Contains(out, "queries.go") {
		t.Fatalf("diff must list the consumers of orders.note:\n%s", out)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte(`ALTER TABLE orders DROP COLUMN note;`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, "-packages", "./examples/1-tables", ddlPath); code != 1 || !strings.Contains(errs, "still depend") {
		t.Fatalf("apply must refuse: code %d\n%s", code, errs)
	}
	if code, out, _ := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, "-packages", "./examples/1-tables", "-force", ddlPath); code != 0 || !strings.Contains(out, "applied") {
		t.Fatalf("apply -force: code %d\n%s", code, out)
	}
}

func TestUsage(t *testing.T) {
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "verify-schema") {
		t.Fatal("help")
	}
	if code, _, errs := run(t, "nope"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Fatal("unknown")
	}
	if code, _, errs := run(t, "diff", "-schema", "x.sql"); code != 2 || !strings.Contains(errs, "exactly one of") {
		t.Fatalf("diff without source: %d %s", code, errs)
	}
}
