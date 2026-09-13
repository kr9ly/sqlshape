package cli

// docs_test.go checks docs/checks.md's (and checks.ja.md's) "The same rules for SQL
// outside Go (`sqlshape check`)" section against the real `check` subcommand, the way
// cmd/sqlshape/internal/vet's own docs_part3_test.go checks the vet analyzer against the
// rest of Part 2/3. It does not edit cli_test.go; it reuses that file's own run() helper
// and pg17Decl constant (same package, unexported, already in scope).
//
// The section's own fence is a `$ sqlshape check ...` transcript with no language tag
// (```, not ```go or ```sql), so it is not one of the fences
// cmd/sqlshape/internal/vet/docs_test.go's parseDocBlocks recognizes (go/sql only); this
// file parses it directly with its own minimal helper instead. The transcript is also
// illustrative rather than exhaustive -- it shows three of the run's output lines
// (":2 ok", ":6 waived", ":8 FAIL") and "2 finding(s)", but a real run over the schema and
// script it implies (the same shape check_test.go's own TestCheck already builds) reports
// more lines than that (a second FAIL at :8, for instance) for the same two-finding count
// docs quotes. So each quoted line is checked with Contains against the real output,
// not with a full-output equality -- what the doc quotes must appear verbatim, but the
// doc is not asserted to be the complete transcript.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var checkTranscriptFenceRE = regexp.MustCompile("(?s)```\\n(\\$ sqlshape check.*?)\\n```")

// docsCheckTranscript reads the untagged ``` fence directly under heading "The same rules
// for SQL outside Go (`sqlshape check`)" out of a docs/*.md file.
func docsCheckTranscript(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := checkTranscriptFenceRE.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("%s: no untagged ```$ sqlshape check ...``` fence found", path)
	}
	return m[1]
}

func TestDocsCheckSubcommandTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"en", "checks.md"},
		{"ja", "checks.ja.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transcript := docsCheckTranscript(t, filepath.Join("..", "..", "..", "..", "docs", tc.doc))
			lines := strings.Split(transcript, "\n")
			if len(lines) < 2 || !strings.HasPrefix(lines[0], "$ sqlshape check ") {
				t.Fatalf("unexpected transcript shape:\n%s", transcript)
			}

			// The schema this transcript implies: an orders table visible where
			// deleted_at IS NULL, pinned by tenant_id, with a context "ops" that
			// waives the pin -- the same declarations the surrounding prose
			// (immediately above this heading, "Different callers, different rules")
			// and the ":2 ok ... pinned(tenant_id)" / ":6 waived ... pinned(tenant_id)"
			// / ":8 FAIL ... visible where" lines require together. Docs does not
			// print the schema itself here, so this harness completes it, the same
			// way cmd/sqlshape/internal/vet/docs_part3_test.go completes schema
			// fragments elsewhere in Part 2.
			dir := t.TempDir()
			schema := `
-- sqlshape: visible where deleted_at IS NULL
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, deleted_at timestamptz);
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
`
			sqlPath := filepath.Join(dir, "ops.sql")
			if err := os.WriteFile(sqlPath, []byte(ops), 0o644); err != nil {
				t.Fatal(err)
			}

			code, out, errs := run(t, "check", "-schema", filepath.Join(dir, "schema.sql"), sqlPath)
			if code != 1 {
				t.Fatalf("exit %d, want 1; stderr: %s", code, errs)
			}

			for _, l := range lines[1:] {
				l = strings.TrimSpace(l)
				if l == "" {
					continue
				}
				if strings.HasPrefix(l, "sqlshape:") {
					// "sqlshape: 2 finding(s)" goes to stderr, not stdout.
					if !strings.Contains(errs, strings.TrimPrefix(l, "sqlshape: ")) {
						t.Errorf("stderr missing %q; got: %s", l, errs)
					}
					continue
				}
				// The transcript's own path is "ops.sql"; the real run's is this
				// test's own temp-dir path for the same file. Docs itself trails
				// the long FAIL line off with a literal "..."; treat that one as a
				// prefix rather than a full line.
				want := strings.Replace(l, "ops.sql:", sqlPath+":", 1)
				if trimmed, ok := strings.CutSuffix(want, "..."); ok {
					if !strings.Contains(out, trimmed) {
						t.Errorf("stdout missing a line starting with %q; got:\n%s", trimmed, out)
					}
					continue
				}
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}
