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
// Seeded tables (schema.Relation.Seed) are brought to their declared rows last, with a
// MERGE per table whose content differs (seed.go).
//
// The plan is a proposal: it does not know how to rename, which value an enum label
// should map to when it goes, or what to backfill a new NOT NULL column with. Those show
// up as statements PostgreSQL will refuse or as a leftover difference in Verify, and are
// the intent declarations' job to settle.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// Plan lists the statements that turn from into to: drops (dependents first), the
// declared renames, then alterations, then additions in the target's order, then the
// rows of the seeded tables (a MERGE per table whose content differs). The intents
// (ParseIntents of the target's source) decide what the diff cannot: a table or column
// that disappears must be declared dropped or renamed, an enum label that disappears
// must say which label its values become, and backfills fill new columns before they
// turn NOT NULL. The error lists every declaration the schemas do not bear out and every
// change no declaration explains; the DDL is still returned for reading.
func Plan(from, to *schema.Schema, list []Intent) ([]string, error) {
	p := &planner{from: from, to: to, recreated: map[string]bool{}, backfilled: map[string]bool{}}
	p.readIntents(list)
	p.enumRecreates()
	p.generatedRecreates()
	p.drops()
	p.createSchemas()
	p.renames()
	p.dropSchemas()
	p.alters()
	p.adds()
	p.seeds()
	if len(p.in.problems) > 0 {
		return p.out, errors.New(strings.Join(p.in.problems, "\n"))
	}
	return p.out, nil
}

// check type-checks a data statement of the plan against the target schema.
func (p *planner) check(sql string) error {
	_, err := analyze.Analyze(p.to, sql)
	return err
}

// Verify applies ddl to a fresh database holding currentSQL and lists what still
// differs from target. An error is a statement PostgreSQL refused (or the server
// failing); no changes and no error means the DDL reaches the target. Column order is
// tolerated (diff.Change.OrderOnly) and returned separately as notes.
func Verify(ctx context.Context, c dump.Canonicalizer, currentSQL, ddl string, target *schema.Schema) (changes, notes []diff.Change, err error) {
	// the current schema is a dump, whose session has search_path emptied; the plan's
	// rendered statements name public objects unqualified
	got, _, err := c.Canonical(ctx, currentSQL+"\nRESET search_path;\n"+ddl, target)
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
	in        *intents
	// fromRels / toRels cache relations(); backfilled marks "rel.col" backfills emitted
	fromRels, toRels map[string]*schema.Relation
	backfilled       map[string]bool
	// redoFK marks "rel.constraint" (target names) foreign keys drops() took down ahead of a
	// key they rest on, unchanged in the target and so re-added by adds() all the same
	redoFK map[string]bool
	// restored marks "rel.name" constraints and indexes a generated-column rewrite
	// (addGenerated) already put back, which adds() must not add again
	restored map[string]bool
	// enumRecreate: enums whose labels shrink, recreated under the same name with the
	// declared label mapping; the tables whose columns carry them, and the views over
	// those tables (dropped first, created anew)
	enumRecreate map[string]diff.UserType
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
	var order []*schema.Function
	for _, f := range s.Functions {
		if f.Language == "internal" {
			continue // a range/multirange constructor: PostgreSQL creates and drops it with its type
		}
		m[diff.Signature(s, f)] = f
		order = append(order, f)
	}
	return m, order
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
		if toRels[p.toName(t.Table)] == nil {
			continue // goes with the table
		}
		if tt := toTrg[p.toName(t.Table)+"."+t.Name]; tt == nil || !same(p.from, p.to, t, tt) {
			p.emit("DROP TRIGGER %s ON %s", q(t.Name), qrel(fromRels[t.Table]))
		}
	}
	for _, r := range fromOrder {
		tr := p.toOf(r)
		if tr == nil || tr.Kind != r.Kind {
			continue
		}
		toRules := tr.Rules()
		for _, n := range sortedKeys(r.Rules()) {
			if rd, ok := toRules[n]; !ok || !same(p.from, p.to, r.Rules()[n], rd) {
				p.emit("DROP RULE %s ON %s", q(n), qrel(r))
			}
		}
		for _, pol := range r.Policies {
			if tp := tr.Policy(pol.Name); tp == nil || !same(p.from, p.to, pol, tp) {
				p.emit("DROP POLICY %s ON %s", q(pol.Name), qrel(r))
			}
		}
	}
	// foreign keys of every surviving table first: they may reference keys dropped below,
	// on this table or another one
	droppedFK := map[string]bool{}
	for _, r := range fromOrder {
		tr := p.toOf(r)
		if tr == nil || tr.Kind != r.Kind {
			continue
		}
		toCon := constraints(tr)
		for _, n := range sortedKeys(constraints(r)) {
			c := constraints(r)[n]
			if c.Kind != schema.ForeignKey {
				continue
			}
			if tc, ok := toCon[n]; !ok || !same(p.from, p.to, c, tc) {
				p.emit("ALTER TABLE %s DROP CONSTRAINT %s", qrel(r), q(n))
				droppedFK[r.FullName()+"."+n] = true
			}
		}
	}
	for _, r := range fromOrder {
		tr := p.toOf(r)
		if tr == nil || tr.Kind != r.Kind {
			continue
		}
		toIdx := indexes(tr)
		for _, n := range sortedKeys(indexes(r)) {
			if ti, ok := toIdx[n]; !ok || !same(p.from, p.to, indexes(r)[n], ti) {
				p.emit("DROP INDEX %s", q(r.Schema)+"."+q(n))
			}
		}
		toCon := constraints(tr)
		for _, n := range sortedKeys(constraints(r)) {
			c := constraints(r)[n]
			if c.Kind == schema.ForeignKey {
				continue
			}
			if tc, ok := toCon[n]; !ok || !same(p.from, p.to, c, tc) {
				if c.Kind == schema.PrimaryKey || c.Kind == schema.Unique {
					// a foreign key elsewhere that rests on this key and survives unchanged
					// blocks the drop (2BP01, measured): it goes ahead of the key and comes
					// back in adds (redoFK), once the target's key over its columns exists
					for _, other := range fromOrder {
						for _, fn := range sortedKeys(constraints(other)) {
							fk := constraints(other)[fn]
							if fk.Kind != schema.ForeignKey || fk.RefTable != r.FullName() || droppedFK[other.FullName()+"."+fn] {
								continue
							}
							if !(len(fk.RefColumns) == 0 && c.Kind == schema.PrimaryKey || sameStrings(fk.RefColumns, c.Columns)) {
								continue
							}
							p.emit("ALTER TABLE %s DROP CONSTRAINT %s", qrel(other), q(fn))
							droppedFK[other.FullName()+"."+fn] = true
							if p.toOf(other) == nil {
								continue // the table goes below; nothing to re-add
							}
							if p.redoFK == nil {
								p.redoFK = map[string]bool{}
							}
							p.redoFK[p.toName(other.FullName())+"."+fn] = true
						}
					}
				}
				p.emit("ALTER TABLE %s DROP CONSTRAINT %s", qrel(r), q(n))
			}
		}
	}
	// views and materialized views, latest declared first (they may depend on each other)
	for i := len(fromOrder) - 1; i >= 0; i-- {
		r := fromOrder[i]
		if r.Kind != schema.View && r.Kind != schema.MatView {
			continue
		}
		tr := p.toOf(r)
		if tr != nil && tr.Kind == r.Kind && !p.recreated[tr.FullName()] && (same(p.from, p.to, r, tr) || r.Kind == schema.View && replaceable(p.from, p.to, r, tr)) {
			continue // a view that only grows is replaced in place; otherwise recreated
		}
		p.emit("DROP %s %s", relWord(r), qrel(r))
		if tr != nil {
			p.recreated[tr.FullName()] = true
		}
	}
	// columns of tables that survive
	for _, r := range fromOrder {
		tr := p.toOf(r)
		if tr == nil || r.Kind != schema.Table || tr.Kind != schema.Table {
			continue
		}
		toCols := columnsOf(tr)
		var gone []*schema.Column
		for _, c := range r.Columns {
			if toCols[p.toCol(r, c.Name)] == nil {
				if !p.in.dropOK[r.FullName()+"."+c.Name] {
					p.problem("column %s.%s is dropped, which no @migrate declares: add `-- @migrate drop %s.%s` or `-- @migrate rename %s.%s -> ...`", r.FullName(), c.Name, r.FullName(), c.Name, r.FullName(), c.Name)
				}
				gone = append(gone, c)
			}
		}
		// a generated column that reads another gone column goes first: PostgreSQL refuses
		// to drop a column a generated column still reads (2BP01, measured)
		for _, c := range orderGoneColumns(gone) {
			p.emit("ALTER TABLE %s DROP COLUMN %s", qrel(r), q(c.Name))
		}
	}
	// a sequence that survives (with different, or no, ownership) needs its OWNED BY
	// cleared before its current owning table drops: DROP TABLE cascades to a sequence
	// still OWNED BY one of its columns (42P01 "does not exist" on the ALTER SEQUENCE
	// alters() runs afterward for a surviving sequence, measured) -- alters() only
	// reaches surviving *tables*, so a table that is itself gone needs this handled here.
	for _, name := range sortedKeys(fromRels) {
		seq := fromRels[name]
		if seq.Kind != schema.Sequence || seq.OwnedBy == "" {
			continue
		}
		owner := fromRels[ownerRelation(seq.OwnedBy)]
		if owner == nil {
			continue
		}
		if tr := p.toOf(owner); tr != nil && tr.Kind == owner.Kind {
			continue // the owning table survives; alters() handles this sequence
		}
		if ts := p.toOf(seq); ts != nil && ts.Kind == schema.Sequence {
			p.emit("ALTER SEQUENCE %s OWNED BY NONE", qrel(seq))
		}
	}
	// tables and sequences: referencing tables before the tables they reference
	var gone []*schema.Relation
	for i := len(fromOrder) - 1; i >= 0; i-- {
		r := fromOrder[i]
		if r.Kind != schema.Table && r.Kind != schema.Sequence {
			continue
		}
		if r.Kind == schema.Sequence && r.OwnedBy != "" && ownerColumnGone(p, fromRels, r) {
			continue // goes with its column (or the column's table)
		}
		if tr := p.toOf(r); tr == nil || tr.Kind != r.Kind {
			gone = append(gone, r)
		}
	}
	for _, r := range fkOrder(gone) {
		if r.Kind == schema.Table && !p.in.dropOK[r.FullName()] {
			p.problem("table %s is dropped, which no @migrate declares: add `-- @migrate drop %s` or `-- @migrate rename %s -> ...`", r.FullName(), r.FullName(), r.FullName())
		}
		p.emit("DROP %s %s", relWord(r), qrel(r))
	}
	// functions after the tables: a trigger function is held by the triggers of a table
	// that goes (they go with the table, not by DROP TRIGGER above), and PostgreSQL refuses
	// to drop it while they exist (2BP01, measured); a view calling one is already gone
	for _, n := range sortedKeys(fromFns) {
		if toFns[n] == nil {
			p.emit("DROP %s %s", fnWord(fromFns[n]), n)
		}
	}
	// types
	fromTypes, toTypes := diff.UserTypes(p.from), diff.UserTypes(p.to)
	for _, n := range sortedKeys(fromTypes) {
		if t, ok := toTypes[n]; !ok || t.Kind != fromTypes[n].Kind {
			p.emit("DROP %s %s", typeWord(fromTypes[n].Kind), n)
		}
	}
	// extensions
	toExt := extensions(p.to)
	for _, e := range p.from.Catalog.Extensions {
		if !toExt[e.Name] {
			p.emit("DROP EXTENSION %s", q(e.Name))
		}
	}
}

// createSchemas creates a schema the target newly declares, before renames(): a table
// declared to move into it (-- @migrate rename public.t -> app.t) needs it to already
// exist (3F000 "schema does not exist", measured) -- adds() (which otherwise creates
// every new object, schemas included) runs too late for that ALTER TABLE ... SET SCHEMA.
func (p *planner) createSchemas() {
	fromSchemas := set(p.from.Schemas())
	for _, n := range p.to.Schemas() {
		if !fromSchemas[n] {
			p.emit("CREATE SCHEMA %s", q(n))
		}
	}
}

// dropSchemas drops a schema the target no longer declares, after renames(): a table
// declared to move out of it (-- @migrate rename app.t -> public.t) still reads as
// belonging to the schema from drops()'s (from-side) point of view, and PostgreSQL
// refuses to drop a schema anything still lives in (2BP01 "other objects depend on it",
// measured) -- the ALTER TABLE ... SET SCHEMA that empties it has to run first.
func (p *planner) dropSchemas() {
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

	_, toOrder := relations(p.to)
	for _, r := range toOrder {
		f := p.fromOf(r)
		if f == nil || f.Kind != r.Kind {
			continue
		}
		switch r.Kind {
		case schema.Table:
			p.alterTable(f, r)
			p.backfillsLeft(r, f)
		case schema.View:
			// replaced in adds, once every column the new definition may read exists
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
		if fnProps(p.from, f)["returns"] != fnProps(p.to, fn)["returns"] || len(f.Args) != len(fn.Args) ||
			f.IsProc != fn.IsProc || f.IsAgg != fn.IsAgg || f.IsWindow != fn.IsWindow {
			// CREATE OR REPLACE cannot change the return type, the parameter list, or
			// the object's own kind (function / procedure / aggregate; a plain function
			// turning into a window one is still CREATE FUNCTION, but PostgreSQL still
			// refuses OR REPLACE to add WINDOW, 42P13 "cannot change routine kind" --
			// simplest to always drop and recreate whenever any of these differ, same
			// as a return type change; CREATE AGGREGATE has no OR REPLACE form at all,
			// so leaving it to the "same returns/args" fallthrough re-declared it over
			// the old function outright, 42723 "already exists", measured)
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
			if _, ok := p.enumRecreate[name]; ok {
				p.recreateEnum(name, f, t)
			} else {
				p.problem("enum %s: labels %s are removed, which no @migrate declares: add `-- @migrate enum %s: drop '<label>' using '<label>'` for each", name, strings.Join(gone, ", "), name)
			}
			return
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
		// ADD / DROP ATTRIBUTE do not touch a using column's stored values and PostgreSQL
		// allows them freely; ALTER ATTRIBUTE TYPE goes through the same path as ALTER
		// COLUMN TYPE (find_composite_type_dependencies) and refuses while any column,
		// anywhere, has the type (0A000 "cannot alter type ... because column ... uses it",
		// measured) -- there is no rewrite to offer in its place (the column would need a
		// DROP COLUMN / ADD COLUMN, which loses data), so this is a problem, not a DDL.
		inUse := p.typeInUse(f.OID)
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
				if inUse {
					p.problem("composite type %s: attribute %s changes type (%s -> %s) while a column uses the type; PostgreSQL refuses ALTER ATTRIBUTE TYPE there (0A000) and there is no lossless rewrite to offer", name, a, f.Props[k], t.Props[k])
					continue
				}
				p.emit("ALTER TYPE %s ALTER ATTRIBUTE %s TYPE %s", name, q(a), t.Props[k])
			}
		}
	case "range":
		p.note("range %s: %v -> %v cannot be altered; drop and recreate it", name, f.Props, t.Props)
	}
}

func (p *planner) alterTable(f, r *schema.Relation) {
	p.rowSecurity(f, r)
	fromCols := columnsOf(f)
	rewritten := map[string]bool{}
	for _, c := range r.Columns {
		fc := fromCols[p.fromCol(r, c.Name)]
		if fc == nil {
			continue // added below
		}
		fp, tp := colProps(p.from, fc), colProps(p.to, c)
		if fp["type"] != tp["type"] && !p.enumColumn(fc) {
			// PostgreSQL refuses to alter the type of a column a generated column reads
			// (0A000 "cannot alter type of a column used by a generated column", measured):
			// the dependents are dropped first and rewritten after, the same DROP COLUMN /
			// ADD COLUMN rewrite a changed expression takes
			var deps []*schema.Column
			for _, g := range r.Columns {
				if g.Generated != nil && fromCols[p.fromCol(r, g.Name)] != nil && !rewritten[g.Name] && readsColumn(g, fc.Name) {
					deps = append(deps, g)
					rewritten[g.Name] = true
					p.dropGenerated(r, g)
				}
			}
			// PostgreSQL also refuses to alter the type of a column a row-level security
			// policy's USING / WITH CHECK reads ("cannot alter type of a column used in a
			// policy definition", 0A000, measured), whether or not the policy itself
			// changes: it goes around the ALTER the same way.
			var polDeps []*schema.Policy
			for _, pol := range r.Policies {
				if (pol.Using != nil && hasString(schema.ColumnRefs(pol.Using), c.Name)) ||
					(pol.WithCheck != nil && hasString(schema.ColumnRefs(pol.WithCheck), c.Name)) {
					polDeps = append(polDeps, pol)
					p.emit("DROP POLICY %s ON %s", q(pol.Name), qrel(r))
				}
			}
			// Same refusal for a rule that reads the column ("cannot alter type of a
			// column used by a view or rule", 0A000, measured): its condition and
			// actions are unanalyzed AST (unlike a policy's USING / WITH CHECK, held as
			// schema.Expr), so this checks the deparsed rule text for the column's name
			// at a word boundary (deparse writes it unquoted, often qualified as
			// "new.id" / "old.id") rather than walking it -- a name that is also a
			// substring of another identifier would false-positive into an unneeded
			// drop/recreate, never a missed one; columnWordRe guards the same way.
			// A rule already due its own DROP RULE / CREATE RULE elsewhere in the plan
			// (drops() / adds(), because it differs from the from-side rule of the same
			// name -- typically the table's own rename reads into the rule's deparsed
			// definition) is left to that: dropping and recreating it here too is a
			// second DROP RULE PostgreSQL refuses the moment the first already ran
			// (42704 "rule ... does not exist", measured).
			colRe := columnWordRe(c.Name)
			fromRules := f.Rules()
			var ruleDeps []string
			for n, rd := range r.Rules() {
				if fd, ok := fromRules[n]; !ok || !same(p.from, p.to, fd, rd) {
					continue
				}
				if colRe.MatchString(schema.DeparseStmt(ruleNode(rd))) {
					ruleDeps = append(ruleDeps, n)
					p.emit("DROP RULE %s ON %s", q(n), qrel(r))
				}
			}
			p.emit("ALTER TABLE %s ALTER COLUMN %s TYPE %s", qrel(r), q(c.Name), typeText(p.to, c))
			for _, g := range deps {
				p.addGenerated(r, g)
			}
			for _, pol := range polDeps {
				p.emit("%s", pol.Definition)
			}
			toRules := r.Rules()
			for _, n := range ruleDeps {
				rd := toRules[n]
				p.emit("%s", schema.DeparseStmt(ruleNode(rd)))
				if !rd.Enabled {
					p.emit("ALTER TABLE %s DISABLE RULE %s", qrel(r), q(n))
				}
			}
			if typmodNarrows(p.to, fc, c) {
				p.note("table %s: column %s type %s -> %s narrows precision; PostgreSQL runs this ALTER without a USING clause and rounds or truncates the existing values silently -- add a USING clause, or fix the data first", r.FullName(), c.Name, fp["type"], tp["type"])
			}
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
				p.backfill(r, c.Name)
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
		if (p.renamedExpr(f, fp["generated"]) != tp["generated"] || fp["generated kind"] != tp["generated kind"]) && !rewritten[c.Name] {
			if c.Generated == nil {
				p.emit("ALTER TABLE %s ALTER COLUMN %s DROP EXPRESSION", qrel(r), q(c.Name))
			} else {
				// a generation expression (and STORED vs VIRTUAL, PostgreSQL 18) cannot be
				// added or changed in place: PostgreSQL has no in-place ALTER, only DROP
				// COLUMN + ADD COLUMN.
				p.dropGenerated(r, c)
				p.addGenerated(r, c)
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

// orderNewColumns orders cols (the columns a table gains) so a generated column comes after
// every other new column its expression reads.
func orderNewColumns(cols []*schema.Column) []*schema.Column {
	var out []*schema.Column
	done := map[string]bool{}
	var visit func(c *schema.Column, stack map[string]bool)
	visit = func(c *schema.Column, stack map[string]bool) {
		if done[c.Name] || stack[c.Name] {
			return
		}
		stack[c.Name] = true
		if c.Generated != nil {
			for _, other := range cols {
				if other != c && readsColumn(c, other.Name) {
					visit(other, stack)
				}
			}
		}
		done[c.Name] = true
		out = append(out, c)
	}
	for _, c := range cols {
		visit(c, map[string]bool{})
	}
	return out
}

// orderGoneColumns orders cols (the columns a table loses) so a generated column comes
// before every other gone column it reads: the reverse of orderNewColumns's order.
func orderGoneColumns(cols []*schema.Column) []*schema.Column {
	fwd := orderNewColumns(cols)
	out := make([]*schema.Column, len(fwd))
	for i, c := range fwd {
		out[len(fwd)-1-i] = c
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renamedExpr is a from-side expression text with f's declared column renames applied:
// RENAME COLUMN rewrites the expressions that name the column (a generated column's, a
// CHECK's) on its own, so an expression that differs from the target's only by the renames
// is not a change (rewriting the generated column for it would drop and recompute it, and
// take a dependent view down with it -- 2BP01, measured).
func (p *planner) renamedExpr(f *schema.Relation, expr string) string {
	for from, to := range p.in.colTo[f.FullName()] {
		if from == to {
			continue
		}
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_"])` + regexp.QuoteMeta(from) + `($|[^A-Za-z0-9_"])`)
		expr = re.ReplaceAllString(expr, "${1}"+to+"${2}")
		expr = strings.ReplaceAll(expr, q(from), q(to))
	}
	return expr
}

// readsColumn reports whether generated column g's expression names column name.
func readsColumn(g *schema.Column, name string) bool {
	for _, ref := range schema.ColumnRefs(g.Generated) {
		if ref == name {
			return true
		}
	}
	return false
}

// dropGenerated and addGenerated are the two halves of rewriting a generated column
// (alterTable): DROP COLUMN silently takes down, without needing CASCADE, any index or
// table constraint defined solely on this column -- and refuses outright if another
// table's foreign key rests on such a constraint. dropGenerated drops what would block
// the DROP COLUMN ahead of it; addGenerated adds the target's column back and restores
// what the drop took down.
func (p *planner) dropGenerated(r *schema.Relation, c *schema.Column) {
	for _, fk := range p.foreignKeysReferencing(r, c.Name) {
		p.emit("ALTER TABLE %s DROP CONSTRAINT %s", qrel(fk.rel), q(fk.con.Name))
	}
	p.emit("ALTER TABLE %s DROP COLUMN %s", qrel(r), q(c.Name))
}

func (p *planner) addGenerated(r *schema.Relation, c *schema.Column) {
	if p.restored == nil {
		p.restored = map[string]bool{}
	}
	p.emit("ALTER TABLE %s ADD COLUMN %s", qrel(r), columnText(p.to, c, true))
	for _, con := range p.soleColumnConstraints(r, c.Name) {
		p.emitConstraint(r, con)
		p.restored[r.FullName()+"."+con.Name] = true
	}
	for _, idx := range p.soleColumnIndexes(r, c.Name) {
		p.emit("%s", idx.Definition)
		p.restored[r.FullName()+"."+idx.Name] = true
	}
	for _, fk := range p.foreignKeysReferencing(r, c.Name) {
		p.emitConstraint(fk.rel, fk.con)
		p.restored[fk.rel.FullName()+"."+fk.con.Name] = true
	}
}

// rewritesGenerated reports whether alterTable will drop and re-add a generated column of
// r (its expression changed, or the type of a column it reads does): the views over r must
// be recreated around it, since PostgreSQL refuses the DROP COLUMN while a view reads the
// column (2BP01, measured).
func (p *planner) rewritesGenerated(f, r *schema.Relation) bool {
	fromCols := columnsOf(f)
	for _, c := range r.Columns {
		fc := fromCols[p.fromCol(r, c.Name)]
		if fc == nil {
			continue
		}
		fp, tp := colProps(p.from, fc), colProps(p.to, c)
		if fp["type"] != tp["type"] && !p.enumColumn(fc) {
			for _, g := range r.Columns {
				if g.Generated != nil && fromCols[p.fromCol(r, g.Name)] != nil && readsColumn(g, fc.Name) {
					return true
				}
			}
		}
		if c.Generated != nil && (p.renamedExpr(f, fp["generated"]) != tp["generated"] || fp["generated kind"] != tp["generated kind"]) {
			return true
		}
	}
	return false
}

// generatedRecreates marks the views over every table whose generated column alterTable
// rewrites for recreation (dropped in drops, created anew in adds).
func (p *planner) generatedRecreates() {
	for _, r := range p.from.Relations {
		tr := p.toOf(r)
		if r.Kind != schema.Table || tr == nil || tr.Kind != schema.Table || !p.rewritesGenerated(r, tr) {
			continue
		}
		for _, v := range p.from.DependentViews(r) {
			if tv := p.toOf(v); tv != nil {
				p.recreated[tv.FullName()] = true
			}
		}
	}
}

// soleColumnIndexes are r's target-schema indexes over column name (alone or with others): a
// generated-column DROP COLUMN / ADD COLUMN rewrite (alterTable) takes these down with
// the column, without PostgreSQL needing CASCADE, and never re-creates them on its own.
func (p *planner) soleColumnIndexes(r *schema.Relation, name string) []*schema.Index {
	var out []*schema.Index
	for _, i := range r.Indexes {
		if hasString(i.Columns, name) {
			out = append(out, i)
		}
	}
	return out
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ownerColumnGone reports whether seq's owning column (a bigserial / IDENTITY column's
// sequence, or a standalone sequence's declared OWNED BY) is itself gone in the target --
// dropped outright, or its table dropped or renamed away. A sequence whose owning column
// survives is not implicitly handled by any column-level DROP and needs its own DROP
// SEQUENCE (measured: an unowned-by-drop sequence otherwise never appeared in the plan
// at all).
func ownerColumnGone(p *planner, fromRels map[string]*schema.Relation, seq *schema.Relation) bool {
	owner, col := ownerRelation(seq.OwnedBy), ownerColumn(seq.OwnedBy)
	fr := fromRels[owner]
	if fr == nil || fr.Column(col) == nil {
		return true
	}
	if fr.Column(col).Identity != 0 {
		// an IDENTITY column's sequence is the server's own: it goes (or is replaced)
		// with ALTER COLUMN DROP IDENTITY / TYPE, emitted elsewhere, not a DROP SEQUENCE
		return true
	}
	tr := p.toOf(fr)
	if tr == nil || tr.Kind != fr.Kind {
		// the owning table is itself gone: its own DROP TABLE (which drops the column
		// and, with it, the column's DEFAULT nextval(...)) takes the sequence down too.
		// A separate DROP SEQUENCE ahead of that DROP TABLE is 2BP01 "other objects
		// depend on it" (measured) -- the column's own DEFAULT still names it.
		return true
	}
	if tr.Column(p.toCol(fr, col)) == nil {
		// the table survives but this column itself does not: its own DROP COLUMN
		// (declared -- @migrate drop) takes the sequence down the same way, and a
		// separate DROP SEQUENCE after it is 42P01 "does not exist" (measured).
		return true
	}
	// the same match intents.go's sequence-follows-rename logic looks for: if some
	// sequence in the target still claims this (possibly renamed) ownership, this one
	// is not gone, just relocated or renamed -- that logic gets it there, and a table's
	// own SET SCHEMA / RENAME (also emitted elsewhere) carries a same-schema owned
	// sequence with it (measured) without this one needing to move separately.
	toOwner, toCol := p.toName(owner), col
	if m := p.in.colTo[owner]; m != nil {
		if c, ok := m[col]; ok {
			toCol = c
		}
	}
	wantOwned := toOwner + "." + toCol
	if !strings.Contains(toOwner, ".") {
		wantOwned = "public." + wantOwned
	}
	toRels, _ := relations(p.to)
	for _, r := range toRels {
		if r.Kind == schema.Sequence && r.OwnedBy == wantOwned {
			return true
		}
	}
	return false
}

// columnWordRe matches name as a whole word (e.g. in "new.id" or "id", not inside
// "identifier"): used to look for a column's name in text that is not walkable AST
// (a rule's deparsed definition).
func columnWordRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
}

// soleColumnConstraints are r's target-schema unique / primary key constraints over column
// name (alone or with others) -- also taken down, silently, by the same rewrite (a
// multi-column constraint goes with any of its columns, measured).
func (p *planner) soleColumnConstraints(r *schema.Relation, name string) []*schema.Constraint {
	var out []*schema.Constraint
	for _, n := range sortedKeys(constraints(r)) {
		c := constraints(r)[n]
		switch {
		case (c.Kind == schema.PrimaryKey || c.Kind == schema.Unique || c.Kind == schema.Exclude) && hasString(c.Columns, name):
			out = append(out, c)
		case c.Kind == schema.Check && hasString(schema.ColumnRefs(c.Expr), name):
			out = append(out, c) // a CHECK reading the column goes with it too (measured)
		}
	}
	return out
}

// referencingFK is a foreign key found on another table, elsewhere in the target
// schema, that references r's column name.
type referencingFK struct {
	rel *schema.Relation
	con *schema.Constraint
}

// foreignKeysReferencing lists the foreign keys, anywhere in the target schema, whose
// REFERENCES points at r's column name: the referenced side is a unique or primary key
// constraint a generated-column rewrite is about to take down along with the column, and
// PostgreSQL refuses the DROP COLUMN outright while such a foreign key still depends on
// it (SQLSTATE 2BP01).
func (p *planner) foreignKeysReferencing(r *schema.Relation, name string) []referencingFK {
	pk := p.soleColumnConstraints(r, name)
	isPK := false
	for _, c := range pk {
		if c.Kind == schema.PrimaryKey {
			isPK = true
		}
	}
	var out []referencingFK
	_, toOrder := relations(p.to)
	for _, other := range toOrder {
		for _, n := range sortedKeys(constraints(other)) {
			c := constraints(other)[n]
			if c.Kind != schema.ForeignKey || c.RefTable != r.FullName() {
				continue
			}
			switch {
			case len(c.RefColumns) == 1 && c.RefColumns[0] == name:
				out = append(out, referencingFK{other, c})
			case len(c.RefColumns) == 0 && isPK:
				// empty RefColumns means the referenced table's primary key
				out = append(out, referencingFK{other, c})
			}
		}
	}
	return out
}

// emitConstraint re-adds a target-schema constraint that a generated-column rewrite took
// down along with the column it was defined on.
func (p *planner) emitConstraint(r *schema.Relation, c *schema.Constraint) {
	if c.Definition != "" {
		p.emit("%s", c.Definition)
		return
	}
	p.emit("ALTER TABLE %s ADD CONSTRAINT %s %s", qrel(r), q(c.Name), constraintText(p.to, c))
}

// --- adds ----------------------------------------------------------------------------

func (p *planner) adds() {
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
	fromRels := p.rels(p.from)
	toRels, toOrder := relations(p.to)
	// A sequence newly owned by a column that is itself new on a table that already
	// exists (a bigserial-style column, no IDENTITY) needs its CREATE SEQUENCE emitted
	// explicitly, and before the column's own ADD COLUMN (whose DEFAULT nextval(...)
	// names it) rather than wherever the sequence happens to fall in toOrder.
	// seqForNewColumn keys that sequence by "table.column"; inlineSeq lists it by its own
	// name so the general per-relation pass below skips it.
	seqForNewColumn := map[string]*schema.Relation{}
	inlineSeq := map[string]bool{}
	for _, seq := range toOrder {
		if seq.Kind != schema.Sequence || seq.OwnedBy == "" {
			continue
		}
		if f := p.fromOf(seq); f != nil && f.Kind == seq.Kind {
			continue // the sequence already exists
		}
		owner := toRels[ownerRelation(seq.OwnedBy)]
		if owner == nil || fromRels[owner.FullName()] == nil {
			continue // no owner, or the owning table is itself new (handled with its creation)
		}
		if tc := owner.Column(ownerColumn(seq.OwnedBy)); tc == nil || tc.Identity != 0 {
			continue // IDENTITY creates its own sequence
		}
		seqForNewColumn[owner.FullName()+"."+ownerColumn(seq.OwnedBy)] = seq
		inlineSeq[seq.FullName()] = true
	}
	// relations in declaration order, with their columns
	for _, r := range toOrder {
		f := p.fromOf(r)
		if p.recreated[r.FullName()] {
			f = nil
		}
		if f == nil || f.Kind != r.Kind {
			if r.Kind == schema.Sequence && r.OwnedBy != "" {
				if inlineSeq[r.FullName()] {
					continue // emitted right before its new column, in the surviving table's own loop below
				}
				if owner := toRels[ownerRelation(r.OwnedBy)]; owner != nil {
					switch tc := owner.Column(ownerColumn(r.OwnedBy)); {
					case fromRels[owner.FullName()] == nil:
						continue // the owning table is new; its own creation block (below) emits this sequence
					case tc != nil && tc.Identity != 0:
						continue // the owning column has (or is gaining) IDENTITY; that ALTER / ADD COLUMN creates the sequence itself
					}
				}
			}
			p.emit("%s", r.Definition)
			if r.Kind == schema.Sequence && r.OwnedBy != "" {
				p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(r), qdot(r.OwnedBy))
			}
			if r.Kind == schema.Table {
				// a dump declares a serial column's sequence and default outside CREATE TABLE;
				// an identity column's sequence is created by the ADD GENERATED ... AS IDENTITY
				// among the table's Alters (emitting the sequence here too is 0A000 "cannot
				// change ownership of identity sequence", measured)
				for _, seq := range toOrder {
					if seq.Kind == schema.Sequence && seq.OwnedBy != "" && ownerRelation(seq.OwnedBy) == r.FullName() {
						if tc := r.Column(ownerColumn(seq.OwnedBy)); tc != nil && tc.Identity != 0 {
							continue
						}
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
		if r.Kind == schema.View && !same(p.from, p.to, f, r) && replaceable(p.from, p.to, f, r) {
			// a view that only grows is replaced in place, here rather than in alters: its
			// new definition may read a column added just above (42703 otherwise, measured)
			p.emit("%s", strings.Replace(r.Definition, "CREATE VIEW", "CREATE OR REPLACE VIEW", 1))
		}
		if r.Kind != schema.Table {
			continue
		}
		fromCols := columnsOf(f)
		var fresh []*schema.Column
		for _, c := range r.Columns {
			if fromCols[p.fromCol(r, c.Name)] == nil {
				fresh = append(fresh, c)
			}
		}
		// a generated column reading a column added alongside it comes after that column
		// (42703 "column does not exist" otherwise, measured)
		for _, c := range orderNewColumns(fresh) {
			{
				// a sequence owned by this new column must exist before ADD COLUMN (its
				// DEFAULT nextval(...) names it), but OWNED BY needs the column to exist -
				// so create it now and defer OWNED BY until right after the column lands.
				seq := seqForNewColumn[r.FullName()+"."+c.Name]
				if seq != nil {
					p.emit("%s", seq.Definition)
				}
				if c.NotNull && len(p.in.backfills[r.FullName()]) > 0 && p.hasBackfill(r, c.Name) {
					// the rows exist already: add nullable, fill, then constrain
					p.emit("ALTER TABLE %s ADD COLUMN %s", qrel(r), columnText(p.to, c, false))
					if seq != nil {
						p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(seq), qdot(seq.OwnedBy))
					}
					p.backfill(r, c.Name)
					p.emit("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", qrel(r), q(c.Name))
					continue
				}
				p.emit("ALTER TABLE %s ADD COLUMN %s", qrel(r), columnText(p.to, c, true))
				if seq != nil {
					p.emit("ALTER SEQUENCE %s OWNED BY %s", qrel(seq), qdot(seq.OwnedBy))
				}
			}
		}
		p.backfillsLeft(r, nil)
	}
	// constraints (keys before foreign keys), indexes, triggers, rules
	for pass := 0; pass < 2; pass++ {
		for _, r := range toOrder {
			f := p.fromOf(r)
			var fromCon map[string]*schema.Constraint
			if f != nil && f.Kind == r.Kind {
				fromCon = constraints(f)
			}
			for _, n := range sortedKeys(constraints(r)) {
				c := constraints(r)[n]
				if (c.Kind == schema.ForeignKey) != (pass == 1) {
					continue
				}
				if fc := fromCon[n]; fc != nil && same(p.from, p.to, fc, c) && !p.redoFK[r.FullName()+"."+n] || p.restored[r.FullName()+"."+n] {
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
		f := p.fromOf(r)
		var fromIdx map[string]*schema.Index
		if f != nil && f.Kind == r.Kind {
			fromIdx = indexes(f)
		}
		for _, n := range sortedKeys(indexes(r)) {
			i := indexes(r)[n]
			if fi := fromIdx[n]; fi != nil && same(p.from, p.to, fi, i) || p.restored[r.FullName()+"."+n] {
				continue
			}
			p.emit("%s", i.Definition)
		}
	}
	fromTrg := triggers(p.from)
	for _, t := range p.to.Triggers {
		if ft := fromTrg[p.fromName(t.Table)+"."+t.Name]; ft != nil && toRels[t.Table] != nil && fromRels[p.fromName(t.Table)] != nil && !p.recreated[t.Table] && same(p.from, p.to, ft, t) {
			continue
		}
		p.emit("%s", t.Definition)
	}
	for _, r := range toOrder {
		f := p.fromOf(r)
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
	// row-level security policies (on tables created above, their ENABLE / FORCE came with
	// the table's own ALTERs)
	for _, r := range toOrder {
		f := p.fromOf(r)
		if f != nil && f.Kind != r.Kind {
			f = nil
		}
		for _, pol := range r.Policies {
			if f != nil && !p.recreated[r.FullName()] {
				if fp := f.Policy(pol.Name); fp != nil && same(p.from, p.to, fp, pol) {
					continue
				}
			}
			p.emit("%s", pol.Definition)
		}
	}
	// comments, the from side's keys spelled as the target names them (a comment on a
	// renamed table is still the same comment; one the target drops is removed under the
	// new name -- measured: the plan left it in place before)
	fromComments := map[string]string{}
	for k, v := range p.from.Comments {
		fromComments[p.commentKeyTo(k)] = v
	}
	for _, k := range sortedKeys(p.to.Comments) {
		v := p.to.Comments[k]
		if fv, ok := fromComments[k]; ok && fv == v {
			continue
		}
		if t := commentText(p.to, k, v); t != "" {
			p.emit("%s", t)
		}
	}
	for _, k := range sortedKeys(fromComments) {
		if _, ok := p.to.Comments[k]; ok {
			continue
		}
		if t := commentText(p.to, k, ""); t != "" {
			p.emit("%s", t)
		}
	}
}

// commentKeyTo is a from-side comment key ("schema.rel" or "schema.rel.column") with the
// declared renames applied.
func (p *planner) commentKeyTo(k string) string {
	rel := commentRelation(k)
	if p.from.Relation(splitRel(rel)) == nil {
		return k
	}
	to := p.toName(rel)
	if rel == k {
		return to
	}
	col := k[len(rel)+1:]
	if m := p.in.colTo[rel]; m != nil {
		if c, ok := m[col]; ok {
			col = c
		}
	}
	return to + "." + col
}

func splitRel(full string) (string, string) {
	if i := strings.Index(full, "."); i > 0 {
		return full[:i], full[i+1:]
	}
	return "public", full
}

// rowSecurity emits the ENABLE / DISABLE / FORCE / NO FORCE ROW LEVEL SECURITY a
// surviving table needs.
func (p *planner) rowSecurity(f, r *schema.Relation) {
	if f.RowSecurity != r.RowSecurity {
		if r.RowSecurity {
			p.emit("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", qrel(r))
		} else {
			p.emit("ALTER TABLE %s DISABLE ROW LEVEL SECURITY", qrel(r))
		}
	}
	if f.ForceRowSecurity != r.ForceRowSecurity {
		if r.ForceRowSecurity {
			p.emit("ALTER TABLE %s FORCE ROW LEVEL SECURITY", qrel(r))
		} else {
			p.emit("ALTER TABLE %s NO FORCE ROW LEVEL SECURITY", qrel(r))
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
