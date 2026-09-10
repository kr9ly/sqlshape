package dialect

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Analyzer is the PostgreSQL analyzer behind the neutral contract: the checker's one way
// of asking about a statement, whatever the dialect.
type Analyzer struct {
	S *schema.Schema
}

// New wraps a loaded schema.
func New(s *schema.Schema) *Analyzer { return &Analyzer{S: s} }

// Analyze types one statement; a statement PostgreSQL would reject comes back as a
// *dialect.Error (its Code spelled "SQLSTATE 42703").
func (a *Analyzer) Analyze(sql string) (*dialect.Result, error) {
	r, err := analyze.Analyze(a.S, sql)
	if err != nil {
		return nil, ErrorOf(err)
	}
	return ResultOf(a.S, r), nil
}

// Problems are what the loader could not apply of the schema text.
func (a *Analyzer) Problems() []string {
	var out []string
	for _, p := range a.S.Problems {
		out = append(out, p.String())
	}
	return out
}

// Traits are pgx's.
func (a *Analyzer) Traits() dialect.Traits { return Traits }

// ErrorOf spells an analyzer error in the contract; other errors pass through.
func ErrorOf(err error) error {
	ae, ok := err.(*analyze.Error)
	if !ok {
		return err
	}
	return &dialect.Error{Message: ae.Message, Code: "SQLSTATE " + ae.Code, Position: int(ae.Position) - 1}
}

// ResultOf spells an analysis in the contract. Positions become 0-based (-1 unknown).
func ResultOf(s *schema.Schema, r *analyze.Result) *dialect.Result {
	out := &dialect.Result{Facts: r.Facts, ManyRowsWhy: r.ManyRowsWhy}
	if r.Facts != nil && !r.AtMostOne {
		out.ManyRowsWhy = r.ManyRowsWhy
		if out.ManyRowsWhy == "" {
			out.ManyRowsWhy = "the analyzer could not prove it"
		}
	}
	for i, t := range r.Params {
		var src *analyze.Source
		if i < len(r.ParamSources) {
			src = r.ParamSources[i]
		}
		out.Params = append(out.Params, ParamOf(s, t, src))
	}
	for _, c := range r.Columns {
		out.Columns = append(out.Columns, ColumnOf(s, c))
	}
	for _, n := range r.Notes {
		out.Notes = append(out.Notes, dialect.Note{Message: n.Message, Position: pos(n.Position), Advisory: n.Advisory()})
	}
	for _, v := range r.Violations {
		out.Violations = append(out.Violations, dialect.Violation{Key: v.Key(), Code: "SQLSTATE " + v.Code, Table: v.Table, Columns: v.Columns, Constraint: v.Constraint, Detail: describeViolation(v), Param: int(v.Param)})
	}
	for _, ref := range r.Relations {
		name := ref.Name
		if ref.Schema != "public" {
			name = ref.Schema + "." + name
		}
		kind := facts.Table
		switch ref.Kind {
		case 'v':
			kind = facts.View
		case 'm':
			kind = facts.MatView
		}
		out.Relations = append(out.Relations, dialect.RelationRef{Name: name, Schema: ref.Schema, Kind: kind, Position: pos(ref.Position), Target: ref.Target})
	}
	for _, u := range r.Uses {
		out.Uses = append(out.Uses, facts.Use{Table: u.Table, Column: u.Column, Position: int32(pos(u.Position))})
	}
	return out
}

// pos turns the analyzer's 1-based position (0 unknown) into the contract's 0-based (-1 unknown).
func pos(p int32) int {
	if p <= 0 {
		return -1
	}
	return int(p) - 1
}

// describeViolation spells a possible violation the way the checker reports it.
func describeViolation(v analyze.Violation) string {
	s := describeViolationAt(v)
	if v.Function != "" {
		s += ", through " + v.Function + "()"
	}
	return s
}

func describeViolationAt(v analyze.Violation) string {
	cols := strings.Join(v.Columns, ", ")
	switch v.Code {
	case "23505":
		return "UNIQUE (" + cols + ") on " + v.Table + ", SQLSTATE 23505"
	case "23503":
		return "FOREIGN KEY (" + cols + ") on " + v.Table + " REFERENCES " + v.RefTable + ", SQLSTATE 23503"
	case "23514":
		return "CHECK on " + v.Table + " (" + cols + "), SQLSTATE 23514"
	case "23502":
		return "NOT NULL on " + v.Table + "." + cols + ", SQLSTATE 23502"
	case "23P01":
		return "EXCLUDE (" + cols + ") on " + v.Table + ", SQLSTATE 23P01"
	}
	if v.Trigger != "" {
		s := "raised by trigger " + v.Trigger + " on " + v.Table
		if v.Name != "" {
			s += " as " + v.Name
		}
		return s + ", SQLSTATE " + v.Code
	}
	if v.Name != "" {
		return "raised as " + v.Name + ", SQLSTATE " + v.Code
	}
	return "SQLSTATE " + v.Code
}
