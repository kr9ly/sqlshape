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
	p.alters()
	p.adds()
	p.backfills()
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
	for _, c := range f.Columns {
		if !tcols[p.toCol(f.Name, c.Name)] {
			goneCols = append(goneCols, c.Name)
			if !p.droppable[f.Name+"."+c.Name] {
				p.problem("column %s.%s is dropped, which no @migrate declares (`-- @migrate drop %s.%s`, or a rename)", f.Name, c.Name, f.Name, c.Name)
			}
		}
	}
	// foreign keys the target lacks, or that a dropped / changed column takes with them
	tfks := foreignKeysByName(t)
	for _, fk := range f.ForeignKeys {
		name := f.ForeignKeyName(fk)
		if p.droppedFKs[f.Name+"."+name] {
			continue
		}
		tf, ok := tfks[name]
		if !ok || !sameProps(diff.ForeignKeyProps(p.renamedFK(f, fk)), diff.ForeignKeyProps(tf)) || touches(fk.Columns, goneCols) {
			p.emit("ALTER TABLE %s DROP FOREIGN KEY %s;", q(f.Name), q(name))
		}
	}
	tkeys := keysByName(t)
	for _, k := range f.Keys {
		name := keyName(k)
		tk, ok := tkeys[name]
		if !ok || !sameProps(diff.KeyProps(k), diff.KeyProps(tk)) || touchesParts(k, goneCols) {
			if k.Kind == schema.Primary {
				p.emit("ALTER TABLE %s DROP PRIMARY KEY;", q(f.Name))
			} else {
				p.emit("ALTER TABLE %s DROP INDEX %s;", q(f.Name), q(k.Name))
			}
		}
	}
	tchecks := checksByName(t)
	for _, c := range f.Checks {
		name := f.CheckName(c)
		tc, ok := tchecks[name]
		if !ok || !sameProps(diff.CheckProps(f, c), diff.CheckProps(t, tc)) {
			p.emit("ALTER TABLE %s DROP CHECK %s;", q(f.Name), q(name))
		}
	}
	for _, c := range goneCols {
		p.emit("ALTER TABLE %s DROP COLUMN %s;", q(f.Name), q(c))
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
		for _, c := range cols {
			p.emit("ALTER TABLE %s RENAME COLUMN %s TO %s;", q(p.toName(t)), q(c), q(p.colRename[t][c]))
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
	for _, v := range p.to.Views {
		if f := p.from.View(v.Name); f != nil && !sameProps(diff.ViewProps(f), diff.ViewProps(v)) {
			p.emit("CREATE OR REPLACE %s;", diff.ViewProps(v)["definition"])
		}
	}
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
		if diff.Definition(fc.Text, fc.Name) == diff.Definition(c.Text, c.Name) {
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
	// the added columns went in AFTER their target predecessor (addParts)
	have := map[string]bool{}
	for _, c := range cur {
		have[c] = true
	}
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

func (p *planner) adds() {
	// new tables, parents before children
	var fresh []*schema.Table
	for _, t := range p.to.Tables {
		if p.from.Table(p.fromName(t.Name)) == nil {
			fresh = append(fresh, t)
		}
	}
	for _, t := range fkOrder(fresh) {
		p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(t.Definition), ";"))
	}
	// parts of tables that stay, then their columns' order
	for _, t := range p.to.Tables {
		f, _ := p.pair(t)
		if f == nil {
			continue
		}
		p.addParts(f, t)
		p.reorder(f, t)
	}
	// views: new ones, and the ones added after the tables they read
	for _, v := range p.to.Views {
		if p.from.View(v.Name) == nil {
			p.emit("%s;", strings.TrimSuffix(strings.TrimSpace(v.Definition), ";"))
		}
	}
}

func (p *planner) addParts(f, t *schema.Table) {
	fcols := map[string]bool{}
	for _, c := range f.Columns {
		fcols[p.toCol(f.Name, c.Name)] = true
	}
	for i, c := range t.Columns {
		if fcols[c.Name] {
			continue
		}
		clause := "ADD COLUMN " + c.Text
		if i == 0 {
			clause += " FIRST"
		} else {
			clause += " AFTER " + q(t.Columns[i-1].Name)
		}
		p.emit("ALTER TABLE %s %s;", q(t.Name), clause)
	}
	fkeys := keysByName(f)
	for _, k := range t.Keys {
		fk, ok := fkeys[keyName(k)]
		if ok && sameProps(diff.KeyProps(renamedKey(p, f, fk)), diff.KeyProps(k)) && !touchesParts(fk, goneColumns(p, f, t)) {
			continue
		}
		p.emit("ALTER TABLE %s ADD %s;", q(t.Name), keyText(k))
	}
	ffks := foreignKeysByName(f)
	for _, fk := range t.ForeignKeys {
		ffk, ok := ffks[t.ForeignKeyName(fk)]
		if ok && sameProps(diff.ForeignKeyProps(p.renamedFK(f, ffk)), diff.ForeignKeyProps(fk)) && !touches(ffk.Columns, goneColumns(p, f, t)) {
			continue
		}
		p.emit("ALTER TABLE %s ADD %s;", q(t.Name), fkText(t, fk))
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
