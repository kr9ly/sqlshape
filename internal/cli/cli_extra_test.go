package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The version subcommand prints "sqlshape <version>"; Version() itself reports the
// release build's version when set, else falls back to build info / "devel".
func TestVersionSubcommand(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = ""
	code, out, _ := run(t, "version")
	if code != 0 || !strings.HasPrefix(out, "sqlshape ") {
		t.Fatalf("version: code %d out %q", code, out)
	}
	// with no release version set, Version() falls back to build info; whatever it
	// reports, it must not be empty, and the subcommand output must match it exactly.
	if got := "sqlshape " + Version() + "\n"; got != out {
		t.Fatalf("version output %q does not match Version() %q", out, got)
	}

	version = "v9.9.9-test"
	code, out, _ = run(t, "version")
	if code != 0 || out != "sqlshape v9.9.9-test\n" {
		t.Fatalf("version (release): code %d out %q", code, out)
	}
	if Version() != "v9.9.9-test" {
		t.Fatalf("Version() = %q, want v9.9.9-test", Version())
	}
}

func TestIsSubcommand(t *testing.T) {
	for _, name := range Subcommands {
		if !IsSubcommand(name) {
			t.Errorf("IsSubcommand(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "nope", "Vet", "diffs"} {
		if IsSubcommand(name) {
			t.Errorf("IsSubcommand(%q) = true, want false", name)
		}
	}
}

// newSchemaFlag builds a fresh flag.FlagSet with -schema registered, parses args, and
// returns the resolver, mirroring how each subcommand wires schemaFlag.
func newSchemaFlag(t *testing.T, args ...string) func() (string, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := schemaFlag(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return get
}

// An explicit -schema wins outright: no filesystem walk, whatever the path.
func TestSchemaFlagExplicit(t *testing.T) {
	get := newSchemaFlag(t, "-schema", "given.sql")
	path, err := get()
	if err != nil || path != "given.sql" {
		t.Fatalf("get() = %q, %v; want given.sql, nil", path, err)
	}
}

// With -schema empty, the resolver walks up from the working directory looking for
// schema.sql or a schema/ directory.
func TestSchemaFlagWalksUpToFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("-- x"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)

	get := newSchemaFlag(t)
	path, err := get()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("get() = %q, want %q", got, want)
	}
}

// A schema/ directory (not a file named schema) also resolves, found by walking up.
func TestSchemaFlagWalksUpToDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "sub")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)

	get := newSchemaFlag(t)
	path, err := get()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "schema"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("get() = %q, want %q", got, want)
	}
}

// A plain file named "schema" (not a directory) does not count: the walk must keep
// going past it.
func TestSchemaFlagFileNamedSchemaDoesNotCount(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("-- x"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "sub")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)

	get := newSchemaFlag(t)
	path, err := get()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("get() = %q, want %q (file named schema must not satisfy the walk)", got, want)
	}
}

// Neither schema.sql nor a schema/ directory anywhere above the working directory is
// the error path.
func TestSchemaFlagNotFound(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)

	get := newSchemaFlag(t)
	_, err := get()
	if err == nil || err.Error() != "schema.sql (or a schema/ directory) not found in the working directory or above (use -schema)" {
		t.Fatalf("get() error = %v", err)
	}
}

// finding.Error returns its message verbatim.
func TestFindingError(t *testing.T) {
	f := &finding{msg: "boom"}
	if got := f.Error(); got != "boom" {
		t.Fatalf("Error() = %q, want boom", got)
	}
}

// problems reports a schema text the loader could not fully apply, quoting each
// problem's String().
func TestProblemsReportsLoaderProblems(t *testing.T) {
	err := problems(`ALTER TABLE missing_table ADD COLUMN x int;`)
	if err == nil {
		t.Fatal("problems: want error, got nil")
	}
	if !strings.Contains(err.Error(), "schema has problems:") ||
		!strings.Contains(err.Error(), `relation "missing_table" does not exist`) {
		t.Fatalf("problems error = %q", err.Error())
	}
}

// A clean schema text has no problems.
func TestProblemsClean(t *testing.T) {
	if err := problems(`CREATE TABLE t (id int PRIMARY KEY);`); err != nil {
		t.Fatalf("problems: %v", err)
	}
}

// diff surfaces problems() through Run: a schema.sql with a loader problem is reported
// as a usage-level error (exit 2), not silently absorbed into the canonical form.
func TestDiffReportsSchemaProblems(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	// LISTEN is valid SQL (real Postgres applies it fine, so loadTarget's
	// canonicalization succeeds) but the loader has no notion of it, so problems()
	// catches it as an unsupported statement.
	if err := os.WriteFile(schemaPath, []byte(`LISTEN some_channel;`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", schemaPath, "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, "schema has problems:") ||
		!strings.Contains(errs, "unsupported statement") {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
}

// diff requires exactly one of -db and -from; giving both is also a usage error.
func TestDiffBothDBAndFrom(t *testing.T) {
	code, _, errs := run(t, "diff", "-db", "x", "-from", "y", "-schema", "z.sql")
	if code != 2 || !strings.Contains(errs, "exactly one of") {
		t.Fatalf("diff -db and -from: code %d\n%s", code, errs)
	}
}

// apply requires exactly one DDL file argument.
func TestApplyMissingDDLArg(t *testing.T) {
	code, _, errs := run(t, "apply", "-db", "x", "-schema", "z.sql")
	if code != 2 || !strings.Contains(errs, "give the DDL file to apply") {
		t.Fatalf("apply without DDL: code %d\n%s", code, errs)
	}
}

// apply requires -db.
func TestApplyMissingDB(t *testing.T) {
	dir := t.TempDir()
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "apply", "-schema", "z.sql", ddlPath)
	if code != 2 || !strings.Contains(errs, "-db is required") {
		t.Fatalf("apply without -db: code %d\n%s", code, errs)
	}
}

// apply reports a missing DDL file as an environment error.
func TestApplyDDLFileNotFound(t *testing.T) {
	code, _, errs := run(t, "apply", "-db", "postgres://x", "-schema", "z.sql", "/no/such/file.sql")
	if code != 2 || !strings.Contains(errs, "no such file") {
		t.Fatalf("apply missing DDL file: code %d\n%s", code, errs)
	}
}

// verify-schema requires -db.
func TestVerifyMissingDB(t *testing.T) {
	code, _, errs := run(t, "verify-schema", "-schema", "z.sql")
	if code != 2 || !strings.Contains(errs, "-db is required") {
		t.Fatalf("verify-schema without -db: code %d\n%s", code, errs)
	}
}

// verify-schema rejects unexpected trailing arguments.
func TestVerifyUnexpectedArgument(t *testing.T) {
	code, _, errs := run(t, "verify-schema", "-db", "x", "-schema", "z.sql", "extra")
	if code != 2 || !strings.Contains(errs, `unexpected argument "extra"`) {
		t.Fatalf("verify-schema extra arg: code %d\n%s", code, errs)
	}
}

// An unreadable schema path is an environment error surfaced through getSchema /
// loadTarget's ReadSource, exercised here via verify-schema so it needs no database
// connection to reach (schemaFlag resolves before -db is dialed... actually -db is
// checked first, so use a db string that at least parses; the point is the schema
// read failure surfaces before any network use).
func TestVerifySchemaPathNotFound(t *testing.T) {
	requirePgDump(t)
	code, _, errs := run(t, "verify-schema", "-db", "postgres://nonexistent-host-for-test/db", "-schema", "/no/such/schema.sql")
	if code != 2 || !strings.Contains(errs, "no such file") {
		t.Fatalf("verify-schema missing schema: code %d\n%s", code, errs)
	}
}
