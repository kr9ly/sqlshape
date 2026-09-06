// Package cli is the sqlshape command: `vet` (the go/analysis checker, also what a bare
// invocation runs so `go vet -vettool` keeps working), `diff` (the DDL from a database or
// schema text to schema.sql), `apply` (a DDL file, checked by its end state and by the
// consumers of what it drops, then run), and `verify-schema` (drift between a database
// and schema.sql).
//
// Every comparison is between canonical forms (dump.Canonical / dump.Load): the live side
// is read back through pg_dump, the target is applied to an embedded PostgreSQL and read
// back the same way, so the two compare object by object in PostgreSQL's own spelling.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/dump"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Subcommands lists the names Main handles; anything else is the checker's business.
var Subcommands = []string{"vet", "diff", "apply", "verify-schema", "help"}

// IsSubcommand reports whether name is one of Subcommands.
func IsSubcommand(name string) bool {
	for _, s := range Subcommands {
		if s == name {
			return true
		}
	}
	return false
}

// Run executes subcommand name with args, writing results to stdout and problems to
// stderr. The exit code is 0 for success, 1 for a finding (a difference, a refused
// apply), 2 for a usage or environment error.
func Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	var err error
	switch name {
	case "diff":
		err = runDiff(ctx, args, stdout, stderr)
	case "apply":
		err = runApply(ctx, args, stdout, stderr)
	case "verify-schema":
		err = runVerify(ctx, args, stdout, stderr)
	case "help", "":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "sqlshape: unknown command %q\n%s", name, usage)
		return 2
	}
	var f *finding
	switch {
	case err == nil:
		return 0
	case errors.As(err, &f):
		fmt.Fprintln(stderr, "sqlshape: "+f.msg)
		return 1
	case errors.Is(err, flag.ErrHelp):
		return 2
	default:
		fmt.Fprintln(stderr, "sqlshape: "+err.Error())
		return 2
	}
}

const usage = `usage: sqlshape <command> [flags] [arguments]

  vet [flags] packages...        check the Go packages (also what a bare 'sqlshape' runs)
  diff [-db DSN | -from FILE] [-schema PATH] [-packages P...]
                                 print the DDL that takes the database (or FILE) to schema.sql
  apply -db DSN [-schema PATH] [-packages P...] [-dry-run] [-force] [-no-transaction] DDL.sql
                                 verify the DDL reaches schema.sql from the database's state, then run it
  verify-schema -db DSN [-schema PATH]
                                 list where the database differs from schema.sql (drift)

-schema defaults to schema.sql, or a schema/ directory of *.sql files, in the working
directory or above. -packages names Go packages (go list patterns) whose statements are
indexed as consumers of the columns a change drops or retypes. pg_dump is needed on PATH
(or $SQLSHAPE_PG_DUMP); its major version must be at least the server's.
`

// finding is a result the command reports rather than a failure to run: exit code 1.
type finding struct{ msg string }

func (f *finding) Error() string { return f.msg }

func found(format string, args ...any) error { return &finding{fmt.Sprintf(format, args...)} }

// schemaFlag adds -schema to fs and returns a getter that resolves the path.
func schemaFlag(fs *flag.FlagSet) func() (string, error) {
	var path string
	fs.StringVar(&path, "schema", "", "schema.sql, or a directory whose *.sql files apply in name order (default: the nearest schema.sql or schema/ from the working directory up)")
	return func() (string, error) {
		if path != "" {
			return path, nil
		}
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		for {
			for _, name := range []string{"schema.sql", "schema"} {
				p := filepath.Join(dir, name)
				if fi, err := os.Stat(p); err == nil && (fi.IsDir() == (name == "schema")) {
					return p, nil
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return "", errors.New("schema.sql (or a schema/ directory) not found in the working directory or above (use -schema)")
			}
			dir = parent
		}
	}
}

// target is the declared schema: its text, canonical form, and the parsed intents.
type target struct {
	path      string
	text      string
	canonical *schema.Schema
}

// loadTarget reads the schema text at path and canonicalizes it on srv.
func loadTarget(ctx context.Context, srv *dump.Server, path string) (*target, error) {
	text, err := schema.ReadSource(path)
	if err != nil {
		return nil, err
	}
	canon, _, err := srv.Canonical(ctx, text, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &target{path: path, text: text, canonical: canon}, nil
}

// problems lists what the loader could not apply of a schema text (the target must be
// clean: a statement the loader skipped would be missing from the canonical form too).
func problems(text string) error {
	s, err := schema.Load(text)
	if err != nil {
		return err
	}
	if len(s.Problems) == 0 {
		return nil
	}
	var lines []string
	for _, p := range s.Problems {
		lines = append(lines, p.String())
	}
	return fmt.Errorf("schema has problems:\n  %s", strings.Join(lines, "\n  "))
}

// printChanges writes changes one per line, indented.
func printChanges(w io.Writer, changes []diff.Change) {
	for _, c := range changes {
		fmt.Fprintln(w, "  "+strings.ReplaceAll(c.String(), "\n", "\n  "))
	}
}

// changeTarget names what a change touches for the consumer index: the relation and,
// for a column, its name.
func changeTarget(c diff.Change) (table, column string, ok bool) {
	switch c.Kind {
	case "table", "view", "matview":
		return c.Name, "", true
	case "column":
		if i := strings.LastIndex(c.Name, "."); i > 0 {
			return c.Name[:i], c.Name[i+1:], true
		}
	}
	return "", "", false
}
