// Package migrate turns the difference between two MySQL schemas into DDL, and checks DDL
// by its end state: applied to a database holding the current schema, does it read back
// as the target?
//
// Both schemas should be canonical (dump.Canonical / dump.Load): the target's element
// texts are then the server's own spelling, which the plan emits as they are (a CREATE
// TABLE from the canonical text, a column from its definition text in MODIFY COLUMN / ADD
// COLUMN, a key, foreign key or check from its inline definition).
//
// The plan is a proposal: it does not know how to rename, which value an ENUM label
// should map to when it goes, or what to backfill a new NOT NULL column with. Those show
// up as statements MySQL will refuse or as a leftover difference in Verify, and are the
// intent declarations' job to settle. MySQL's DDL commits implicitly, so the plan is a
// sequence of independent statements; apply runs them one by one and stops at the first
// the server refuses.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/analyze"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// Plan lists the statements that turn from into to: drops (referencing foreign keys
// first, then views, tables, columns, keys, checks), the declared renames, then
// alterations (table options, columns by MODIFY COLUMN with their position, keys /
// foreign keys / checks by DROP and ADD, views by CREATE OR REPLACE), then additions in
// dependency order, then the backfills. The intents (ParseIntents of the target's source)
// decide what the diff cannot: a table or column that disappears must be declared dropped
// or renamed, an ENUM label that disappears must say which label its values become, and
// backfills fill new columns. The error lists every declaration the schemas do not bear
// out and every change no declaration explains; the DDL is still returned for reading.
func Plan(from, to *schema.Schema, list []Intent) ([]string, error) {
	p := &planner{from: from, to: to}
	p.readIntents(list)
	p.drops()
	p.renames()
	p.coordinateForeignKeys()
	p.alters()
	p.adds()
	p.backfills()
	// triggers are created last, after the backfills: a newly added trigger should not
	// fire on the migration's own backfill UPDATEs (measured against mysqld: a trigger
	// created after a table's rows already exist does not run for those existing rows,
	// only for statements from here on; PostgreSQL's planner keeps the same order for the
	// same reason, see check/postgres/migrate).
	p.addTriggers()
	// events last of all: nothing depends on an event, and an event's body is bound late
	p.addEvents()
	if len(p.problems) > 0 {
		return p.out, errors.New(strings.Join(p.problems, "\n"))
	}
	return p.out, nil
}

// Verify applies ddl to a database holding currentSQL (canonical text) and lists where the
// result still differs from target. An error is a statement MySQL refused (or the server
// failing); no changes and no error means the DDL reaches the target.
func Verify(ctx context.Context, c dump.Canonicalizer, currentSQL, ddl string, target *schema.Schema) ([]diff.Change, error) {
	got, _, err := c.Canonical(ctx, currentSQL+"\nSET FOREIGN_KEY_CHECKS=1;\n"+ddl)
	if err != nil {
		return nil, err
	}
	return diff.Compare(got, target), nil
}

type planner struct {
	from, to *schema.Schema
	out      []string
	problems []string
	// renames: from-name -> to-name for tables, and per table for columns (by the
	// from table's name)
	tableRename map[string]string
	colRename   map[string]map[string]string
	droppable   map[string]bool // "table" or "table.column" declared droppable
	enumDrops   []Intent
	backfillsOf []Intent
	droppedFKs  map[string]bool // "table.fk" already dropped ahead of a table that goes
	earlyKeys   map[string]bool // "table.key" (target names) already added by dropKeysOf
	earlyMods   map[string]bool // "table.column" (target names) already MODIFYed by renames
}

func (p *planner) emit(format string, args ...any) {
	p.out = append(p.out, fmt.Sprintf(format, args...))
}

func (p *planner) problem(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

// check type-checks a data statement of the plan against the target schema.
func (p *planner) check(sql string) error {
	_, err := analyze.Analyze(p.to, sql)
	return err
}

func q(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// toName is the target's name of a from table (its rename, or itself).
func (p *planner) toName(from string) string {
	if to, ok := p.tableRename[from]; ok {
		return to
	}
	return from
}

// fromName is the from name of a target table (the source of its rename, or itself).
func (p *planner) fromName(to string) string {
	for f, t := range p.tableRename {
		if t == to {
			return f
		}
	}
	return to
}

// toCol is the target's name of a from column.
func (p *planner) toCol(fromTable, col string) string {
	if m, ok := p.colRename[fromTable]; ok {
		if to, ok := m[col]; ok {
			return to
		}
	}
	return col
}

// fromCol is the from name of a target column of a from table.
func (p *planner) fromCol(fromTable, toCol string) string {
	for f, t := range p.colRename[fromTable] {
		if t == toCol {
			return f
		}
	}
	return toCol
}

// pair returns the from and to versions of a table that exists on both sides (renames
// followed), or nil for one that does not.
func (p *planner) pair(toTable *schema.Table) (*schema.Table, *schema.Table) {
	f := p.from.Table(p.fromName(toTable.Name))
	return f, toTable
}

// --- drops -----------------------------------------------------------------------

func (p *planner) drops() {
	// tables that go, with the foreign keys of other tables that reference them
	var gone []*schema.Table
	for _, f := range p.from.Tables {
		if p.to.Table(p.toName(f.Name)) == nil {
			gone = append(gone, f)
		}
	}
	goneNames := map[string]bool{}
	for _, f := range gone {
		goneNames[f.Name] = true
	}
	// triggers that go or change, ahead of the table drops below: a trigger dropped along
	// with its own table (DROP TABLE takes it silently) needs no DROP TRIGGER of its own.
	p.dropTriggers(goneNames)
	p.dropEvents()
	for _, f := range gone {
		if !p.droppable[f.Name] {
			p.problem("table %s is dropped, which no @migrate declares (`-- @migrate drop %s`, or a rename)", f.Name, f.Name)
		}
		for _, other := range p.from.Tables {
			if other == f {
				continue
			}
			for _, fk := range other.ForeignKeys {
				if fk.RefTable == f.Name && p.to.Table(p.toName(other.Name)) != nil {
					name := other.ForeignKeyName(fk)
					p.emit("ALTER TABLE %s DROP FOREIGN KEY %s;", q(other.Name), q(name))
					if p.droppedFKs == nil {
						p.droppedFKs = map[string]bool{}
					}
					p.droppedFKs[other.Name+"."+name] = true
				}
			}
		}
	}
	// views that go, or whose definition changes (replaced in alters)
	for _, v := range p.from.Views {
		if p.to.View(v.Name) == nil {
			p.emit("DROP VIEW %s;", q(v.Name))
		}
	}
	// routines that go or change, after the view drops above (a view may call a function)
	// and before the table drops below (their bodies may still name a table that goes).
	p.dropRoutines()
	for _, f := range gone {
		p.emit("DROP TABLE %s;", q(f.Name))
	}
	// parts of tables that stay: columns (with what depends on them), keys, foreign keys,
	// checks that the target no longer has
	for _, t := range p.to.Tables {
		f, _ := p.pair(t)
		if f == nil {
			continue
		}
		p.dropParts(f, t)
	}
}

func (p *planner) dropParts(f, t *schema.Table) {
	tcols := map[string]bool{}
	for _, c := range t.Columns {
		tcols[c.Name] = true
	}
	var goneCols []string
	var goneColObjs []*schema.Column
	for _, c := range f.Columns {
		if !tcols[p.toCol(f.Name, c.Name)] {
			goneCols = append(goneCols, c.Name)
			goneColObjs = append(goneColObjs, c)
			if !p.droppable[f.Name+"."+c.Name] {
				p.problem("column %s.%s is dropped, which no @migrate declares (`-- @migrate drop %s.%s`, or a rename)", f.Name, c.Name, f.Name, c.Name)
			}
		}
	}
	// foreign keys the target lacks, or that a dropped / changed column takes with them
	tfks := foreignKeysByName(t)
	var keptFKs []*schema.ForeignKey
	for _, fk := range f.ForeignKeys {
		name := f.ForeignKeyName(fk)
		if p.droppedFKs[f.Name+"."+name] {
			continue
		}
		tf, ok := tfks[name]
		if !ok || !sameProps(diff.ForeignKeyProps(p.renamedFK(f, fk)), diff.ForeignKeyProps(tf)) || touches(fk.Columns, goneCols) {
			p.emit("ALTER TABLE %s DROP FOREIGN KEY %s;", q(f.Name), q(name))
		} else {
			keptFKs = append(keptFKs, fk)
		}
	}
	tkeys := keysByName(t)
	var dropKeys []*schema.Key
	for _, k := range f.Keys {
		name := keyName(k)
		tk, ok := tkeys[name]
		// the key's own columns are translated through any `-- @migrate rename` before the
		// comparison, the same way renamedFK is above and renamedKey is in addParts: a
		// rename of one of the key's columns must not, on its own, make the (otherwise
		// unchanged) key look different and get dropped.
		if !ok || !sameProps(diff.KeyProps(renamedKey(p, f, k)), diff.KeyProps(tk)) || touchesParts(k, goneCols) {
			dropKeys = append(dropKeys, k)
		}
	}
	p.dropKeysOf(f, t, dropKeys, keptFKs)
	tchecks := checksByName(t)
	for _, c := range f.Checks {
		name := f.CheckName(c)
		tc, ok := tchecks[name]
		if !ok || !sameProps(diff.CheckProps(f, c), diff.CheckProps(t, tc)) {
			p.emit("ALTER TABLE %s DROP CHECK %s;", q(f.Name), q(name))
		}
	}
	for _, c := range orderGoneColumns(goneColObjs) {
		p.emit("ALTER TABLE %s DROP COLUMN %s;", q(f.Name), q(c.Name))
	}
}

// dropKeysOf emits the DROP PRIMARY KEY / DROP INDEX statements for dropKeys (the from-side
// keys dropParts found changed or gone). MySQL requires an AUTO_INCREMENT column to be the
// leading column of some index at every statement boundary (Error 1075 the instant it is
// not, measured -- not only checked once at the end of a script): dropping dropKeys alone
// can leave such a column with no index at all, if the key that covered it is going and the
// target's replacement key for it has not been added yet (addParts only runs later, once
// every column exists). When that would happen, the drop and that replacement ADD are folded
// into one ALTER TABLE (measured against mysqld 8.4: DROP PRIMARY KEY and ADD KEY on the
// auto_increment column, as two clauses of the same statement, succeed where two separate
// statements do not -- the invariant is only checked once the whole ALTER TABLE has applied
// all of its clauses), and addParts is told (earlyKeys) not to add that key again.
//
// A foreign key that stays (keptFKs), and the columns other tables' foreign keys reference,
// have the same need: they must lead some index at every statement boundary (Error 1553 "Cannot drop index: needed in a foreign key
// constraint" on the DROP INDEX, measured -- the index the server created for the
// constraint goes when the target declares its own key over the same columns, so the
// dropped and the replacing key are the same fold).
func (p *planner) dropKeysOf(f, t *schema.Table, dropKeys []*schema.Key, keptFKs []*schema.ForeignKey) {
	if len(dropKeys) == 0 {
		return
	}
	dropping := map[string]bool{}
	for _, k := range dropKeys {
		dropping[keyName(k)] = true
	}
	fkeys := keysByName(f)
	var adds []string
	added := map[string]bool{}
	// coverEarly folds the target's key leading with cols (the from side's names) into this
	// ALTER when no surviving from-side key leads with them.
	coverEarly := func(cols []string) {
		for _, k := range f.Keys {
			if !dropping[keyName(k)] && leadsWith(k, cols) {
				return
			}
		}
		toCols := make([]string, len(cols))
		for i, c := range cols {
			toCols[i] = p.toCol(f.Name, c)
		}
		for _, tk := range t.Keys {
			if !leadsWith(tk, toCols) || added[keyName(tk)] {
				continue
			}
			fk, ok := fkeys[keyName(tk)]
			if ok && !dropping[keyName(fk)] && sameProps(diff.KeyProps(renamedKey(p, f, fk)), diff.KeyProps(tk)) && !touchesParts(fk, goneColumns(p, f, t)) {
				continue // this key of the target already exists unchanged, not being added
			}
			adds = append(adds, "ADD "+keyText(tk))
			added[keyName(tk)] = true
			if p.earlyKeys == nil {
				p.earlyKeys = map[string]bool{}
			}
			p.earlyKeys[t.Name+"."+keyName(tk)] = true
			return
		}
	}
	for _, ai := range f.Columns {
		if ai.AutoIncrement {
			coverEarly([]string{ai.Name})
		}
	}
	for _, fk := range keptFKs {
		coverEarly(fk.Columns)
	}
	// and the referenced side: a foreign key of another table pointing at f needs f's
	// referenced columns to lead an index just the same (the same 1553 on DROP PRIMARY KEY
	// when the primary key moves off a referenced column, measured)
	for _, other := range p.from.Tables {
		if other == f {
			continue
		}
		for _, fk := range other.ForeignKeys {
			if strings.EqualFold(fk.RefTable, f.Name) {
				coverEarly(fk.RefColumns)
			}
		}
	}
	var clauses []string
	for _, k := range dropKeys {
		if k.Kind == schema.Primary {
			clauses = append(clauses, "DROP PRIMARY KEY")
		} else {
			clauses = append(clauses, "DROP INDEX "+q(k.Name))
		}
	}
	clauses = append(clauses, adds...)
	p.emit("ALTER TABLE %s %s;", q(f.Name), strings.Join(clauses, ", "))
}

// leadsWith reports whether k's leading parts are cols, in order (the index a foreign key
// or an AUTO_INCREMENT column needs).
func leadsWith(k *schema.Key, cols []string) bool {
	if len(k.Parts) < len(cols) {
		return false
	}
	for i, c := range cols {
		if !strings.EqualFold(k.Parts[i].Column, c) {
			return false
		}
	}
	return true
}

// dropTriggers drops every trigger that the target lacks or whose definition differs
// (MySQL has no CREATE OR REPLACE TRIGGER: a changed trigger is a DROP and a CREATE, the
// CREATE emitted by addTriggers). gone is the from tables the target no longer has: their
// triggers go with them (DROP TABLE takes them silently) and are not dropped here.
func (p *planner) dropTriggers(gone map[string]bool) {
	for _, t := range p.from.Triggers {
		if gone[t.Table] {
			continue
		}
		tt := p.to.Trigger(t.Name)
		if tt == nil || diff.TriggerProps(t)["definition"] != diff.TriggerProps(tt)["definition"] {
			p.emit("DROP TRIGGER %s;", q(t.Name))
		}
	}
}

// dropRoutines drops every procedure/function that the target lacks or whose definition
// differs (MySQL's CREATE PROCEDURE/FUNCTION has no OR REPLACE either).
func (p *planner) dropRoutines() {
	for _, r := range p.from.Routines {
		tr := p.to.RoutineOf(r.Kind, r.Name)
		if tr == nil || diff.RoutineProps(r)["definition"] != diff.RoutineProps(tr)["definition"] {
			p.emit("DROP %s %s;", r.Kind, q(r.Name))
		}
	}
}

// addRoutines creates every procedure/function the from side lacks or whose definition
// differs, from the target's own CREATE text.
func (p *planner) addRoutines() {
	for _, r := range p.to.Routines {
		fr := p.from.RoutineOf(r.Kind, r.Name)
		if fr != nil && diff.RoutineProps(fr)["definition"] == diff.RoutineProps(r)["definition"] {
			continue
		}
		p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(r.Definition), ";"))
	}
}

// addTriggers creates every trigger the from side lacks or whose definition differs, from
// the target's own CREATE text (which already carries the FOLLOWS clause dump.Read gives
// it, so the target's firing order is reproduced).
func (p *planner) addTriggers() {
	for _, t := range p.to.Triggers {
		ft := p.from.Trigger(t.Name)
		if ft != nil && diff.TriggerProps(ft)["definition"] == diff.TriggerProps(t)["definition"] {
			continue
		}
		p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(t.Definition), ";"))
	}
}

// dropEvents drops every event the target lacks or whose definition differs (MySQL has no
// CREATE OR REPLACE EVENT: a changed event is a DROP and a CREATE, the CREATE emitted by
// addEvents). A time the target leaves to the server (an omitted STARTS) is not a
// difference (diff.EventChanged).
func (p *planner) dropEvents() {
	for _, e := range p.from.Events {
		te := p.to.Event(e.Name)
		if te == nil || diff.EventChanged(e, te) {
			p.emit("DROP EVENT %s;", q(e.Name))
		}
	}
}

// addEvents creates every event the from side lacks or whose definition differs, from the
// target's own CREATE text.
func (p *planner) addEvents() {
	for _, e := range p.to.Events {
		fe := p.from.Event(e.Name)
		if fe != nil && !diff.EventChanged(fe, e) {
			continue
		}
		p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(e.Definition), ";"))
	}
}

// renamedFK is fk with its column and table names as the target spells them, for
// comparison with the target's.
func (p *planner) renamedFK(f *schema.Table, fk *schema.ForeignKey) *schema.ForeignKey {
	r := *fk
	r.Columns = nil
	for _, c := range fk.Columns {
		r.Columns = append(r.Columns, p.toCol(f.Name, c))
	}
	r.RefTable = p.toName(fk.RefTable)
	r.RefColumns = nil
	for _, c := range fk.RefColumns {
		r.RefColumns = append(r.RefColumns, p.toCol(fk.RefTable, c))
	}
	return &r
}

// --- renames ---------------------------------------------------------------------

func (p *planner) renames() {
	var froms []string
	for f := range p.tableRename {
		froms = append(froms, f)
	}
	sort.Strings(froms)
	for _, f := range froms {
		p.emit("RENAME TABLE %s TO %s;", q(f), q(p.tableRename[f]))
	}
	var tables []string
	for t := range p.colRename {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		var cols []string
		for c := range p.colRename[t] {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		f, tt := p.from.Table(t), p.to.Table(p.toName(t))
		for _, c := range cols {
			clauses := []string{fmt.Sprintf("RENAME COLUMN %s TO %s", q(c), q(p.colRename[t][c]))}
			// MySQL refuses to rename a column a generated column reads (Error 3108 "has a
			// generated column dependency", measured against mysqld 8.4) unless the same
			// ALTER TABLE also rewrites every such generated column with the new name: the
			// target's definitions of them ride along, and alterTable skips them.
			if f != nil && tt != nil {
				for _, g := range f.Columns {
					if !readsColumn(g, &schema.Column{Name: c}) {
						continue
					}
					tg := tt.Column(p.toCol(t, g.Name))
					if tg == nil || tg.Text == "" {
						continue
					}
					clauses = append(clauses, "MODIFY COLUMN "+tg.Text)
					if p.earlyMods == nil {
						p.earlyMods = map[string]bool{}
					}
					p.earlyMods[tt.Name+"."+tg.Name] = true
				}
			}
			p.emit("ALTER TABLE %s %s;", q(p.toName(t)), strings.Join(clauses, ", "))
		}
	}
}

// changedColumns are the target names of the pair f, t's columns whose definition (type,
// nullability, default, ...) differs between them, ignoring position: the columns alterTable
// is about to MODIFY.
func changedColumns(p *planner, f, t *schema.Table) map[string]bool {
	fcols := map[string]*schema.Column{}
	for _, c := range f.Columns {
		fcols[p.toCol(f.Name, c.Name)] = c
	}
	out := map[string]bool{}
	for _, c := range t.Columns {
		fc, ok := fcols[c.Name]
		if ok && diff.Definition(fc.Text, fc.Name) != diff.Definition(c.Text, c.Name) {
			out[c.Name] = true
		}
	}
	return out
}

// coordinateForeignKeys drops (ahead of alters, the MODIFY COLUMNs below) every foreign key
// that persists unchanged in the target but whose referencing or referenced column is about
// to change type, and marks it so addParts re-adds it once both sides are done: MySQL checks
// a foreign key's type compatibility the moment either side's ALTER runs, and the planner
// alters one table at a time (alphabetically, since dump.Read lists tables in that order), so
// a parent's MODIFY and the child's MODIFY are never in the same statement for the server to
// see together (measured against mysqld 8.4: Error 3780, Referencing column ... incompatible,
// on the very first MODIFY, parent or child, whichever runs first). Dropping the foreign key
// first and re-adding it after both MODIFYs is the minimal fix measured to work; combining a
// table's own MODIFY and its foreign key's maintenance into one ALTER TABLE does not help
// here since the two tables are always separate statements regardless.
func (p *planner) coordinateForeignKeys() {
	changed := map[string]map[string]bool{} // from table name -> its changing target column names
	for _, t := range p.to.Tables {
		f, _ := p.pair(t)
		if f == nil {
			continue
		}
		changed[f.Name] = changedColumns(p, f, t)
	}
	for _, t := range p.to.Tables {
		f, _ := p.pair(t)
		if f == nil {
			continue
		}
		tfks := foreignKeysByName(t)
		for _, fk := range f.ForeignKeys {
			name := f.ForeignKeyName(fk)
			if p.droppedFKs[f.Name+"."+name] {
				continue // already handled (e.g. a table it references is going)
			}
			tf, ok := tfks[name]
			if !ok || !sameProps(diff.ForeignKeyProps(p.renamedFK(f, fk)), diff.ForeignKeyProps(tf)) {
				continue // dropParts already drops a foreign key that changes or goes
			}
			touched := false
			for _, c := range fk.Columns {
				if changed[f.Name][p.toCol(f.Name, c)] {
					touched = true
				}
			}
			if refFrom := p.from.Table(fk.RefTable); refFrom != nil {
				for _, c := range fk.RefColumns {
					if changed[refFrom.Name][p.toCol(refFrom.Name, c)] {
						touched = true
					}
				}
			}
			if !touched {
				continue
			}
			// after renames: the table already bears its target name (measured: the from
			// name is Error 1146 here when the table was renamed)
			p.emit("ALTER TABLE %s DROP FOREIGN KEY %s;", q(p.toName(f.Name)), q(name))
			if p.droppedFKs == nil {
				p.droppedFKs = map[string]bool{}
			}
			p.droppedFKs[f.Name+"."+name] = true
		}
	}
}

// --- alters ----------------------------------------------------------------------

func (p *planner) alters() {
	for _, t := range p.to.Tables {
		f, _ := p.pair(t)
		if f == nil {
			continue
		}
		p.alterTable(f, t)
	}
	// views that change are replaced in adds, once every column they may read exists
}

func (p *planner) alterTable(f, t *schema.Table) {
	var opts []string
	fp, tp := diff.TableProps(f), diff.TableProps(t)
	if fp["engine"] != tp["engine"] && t.Engine != "" {
		opts = append(opts, "ENGINE="+t.Engine)
	}
	if fp["charset"] != tp["charset"] || fp["collation"] != tp["collation"] {
		if t.Charset != "" {
			opts = append(opts, "DEFAULT CHARSET="+t.Charset)
		}
		if t.Collation != "" {
			opts = append(opts, "COLLATE="+t.Collation)
		}
	}
	if fp["comment"] != tp["comment"] {
		opts = append(opts, "COMMENT="+lit(t.Comment))
	}
	if len(opts) > 0 {
		p.emit("ALTER TABLE %s %s;", q(t.Name), strings.Join(opts, " "))
	}
	if fp["partitioned"] != tp["partitioned"] {
		p.problem("table %s: partitioning differs; the plan does not write PARTITION BY clauses", t.Name)
	}
	// columns: the ENUM label drops first (an UPDATE the type change needs), then MODIFY
	// for every column whose definition or position differs
	fcols := map[string]*schema.Column{}
	for _, c := range f.Columns {
		fcols[p.toCol(f.Name, c.Name)] = c
	}
	for i, c := range t.Columns {
		fc, ok := fcols[c.Name]
		if !ok {
			continue // added in adds
		}
		for _, in := range p.enumDrops {
			if in.Table == t.Name && in.Column == c.Name {
				sql := fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s", q(t.Name), q(c.Name), lit(in.Using), q(c.Name), lit(in.Label))
				if !hasLabel(c, in.Using) {
					p.problem("line %d: enum %s.%s: %s is not a label of the target column", in.Line, in.Table, in.Column, lit(in.Using))
				}
				p.emit("%s;", sql)
			}
		}
		_ = i
		if diff.Definition(fc.Text, fc.Name) == diff.Definition(c.Text, c.Name) || p.earlyMods[t.Name+"."+c.Name] {
			continue // the position, if it differs, is settled once every column exists (reorder)
		}
		p.emit("ALTER TABLE %s MODIFY COLUMN %s;", q(t.Name), c.Text)
	}
}

// reorder moves the columns of a table that stays into the target's order, once the
// dropped ones are gone and the added ones are in: it follows the order the earlier
// statements leave and emits a MODIFY COLUMN ... FIRST / AFTER for each column out of
// place, fewest moves first to last.
func (p *planner) reorder(f, t *schema.Table) {
	var cur []string
	gone := map[string]bool{}
	for _, c := range goneColumns(p, f, t) {
		gone[c] = true
	}
	for _, c := range f.Columns {
		if !gone[c.Name] {
			cur = append(cur, p.toCol(f.Name, c.Name))
		}
	}
	have := map[string]bool{}
	for _, c := range cur {
		have[c] = true
	}
	newCols, positioned := newColumnsOrder(t, have)
	if positioned {
		// addParts placed each one directly at its target position (AFTER its target
		// predecessor), the same as this loop assumed before dependency ordering existed.
		for i, c := range t.Columns {
			if have[c.Name] {
				continue
			}
			at := 0
			if i > 0 {
				at = indexOf(cur, t.Columns[i-1].Name) + 1
			}
			cur = append(cur[:at], append([]string{c.Name}, cur[at:]...)...)
			have[c.Name] = true
		}
	} else {
		// a generated column among the new ones reads another new column out of the
		// target's own order: addParts appended them all at the end of the table instead,
		// in dependency-safe order, and the loop below reaches each one's real target
		// position via an explicit MODIFY COLUMN.
		for _, c := range newCols {
			cur = append(cur, c.Name)
			have[c.Name] = true
		}
	}
	for i, c := range t.Columns {
		if i < len(cur) && cur[i] == c.Name {
			continue
		}
		clause := "MODIFY COLUMN " + c.Text
		if i == 0 {
			clause += " FIRST"
		} else {
			clause += " AFTER " + q(t.Columns[i-1].Name)
		}
		p.emit("ALTER TABLE %s %s;", q(t.Name), clause)
		j := indexOf(cur, c.Name)
		cur = append(cur[:j], cur[j+1:]...)
		cur = append(cur[:i], append([]string{c.Name}, cur[i:]...)...)
	}
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func hasLabel(c *schema.Column, label string) bool {
	for _, v := range c.Type.Values {
		if v == label {
			return true
		}
	}
	return false
}

// --- adds ------------------------------------------------------------------------

// adds emits what the target has and the from side lacks, in an order every statement's
// dependencies allow (measured against mysqld 8.4, each the probe's finding): the columns,
// keys and checks of tables that stay first, since a brand new table's own CREATE TABLE may
// reference a column those tables are only now gaining (Error 3734 "Missing column ... in the
// referenced table" otherwise); then the new tables, parents before children; then the
// foreign keys of the tables that stay, which may reference a new table; then the tables'
// column order; then the views, new and replaced alike, in the target's dependency order,
// since a view's new definition may read a column added just above (Error 1054 otherwise).
func (p *planner) adds() {
	// routines ahead of everything else: a view (below) may call one.
	p.addRoutines()
	staying := map[*schema.Table]*schema.Table{}
	for _, t := range p.to.Tables {
		if f, _ := p.pair(t); f != nil {
			staying[t] = f
			p.addParts(f, t)
		}
	}
	// new tables, parents before children
	var fresh []*schema.Table
	for _, t := range p.to.Tables {
		if p.from.Table(p.fromName(t.Name)) == nil {
			fresh = append(fresh, t)
		}
	}
	for _, t := range fkOrder(fresh) {
		def := strings.TrimSuffix(strings.TrimSpace(t.Definition), ";")
		if t.AutoIncrementStart != "" {
			// t.Definition is the canonical CREATE TABLE text, which never carries
			// AUTO_INCREMENT=<n> (dump.normalizeTable strips a live counter from every
			// canonicalized table); a brand new table's own declared starting value
			// (dump.pinAutoIncrement) is a schema decision, not data, and belongs on the
			// CREATE that brings the table into existence.
			def += " AUTO_INCREMENT=" + t.AutoIncrementStart
		}
		p.emit("%s;", def)
	}
	for _, t := range p.to.Tables {
		if f := staying[t]; f != nil {
			p.addForeignKeys(f, t)
			p.reorder(f, t)
		}
	}
	for _, v := range p.to.Views {
		if f := p.from.View(v.Name); f == nil {
			p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(v.Definition), ";"))
		} else if !sameProps(diff.ViewProps(f), diff.ViewProps(v)) {
			p.emit("CREATE OR REPLACE %s;", diff.ViewProps(v)["definition"])
		}
	}
}

func (p *planner) addParts(f, t *schema.Table) {
	fcols := map[string]bool{}
	for _, c := range f.Columns {
		fcols[p.toCol(f.Name, c.Name)] = true
	}
	// Ordinarily a new column is added directly at its target position (AFTER its target
	// predecessor, same as always). But a generated column reading a column added alongside
	// it must not run before that column exists (MySQL resolves a generated expression
	// against the table as it stands at that ADD, not the final target -- measured: Error
	// 1054, Unknown column, otherwise); newColumnsOrder reports when the target's own order
	// does not already satisfy every such dependency, and then the new columns go in at the
	// end instead, in dependency-safe order, with no position clause -- reorder (below,
	// after addParts for every table) moves every column into its final target position
	// with its own MODIFY COLUMN FIRST/AFTER pass once all columns exist.
	newCols, positioned := newColumnsOrder(t, fcols)
	targetIndex := map[string]int{}
	for i, c := range t.Columns {
		targetIndex[c.Name] = i
	}
	for _, c := range newCols {
		clause := "ADD COLUMN " + c.Text
		if positioned {
			if i := targetIndex[c.Name]; i == 0 {
				clause += " FIRST"
			} else {
				clause += " AFTER " + q(t.Columns[i-1].Name)
			}
		}
		p.emit("ALTER TABLE %s %s;", q(t.Name), clause)
	}
	fkeys := keysByName(f)
	for _, k := range t.Keys {
		if p.earlyKeys[t.Name+"."+keyName(k)] {
			continue // dropKeysOf already added this one, folded into its own DROP
		}
		fk, ok := fkeys[keyName(k)]
		if ok && sameProps(diff.KeyProps(renamedKey(p, f, fk)), diff.KeyProps(k)) && !touchesParts(fk, goneColumns(p, f, t)) {
			continue
		}
		p.emit("ALTER TABLE %s ADD %s;", q(t.Name), keyText(k))
	}
	fchecks := checksByName(f)
	for _, c := range t.Checks {
		fc, ok := fchecks[t.CheckName(c)]
		if ok && sameProps(diff.CheckProps(f, fc), diff.CheckProps(t, c)) {
			continue
		}
		p.emit("ALTER TABLE %s ADD %s;", q(t.Name), checkText(t, c))
	}
}

// addForeignKeys adds the foreign keys of a table that stays, after every table and column
// they may reference exists.
func (p *planner) addForeignKeys(f, t *schema.Table) {
	ffks := foreignKeysByName(f)
	for _, fk := range t.ForeignKeys {
		ffk, ok := ffks[t.ForeignKeyName(fk)]
		// coordinateForeignKeys drops an otherwise-unchanged foreign key ahead of a type
		// change on either of its sides (droppedFKs), which must still be re-added here.
		if ok && sameProps(diff.ForeignKeyProps(p.renamedFK(f, ffk)), diff.ForeignKeyProps(fk)) &&
			!touches(ffk.Columns, goneColumns(p, f, t)) && !p.droppedFKs[f.Name+"."+t.ForeignKeyName(fk)] {
			continue
		}
		p.emit("ALTER TABLE %s ADD %s;", q(t.Name), fkText(t, fk))
	}
}

// goneColumns are the from columns the target lacks (by their from names).
func goneColumns(p *planner, f, t *schema.Table) []string {
	tcols := map[string]bool{}
	for _, c := range t.Columns {
		tcols[c.Name] = true
	}
	var out []string
	for _, c := range f.Columns {
		if !tcols[p.toCol(f.Name, c.Name)] {
			out = append(out, c.Name)
		}
	}
	return out
}

// newColumns are the target's columns (in target order) that have is missing (have keyed by
// target column name): the columns addParts is about to ADD, or reorder has already accounted
// for as added.
func newColumns(t *schema.Table, have map[string]bool) []*schema.Column {
	var out []*schema.Column
	for _, c := range t.Columns {
		if !have[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// newColumnsOrder is the columns a table is gaining (have keyed by target column name, as
// newColumns takes), and whether the target's own order already puts every generated column
// after every other new column it reads: when it does, addParts can add each one directly at
// its target position (AFTER its target predecessor) as it always could; when it does not, a
// real reordering is needed (orderNewColumns), and the caller adds them at the end instead,
// in that safe order, leaving reorder to place them.
func newColumnsOrder(t *schema.Table, have map[string]bool) (cols []*schema.Column, positioned bool) {
	cols = newColumns(t, have)
	ordered := orderNewColumns(cols)
	for i, c := range ordered {
		if cols[i] != c {
			return ordered, false
		}
	}
	return cols, true
}

// readsColumn reports whether a generated column's own definition text reads another column
// of the same ADD/DROP batch, by whether its text names that column (quoted, as the server's
// own canonical spelling of a generated expression always does: GENERATED ALWAYS AS ((`x` +
// 1)) VIRTUAL). Only cols of the same table and the same batch (new columns together, or
// gone columns together) are checked, so this need not parse the expression itself.
func readsColumn(c, other *schema.Column) bool {
	return c.Generated != nil && c.Name != other.Name && strings.Contains(c.Text, q(other.Name))
}

// orderNewColumns orders cols (the columns a table is gaining, in the target's own order) so
// that a generated column comes after every other new column its own expression reads:
// MySQL resolves a generated column's expression against the table as it stands at the ADD
// that creates it, not the table's eventual final shape, so a column ADDed before one it
// reads fails (Error 1054, measured). The position each ADD ends up in the table does not
// depend on this order (reorder moves every column into its target position afterward), only
// which columns already exist when a given ADD runs.
func orderNewColumns(cols []*schema.Column) []*schema.Column {
	var out []*schema.Column
	done := map[string]bool{}
	var visit func(c *schema.Column, stack map[string]bool)
	visit = func(c *schema.Column, stack map[string]bool) {
		if done[c.Name] || stack[c.Name] {
			return
		}
		stack[c.Name] = true
		for _, other := range cols {
			if readsColumn(c, other) {
				visit(other, stack)
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

// orderGoneColumns orders cols (the columns a table is losing) so that a generated column
// comes before every other gone column its own expression reads: MySQL refuses to drop a
// column a live generated column still reads (Error 3108, measured), even when the plan
// intends to drop both, so the generated column must go first.
func orderGoneColumns(cols []*schema.Column) []*schema.Column {
	// orderNewColumns already computes a valid dependency order for this same graph (a
	// generated column after every other column its own expression reads); reversing a
	// topological order of a DAG is a topological order of the reverse graph, so this puts a
	// generated column before every other column it reads, exactly what DROP COLUMN needs.
	fwd := orderNewColumns(cols)
	out := make([]*schema.Column, len(fwd))
	for i, c := range fwd {
		out[len(fwd)-1-i] = c
	}
	return out
}

// renamedKey is k with its columns as the target spells them.
func renamedKey(p *planner, f *schema.Table, k *schema.Key) *schema.Key {
	r := *k
	r.Parts = nil
	for _, part := range k.Parts {
		part.Column = p.toCol(f.Name, part.Column)
		r.Parts = append(r.Parts, part)
	}
	if r.Text != "" {
		for _, part := range k.Parts {
			if to := p.toCol(f.Name, part.Column); to != part.Column {
				r.Text = strings.ReplaceAll(r.Text, q(part.Column), q(to))
			}
		}
	}
	return &r
}

// keyText is the key as an ALTER TABLE ADD clause takes it.
func keyText(k *schema.Key) string {
	if k.Text != "" {
		return k.Text
	}
	return diff.RenderKey(k)
}

// fkText is the foreign key as an ALTER TABLE ADD clause takes it.
func fkText(t *schema.Table, fk *schema.ForeignKey) string {
	if fk.Text != "" {
		return fk.Text
	}
	s := "CONSTRAINT " + q(t.ForeignKeyName(fk)) + " FOREIGN KEY (" + qlist(fk.Columns) + ") REFERENCES " + q(fk.RefTable) + " (" + qlist(fk.RefColumns) + ")"
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}

// checkText is the check as an ALTER TABLE ADD clause takes it.
func checkText(t *schema.Table, c *schema.Check) string {
	if c.Text != "" {
		return c.Text
	}
	props := diff.CheckProps(t, c)
	s := "CONSTRAINT " + q(t.CheckName(c)) + " CHECK " + props["expression"]
	if !c.Enforced {
		s += " NOT ENFORCED"
	}
	return s
}

func qlist(names []string) string {
	var out []string
	for _, n := range names {
		out = append(out, q(n))
	}
	return strings.Join(out, ", ")
}

// --- backfills -------------------------------------------------------------------

func (p *planner) backfills() {
	for _, in := range p.backfillsOf {
		t := p.to.Table(in.Table)
		if t == nil || t.Column(in.Column) == nil {
			p.problem("line %d: backfill %s.%s: no such column in the target schema", in.Line, in.Table, in.Column)
			continue
		}
		sql := fmt.Sprintf("UPDATE %s SET %s = %s", q(in.Table), q(in.Column), in.Expr)
		if in.Where != "" {
			sql += " WHERE " + in.Where
		}
		if err := p.check(sql); err != nil {
			p.problem("line %d: backfill %s.%s: %v", in.Line, in.Table, in.Column, err)
		}
		p.emit("%s;", sql)
	}
}

// --- helpers ---------------------------------------------------------------------

func keyName(k *schema.Key) string {
	if k.Kind == schema.Primary {
		return "PRIMARY"
	}
	return k.Name
}

func keysByName(t *schema.Table) map[string]*schema.Key {
	out := map[string]*schema.Key{}
	for _, k := range t.Keys {
		out[keyName(k)] = k
	}
	return out
}

func foreignKeysByName(t *schema.Table) map[string]*schema.ForeignKey {
	out := map[string]*schema.ForeignKey{}
	for _, fk := range t.ForeignKeys {
		out[t.ForeignKeyName(fk)] = fk
	}
	return out
}

func checksByName(t *schema.Table) map[string]*schema.Check {
	out := map[string]*schema.Check{}
	for _, c := range t.Checks {
		out[t.CheckName(c)] = c
	}
	return out
}

func sameProps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func touches(cols, gone []string) bool {
	for _, c := range cols {
		for _, g := range gone {
			if c == g {
				return true
			}
		}
	}
	return false
}

func touchesParts(k *schema.Key, gone []string) bool {
	for _, p := range k.Parts {
		for _, g := range gone {
			if p.Column == g {
				return true
			}
		}
	}
	return false
}

// fkOrder sorts tables so that a table comes after the tables its foreign keys reference
// (among the given ones); a cycle is left in name order.
func fkOrder(tables []*schema.Table) []*schema.Table {
	byName := map[string]*schema.Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	var out []*schema.Table
	done := map[string]bool{}
	var visit func(t *schema.Table, stack map[string]bool)
	visit = func(t *schema.Table, stack map[string]bool) {
		if done[t.Name] || stack[t.Name] {
			return
		}
		stack[t.Name] = true
		var refs []string
		for _, fk := range t.ForeignKeys {
			if _, ok := byName[fk.RefTable]; ok && fk.RefTable != t.Name {
				refs = append(refs, fk.RefTable)
			}
		}
		sort.Strings(refs)
		for _, r := range refs {
			visit(byName[r], stack)
		}
		done[t.Name] = true
		out = append(out, t)
	}
	for _, t := range tables {
		visit(t, map[string]bool{})
	}
	return out
}

// Split cuts a DDL script into statements the way the mysql client does (at the `;`
// outside quotes and comments), for apply to run them one by one: MySQL's DDL commits
// implicitly, so a script is not a transaction, and the statement that fails is the one
// to report.
func Split(ddl string) []string {
	var out []string
	for _, st := range mysqlparse.Split(ddl) {
		out = append(out, strings.TrimSpace(st.SQL))
	}
	return out
}

// SplitFor is Split under the sql_mode s declares: mysqlparse.Split already cuts a CREATE
// TRIGGER / PROCEDURE / FUNCTION / EVENT body as one statement regardless of mode (mode
// only matters to a candidate cut ambiguous enough that sql_mode decides whether it parses
// as a complete statement on its own), but a plan should still be split under the schema
// it targets rather than the parser's default.
func SplitFor(ddl string, s *schema.Schema) []string {
	var out []string
	for _, st := range mysqlparse.SplitMode(ddl, s.Settings.ParseMode()) {
		out = append(out, strings.TrimSpace(st.SQL))
	}
	return out
}
