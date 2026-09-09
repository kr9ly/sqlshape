package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/internal/consumers"
	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/pgparse"
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
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte(pg17Decl+"-- x"), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, "schema"), []byte(pg17Decl+"not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte(pg17Decl+"-- x"), 0o644); err != nil {
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
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+`LISTEN some_channel;`), 0o644); err != nil {
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

// -h on any subcommand makes flag.Parse return flag.ErrHelp, which Run reports as a
// usage error (exit 2) distinct from both a finding and a generic error.
func TestSubcommandHelpFlag(t *testing.T) {
	for _, args := range [][]string{
		{"diff", "-h"},
		{"apply", "-h"},
		{"verify-schema", "-h"},
	} {
		code, _, _ := run(t, args[0], args[1:]...)
		if code != 2 {
			t.Fatalf("%v: code %d, want 2 (flag.ErrHelp)", args, code)
		}
	}
}

// schemaFlag's walk-up needs the working directory; a working directory removed out
// from under the process makes os.Getwd fail.
func TestSchemaFlagGetwdError(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	get := newSchemaFlag(t)
	if _, err := get(); err == nil {
		t.Fatal("get(): want error from a removed working directory")
	}
}

// changeTarget resolves nothing for a change diff.Compare would not put in front of the
// consumer index: a kind other than table / view / matview / column, or a column change
// whose name carries no dotted relation.
func TestChangeTargetUnmatched(t *testing.T) {
	if _, _, ok := changeTarget(diff.Change{Kind: "index", Name: "t_idx"}); ok {
		t.Fatal("an index change must not resolve to a consumer-index target")
	}
	if _, _, ok := changeTarget(diff.Change{Kind: "column", Name: "nodothere"}); ok {
		t.Fatal("a column change without a dotted name must not resolve")
	}
}

// impacts skips a change whose kind changeTarget does not resolve (an index, say):
// there is nothing to look up in the consumer index.
func TestImpactsSkipsUnmatchedKind(t *testing.T) {
	idx := consumers.New()
	idx.AddRelation("t", consumers.Site{Owner: "pkg.Foo"})
	got := impacts([]diff.Change{{Op: diff.Drop, Kind: "index", Name: "t_idx"}}, idx)
	if len(got) != 0 {
		t.Fatalf("impacts = %+v, want none", got)
	}
}

// impacts looks up a dropped table by relation only (no column), and reports it when
// the index has sites for the whole relation.
func TestImpactsRelationLevel(t *testing.T) {
	idx := consumers.New()
	idx.AddRelation("t", consumers.Site{Owner: "pkg.Baz"})
	got := impacts([]diff.Change{{Op: diff.Drop, Kind: "table", Name: "t"}}, idx)
	if len(got) != 1 || len(got[0].sites) != 1 {
		t.Fatalf("impacts = %+v", got)
	}
}

// impacts also reaches for an Alter that changes a column's type, not just a Drop.
func TestImpactsAlterTypeColumn(t *testing.T) {
	idx := consumers.New()
	idx.AddColumn("t", "c", consumers.Site{Owner: "pkg.Bar"})
	changes := []diff.Change{{
		Op: diff.Alter, Kind: "column", Name: "t.c",
		Fields: []diff.Field{{Name: "type", From: "text", To: "int"}},
	}}
	got := impacts(changes, idx)
	if len(got) != 1 || len(got[0].sites) != 1 {
		t.Fatalf("impacts = %+v", got)
	}
}

// impactText truncates a change's head to its first line: an Alter's String() carries
// its Fields on following lines, which must not leak into the "reaches N statement(s)"
// summary line.
func TestImpactTextMultilineHead(t *testing.T) {
	list := []impact{{
		change: diff.Change{
			Op: diff.Alter, Kind: "column", Name: "t.c",
			Fields: []diff.Field{{Name: "type", From: "text", To: "int"}},
		},
		sites: []consumers.Site{{Owner: "pkg.Bar"}},
	}}
	text := impactText(list, "-- ")
	if !strings.Contains(text, "-- ~ column t.c reaches 1 statement(s):") {
		t.Fatalf("impactText head not truncated to one line:\n%s", text)
	}
}

// indexConsumers reports a package pattern that fails to load (packages.PrintErrors)
// distinctly from one that loads clean.
func TestIndexConsumersBadPackage(t *testing.T) {
	_, err := indexConsumers([]string{"./no/such/dir"}, pgparse.Default, "")
	if err == nil || !strings.Contains(err.Error(), "packages did not load") {
		t.Fatalf("indexConsumers: %v", err)
	}
}

// indexConsumers writes currentSQL to a temp file first; an unwritable TMPDIR makes
// that os.CreateTemp fail.
func TestIndexConsumersTempFileError(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := indexConsumers([]string{"./no/such/dir"}, pgparse.Default, "x"); err == nil {
		t.Fatal("indexConsumers: want error from an unwritable TMPDIR")
	}
}

// newServer failing (the embedded verification server could not boot) is a plain
// environment error for every subcommand that needs one.
func TestNewServerErrorPropagates(t *testing.T) {
	old := newServer
	newServer = func(context.Context, pgparse.Version) (server, error) { return nil, errors.New("boom-newserver") }
	defer func() { newServer = old }()

	dir := t.TempDir()
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	dummySchema := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(dummySchema, []byte(pg17Decl), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, errs := run(t, "diff", "-from", "x", "-schema", dummySchema); code != 2 || !strings.Contains(errs, "boom-newserver") {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
	if code, _, errs := run(t, "verify-schema", "-db", "x", "-schema", dummySchema); code != 2 || !strings.Contains(errs, "boom-newserver") {
		t.Fatalf("verify-schema: code %d\n%s", code, errs)
	}
	if code, _, errs := run(t, "apply", "-db", "x", "-schema", dummySchema, ddlPath); code != 2 || !strings.Contains(errs, "boom-newserver") {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// newServer's default value (before TestMain overrides it with the shared embedded
// server) really does boot dump.NewServer; exercised once here so that call path is
// covered, separately from the shared-server double every other test uses.
func TestNewServerDefault(t *testing.T) {
	requirePgDump(t)
	old := newServer
	newServer = defaultNewServer
	defer func() { newServer = old }()

	srv, err := newServer(context.Background(), pgparse.Default)
	if err != nil {
		t.Fatalf("newServer (default): %v", err)
	}
	defer srv.Close()
}

// Neither schema.sql nor schema/ above an empty working directory is a usage error for
// every subcommand that resolves -schema, not just the resolver itself.
func TestGetSchemaErrorPropagates(t *testing.T) {
	empty := t.TempDir()
	t.Chdir(empty)

	if code, _, errs := run(t, "diff", "-from", "x"); code != 2 || !strings.Contains(errs, "schema.sql") {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
	if code, _, errs := run(t, "verify-schema", "-db", "x"); code != 2 || !strings.Contains(errs, "schema.sql") {
		t.Fatalf("verify-schema: code %d\n%s", code, errs)
	}
	dir := t.TempDir()
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "apply", "-db", "x", ddlPath); code != 2 || !strings.Contains(errs, "schema.sql") {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// A schema text real PostgreSQL cannot apply (a statement naming a table that does not
// exist) fails loadTarget's Canonical call, before problems() ever runs.
func TestLoadTargetCanonicalError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"ALTER TABLE missing_table ADD COLUMN x int;"), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "apply", "-db", "unused", "-schema", schemaPath, ddlPath)
	if code != 2 || !strings.Contains(errs, schemaPath) {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// apply and verify-schema both wrap problems() the same way diff does: LISTEN is valid
// SQL (loadTarget's Canonical applies it fine) but the loader has no notion of it.
func TestApplyReportsSchemaProblems(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"LISTEN some_channel;"), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "apply", "-db", "unused", "-schema", schemaPath, ddlPath)
	if code != 2 || !strings.Contains(errs, "schema has problems:") {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

func TestVerifyReportsSchemaProblems(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"LISTEN some_channel;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "verify-schema", "-db", "unused", "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, "schema has problems:") {
		t.Fatalf("verify-schema: code %d\n%s", code, errs)
	}
}

// problems() itself surfaces a genuine parse error (not a loader Problem) when the
// schema text is not valid SQL at all.
func TestProblemsParseError(t *testing.T) {
	err := problems("CREATE TABLE (")
	if err == nil || !strings.Contains(err.Error(), "parse schema") {
		t.Fatalf("problems: %v", err)
	}
}

// A -db that refuses the connection is a plain environment error for diff, apply and
// verify-schema alike, once the schema side has already checked out.
func TestDumpLoadConnectionError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDB := "postgres://localhost:1/nonexistent-sqlshape-test-db"

	if code, _, errs := run(t, "diff", "-db", badDB, "-schema", schemaPath); code != 2 {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
	if code, _, errs := run(t, "verify-schema", "-db", badDB, "-schema", schemaPath); code != 2 {
		t.Fatalf("verify-schema: code %d\n%s", code, errs)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "apply", "-db", badDB, "-schema", schemaPath, ddlPath); code != 2 {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// diff -from a path that does not exist fails at schema.ReadSource.
func TestDiffFromReadSourceError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", "/no/such/source.sql", "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, "no such file") {
		t.Fatalf("diff -from missing: code %d\n%s", code, errs)
	}
}

// diff -from a file real PostgreSQL rejects (a duplicate CREATE TABLE) fails at
// srv.Canonical, distinctly from -db's dump.Load error.
func TestDiffFromCanonicalError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	fromPath := filepath.Join(dir, "from.sql")
	if err := os.WriteFile(fromPath, []byte("CREATE TABLE dup (id int); CREATE TABLE dup (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", fromPath, "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, fromPath) {
		t.Fatalf("diff -from bad SQL: code %d\n%s", code, errs)
	}
}

// A `-- @migrate` line diff cannot parse is reported by ParseIntents itself, distinct
// from a plan needing declarations it can parse but not reconcile.
func TestDiffParseIntentsError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	text := "CREATE TABLE t (id int PRIMARY KEY);\n-- @migrate this is not a real declaration\n"
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", schemaPath, "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, "unknown @migrate declaration") {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
}

// diff and apply both report a -packages pattern that fails to load, not just
// indexConsumers itself.
func TestDiffPackagesLoadError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", schemaPath, "-schema", schemaPath, "-packages", "./no/such/dir")
	if code != 2 || !strings.Contains(errs, "packages did not load") {
		t.Fatalf("diff -packages bad pattern: code %d\n%s", code, errs)
	}
}

func TestApplyPackagesLoadError(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := "CREATE TABLE t (id int PRIMARY KEY);"
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+base), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, "-packages", "./no/such/dir", ddlPath)
	if code != 2 || !strings.Contains(errs, "packages did not load") {
		t.Fatalf("apply -packages bad pattern: code %d\n%s", code, errs)
	}
}

// A DDL that leaves the database still differing from the target (here: a NOT NULL
// column with a default that the no-op DDL never adds) is reported before anything runs.
func TestApplyDDLDoesNotReachTarget(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := "CREATE TABLE t (id int PRIMARY KEY);"
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	target := base + "\nALTER TABLE t ADD COLUMN extra text NOT NULL DEFAULT 'z';\n"
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+target), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath)
	if code != 1 || !strings.Contains(errs, "does not reach") {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// A change that is nothing but two text columns swapping declared order is an
// OrderOnly note, not a finding: apply prints it to stderr and proceeds.
func TestApplyOrderOnlyNote(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := "CREATE TABLE t (id int PRIMARY KEY, a text, b text);"
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	target := "CREATE TABLE t (id int PRIMARY KEY, b text, a text);"
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+target), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath)
	if code != 0 || !strings.Contains(out, "applied") || !strings.Contains(errs, "note:") {
		t.Fatalf("apply: code %d\n%s%s", code, out, errs)
	}
}

// A DDL that Verify (on the clean embedded server, from a schema-only dump with no
// rows) accepts can still fail for real: a NOT NULL column added to a table that
// already holds a row without one. Inside the default transaction, that rolls back.
func TestApplyRealDBTxExecFailsAndRollsBack(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := "CREATE TABLE t (id int PRIMARY KEY);"
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	conn, err := pgx.Connect(ctx, db.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t (id) VALUES (1);"); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY, x int NOT NULL);"), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("ALTER TABLE t ADD COLUMN x int NOT NULL;"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath)
	if code != 2 || !strings.Contains(errs, "rolled back") {
		t.Fatalf("apply: code %d\n%s", code, errs)
	}
}

// The same real failure, but with -no-transaction: the DDL runs directly, and its
// error is reported without a rollback message.
func TestApplyRealDBExecFailsNoTransaction(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := "CREATE TABLE t (id int PRIMARY KEY);"
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	conn, err := pgx.Connect(ctx, db.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t (id) VALUES (1);"); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"CREATE TABLE t (id int PRIMARY KEY, x int NOT NULL);"), 0o644); err != nil {
		t.Fatal(err)
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte("ALTER TABLE t ADD COLUMN x int NOT NULL;"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, "-no-transaction", ddlPath)
	if code != 2 || !strings.Contains(errs, "apply:") {
		t.Fatalf("apply -no-transaction: code %d\n%s", code, errs)
	}
}

// diff rejects unexpected trailing arguments, same as verify-schema.
func TestDiffUnexpectedArgument(t *testing.T) {
	code, _, errs := run(t, "diff", "-db", "x", "-schema", "z.sql", "extra")
	if code != 2 || !strings.Contains(errs, `unexpected argument "extra"`) {
		t.Fatalf("diff extra arg: code %d\n%s", code, errs)
	}
}

// diff's own loadTarget error path (a schema real PostgreSQL cannot apply), distinct
// from apply's / from loadTarget failing on a missing -schema path.
func TestDiffLoadTargetCanonicalError(t *testing.T) {
	requirePgDump(t)
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(pg17Decl+"ALTER TABLE missing_table ADD COLUMN x int;"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "diff", "-from", schemaPath, "-schema", schemaPath)
	if code != 2 || !strings.Contains(errs, schemaPath) {
		t.Fatalf("diff: code %d\n%s", code, errs)
	}
}
