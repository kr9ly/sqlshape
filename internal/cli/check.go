package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/obligation"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// runCheck judges SQL statements outside Go code -- an operator's UPDATE, a backfill, an
// agent's query -- against schema.sql and the obligations it declares. Every judgment is
// printed (the audit: which statement discharged which obligation by which path, and what
// it waived); a statement that fails one, or does not analyze, is a finding (exit 1).
func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sqlshape check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	getSchema := schemaFlag(fs)
	quiet := fs.Bool("quiet", false, "print failures only, not every judgment")
	requireCols := fs.String("require-columns", "", "as vet's flag: pin these columns on every table that has them")
	noTables := fs.Bool("no-tables", false, "as vet's flag: no direct table reference")
	noTableReads := fs.Bool("no-table-reads", false, "as vet's flag: tables are written, not read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	schemaPath, err := getSchema()
	if err != nil {
		return err
	}
	text, err := schema.ReadSource(schemaPath)
	if err != nil {
		return err
	}
	s, err := analyze.Load(text)
	if err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	if err := problems(text); err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	decls, oblProblems := obligation.Declarations(s)
	if len(oblProblems) > 0 {
		var lines []string
		for _, p := range oblProblems {
			lines = append(lines, fmt.Sprintf("%s: directive %q: %s", p.Subject, p.Source, p.Message))
		}
		return fmt.Errorf("%s: %s", schemaPath, strings.Join(lines, "\n  "))
	}
	decls = append(decls, obligation.FromFlags(s, *requireCols, *noTables, *noTableReads)...)

	inputs := fs.Args()
	if len(inputs) == 0 {
		inputs = []string{"-"}
	}
	failures := 0
	for _, in := range inputs {
		var sql []byte
		name := in
		if in == "-" {
			sql, err = io.ReadAll(os.Stdin)
			name = "stdin"
		} else {
			sql, err = os.ReadFile(in)
		}
		if err != nil {
			return err
		}
		n, err := checkText(s, decls, name, string(sql), *quiet, stdout)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		failures += n
	}
	if failures > 0 {
		return found("%d finding(s)", failures)
	}
	return nil
}

// checkText judges each statement of text and returns how many failed.
func checkText(s *schema.Schema, decls []obligation.Obligation, name, text string, quiet bool, w io.Writer) (int, error) {
	tree, err := pg_query.Parse(text)
	if err != nil {
		return 0, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "syntax error "))
	}
	failures := 0
	prevEnd := 0
	for _, raw := range tree.Stmts {
		end := int(raw.StmtLocation) + int(raw.StmtLen)
		if raw.StmtLen == 0 {
			end = len(text)
		}
		// the chunk runs from the previous statement's end, so the `-- sqlshape:` lines
		// written above this statement belong to it
		chunk := strings.TrimRight(text[prevEnd:end], "; \t\r\n")
		chunkStart := prevEnd
		prevEnd = end + 1
		// the statement's own line: past the blank and comment lines above it
		first := 0
		for _, l := range strings.SplitAfter(chunk, "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "--") {
				break
			}
			first += len(l)
		}
		line := 1 + strings.Count(text[:chunkStart+first], "\n")
		lineAt := func(pos int32) int {
			if pos < 0 || chunkStart+int(pos) > len(text) {
				return line
			}
			return 1 + strings.Count(text[:chunkStart+int(pos)], "\n")
		}
		r, err := analyze.Analyze(s, chunk)
		if err != nil {
			failures++
			fmt.Fprintf(w, "%s:%d: FAIL %v\n", name, line, err)
			continue
		}
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			at := lineAt(d.Position)
			what := d.Leaf.Table + ": " + d.Obligation.Source
			switch {
			case d.Failed():
				failures++
				fmt.Fprintf(w, "%s:%d: FAIL %s: %s\n", name, at, what, d.Message)
			case quiet:
			case d.Message != "":
				fmt.Fprintf(w, "%s:%d: %s %s: %s\n", name, at, pathName(d.Path), what, d.Message)
			default:
				fmt.Fprintf(w, "%s:%d: %s %s\n", name, at, pathName(d.Path), what)
			}
		}
	}
	return failures, nil
}

func pathName(p obligation.Path) string {
	switch p {
	case obligation.ByStatement:
		return "ok"
	case obligation.ByView:
		return "ok(view)"
	case obligation.ByPolicy:
		return "ok(policy)"
	case obligation.ByForeignKey:
		return "ok(fk)"
	case obligation.Waived:
		return "waived"
	}
	return "FAIL"
}

// lowerer is internal/obligation's view of the PostgreSQL analyzer.
type lowerer struct{ s *schema.Schema }

func (l lowerer) Lower(expr string, rel *schema.Relation) ([]facts.Pred, error) {
	return analyze.Lower(l.s, expr, rel)
}
