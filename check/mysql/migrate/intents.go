package migrate

import (
	"fmt"
	"regexp"
	"strings"
)

// Intent is one `-- @migrate` declaration in the schema source: what a diff of two
// schemas cannot decide on its own. Declarations describe the step from the database's
// current state to schema.sql, and go away once applied: a declaration the diff no
// longer bears out is an error, so stale ones cannot linger. The grammar is the
// PostgreSQL one's; `enum` names a column, since MySQL's ENUM is a column type.
//
//	-- @migrate rename orders.state -> orders.status     column (or table: rename old -> new);
//	                                                     the left side names things as they are
//	                                                     now, the right as they will be
//	-- @migrate drop orders.legacy                        a column or table may go (data loss accepted)
//	-- @migrate enum orders.status: drop 'canceled' using 'cancelled'
//	-- @migrate backfill orders.status = 'pending' where status is null
type Intent struct {
	Kind IntentKind
	// Rename / Drop: dotted names (table.column or table).
	From, To string
	// EnumDrop: the ENUM column, the label removed and the label its values become.
	Label, Using string
	// EnumDrop / Backfill: the column; Backfill: the SQL expression and the optional WHERE.
	Table, Column, Expr, Where string
	Line                       int
}

// IntentKind is the kind of declaration.
type IntentKind int

const (
	Rename IntentKind = iota + 1
	Drop
	EnumDrop
	Backfill
)

var (
	intentLine   = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*@migrate[ \t]+(.+?)[ \t]*$`)
	renameRe     = regexp.MustCompile(`^rename\s+(\S+)\s*->\s*(\S+)$`)
	dropRe       = regexp.MustCompile(`^drop\s+(\S+)$`)
	enumDropRe   = regexp.MustCompile(`^enum\s+(\S+)\s*:\s*drop\s+'((?:[^']|'')*)'\s+using\s+'((?:[^']|'')*)'$`)
	backfillRe   = regexp.MustCompile(`^backfill\s+(\S+)\s*=\s*(.+)$`)
	backfillWhen = regexp.MustCompile(`(?i)\s+where\s+`)
)

// ParseIntents reads the `-- @migrate` lines of schema source text.
func ParseIntents(schemaSQL string) ([]Intent, error) {
	var out []Intent
	var errs []string
	for _, m := range intentLine.FindAllStringSubmatchIndex(schemaSQL, -1) {
		text := schemaSQL[m[2]:m[3]]
		line := 1 + strings.Count(schemaSQL[:m[0]], "\n")
		in := Intent{Line: line}
		switch {
		case renameRe.MatchString(text):
			g := renameRe.FindStringSubmatch(text)
			in.Kind, in.From, in.To = Rename, g[1], g[2]
		case dropRe.MatchString(text):
			in.Kind, in.From = Drop, dropRe.FindStringSubmatch(text)[1]
		case enumDropRe.MatchString(text):
			g := enumDropRe.FindStringSubmatch(text)
			t, c, ok := splitColumn(g[1])
			if !ok {
				errs = append(errs, fmt.Sprintf("line %d: enum needs table.column, got %q", line, g[1]))
				continue
			}
			in.Kind, in.Table, in.Column = EnumDrop, t, c
			in.Label, in.Using = strings.ReplaceAll(g[2], "''", "'"), strings.ReplaceAll(g[3], "''", "'")
		case backfillRe.MatchString(text):
			g := backfillRe.FindStringSubmatch(text)
			t, c, ok := splitColumn(g[1])
			if !ok {
				errs = append(errs, fmt.Sprintf("line %d: backfill needs table.column, got %q", line, g[1]))
				continue
			}
			in.Kind, in.Table, in.Column, in.Expr = Backfill, t, c, strings.TrimSpace(g[2])
			if w := backfillWhen.FindStringIndex(in.Expr); w != nil {
				in.Where = strings.TrimSpace(in.Expr[w[1]:])
				in.Expr = strings.TrimSpace(in.Expr[:w[0]])
			}
		default:
			errs = append(errs, fmt.Sprintf("line %d: unknown @migrate declaration %q", line, text))
			continue
		}
		out = append(out, in)
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return out, nil
}

func splitColumn(dotted string) (table, column string, ok bool) {
	i := strings.LastIndex(dotted, ".")
	if i <= 0 || i == len(dotted)-1 {
		return "", "", false
	}
	return dotted[:i], dotted[i+1:], true
}

// String renders the declaration as it is written.
func (in Intent) String() string {
	switch in.Kind {
	case Rename:
		return "rename " + in.From + " -> " + in.To
	case Drop:
		return "drop " + in.From
	case EnumDrop:
		return "enum " + in.Table + "." + in.Column + ": drop " + lit(in.Label) + " using " + lit(in.Using)
	case Backfill:
		s := "backfill " + in.Table + "." + in.Column + " = " + in.Expr
		if in.Where != "" {
			s += " where " + in.Where
		}
		return s
	}
	return ""
}

// readIntents checks each declaration against the two schemas and records what the plan
// does with it.
func (p *planner) readIntents(list []Intent) {
	p.tableRename = map[string]string{}
	p.colRename = map[string]map[string]string{}
	p.droppable = map[string]bool{}
	for _, in := range list {
		switch in.Kind {
		case Rename:
			ft, fc, fcol := splitColumn(in.From)
			tt, tc, tcol := splitColumn(in.To)
			if fcol != tcol {
				p.problem("line %d: rename %s -> %s: both sides must be tables, or both columns", in.Line, in.From, in.To)
				continue
			}
			if !fcol {
				if p.from.Table(in.From) == nil {
					p.problem("line %d: rename %s -> %s: %s is not in the current schema", in.Line, in.From, in.To, in.From)
					continue
				}
				if p.to.Table(in.To) == nil {
					p.problem("line %d: rename %s -> %s: %s is not in the target schema", in.Line, in.From, in.To, in.To)
					continue
				}
				if p.from.Table(in.To) != nil || p.to.Table(in.From) != nil {
					p.problem("line %d: rename %s -> %s: both names exist on both sides", in.Line, in.From, in.To)
					continue
				}
				p.tableRename[in.From] = in.To
				continue
			}
			if p.toName(ft) != tt && ft != tt {
				p.problem("line %d: rename %s -> %s: a column rename stays within its table (rename the table separately)", in.Line, in.From, in.To)
				continue
			}
			f := p.from.Table(ft)
			t := p.to.Table(tt)
			if f == nil || f.Column(fc) == nil {
				p.problem("line %d: rename %s -> %s: %s is not in the current schema", in.Line, in.From, in.To, in.From)
				continue
			}
			if t == nil || t.Column(tc) == nil {
				p.problem("line %d: rename %s -> %s: %s is not in the target schema", in.Line, in.From, in.To, in.To)
				continue
			}
			if f.Column(tc) != nil || t.Column(fc) != nil {
				p.problem("line %d: rename %s -> %s: both names exist on both sides", in.Line, in.From, in.To)
				continue
			}
			if p.colRename[ft] == nil {
				p.colRename[ft] = map[string]string{}
			}
			p.colRename[ft][fc] = tc
		case Drop:
			t, c, isCol := splitColumn(in.From)
			if !isCol {
				if p.from.Table(in.From) == nil {
					p.problem("line %d: drop %s: not in the current schema", in.Line, in.From)
					continue
				}
				if p.to.Table(in.From) != nil {
					p.problem("line %d: drop %s: %s still exists in the target schema", in.Line, in.From, in.From)
					continue
				}
				p.droppable[in.From] = true
				continue
			}
			f := p.from.Table(t)
			if f == nil || f.Column(c) == nil {
				p.problem("line %d: drop %s: not in the current schema", in.Line, in.From)
				continue
			}
			if tt := p.to.Table(p.toName(t)); tt != nil && tt.Column(c) != nil {
				p.problem("line %d: drop %s: %s still exists in the target schema", in.Line, in.From, in.From)
				continue
			}
			p.droppable[t+"."+c] = true
		case EnumDrop:
			t := p.to.Table(in.Table)
			if t == nil || t.Column(in.Column) == nil {
				p.problem("line %d: enum %s.%s: not in the target schema", in.Line, in.Table, in.Column)
				continue
			}
			if t.Column(in.Column).Type.Name != "enum" {
				p.problem("line %d: enum %s.%s: not an ENUM column", in.Line, in.Table, in.Column)
				continue
			}
			f := p.from.Table(p.fromName(in.Table))
			var fc *schemaColumn
			if f != nil {
				if c := f.Column(p.fromCol(f.Name, in.Column)); c != nil {
					fc = &schemaColumn{c.Type.Values}
				}
			}
			if fc == nil || !contains(fc.values, in.Label) {
				p.problem("line %d: enum %s.%s: %s is not a label of the current column", in.Line, in.Table, in.Column, lit(in.Label))
				continue
			}
			if contains(t.Column(in.Column).Type.Values, in.Label) {
				p.problem("line %d: enum %s.%s: %s is still a label in the target schema", in.Line, in.Table, in.Column, lit(in.Label))
				continue
			}
			p.enumDrops = append(p.enumDrops, in)
		case Backfill:
			p.backfillsOf = append(p.backfillsOf, in)
		}
	}
}

type schemaColumn struct{ values []string }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
