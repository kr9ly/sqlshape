package migrate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Intent is one `-- @migrate` declaration in the schema source: what a diff of two
// schemas cannot decide on its own. Declarations describe the step from the database's
// current state to schema.sql, and go away once applied: a declaration the diff no
// longer bears out is an error, so stale ones cannot linger.
//
//	-- @migrate rename orders.state -> orders.status     column (or table: rename old -> new);
//	                                                     the left side names things as they are
//	                                                     now, the right as they will be
//	-- @migrate drop orders.legacy                        a column or table may go (data loss accepted)
//	-- @migrate enum order_status: drop 'canceled' using 'cancelled'
//	-- @migrate backfill orders.status = 'pending' where status is null
type Intent struct {
	Kind IntentKind
	// Rename / Drop: dotted names (schema.table.column, table.column or table; two parts
	// are a table in a non-public schema when one exists, else a public table's column).
	From, To string
	// EnumDrop: the enum type, the label removed and the label its values become.
	Enum, Label, Using string
	// Backfill: the column assigned, the SQL expression and the optional WHERE predicate.
	Table, Column, Expr, Where string
	Line                       int
}

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
			in.Kind, in.Enum, in.Label, in.Using = EnumDrop, g[1], strings.ReplaceAll(g[2], "''", "'"), strings.ReplaceAll(g[3], "''", "'")
		case backfillRe.MatchString(text):
			g := backfillRe.FindStringSubmatch(text)
			in.Kind = Backfill
			i := strings.LastIndex(g[1], ".")
			if i < 0 {
				errs = append(errs, fmt.Sprintf("line %d: backfill needs table.column, got %q", line, g[1]))
				continue
			}
			in.Table, in.Column, in.Expr = g[1][:i], g[1][i+1:], strings.TrimSpace(g[2])
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

// String renders the declaration as it is written.
func (in Intent) String() string {
	switch in.Kind {
	case Rename:
		return "rename " + in.From + " -> " + in.To
	case Drop:
		return "drop " + in.From
	case EnumDrop:
		return "enum " + in.Enum + ": drop " + lit(in.Label) + " using " + lit(in.Using)
	case Backfill:
		s := "backfill " + in.Table + "." + in.Column + " = " + in.Expr
		if in.Where != "" {
			s += " where " + in.Where
		}
		return s
	}
	return "?"
}

// resolveName finds the relation (and column) a dotted name means in s. Two parts are
// schema.table when that relation exists, else public table.column.
func resolveName(s *schema.Schema, dotted string) (rel *schema.Relation, column string) {
	parts := strings.Split(dotted, ".")
	switch len(parts) {
	case 1:
		return s.Relation("public", parts[0]), ""
	case 2:
		if r := s.Relation(parts[0], parts[1]); r != nil {
			return r, ""
		}
		if r := s.Relation("public", parts[0]); r != nil && r.Column(parts[1]) != nil {
			return r, parts[1]
		}
	case 3:
		if r := s.Relation(parts[0], parts[1]); r != nil && r.Column(parts[2]) != nil {
			return r, parts[2]
		}
	}
	return nil, ""
}

// intents is the planner's reading of the declarations against from and to.
type intents struct {
	// relTo: from relation → to relation full names of renamed relations; relFrom the reverse
	relTo, relFrom map[string]string
	// colTo[from rel][from col] = to col; colFrom[to rel][to col] = from col
	colTo, colFrom map[string]map[string]string
	// dropOK: from relations / "rel.col" a drop declaration allows to go
	dropOK map[string]bool
	// enumUsing[enum name][label] = label it becomes
	enumUsing map[string]map[string]string
	// backfills by to relation full name, in declaration order
	backfills map[string][]Intent
	problems  []string
}

func (p *planner) readIntents(list []Intent) {
	in := &intents{
		relTo: map[string]string{}, relFrom: map[string]string{},
		colTo: map[string]map[string]string{}, colFrom: map[string]map[string]string{},
		dropOK: map[string]bool{}, enumUsing: map[string]map[string]string{}, backfills: map[string][]Intent{},
	}
	p.in = in
	bad := func(i Intent, format string, args ...any) {
		in.problems = append(in.problems, fmt.Sprintf("@migrate %s (line %d): %s", i, i.Line, fmt.Sprintf(format, args...)))
	}
	for _, i := range list {
		switch i.Kind {
		case Rename:
			fr, fc := resolveName(p.from, i.From)
			tr, tc := resolveName(p.to, i.To)
			switch {
			case fr == nil:
				bad(i, "%s is not in the current schema", i.From)
			case tr == nil:
				bad(i, "%s is not in the target schema", i.To)
			case (fc == "") != (tc == ""):
				bad(i, "renames a table to a column or the reverse")
			case fc == "":
				if fr.Kind != tr.Kind {
					bad(i, "%s is a %s, %s a %s", i.From, relWord(fr), i.To, relWord(tr))
				} else if p.rels(p.to)[fr.FullName()] != nil {
					bad(i, "%s still exists in the target schema", i.From)
				} else if p.rels(p.from)[tr.FullName()] != nil {
					bad(i, "%s already exists in the current schema", i.To)
				} else {
					in.relTo[fr.FullName()] = tr.FullName()
					in.relFrom[tr.FullName()] = fr.FullName()
				}
			default:
				// the column's table may itself be renamed (declared on any line)
				toRel := tr.FullName()
				if fromRel := in.relFrom[toRel]; fromRel == "" {
					fromRel = toRel
					if fromRel != fr.FullName() {
						bad(i, "the columns belong to different tables (declare the table rename too)")
						continue
					}
				} else if fromRel != fr.FullName() {
					bad(i, "the columns belong to different tables")
					continue
				}
				if tr.Column(fc) != nil {
					bad(i, "%s still exists in the target schema", i.From)
				} else if fr.Column(tc) != nil {
					bad(i, "%s already exists in the current schema", i.To)
				} else {
					if in.colTo[fr.FullName()] == nil {
						in.colTo[fr.FullName()] = map[string]string{}
					}
					if in.colFrom[toRel] == nil {
						in.colFrom[toRel] = map[string]string{}
					}
					in.colTo[fr.FullName()][fc] = tc
					in.colFrom[toRel][tc] = fc
				}
			}
		case Drop:
			fr, fc := resolveName(p.from, i.From)
			if fr == nil {
				bad(i, "%s is not in the current schema", i.From)
				continue
			}
			key := fr.FullName()
			if fc != "" {
				key += "." + fc
			}
			if tr := p.rels(p.to)[fr.FullName()]; tr != nil && (fc == "" || tr.Column(fc) != nil) {
				bad(i, "%s still exists in the target schema", i.From)
				continue
			}
			in.dropOK[key] = true
		case EnumDrop:
			if in.enumUsing[i.Enum] == nil {
				in.enumUsing[i.Enum] = map[string]string{}
			}
			in.enumUsing[i.Enum][i.Label] = i.Using
		case Backfill:
			tr, tc := resolveName(p.to, i.Table+"."+i.Column)
			if tr == nil || tc == "" {
				bad(i, "%s.%s is not in the target schema", i.Table, i.Column)
				continue
			}
			in.backfills[tr.FullName()] = append(in.backfills[tr.FullName()], i)
		}
	}
}

// rels caches the relation maps of the two schemas.
func (p *planner) rels(s *schema.Schema) map[string]*schema.Relation {
	if s == p.from {
		if p.fromRels == nil {
			p.fromRels, _ = relations(s)
		}
		return p.fromRels
	}
	if p.toRels == nil {
		p.toRels, _ = relations(s)
	}
	return p.toRels
}

// toName is the target name of a current relation (itself unless renamed).
func (p *planner) toName(fromFull string) string {
	if n, ok := p.in.relTo[fromFull]; ok {
		return n
	}
	return fromFull
}

// fromName is the current name of a target relation (itself unless renamed).
func (p *planner) fromName(toFull string) string {
	if n, ok := p.in.relFrom[toFull]; ok {
		return n
	}
	return toFull
}

// fromOf is the current relation a target relation continues, nil when new.
func (p *planner) fromOf(r *schema.Relation) *schema.Relation {
	return p.rels(p.from)[p.fromName(r.FullName())]
}

// toOf is the target relation a current relation becomes, nil when dropped.
func (p *planner) toOf(r *schema.Relation) *schema.Relation {
	return p.rels(p.to)[p.toName(r.FullName())]
}

// fromCol is the current name of a target relation's column.
func (p *planner) fromCol(r *schema.Relation, col string) string {
	if n, ok := p.in.colFrom[r.FullName()][col]; ok {
		return n
	}
	return col
}

// toCol is the target name of a current relation's column.
func (p *planner) toCol(r *schema.Relation, col string) string {
	if n, ok := p.in.colTo[r.FullName()][col]; ok {
		return n
	}
	return col
}

func (p *planner) problem(format string, args ...any) {
	p.in.problems = append(p.in.problems, fmt.Sprintf(format, args...))
}

// --- renames -------------------------------------------------------------------------

// renames emits the declared renames, after the drops and before anything refers to
// the new names.
func (p *planner) renames() {
	for _, fromFull := range sortedKeys(p.in.relTo) {
		f := p.rels(p.from)[fromFull]
		t := p.rels(p.to)[p.in.relTo[fromFull]]
		cur := qrel(f)
		if f.Schema != t.Schema {
			p.emit("ALTER %s %s SET SCHEMA %s", relWord(f), cur, q(t.Schema))
			cur = q(t.Schema) + "." + q(f.Name)
		}
		if f.Name != t.Name {
			p.emit("ALTER %s %s RENAME TO %s", relWord(f), cur, q(t.Name))
		}
	}
	for _, fromFull := range sortedKeys(p.in.colTo) {
		f := p.rels(p.from)[fromFull]
		t := p.rels(p.to)[p.toName(fromFull)]
		cols := p.in.colTo[fromFull]
		for _, from := range sortedKeys(cols) {
			if f.Column(from) == nil {
				continue
			}
			p.emit("ALTER %s %s RENAME COLUMN %s TO %s", relWord(f), qrel(t), q(from), q(cols[from]))
		}
	}
}

// --- backfills -----------------------------------------------------------------------

// backfill emits the declared UPDATEs for a column of r, type-checked against the target
// schema (the database holds the target's shape for this column by then).
func (p *planner) backfill(r *schema.Relation, col string) {
	for _, b := range p.in.backfills[r.FullName()] {
		if b.Column != col {
			continue
		}
		sql := fmt.Sprintf("UPDATE %s SET %s = %s", qrel(r), q(col), b.Expr)
		if b.Where != "" {
			sql += " WHERE " + b.Where
		}
		if err := p.check(sql); err != nil {
			p.problem("@migrate %s (line %d): %v", b, b.Line, err)
		}
		p.emit("%s", sql)
		p.backfilled[r.FullName()+"."+col] = true
	}
}

// backfillsLeft emits the backfills no column change consumed: plain data fixes, after
// the alterations of their table. With f (the current relation) only columns that exist
// already are filled; the rest wait for the add phase (f nil).
func (p *planner) backfillsLeft(r, f *schema.Relation) {
	for _, b := range p.in.backfills[r.FullName()] {
		if p.backfilled[r.FullName()+"."+b.Column] {
			continue
		}
		if f != nil && f.Column(p.fromCol(r, b.Column)) == nil {
			continue
		}
		p.backfill(r, b.Column)
	}
}

// hasBackfill reports whether a backfill is declared for r.col.
func (p *planner) hasBackfill(r *schema.Relation, col string) bool {
	for _, b := range p.in.backfills[r.FullName()] {
		if b.Column == col {
			return true
		}
	}
	return false
}

// --- enum label removal ----------------------------------------------------------------

// enumRecreates finds the enums whose labels shrink with a complete declared mapping,
// and marks the views over the tables carrying them for recreation (a column's type
// cannot change under a view).
func (p *planner) enumRecreates() {
	p.enumRecreate = map[string]diff.UserType{}
	fromTypes, toTypes := diff.UserTypes(p.from), diff.UserTypes(p.to)
	for _, n := range sortedKeys(fromTypes) {
		f := fromTypes[n]
		t, ok := toTypes[n]
		if !ok || f.Kind != "enum" || t.Kind != "enum" {
			continue
		}
		have := set(labels(t.Props["labels"]))
		using := p.in.enumUsing[n]
		gone := false
		for _, l := range labels(f.Props["labels"]) {
			if have[l] {
				continue
			}
			gone = true
			if to, ok := using[l]; !ok {
				// alterType reports the missing declaration
				gone = false
				break
			} else if !have[to] {
				p.problem("@migrate enum %s: drop %s using %s: %s is not a label of the target enum", n, lit(l), lit(to), lit(to))
			}
		}
		for l := range using {
			if have[l] {
				p.problem("@migrate enum %s: drop %s using ...: %s is still a label of the target enum", n, lit(l), lit(l))
			}
		}
		if !gone {
			continue
		}
		p.enumRecreate[n] = f
		for _, r := range p.from.Relations {
			if r.Kind != schema.Table || !p.usesEnum(r, f.OID) {
				continue
			}
			for _, v := range p.from.DependentViews(r) {
				if tv := p.toOf(v); tv != nil {
					p.recreated[tv.FullName()] = true
				}
			}
		}
	}
	for n := range p.in.enumUsing {
		if _, ok := p.enumRecreate[n]; !ok {
			if _, exists := fromTypes[n]; !exists {
				p.problem("@migrate enum %s: no such enum in the current schema", n)
			} else if _, ok := toTypes[n]; ok && fmt.Sprint(fromTypes[n].Props) == fmt.Sprint(toTypes[n].Props) {
				p.problem("@migrate enum %s: the enum does not change", n)
			}
		}
	}
}

// usesEnum reports whether a column of r has the enum type oid (or an array of it).
func (p *planner) usesEnum(r *schema.Relation, oid catalog.OID) bool {
	for _, c := range r.Columns {
		if c.Type.OID == oid || p.from.Types.ArrayOf(oid) == c.Type.OID {
			return true
		}
	}
	return false
}

// enumColumn reports whether a current column's type is an enum being recreated (its
// type change is emitted by recreateEnum, not as a plain ALTER COLUMN TYPE).
func (p *planner) enumColumn(c *schema.Column) bool {
	for _, f := range p.enumRecreate {
		if c.Type.OID == f.OID || p.from.Types.ArrayOf(f.OID) == c.Type.OID {
			return true
		}
	}
	return false
}

// recreateEnum replaces an enum whose labels shrink: the old type is renamed away, the
// target type created under the name, every column carrying it converted through the
// declared mapping, and the old type dropped.
func (p *planner) recreateEnum(name string, f, t diff.UserType) {
	old := name + "__old"
	oldQ, oldName := old, old
	if i := strings.LastIndex(name, "."); i >= 0 {
		oldName = name[i+1:] + "__old"
		oldQ = name[:i] + "." + oldName
	}
	p.emit("ALTER TYPE %s RENAME TO %s", name, q(oldName))
	var quoted []string
	for _, l := range labels(t.Props["labels"]) {
		quoted = append(quoted, lit(l))
	}
	p.emit("CREATE TYPE %s AS ENUM (%s)", name, strings.Join(quoted, ", "))
	using := p.in.enumUsing[name]
	var cases []string
	for _, l := range sortedKeys(using) {
		cases = append(cases, fmt.Sprintf("WHEN %s THEN %s", lit(l), lit(using[l])))
	}
	_, fromOrder := relations(p.from)
	arr := p.from.Types.ArrayOf(f.OID)
	for _, r := range fromOrder {
		if r.Kind != schema.Table {
			continue
		}
		tr := p.toOf(r)
		if tr == nil {
			continue
		}
		for _, c := range r.Columns {
			tc := tr.Column(p.toCol(r, c.Name))
			if tc == nil {
				continue
			}
			switch c.Type.OID {
			case f.OID:
				if c.Default != nil {
					p.emit("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", qrel(tr), q(tc.Name))
				}
				p.emit("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING (CASE %s::text %s ELSE %s::text END)::%s",
					qrel(tr), q(tc.Name), name, q(tc.Name), strings.Join(cases, " "), q(tc.Name), name)
				if tc.Default != nil {
					p.emit("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", qrel(tr), q(tc.Name), schema.Deparse(tc.Default))
				}
			case arr:
				p.problem("enum %s: column %s.%s is an array of it; the plan cannot map its values (convert it by hand)", name, r.FullName(), c.Name)
			}
		}
	}
	p.emit("DROP TYPE %s", oldQ)
}
