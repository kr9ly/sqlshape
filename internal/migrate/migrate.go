// Package migrate turns the difference between two schemas into DDL, and checks DDL by
// its end state: applied to a PostgreSQL holding the current schema, does the database
// read back as the target?
//
// Both schemas should be canonical (dump.Canonical / dump.Load): the target's statement
// texts are then PostgreSQL's own, fully qualified, and its objects come in dependency
// order, which the plan follows for additions. Whole objects are created from the text
// that declared them (Relation.Definition and friends); parts (columns, constraints,
// labels) are rendered from the model.
//
// The plan is a proposal: it does not know how to rename, which value an enum label
// should map to when it goes, or what to backfill a new NOT NULL column with. Those show
// up as statements PostgreSQL will refuse or as a leftover difference in Verify, and are
// the intent declarations' job to settle.
package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/dump"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Plan lists the statements that turn from into to: drops (dependents first), then
// alterations, then additions in the target's order.
func Plan(from, to *schema.Schema) []string {
	p := &planner{from: from, to: to, recreated: map[string]bool{}}
	p.drops()
	p.alters()
	p.adds()
	return p.out
}

// Verify applies ddl to a fresh database holding currentSQL and lists what still
// differs from target. An error is a statement PostgreSQL refused (or the server
// failing); no changes and no error means the DDL reaches the target. Column order is
// tolerated (diff.Change.OrderOnly) and returned separately as notes.
func Verify(ctx context.Context, c dump.Canonicalizer, currentSQL, ddl string, target *schema.Schema) (changes, notes []diff.Change, err error) {
	// the current schema is a dump, whose session has search_path emptied; the plan's
	// rendered statements name public objects unqualified
	got, _, err := c.Canonical(ctx, currentSQL+"\nRESET search_path;\n"+ddl)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range diff.Compare(got, target) {
		if c.OrderOnly() {
			notes = append(notes, c)
		} else {
			changes = append(changes, c)
		}
	}
	return changes, notes, nil
}

type planner struct {
	from, to *schema.Schema
	out      []string
	// recreated are relations dropped in the drop phase that the add phase creates anew
	recreated map[string]bool
}

func (p *planner) emit(format string, args ...any) {
	p.out = append(p.out, fmt.Sprintf(format, args...)+";")
}

// note leaves a comment for what the plan cannot do.
func (p *planner) note(format string, args ...any) {
	p.out = append(p.out, "-- "+fmt.Sprintf(format, args...))
}

// --- naming --------------------------------------------------------------------

// q quotes an identifier.
func q(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// qrel is a relation's fully qualified, quoted name.
func qrel(r *schema.Relation) string { return q(r.Schema) + "." + q(r.Name) }

// lit is a string literal.
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func qlist(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = q(n)
	}
	return strings.Join(out, ", ")
}

// --- lookups -------------------------------------------------------------------

func relations(s *schema.Schema) (map[string]*schema.Relation, []*schema.Relation) {
	m := map[string]*schema.Relation{}
	var order []*schema.Relation
	for _, r := range s.Relations {
		if r.Temp || r.Kind == 'c' {
			continue
		}
		m[r.FullName()] = r
		order = append(order, r)
	}
	return m, order
}

func functions(s *schema.Schema) (map[string]*schema.Function, []*schema.Function) {
	m := map[string]*schema.Function{}
	for _, f := range s.Functions {
		m[diff.Signature(s, f)] = f
	}
	return m, s.Functions
}

func triggers(s *schema.Schema) map[string]*schema.Trigger {
	m := map[string]*schema.Trigger{}
	for _, t := range s.Triggers {
		m[t.Table+"."+t.Name] = t
	}
	return m
}

func constraints(r *schema.Relation) map[string]*schema.Constraint {
	idx := map[string]bool{}
	for _, i := range r.Indexes {
		idx[i.Name] = true
	}
	m := map[string]*schema.Constraint{}
	for _, c := range r.Constraints {
		if c.Kind == schema.Unique && idx[c.Name] {
			continue // the loader's mirror of a unique index
		}
		m[c.Name] = c
	}
	return m
}

func indexes(r *schema.Relation) map[string]*schema.Index {
	m := map[string]*schema.Index{}
	for _, i := range r.Indexes {
		m[i.Name] = i
	}
	return m
}

func columnsOf(r *schema.Relation) map[string]*schema.Column {
	m := map[string]*schema.Column{}
	for _, c := range r.Columns {
		m[c.Name] = c
	}
	return m
}

// same reports whether two objects compare equal under diff (their properties match).
func same(a, b *schema.Schema, x, y any) bool {
	return len(diff.Props(a, x)) > 0 && fmt.Sprint(diff.Props(a, x)) == fmt.Sprint(diff.Props(b, y))
}

// --- drops -------------------------------------------------------------------------

func (p *planner) drops() {
	fromRels, fromOrder := relations(p.from)
	toRels, _ := relations(p.to)
	fromFns, _ := functions(p.from)
	toFns, _ := functions(p.to)
	fromTrg, toTrg := triggers(p.from), triggers(p.to)

	// parts of relations that survive: triggers, rules, indexes, constraints
	for _, n := range sortedKeys(fromTrg) {
		t := fromTrg[n]
		if toRels[t.Table] == nil {
			continue // goes with the table
		}
		if tt := toTrg[n]; tt == nil || !same(p.from, p.to, t, tt) {
			p.emit("DROP TRIGGER %s ON %s", q(t.Name), qrel(fromRels[t.Table]))
		}
	}
	for _, r := range fromOrder {
		tr := toRels[r.FullName()]
		if tr == nil || tr.Kind != r.Kind {
			continue
		}
		toRules := tr.Rules()
		for _, n := range sortedKeys(r.Rules()) {
			if rd, ok := toRules[n]; !ok || !same(p.from, p.to, r.Rules()[n], rd) {
				p.emit("DROP RULE %s ON %s", q(n), qrel(r))
			}
		}
		toIdx := indexes(tr)
		for _, n := range sortedKeys(indexes(r)) {
			if ti, ok := toIdx[n]; !ok || !same(p.from, p.to, indexes(r)[n], ti) {
				p.emit("DROP INDEX %s", q(r.Schema)+"."+q(n))
			}
		}
		toCon := constraints(tr)
		// foreign keys first: they may reference keys dropped below
		for pass := 0; pass < 2; pass++ {
			for _, n := range sortedKeys(constraints(r)) {
				c := constraints(r)[n]
				if (c.Kind == schema.ForeignKey) != (pass == 0) {
					continue
				}
				if tc, ok := toCon[n]; !ok || !same(p.from, p.to, c, tc) {
					p.emit("ALTER TABLE %s DROP CONSTRAINT %s", qrel(r), q(n))
				}
			}
		}
	}
	// views and materialized views, latest declared first (they may depend on each other)
	for i := len(fromOrder) - 1; i >= 0; i-- {
		r := fromOrder[i]
		if r.Kind != schema.View && r.Kind != schema.MatView {
			continue
		}
		tr := toRels[r.FullName()]
		if tr != nil && tr.Kind == r.Kind && (same(p.from, p.to, r, tr) || r.Kind == schema.View && replaceable(p.from, p.to, r, tr)) {
			continue // a view that only grows is replaced in place; otherwise recreated
		}
		p.emit("DROP %s %s", relWord(r), qrel(r))
		p.recreated[r.FullName()] = true
	}
	for _, n := range sortedKeys(fromFns) {
		if toFns[n] == nil {
			p.emit("DROP %s %s", fnWord(fromFns[n]), n)
		}
	}
	// columns of tables that survive
	for _, r := range fromOrder {
		tr := toRels[r.FullName()]
		if tr == nil || r.Kind != schema.Table || tr.Kind != schema.Table {
			continue
		}
		toCols := columnsOf(tr)
		for _, c := range r.Columns {
			if toCols[c.Name] == nil {
				p.emit("ALTER TABLE %s DROP COLUMN %s", qrel(r), q(c.Name))
			}
		}
	}
	// tables and sequences: referencing tables before the tables they reference
	var gone []*schema.Relation
	for i := len(fromOrder) - 1; i >= 0; i-- {
		r := fromOrder[i]
		if r.Kind != schema.Table && r.Kind != schema.Sequence {
			continue
		}
		if r.Kind == schema.Sequence && r.OwnedBy != "" {
			continue // goes with its column
		}
		if tr := toRels[r.FullName()]; tr == nil || tr.Kind != r.Kind {
			gone = append(gone, r)
		}
	}
	for _, r := range fkOrder(gone) {
		p.emit("DROP %s %s", relWord(r), qrel(r))
	}
	// types
	fromTypes, toTypes := diff.UserTypes(p.from), diff.UserTypes(p.to)
	for _, n := range sortedKeys(fromTypes) {
		if t, ok := toTypes[n]; !ok || t.Kind != fromTypes[n].Kind {
			p.emit("DROP %s %s", typeWord(fromTypes[n].Kind), n)
		}
	}
	// extensions, schemas
	toExt := extensions(p.to)
	for _, e := range p.from.Catalog.Extensions {
		if !toExt[e.Name] {
			p.emit("DROP EXTENSION %s", q(e.Name))
		}
	}
	toSchemas := set(p.to.Schemas())
	for _, n := range p.from.Schemas() {
		if !toSchemas[n] {
			p.emit("DROP SCHEMA %s", q(n))
		}
	}
}

// --- alters ------------------------------------------------------------------------

func (p *planner) alters() {
	fromTypes, toTypes := diff.UserTypes(p.from), diff.UserTypes(p.to)
	for _, n := range sortedKeys(toTypes) {
		t := toTypes[n]
		f, ok := fromTypes[n]
		if !ok || f.Kind != t.Kind || fmt.Sprint(f.Props) == fmt.Sprint(t.Props) {
			continue
		}
		p.alterType(n, f, t)
	}

	fromRels, _ := relations(p.from)
	_, toOrder := relations(p.to)
	for _, r := range toOrder {
		f := fromRels[r.FullName()]
		if f == nil || f.Kind != r.Kind {
			continue
		}
		switch r.Kind {
		case schema.Table:
			p.alterTable(f, r)
		case schema.View:
			if !same(p.from, p.to, f, r) && replaceable(p.from, p.to, f, r) {
				p.emit("%s", strings.Replace(r.Definition, "CREATE VIEW", "CREATE OR REPLACE VIEW", 1))
			}
		case schema.Sequence:
			if f.OwnedBy != r.OwnedBy {
				owner := "NONE"
				if r.OwnedBy != "" {
					parts := strings.Split(r.OwnedBy, ".")
					owner = qdot(strings.Join(parts, "."))
				}
				p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(r), owner)
			}
		}
	}

	fromFns, _ := functions(p.from)
	_, toFnOrder := functions(p.to)
	for _, fn := range toFnOrder {
		n := diff.Signature(p.to, fn)
		f := fromFns[n]
		if f == nil || same(p.from, p.to, f, fn) {
			continue
		}
		def := fn.Definition
		if fnProps(p.from, f)["returns"] != fnProps(p.to, fn)["returns"] || len(f.Args) != len(fn.Args) {
			// CREATE OR REPLACE cannot change the return type or the parameter list
			p.emit("DROP %s %s", fnWord(f), n)
		} else {
			def = strings.Replace(def, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
			def = strings.Replace(def, "CREATE PROCEDURE", "CREATE OR REPLACE PROCEDURE", 1)
		}
		p.emit("%s", def)
	}
}

func (p *planner) alterType(name string, f, t diff.UserType) {
	switch t.Kind {
	case "enum":
		from, to := labels(f.Props["labels"]), labels(t.Props["labels"])
		have := set(to)
		var gone []string
		for _, l := range from {
			if !have[l] {
				gone = append(gone, l)
			}
		}
		if len(gone) > 0 {
			p.note("enum %s: labels %s are removed; PostgreSQL cannot drop enum labels (declare the mapping with @migrate)", name, strings.Join(gone, ", "))
		}
		had := set(from)
		for i, l := range to {
			if had[l] {
				continue
			}
			pos := ""
			for _, next := range to[i+1:] {
				if had[next] {
					pos = " BEFORE " + lit(next)
					break
				}
			}
			if pos == "" && i > 0 {
				pos = " AFTER " + lit(to[i-1])
			}
			p.emit("ALTER TYPE %s ADD VALUE %s%s", name, lit(l), pos)
			had[l] = true
		}
	case "domain":
		if f.Props["not null"] != t.Props["not null"] {
			if t.Props["not null"] == "true" {
				p.emit("ALTER DOMAIN %s SET NOT NULL", name)
			} else {
				p.emit("ALTER DOMAIN %s DROP NOT NULL", name)
			}
		}
		if f.Props["base"] != t.Props["base"] {
			p.note("domain %s: base type %s -> %s cannot be altered; drop and recreate it", name, f.Props["base"], t.Props["base"])
		}
		for _, k := range sortedKeys(f.Props) {
			if strings.HasPrefix(k, "check ") && f.Props[k] != t.Props[k] {
				p.emit("ALTER DOMAIN %s DROP CONSTRAINT %s", name, q(strings.TrimPrefix(k, "check ")))
			}
		}
		for _, k := range sortedKeys(t.Props) {
			if strings.HasPrefix(k, "check ") && f.Props[k] != t.Props[k] {
				p.emit("ALTER DOMAIN %s ADD CONSTRAINT %s CHECK (%s)", name, q(strings.TrimPrefix(k, "check ")), t.Props[k])
			}
		}
	case "composite":
		for _, k := range sortedKeys(f.Props) {
			if strings.HasPrefix(k, "attribute ") && t.Props[k] == "" {
				p.emit("ALTER TYPE %s DROP ATTRIBUTE %s", name, q(strings.TrimPrefix(k, "attribute ")))
			}
		}
		for _, a := range labels(t.Props["attributes"]) {
			k := "attribute " + a
			switch {
			case f.Props[k] == "":
				p.emit("ALTER TYPE %s ADD ATTRIBUTE %s %s", name, q(a), t.Props[k])
			case f.Props[k] != t.Props[k]:
				p.emit("ALTER TYPE %s ALTER ATTRIBUTE %s TYPE %s", name, q(a), t.Props[k])
			}
		}
	case "range":
		p.note("range %s: %v -> %v cannot be altered; drop and recreate it", name, f.Props, t.Props)
	}
}

func (p *planner) alterTable(f, r *schema.Relation) {
	fromCols := columnsOf(f)
	for _, c := range r.Columns {
		fc := fromCols[c.Name]
		if fc == nil {
			continue // added below
		}
		fp, tp := colProps(p.from, fc), colProps(p.to, c)
		if fp["type"] != tp["type"] {
			p.emit("ALTER TABLE %s ALTER COLUMN %s TYPE %s", qrel(r), q(c.Name), typeText(p.to, c))
		}
		if fp["default"] != tp["default"] {
			if c.Default == nil {
				p.emit("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", qrel(r), q(c.Name))
			} else {
				p.emit("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", qrel(r), q(c.Name), schema.Deparse(c.Default))
			}
		}
		if fp["not null"] != tp["not null"] {
			if c.NotNull {
				p.emit("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", qrel(r), q(c.Name))
			} else {
				p.emit("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", qrel(r), q(c.Name))
			}
		}
		if fp["identity"] != tp["identity"] {
			switch {
			case c.Identity == 0:
				p.emit("ALTER TABLE %s ALTER COLUMN %s DROP IDENTITY", qrel(r), q(c.Name))
			case fc.Identity == 0:
				p.emit("ALTER TABLE %s ALTER COLUMN %s ADD GENERATED %s AS IDENTITY", qrel(r), q(c.Name), identityWord(c.Identity))
			default:
				p.emit("ALTER TABLE %s ALTER COLUMN %s SET GENERATED %s", qrel(r), q(c.Name), identityWord(c.Identity))
			}
		}
		if fp["generated"] != tp["generated"] {
			if c.Generated == nil {
				p.emit("ALTER TABLE %s ALTER COLUMN %s DROP EXPRESSION", qrel(r), q(c.Name))
			} else {
				// a generation expression cannot be added or changed in place
				p.emit("ALTER TABLE %s DROP COLUMN %s", qrel(r), q(c.Name))
				p.emit("ALTER TABLE %s ADD COLUMN %s", qrel(r), columnText(p.to, c))
			}
		}
	}
	fromProps, toProps := diff.Props(p.from, f), diff.Props(p.to, r)
	for _, k := range []string{"inherits", "partition of", "partition key", "of type"} {
		if fromProps[k] != toProps[k] {
			p.note("table %s: %s %q -> %q cannot be altered by the plan", r.FullName(), k, fromProps[k], toProps[k])
		}
	}
}

// --- adds ----------------------------------------------------------------------------

func (p *planner) adds() {
	fromSchemas := set(p.from.Schemas())
	for _, n := range p.to.Schemas() {
		if !fromSchemas[n] {
			p.emit("CREATE SCHEMA %s", q(n))
		}
	}
	fromExt := extensions(p.from)
	for _, e := range p.to.Catalog.Extensions {
		if !fromExt[e.Name] {
			p.emit("CREATE EXTENSION %s", q(e.Name))
		}
	}
	// types in declaration order (a domain over another domain, a composite of an enum)
	fromTypes, toTypes := diff.UserTypes(p.from), diff.UserTypes(p.to)
	for _, t := range p.to.Types.User() {
		def := p.to.TypeDefs[t.OID]
		if def == "" {
			continue
		}
		for n, ut := range toTypes {
			if ut.OID != t.OID {
				continue
			}
			if f, ok := fromTypes[n]; !ok || f.Kind != ut.Kind {
				p.emit("%s", def)
			}
		}
	}
	fromFns, _ := functions(p.from)
	_, toFnOrder := functions(p.to)
	// functions before relations (defaults, generated columns and views use them); their
	// bodies may name tables not yet there
	var newFns []*schema.Function
	for _, fn := range toFnOrder {
		if fromFns[diff.Signature(p.to, fn)] == nil {
			newFns = append(newFns, fn)
		}
	}
	if len(newFns) > 0 {
		p.emit("SET check_function_bodies = false")
		for _, fn := range newFns {
			p.emit("%s", fn.Definition)
		}
	}
	fromRels, _ := relations(p.from)
	toRels, toOrder := relations(p.to)
	// relations in declaration order, with their columns
	for _, r := range toOrder {
		f := fromRels[r.FullName()]
		if p.recreated[r.FullName()] {
			f = nil
		}
		if f == nil || f.Kind != r.Kind {
			if r.Kind == schema.Sequence && r.OwnedBy != "" {
				if owner := toRels[ownerRelation(r.OwnedBy)]; owner != nil && (fromRels[owner.FullName()] == nil || fromRels[owner.FullName()].Column(ownerColumn(r.OwnedBy)) == nil) {
					continue // created with its serial / identity column
				}
			}
			p.emit("%s", r.Definition)
			if r.Kind == schema.Sequence && r.OwnedBy != "" {
				p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(r), qdot(r.OwnedBy))
			}
			if r.Kind == schema.Table {
				// a dump declares a serial column's sequence and default outside CREATE TABLE
				for _, seq := range toOrder {
					if seq.Kind == schema.Sequence && seq.OwnedBy != "" && ownerRelation(seq.OwnedBy) == r.FullName() {
						p.emit("%s", seq.Definition)
						p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(seq), qdot(seq.OwnedBy))
					}
				}
				for _, a := range r.Alters {
					p.emit("%s", a)
				}
			}
			continue
		}
		if r.Kind != schema.Table {
			continue
		}
		fromCols := columnsOf(f)
		for _, c := range r.Columns {
			if fromCols[c.Name] == nil {
				p.emit("ALTER TABLE %s ADD COLUMN %s", qrel(r), columnText(p.to, c))
			}
		}
	}
	// constraints (keys before foreign keys), indexes, triggers, rules
	for pass := 0; pass < 2; pass++ {
		for _, r := range toOrder {
			f := fromRels[r.FullName()]
			var fromCon map[string]*schema.Constraint
			if f != nil && f.Kind == r.Kind {
				fromCon = constraints(f)
			}
			for _, n := range sortedKeys(constraints(r)) {
				c := constraints(r)[n]
				if (c.Kind == schema.ForeignKey) != (pass == 1) {
					continue
				}
				if fc := fromCon[n]; fc != nil && same(p.from, p.to, fc, c) {
					continue
				}
				if f == nil && c.Definition == "" {
					continue // declared inside the CREATE TABLE just emitted
				}
				if c.Definition != "" {
					p.emit("%s", c.Definition)
				} else {
					p.emit("ALTER TABLE %s ADD CONSTRAINT %s %s", qrel(r), q(c.Name), constraintText(p.to, c))
				}
			}
		}
	}
	for _, r := range toOrder {
		f := fromRels[r.FullName()]
		var fromIdx map[string]*schema.Index
		if f != nil && f.Kind == r.Kind {
			fromIdx = indexes(f)
		}
		for _, n := range sortedKeys(indexes(r)) {
			i := indexes(r)[n]
			if fi := fromIdx[n]; fi != nil && same(p.from, p.to, fi, i) {
				continue
			}
			p.emit("%s", i.Definition)
		}
	}
	fromTrg := triggers(p.from)
	for _, t := range p.to.Triggers {
		if ft := fromTrg[t.Table+"."+t.Name]; ft != nil && toRels[t.Table] != nil && fromRels[t.Table] != nil && same(p.from, p.to, ft, t) {
			continue
		}
		p.emit("%s", t.Definition)
	}
	for _, r := range toOrder {
		f := fromRels[r.FullName()]
		var fromRules map[string]schema.RuleDef
		if f != nil && f.Kind == r.Kind {
			fromRules = f.Rules()
		}
		rules := r.Rules()
		for _, n := range sortedKeys(rules) {
			rd := rules[n]
			if fr, ok := fromRules[n]; ok && same(p.from, p.to, fr, rd) {
				continue
			}
			p.emit("%s", schema.DeparseStmt(ruleNode(rd)))
			if !rd.Enabled {
				p.emit("ALTER TABLE %s DISABLE RULE %s", qrel(r), q(n))
			}
		}
	}
	// comments
	for _, k := range sortedKeys(p.to.Comments) {
		v := p.to.Comments[k]
		if fv, ok := p.from.Comments[k]; ok && fv == v {
			continue
		}
		if t := commentText(p.to, k, v); t != "" {
			p.emit("%s", t)
		}
	}
	for _, k := range sortedKeys(p.from.Comments) {
		if _, ok := p.to.Comments[k]; ok {
			continue
		}
		if t := commentText(p.to, k, ""); t != "" {
			p.emit("%s", t)
		}
	}
}

// fkOrder sorts tables to drop so that a table comes before the tables its foreign
// keys reference (among those dropped).
func fkOrder(rels []*schema.Relation) []*schema.Relation {
	pending := map[string]*schema.Relation{}
	for _, r := range rels {
		pending[r.FullName()] = r
	}
	var out []*schema.Relation
	for len(pending) > 0 {
		progress := false
		for _, r := range rels {
			if pending[r.FullName()] == nil {
				continue
			}
			referenced := false
			for _, o := range pending {
				if o == r {
					continue
				}
				for _, c := range o.Constraints {
					if c.Kind == schema.ForeignKey && strings.TrimPrefix(c.RefTable, "public.") == r.FullName() {
						referenced = true
					}
				}
			}
			if !referenced {
				out = append(out, r)
				delete(pending, r.FullName())
				progress = true
			}
		}
		if !progress { // a cycle: take the rest as they come
			for _, r := range rels {
				if pending[r.FullName()] != nil {
					out = append(out, r)
					delete(pending, r.FullName())
				}
			}
		}
	}
	return out
}
