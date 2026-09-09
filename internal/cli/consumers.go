package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/kr9ly/sqlshape/internal/consumers"
	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/pgparse"
	"github.com/kr9ly/sqlshape/internal/vet"
)

// indexConsumers runs the vet analyzer over the packages matching patterns and merges
// the per-package consumer indexes. The statements are read against currentSQL, the
// schema as it is before the change: a column the target no longer has cannot be resolved
// against the target, and it is exactly the statements still naming it that matter.
func indexConsumers(patterns []string, v pgparse.Version, currentSQL string) (*consumers.Index, error) {
	f, err := os.CreateTemp("", "sqlshape-current-*.sql")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	// the canonical text comes from pg_dump, which writes no declaration: the packages are
	// judged against the version the target schema declares
	if _, err := fmt.Fprintf(f, "-- sqlshape: postgres %d\n%s", int(v.Or()), currentSQL); err != nil {
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

// impact is a change and the Go sites that depend on what it removes or retypes.
type impact struct {
	change diff.Change
	sites  []consumers.Site
}

// impacts lists, for each change that drops a relation or a column or changes a column's
// type, the consumers the index knows.
func impacts(changes []diff.Change, index *consumers.Index) []impact {
	var out []impact
	for _, c := range changes {
		table, column, ok := changeTarget(c)
		if !ok {
			continue
		}
		reaches := c.Op == diff.Drop
		if c.Op == diff.Alter && column != "" {
			for _, f := range c.Fields {
				if f.Name == "type" {
					reaches = true
				}
			}
		}
		if !reaches {
			continue
		}
		var sites []consumers.Site
		if column == "" {
			sites = index.Relation(table)
		} else {
			sites = index.Column(table, column)
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
		head := im.change.String()
		if i := strings.Index(head, "\n"); i > 0 {
			head = head[:i]
		}
		fmt.Fprintf(&b, "%s%s reaches %d statement(s):\n", prefix, head, len(im.sites))
		for _, s := range im.sites {
			fmt.Fprintf(&b, "%s    %s\n", prefix, s)
		}
	}
	return b.String()
}
