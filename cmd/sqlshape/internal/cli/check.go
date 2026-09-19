package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
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
	ctxName := fs.String("context", "", "the obligation context to judge under (schema.sql's `context <name>: ...` declarations)")
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
	decls, oblProblems := obligation.Declarations(s.Contract())
	if len(oblProblems) > 0 {
		var lines []string
		for _, p := range oblProblems {
			lines = append(lines, fmt.Sprintf("%s: directive %q: %s", p.Subject, p.Source, p.Message))
		}
		return fmt.Errorf("%s: %s", schemaPath, strings.Join(lines, "\n  "))
	}
	if *ctxName != "" && !slices.Contains(obligation.Contexts(decls), *ctxName) {
		return fmt.Errorf("%s: context %q is not declared (declared: %s)", schemaPath, *ctxName, strings.Join(obligation.Contexts(decls), ", "))
	}
	// the flags are shorthand for declarations: a context's waive lifts them the same way
	decls = obligation.InContext(append(decls, obligation.FromFlags(s.Contract(), *requireCols, *noTables, *noTableReads)...), *ctxName)

	inputs := fs.Args()
	if len(inputs) == 0 {
		inputs = []string{"-"}
	}
	// the schema's own statements first: a function body (a trigger's included) or a view
	// body that breaks an obligation is a finding wherever the schema is judged
	failures := checkSchemaBodies(s, decls, schemaPath, *quiet, stdout)
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

// checkSchemaBodies judges the schema's function and view bodies (what vet reports as
// schema problems) and returns how many failed.
func checkSchemaBodies(s *schema.Schema, decls []obligation.Obligation, name string, quiet bool, w io.Writer) int {
	failures := 0
	judge := func(what string, f *facts.Facts) {
		for _, d := range obligation.Check(s.Contract(), decls, f, lowerer{s}) {
			if d.Failed() {
				failures++
				fmt.Fprintf(w, "%s: %s: FAIL %s: %s: %s\n", name, what, d.Leaf.Table, d.Obligation.Source, d.Message)
			}
		}
	}
	for _, fn := range s.Functions {
		fr, err := analyze.AnalyzeFunction(s, fn)
		if err != nil {
			continue // the body's own errors are schema problems, reported by vet
		}
		for _, st := range fr.Statements {
			what := "function " + fn.Name
			if st.Line > 0 {
				what = fmt.Sprintf("%s line %d", what, st.Line)
			}
			judge(what, st.Facts)
		}
	}
	for _, rel := range s.Relations {
		if rel.Kind != schema.View && rel.Kind != schema.MatView {
			continue
		}
		if r, err := analyze.AnalyzeView(s, rel); err == nil {
			judge("view "+rel.Name, r.Facts)
		}
	}
	return failures
}

// statementSpans splits text into [start, end) byte spans, one per statement, each span
// starting right after the previous statement's semicolon (so the `-- sqlshape:` lines
// written above a statement belong to it). A text the parser rejects as a whole is split
// by the scanner instead, so that one bad statement leaves the others judged.
func statementSpans(v pgparse.Version, text string) ([][2]int, error) {
	var spans [][2]int
	prevEnd := 0
	if tree, err := v.Parse(text); err == nil {
		for _, raw := range tree.Stmts {
			end := int(raw.StmtLocation) + int(raw.StmtLen)
			if raw.StmtLen == 0 {
				end = len(text)
			}
			spans = append(spans, [2]int{prevEnd, end})
			prevEnd = end + 1
		}
		return spans, nil
	}
	parts, err := v.SplitWithScanner(text, false)
	if err != nil {
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "syntax error "))
	}
	for _, part := range parts {
		i := strings.Index(text[prevEnd:], part)
		if i < 0 {
			break
		}
		end := prevEnd + i + len(part)
		spans = append(spans, [2]int{prevEnd, end})
		prevEnd = end + 1
	}
	return spans, nil
}

// checkText judges each statement of text and returns how many failed.
func checkText(s *schema.Schema, decls []obligation.Obligation, name, text string, quiet bool, w io.Writer) (int, error) {
	text = strings.TrimPrefix(text, "\ufeff") // a byte order mark is not part of the SQL
	spans, err := statementSpans(s.Version, text)
	if err != nil {
		return 0, err
	}
	failures := 0
	for _, span := range spans {
		chunk := strings.TrimRight(text[span[0]:span[1]], "; \t\r\n")
		chunkStart := span[0]
		if strings.TrimSpace(chunk) == "" {
			continue
		}
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
		for _, d := range obligation.Check(s.Contract(), decls, r.Facts, lowerer{s}) {
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

func (l lowerer) Lower(expr string, rel obligation.Relation) ([]facts.Pred, error) {
	return analyze.Lower(l.s, expr, l.s.ByFullName(rel.FullName()))
}
