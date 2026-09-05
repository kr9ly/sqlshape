// Package verify compares the analyzer's conclusion about a statement with a running
// PostgreSQL's: parameter types, result column names and types, and whether the
// statement is rejected (by SQLSTATE class). It is what pgtest.Verify runs for every
// expansion of an application's templates, and the same comparison the analyzer's golden
// tests make (those cannot import this package: it would cycle through analyze).
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/expand"
	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Mismatch is one expansion the analyzer and PostgreSQL disagree about.
type Mismatch struct {
	Template string // the template text
	Branch   string // which branches were taken ("" for a template without branches)
	SQL      string // the expanded SQL
	Oracle   string // PG's description (or "error: ...")
	Analyzer string // the analyzer's description (or "error: ...")
}

func (m *Mismatch) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sqlshape: the analyzer disagrees with PostgreSQL")
	if m.Branch != "" {
		fmt.Fprintf(&b, " [%s]", m.Branch)
	}
	fmt.Fprintf(&b, "\n--- sql\n%s\n--- postgres\n%s--- analyzer\n%s", strings.TrimSpace(m.SQL), m.Oracle, m.Analyzer)
	return b.String()
}

// Template expands tmpl and compares every expansion. It returns one Mismatch per
// disagreeing expansion (nil when all agree) and an error when the template itself
// cannot be expanded.
func Template(ctx context.Context, o *oracle.Oracle, s *schema.Schema, tmpl string) ([]*Mismatch, error) {
	res, err := expand.Expand(tmpl)
	if err != nil {
		return nil, err
	}
	var out []*Mismatch
	for _, e := range res.Expansions {
		want := RenderOracle(ctx, o, e.SQL)
		got := RenderAnalyzer(s, e.SQL)
		if !Match(want, got) {
			out = append(out, &Mismatch{Template: tmpl, Branch: e.Branch, SQL: e.SQL, Oracle: want, Analyzer: got})
		}
	}
	return out, nil
}

// Match compares the two renderings. Error lines only need the same SQLSTATE class prefix
// (first 5 chars of "error: 42XXX") to count as agreeing; messages are informative.
func Match(want, got string) bool {
	if strings.HasPrefix(want, "error:") && strings.HasPrefix(got, "error:") {
		return strings.HasPrefix(got, want[:len("error: 42XXX")])
	}
	return want == got
}

// RenderOracle describes sql through PostgreSQL in the comparison format.
func RenderOracle(ctx context.Context, o *oracle.Oracle, sql string) string {
	d, err := o.Describe(ctx, sql)
	if err != nil {
		var pgErr *oracle.PgError
		if errors.As(err, &pgErr) {
			return "error: " + pgErr.Error() + "\n"
		}
		return "internal error: " + err.Error() + "\n"
	}
	return d.String()
}

// RenderAnalyzer describes sql through the analyzer in the comparison format.
func RenderAnalyzer(s *schema.Schema, sql string) string {
	r, err := analyze.Analyze(s, sql)
	if err != nil {
		var aerr *analyze.Error
		if errors.As(err, &aerr) {
			return "error: " + aerr.Error() + "\n"
		}
		return "internal error: " + err.Error() + "\n"
	}
	return r.String(s.Types)
}
