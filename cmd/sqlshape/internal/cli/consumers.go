package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	_ "github.com/kr9ly/sqlshape/check/mysql/v2/dialect" // the MySQL dialect: the consumer index over a MySQL schema, and the declaration that selects the MySQL path
	mydiff "github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	pgdialect "github.com/kr9ly/sqlshape/check/postgres/v2/dialect"
	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/cmd/sqlshape/v2/internal/consumers"
	"github.com/kr9ly/sqlshape/cmd/sqlshape/v2/internal/vet"
)

// indexConsumers runs the vet analyzer over the packages matching patterns and merges
// the per-package consumer indexes. The statements are read against currentSQL, the
// schema as it is before the change, with its dialect declaration in front: a column the
// target no longer has cannot be resolved against the target, and it is exactly the
// statements still naming it that matter.
func indexConsumers(patterns []string, currentSQL string) (*consumers.Index, error) {
	f, err := os.CreateTemp("", "sqlshape-current-*.sql")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(currentSQL); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := vet.Analyzer.Flags.Set("schema", f.Name()); err != nil {
		return nil, err
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadAllSyntax}, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, fmt.Errorf("packages did not load")
	}
	graph, err := checker.Analyze([]*analysis.Analyzer{vet.Analyzer}, pkgs, nil)
	if err != nil {
		return nil, err
	}
	index := consumers.New()
	for _, act := range graph.Roots {
		if act.Err != nil {
			return nil, fmt.Errorf("%s: %w", act.Package.PkgPath, act.Err)
		}
		if x, ok := act.Result.(*consumers.Index); ok && x != nil {
			index.Merge(x)
		}
	}
	return index, nil
}

// change is what a schema change means to the consumer index, whatever the dialect: the
// relation (and column) it touches, whether it removes or retypes it, and its first line
// for the report.
type change struct {
	head          string
	table, column string
	drop, retype  bool
}

// pgChanges reads PostgreSQL diff changes.
func pgChanges(changes []diff.Change) []change {
	var out []change
	for _, c := range changes {
		table, column, ok := changeTarget(c)
		if !ok {
			continue
		}
		ch := change{head: firstLine(c.String()), table: table, column: column, drop: c.Op == diff.Drop}
		if c.Op == diff.Alter && column != "" {
			for _, f := range c.Fields {
				if f.Name == "type" {
					ch.retype = true
				}
			}
		}
		out = append(out, ch)
	}
	return out
}

// mysqlChanges reads MySQL diff changes.
func mysqlChanges(changes []mydiff.Change) []change {
	var out []change
	for _, c := range changes {
		ch := change{head: firstLine(c.String()), drop: c.Op == mydiff.Drop, retype: c.Retypes()}
		switch c.Kind {
		case "table", "view":
			ch.table = c.Name
		case "column":
			i := strings.LastIndex(c.Name, ".")
			if i <= 0 {
				continue
			}
			ch.table, ch.column = c.Name[:i], c.Name[i+1:]
		default:
			continue
		}
		out = append(out, ch)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

// impact is a change and the Go sites that depend on what it removes or retypes.
type impact struct {
	change change
	sites  []consumers.Site
}

// impacts lists, for each change that drops a relation or a column or changes a column's
// type, the consumers the index knows.
func impacts(changes []change, index *consumers.Index) []impact {
	var out []impact
	for _, c := range changes {
		if !c.drop && !c.retype {
			continue
		}
		var sites []consumers.Site
		if c.column == "" {
			sites = index.Relation(c.table)
		} else {
			sites = index.Column(c.table, c.column)
		}
		if len(sites) > 0 {
			out = append(out, impact{c, sites})
		}
	}
	return out
}

// String renders the impacts as a report: the change's first line, then its sites.
func impactText(list []impact, prefix string) string {
	var b strings.Builder
	for _, im := range list {
		fmt.Fprintf(&b, "%s%s reaches %d statement(s):\n", prefix, im.change.head, len(im.sites))
		for _, s := range im.sites {
			fmt.Fprintf(&b, "%s    %s\n", prefix, s)
		}
	}
	return b.String()
}

// The consumers index runs the checker over the packages; the checker needs the
// dialects registered, which importing their adapters does (the MySQL one above).
var _ = pgdialect.Load
