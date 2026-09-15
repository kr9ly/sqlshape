package migrate

// The migrate probe: an oracle for Plan that a hand-written adversarial round cannot be. The
// second adversarial round's migrate lane found six problems, all of one class -- the order
// the plan's DDL runs in against a real server -- and a hand-picked case only ever catches
// the one instance its author thought of. This probe generates schemas from the vocabulary
// the planner handles (column types, generated columns, keys, foreign keys, AUTO_INCREMENT,
// CHECKs, views, triggers, functions), mutates each into a target (columns added / dropped /
// renamed / retyped / moved, keys and foreign keys attached and detached, the primary key
// moved, tables added / dropped / renamed, ENUM labels, views, triggers, functions), and
// judges the plan the way `sqlshape apply` would live: the DDL runs on a server holding the
// source, the result must read back as the target's canonical form, and a second Plan from
// there must be empty. Every pair is a change a real mysqld accepts written as one schema
// (the target canonicalizes cleanly before the plan is judged), so a refused statement or a
// leftover difference is the planner's.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// A failing pair is minimized (mutations removed while the failure stands) and written to
// the report with its source, target, DDL and what went wrong; the test fails on any finding.
// Skipped without a mysqld on PATH (nix-shell -p mysql84).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- the model: a schema the generator can render and mutate --------------------------

type pSchema struct {
	tables   []*pTable
	views    []*pView
	triggers []*pTrigger
	funcs    []*pFunc
	intents  []string // `-- @migrate` lines the mutations declared (target side only)
	seq      int      // name counter
}

type pTable struct {
	name    string
	orig    string // the name the source schema knows the table by (a declaration's left side)
	cols    []*pCol
	keys    []*pKey
	fks     []*pFK
	checks  []*pCheck
	comment string
	autoInc int // AUTO_INCREMENT=<n>, 0 for none
}

type pCol struct {
	name    string
	typ     string // as rendered: int, bigint unsigned, varchar(50), ...
	labels  []string
	notNull bool
	def     string // default expression text, "" for none
	pk      bool
	auto    bool
	gen     string // the column this generated column reads ("" for a plain column)
	stored  bool
}

type pKey struct {
	name   string
	unique bool
	cols   []string
}

type pFK struct {
	name     string
	col      string
	refTable string
	refCol   string
	onDelete string
}

type pCheck struct {
	name string
	col  string
}

type pView struct {
	name  string
	table string
	cols  []string
}

type pTrigger struct {
	name  string
	table string
	col   string // the column its body reads and sets (NEW.col)
	n     int    // the constant the body adds (a body change is a different n)
}

type pFunc struct {
	name string
	n    int
}

func (s *pSchema) next(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%d", prefix, s.seq)
}

func (s *pSchema) clone() *pSchema {
	c := &pSchema{seq: s.seq}
	for _, t := range s.tables {
		nt := &pTable{name: t.name, orig: t.orig, comment: t.comment, autoInc: t.autoInc}
		for _, col := range t.cols {
			nc := *col
			nc.labels = append([]string(nil), col.labels...)
			nt.cols = append(nt.cols, &nc)
		}
		for _, k := range t.keys {
			nk := *k
			nk.cols = append([]string(nil), k.cols...)
			nt.keys = append(nt.keys, &nk)
		}
		for _, fk := range t.fks {
			nfk := *fk
			nt.fks = append(nt.fks, &nfk)
		}
		for _, ck := range t.checks {
			nck := *ck
			nt.checks = append(nt.checks, &nck)
		}
		c.tables = append(c.tables, nt)
	}
	for _, v := range s.views {
		nv := *v
		nv.cols = append([]string(nil), v.cols...)
		c.views = append(c.views, &nv)
	}
	for _, tr := range s.triggers {
		ntr := *tr
		c.triggers = append(c.triggers, &ntr)
	}
	for _, f := range s.funcs {
		nf := *f
		c.funcs = append(c.funcs, &nf)
	}
	return c
}

func (s *pSchema) table(name string) *pTable {
	for _, t := range s.tables {
		if t.name == name {
			return t
		}
	}
	return nil
}

func (t *pTable) col(name string) *pCol {
	for _, c := range t.cols {
		if c.name == name {
			return c
		}
	}
	return nil
}

// inFK: a CHECK may not read a column a foreign key's referential action writes (Error 3823).
func (t *pTable) inFK(col string) bool {
	for _, fk := range t.fks {
		if fk.col == col {
			return true
		}
	}
	return false
}

func (t *pTable) pk() *pCol {
	for _, c := range t.cols {
		if c.pk {
			return c
		}
	}
	return nil
}

// ---- the type vocabulary --------------------------------------------------------------

var probeTypes = []string{"int", "bigint unsigned", "varchar(50)", "decimal(10,2)", "datetime(6)", "date", "text", "enum", "json", "tinyint(1)", "smallint", "double"}

func isNumeric(typ string) bool {
	switch {
	case strings.HasPrefix(typ, "int"), strings.HasPrefix(typ, "bigint"), strings.HasPrefix(typ, "decimal"),
		strings.HasPrefix(typ, "tinyint"), strings.HasPrefix(typ, "smallint"), typ == "double":
		return true
	}
	return false
}

func isInteger(typ string) bool {
	return isNumeric(typ) && !strings.HasPrefix(typ, "decimal") && typ != "double"
}

// keyable: a type an index takes without a prefix length.
func keyable(typ string) bool { return typ != "text" && typ != "json" }

func defaultFor(c *pCol) string {
	switch {
	case isInteger(c.typ):
		return "'0'"
	case strings.HasPrefix(c.typ, "decimal"):
		return "'0.00'"
	case c.typ == "double":
		return "'0'"
	case strings.HasPrefix(c.typ, "varchar"):
		return "'x'"
	case c.typ == "datetime(6)":
		return "CURRENT_TIMESTAMP(6)"
	case c.typ == "date":
		return "'2020-01-01'"
	case c.typ == "enum":
		return "'" + c.labels[0] + "'"
	}
	return "" // text / json take no literal default
}

func widen(typ string) string {
	switch typ {
	case "int":
		return "bigint"
	case "smallint", "tinyint(1)":
		return "int"
	case "varchar(50)":
		return "varchar(100)"
	case "decimal(10,2)":
		return "decimal(14,2)"
	case "bigint unsigned":
		return "decimal(20,0)"
	}
	return ""
}

// ---- rendering ------------------------------------------------------------------------

func (c *pCol) typeText() string {
	if c.typ == "enum" {
		quoted := make([]string, len(c.labels))
		for i, l := range c.labels {
			quoted[i] = "'" + l + "'"
		}
		return "enum(" + strings.Join(quoted, ",") + ")"
	}
	return c.typ
}

func (c *pCol) render() string {
	var b strings.Builder
	b.WriteString(q(c.name) + " " + c.typeText())
	if c.gen != "" {
		b.WriteString(" GENERATED ALWAYS AS ((" + q(c.gen) + " + 1))")
		if c.stored {
			b.WriteString(" STORED")
		} else {
			b.WriteString(" VIRTUAL")
		}
		return b.String()
	}
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.auto {
		b.WriteString(" AUTO_INCREMENT")
	} else if c.def != "" {
		b.WriteString(" DEFAULT " + c.def)
	}
	return b.String()
}

func (t *pTable) render() string {
	var parts []string
	for _, c := range t.cols {
		parts = append(parts, "  "+c.render())
	}
	if pk := t.pk(); pk != nil {
		parts = append(parts, "  PRIMARY KEY ("+q(pk.name)+")")
	}
	for _, k := range t.keys {
		kind := "KEY"
		if k.unique {
			kind = "UNIQUE KEY"
		}
		parts = append(parts, "  "+kind+" "+q(k.name)+" ("+qlist(k.cols)+")")
	}
	for _, fk := range t.fks {
		s := "  CONSTRAINT " + q(fk.name) + " FOREIGN KEY (" + q(fk.col) + ") REFERENCES " + q(fk.refTable) + " (" + q(fk.refCol) + ")"
		if fk.onDelete != "" {
			s += " ON DELETE " + fk.onDelete
		}
		parts = append(parts, s)
	}
	for _, ck := range t.checks {
		c := t.col(ck.col)
		expr := "(" + q(ck.col) + " > 0)"
		if !isNumeric(c.typ) {
			expr = "(char_length(" + q(ck.col) + ") > 0)"
		}
		parts = append(parts, "  CONSTRAINT "+q(ck.name)+" CHECK "+expr)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + q(t.name) + " (\n" + strings.Join(parts, ",\n") + "\n) ENGINE=InnoDB")
	if t.autoInc > 0 {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", t.autoInc)
	}
	if t.comment != "" {
		b.WriteString(" COMMENT=" + lit(t.comment))
	}
	b.WriteString(";\n")
	return b.String()
}

func (s *pSchema) render() string {
	var b strings.Builder
	b.WriteString("-- sqlshape: mysql 8.4\n")
	for _, in := range s.intents {
		b.WriteString(in + "\n")
	}
	for _, t := range s.tables {
		b.WriteString(t.render())
	}
	for _, v := range s.views {
		b.WriteString("CREATE VIEW " + q(v.name) + " AS SELECT " + qlist(v.cols) + " FROM " + q(v.table) + ";\n")
	}
	for _, f := range s.funcs {
		fmt.Fprintf(&b, "CREATE FUNCTION %s(a INT) RETURNS INT DETERMINISTIC RETURN a + %d;\n", q(f.name), f.n)
	}
	for _, tr := range s.triggers {
		fmt.Fprintf(&b, "CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW SET NEW.%s = COALESCE(NEW.%s, 0) + %d;\n",
			q(tr.name), q(tr.table), q(tr.col), q(tr.col), tr.n)
	}
	return b.String()
}

// ---- generating a source schema -------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "enum" {
		c.labels = []string{"a", "b", "c"}[:2+r.Intn(2)]
	}
	c.notNull = r.Intn(2) == 0
	if c.notNull || r.Intn(3) == 0 {
		c.def = defaultFor(c)
	}
	return c
}

// numericCols: the plain (not generated, not AUTO_INCREMENT) numeric columns a generated
// column or a trigger body may read.
func (t *pTable) numericCols() []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if isNumeric(c.typ) && c.gen == "" && !c.auto {
			out = append(out, c)
		}
	}
	return out
}

func (s *pSchema) newTable(r *rand.Rand) *pTable {
	t := &pTable{name: s.next("t")}
	t.orig = t.name
	id := &pCol{name: "id", typ: pick(r, []string{"int", "bigint unsigned"}), notNull: true, pk: true, auto: r.Intn(2) == 0}
	t.cols = append(t.cols, id)
	n := 2 + r.Intn(4)
	for i := 0; i < n; i++ {
		t.cols = append(t.cols, s.newCol(r))
	}
	if src := t.numericCols(); len(src) > 0 && r.Intn(3) == 0 {
		g := &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name, stored: r.Intn(2) == 0}
		// before or after the column it reads: both are one CREATE TABLE
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
	}
	if r.Intn(2) == 0 {
		s.addKey(r, t)
	}
	if r.Intn(3) == 0 {
		s.addCheck(r, t)
	}
	if len(s.tables) > 0 && r.Intn(2) == 0 {
		s.addFK(r, t, pick(r, s.tables))
	}
	if r.Intn(4) == 0 {
		t.comment = "about " + t.name
	}
	if id.auto && r.Intn(3) == 0 {
		t.autoInc = 100 + r.Intn(900)
	}
	return t
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if keyable(c.typ) && !c.pk && (c.gen == "" || c.stored) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	r.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	n := 1
	if len(cands) > 1 && r.Intn(2) == 0 {
		n = 2
	}
	t.keys = append(t.keys, &pKey{name: s.next("k"), unique: r.Intn(2) == 0, cols: cands[:n]})
	return true
}

func (s *pSchema) addCheck(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if !c.auto && !c.pk && !t.inFK(c.name) && (isNumeric(c.typ) || strings.HasPrefix(c.typ, "varchar")) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.checks = append(t.checks, &pCheck{name: s.next("ck"), col: pick(r, cands)})
	return true
}

// addFK gives t a new column referencing parent's primary key.
func (s *pSchema) addFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	ppk := parent.pk()
	c := &pCol{name: s.next("r"), typ: ppk.typ, notNull: r.Intn(2) == 0}
	t.cols = append(t.cols, c)
	t.fks = append(t.fks, &pFK{name: s.next("fk"), col: c.name, refTable: parent.name, refCol: ppk.name,
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT"})})
	if t.fks[len(t.fks)-1].onDelete == "SET NULL" {
		c.notNull = false
	}
	return true
}

func (s *pSchema) addView(r *rand.Rand) bool {
	t := pick(r, s.tables)
	var cols []string
	for _, c := range t.cols {
		if r.Intn(2) == 0 {
			cols = append(cols, c.name)
		}
	}
	if len(cols) == 0 {
		cols = []string{t.cols[0].name}
	}
	s.views = append(s.views, &pView{name: s.next("v"), table: t.name, cols: cols})
	return true
}

func (s *pSchema) addTrigger(r *rand.Rand) bool {
	t := pick(r, s.tables)
	src := t.numericCols()
	if len(src) == 0 {
		return false
	}
	s.triggers = append(s.triggers, &pTrigger{name: s.next("tr"), table: t.name, col: pick(r, src).name, n: 1 + r.Intn(9)})
	return true
}

func generate(r *rand.Rand) *pSchema {
	s := &pSchema{}
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		s.tables = append(s.tables, s.newTable(r))
	}
	if r.Intn(2) == 0 {
		s.addView(r)
	}
	if r.Intn(3) == 0 {
		s.addTrigger(r)
	}
	if r.Intn(3) == 0 {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9)})
	}
	return s
}

// ---- mutations ------------------------------------------------------------------------

// A mutation edits the schema toward the target and declares what the diff cannot see; it
// reports false when the schema offers nothing it applies to. touched is the set of names
// ("t", "t.c") a mutation already moved: a second mutation keeps off them so every pair's
// declarations describe one step per object (a column renamed and then dropped is a
// declaration the planner is right to refuse, not a probe of its DDL).
type mutation struct {
	name  string
	apply func(r *rand.Rand, s *pSchema, touched map[string]bool) bool
}

func (t *pTable) plainCols(touched map[string]bool) []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if !c.pk && c.gen == "" && !touched[t.name+"."+c.name] {
			out = append(out, c)
		}
	}
	return out
}

func untouched(s *pSchema, touched map[string]bool) []*pTable {
	var out []*pTable
	for _, t := range s.tables {
		if !touched[t.name] {
			out = append(out, t)
		}
	}
	return out
}

// referencedBy: the foreign keys (in any table) that reference table t's column col.
func referencedBy(s *pSchema, t *pTable, col string) []*pFK {
	var out []*pFK
	for _, other := range s.tables {
		for _, fk := range other.fks {
			if fk.refTable == t.name && fk.refCol == col {
				out = append(out, fk)
			}
		}
	}
	return out
}

// detach removes every part of the schema that reads t's column col: keys and checks over
// it, foreign keys from it, generated columns reading it (dropped, declared), views'
// projections of it (the view goes when it projected nothing else), triggers on it.
func detach(s *pSchema, t *pTable, col string) {
	var keys []*pKey
	for _, k := range t.keys {
		if indexOf(k.cols, col) < 0 {
			keys = append(keys, k)
		}
	}
	t.keys = keys
	var checks []*pCheck
	for _, ck := range t.checks {
		if ck.col != col {
			checks = append(checks, ck)
		}
	}
	t.checks = checks
	var fks []*pFK
	for _, fk := range t.fks {
		if fk.col != col {
			fks = append(fks, fk)
		}
	}
	t.fks = fks
	var cols []*pCol
	for _, c := range t.cols {
		if c.gen == col {
			s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
			detach(s, t, c.name)
			continue
		}
		cols = append(cols, c)
	}
	t.cols = cols
	var views []*pView
	for _, v := range s.views {
		if v.table == t.name {
			var keep []string
			for _, c := range v.cols {
				if c != col {
					keep = append(keep, c)
				}
			}
			if len(keep) == 0 {
				continue
			}
			v.cols = keep
		}
		views = append(views, v)
	}
	s.views = views
	var triggers []*pTrigger
	for _, tr := range s.triggers {
		if tr.table != t.name || tr.col != col {
			triggers = append(triggers, tr)
		}
	}
	s.triggers = triggers
}

// renameRefs spells t's column from as to everywhere it is read.
func renameRefs(s *pSchema, t *pTable, from, to string) {
	for _, k := range t.keys {
		for i, c := range k.cols {
			if c == from {
				k.cols[i] = to
			}
		}
	}
	for _, ck := range t.checks {
		if ck.col == from {
			ck.col = to
		}
	}
	for _, fk := range t.fks {
		if fk.col == from {
			fk.col = to
		}
	}
	for _, c := range t.cols {
		if c.gen == from {
			c.gen = to
		}
	}
	for _, fk := range referencedBy(s, t, from) {
		fk.refCol = to
	}
	for _, v := range s.views {
		if v.table == t.name {
			for i, c := range v.cols {
				if c == from {
					v.cols[i] = to
				}
			}
		}
	}
	for _, tr := range s.triggers {
		if tr.table == t.name && tr.col == from {
			tr.col = to
		}
	}
}

func dropTable(s *pSchema, t *pTable) {
	var tables []*pTable
	for _, other := range s.tables {
		if other == t {
			continue
		}
		var fks []*pFK
		for _, fk := range other.fks {
			if fk.refTable != t.name {
				fks = append(fks, fk)
			}
		}
		other.fks = fks
		tables = append(tables, other)
	}
	s.tables = tables
	var views []*pView
	for _, v := range s.views {
		if v.table != t.name {
			views = append(views, v)
		}
	}
	s.views = views
	var triggers []*pTrigger
	for _, tr := range s.triggers {
		if tr.table != t.name {
			triggers = append(triggers, tr)
		}
	}
	s.triggers = triggers
}

var mutations = []mutation{
	{"add column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var c *pCol
		if src := t.numericCols(); len(src) > 0 && r.Intn(3) == 0 {
			c = &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name, stored: r.Intn(2) == 0}
		} else {
			c = s.newCol(r)
		}
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add generated column reading a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"int", "decimal(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name, stored: r.Intn(2) == 0}
		// the generated column first or last, before or after its source
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
		at = r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{x}, t.cols[at:]...)...)
		touched[t.name+"."+x.name], touched[t.name+"."+g.name] = true, true
		return true
	}},
	{"drop column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if len(t.plainCols(touched)) > 0 {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		c := pick(r, t.plainCols(touched))
		for _, fk := range referencedBy(s, t, c.name) {
			owner := s.table(fkOwner(s, fk))
			var fks []*pFK
			for _, f := range owner.fks {
				if f != fk {
					fks = append(fks, f)
				}
			}
			owner.fks = fks
		}
		s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
		detach(s, t, c.name)
		t.cols = append(t.cols[:indexOfCol(t, c.name)], t.cols[indexOfCol(t, c.name)+1:]...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"drop generated column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.gen != "" && !touched[t.name+"."+c.name] {
					s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
					detach(s, t, c.name)
					t.cols = append(t.cols[:indexOfCol(t, c.name)], t.cols[indexOfCol(t, c.name)+1:]...)
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"rename column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.cols {
			if !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		to := c.name + "_new"
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.name+"."+to)
		renameRefs(s, t, c.name, to)
		touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
		c.name = to
		return true
	}},
	{"widen column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.cols {
			if widen(c.typ) != "" && c.gen == "" && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		w := widen(c.typ)
		if c.auto && !isInteger(w) {
			return false
		}
		for _, fk := range t.fks {
			if fk.col == c.name {
				return false // a referencing column follows its parent's key (widened below)
			}
		}
		// a referenced key and every column referencing it widen together
		for _, fk := range referencedBy(s, t, c.name) {
			child := s.table(fkOwner(s, fk))
			cc := child.col(fk.col)
			if touched[child.name+"."+cc.name] {
				return false
			}
			cc.typ = w
			touched[child.name+"."+cc.name] = true
		}
		c.typ = w
		if c.def != "" {
			c.def = defaultFor(c)
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"move column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		if len(t.cols) < 2 {
			return false
		}
		i := r.Intn(len(t.cols))
		c := t.cols[i]
		if touched[t.name+"."+c.name] {
			return false
		}
		t.cols = append(t.cols[:i], t.cols[i+1:]...)
		j := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:j], append([]*pCol{c}, t.cols[j:]...)...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"change nullability and default", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		cands := t.plainCols(touched)
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		for _, fk := range t.fks {
			if fk.col == c.name && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = defaultFor(c)
		} else {
			c.def = ""
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addKey(r, pick(r, ts))
	}},
	{"drop key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.keys) > 0 {
				i := r.Intn(len(t.keys))
				// the index an AUTO_INCREMENT column or a foreign key (this table's, or one
				// referencing this table) needs stays: without it the target is not a schema
				lead := t.keys[i].cols[0]
				if c := t.col(lead); c.auto || len(referencedBy(s, t, lead)) > 0 {
					return false
				}
				for _, fk := range t.fks {
					if lead == fk.col {
						return false
					}
				}
				t.keys = append(t.keys[:i], t.keys[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"move primary key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		old := t.pk()
		var cands []*pCol
		for _, c := range t.cols {
			if !c.pk && c.gen == "" && isInteger(c.typ) && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 || touched[t.name+"."+old.name] {
			return false
		}
		c := pick(r, cands)
		for _, fk := range t.fks {
			if fk.col == c.name {
				return false
			}
		}
		c.pk, c.notNull, c.def = true, true, ""
		old.pk = false
		// the old key column: an AUTO_INCREMENT column must stay a key, and so must a
		// referenced one
		if old.auto || len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		} else if r.Intn(2) == 0 {
			old.notNull = false
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
	}},
	{"add foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) < 2 {
			return false
		}
		i, j := r.Intn(len(ts)), r.Intn(len(ts))
		if i == j {
			return false
		}
		if i < j {
			i, j = j, i
		}
		t, parent := ts[i], ts[j] // the parent is created first in the rendered schema
		if !s.addFK(r, t, parent) {
			return false
		}
		touched[t.name+"."+t.cols[len(t.cols)-1].name] = true
		return true
	}},
	{"drop foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) > 0 {
				i := r.Intn(len(t.fks))
				t.fks = append(t.fks[:i], t.fks[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"add check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addCheck(r, pick(r, ts))
	}},
	{"drop check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.checks) > 0 {
				i := r.Intn(len(t.checks))
				t.checks = append(t.checks[:i], t.checks[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"add table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		t := s.newTable(r)
		s.tables = append(s.tables, t)
		touched[t.name] = true
		return true
	}},
	{"drop table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) < 2 {
			return false
		}
		t := pick(r, ts)
		for k := range touched {
			if strings.HasPrefix(k, t.name+".") {
				return false // a step already moved one of its columns: one declaration per object
			}
		}
		s.intents = append(s.intents, "-- @migrate drop "+t.orig)
		dropTable(s, t)
		touched[t.name] = true
		return true
	}},
	{"rename table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		to := t.name + "_new"
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> "+to)
		// declarations written before this one name the table on their right side as it
		// will be: the new name
		for i, in := range s.intents {
			s.intents[i] = strings.ReplaceAll(strings.ReplaceAll(in, " -> "+t.name+".", " -> "+to+"."), "enum "+t.name+".", "enum "+to+".")
		}
		for _, other := range s.tables {
			for _, fk := range other.fks {
				if fk.refTable == t.name {
					fk.refTable = to
				}
			}
		}
		for _, v := range s.views {
			if v.table == t.name {
				v.table = to
			}
		}
		for _, tr := range s.triggers {
			if tr.table == t.name {
				tr.table = to
			}
		}
		touched[t.name], touched[to] = true, true
		t.name = to
		return true
	}},
	{"add enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "enum" && !touched[t.name+"."+c.name] {
					c.labels = append(c.labels, "z")
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"drop enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "enum" && len(c.labels) > 1 && !touched[t.name+"."+c.name] {
					gone := c.labels[len(c.labels)-1]
					c.labels = c.labels[:len(c.labels)-1]
					s.intents = append(s.intents, fmt.Sprintf("-- @migrate enum %s.%s: drop '%s' using '%s'", t.name, c.name, gone, c.labels[0]))
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"change generated expression", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.gen == "" || touched[t.name+"."+c.name] {
					continue
				}
				for _, src := range t.numericCols() {
					if src.name != c.gen && !touched[t.name+"."+src.name] {
						c.gen = src.name
						touched[t.name+"."+c.name] = true
						return true
					}
				}
			}
		}
		return false
	}},
	{"move primary key onto a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		old := t.pk()
		if touched[t.name+"."+old.name] {
			return false
		}
		c := &pCol{name: s.next("c"), typ: pick(r, []string{"int", "bigint unsigned"}), notNull: true, pk: true}
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
		old.pk = false
		if old.auto || len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
	}},
	{"table comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		if t.comment == "" {
			t.comment = "now about " + t.name
		} else {
			t.comment = ""
		}
		return true
	}},
	{"add view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addView(r) }},
	{"drop view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.views) == 0 {
			return false
		}
		i := r.Intn(len(s.views))
		s.views = append(s.views[:i], s.views[i+1:]...)
		return true
	}},
	{"change view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.views) == 0 {
			return false
		}
		v := pick(r, s.views)
		t := s.table(v.table)
		for _, c := range t.cols {
			if indexOf(v.cols, c.name) < 0 {
				v.cols = append(v.cols, c.name)
				return true
			}
		}
		return false
	}},
	{"add trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addTrigger(r) }},
	{"drop trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		i := r.Intn(len(s.triggers))
		s.triggers = append(s.triggers[:i], s.triggers[i+1:]...)
		return true
	}},
	{"change trigger body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.n += 10
		return true
	}},
	{"add function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9)})
		return true
	}},
	{"drop function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.funcs) == 0 {
			return false
		}
		i := r.Intn(len(s.funcs))
		s.funcs = append(s.funcs[:i], s.funcs[i+1:]...)
		return true
	}},
	{"change function body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.funcs) == 0 {
			return false
		}
		pick(r, s.funcs).n += 10
		return true
	}},
}

func indexOfCol(t *pTable, name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

func fkOwner(s *pSchema, fk *pFK) string {
	for _, t := range s.tables {
		for _, f := range t.fks {
			if f == fk {
				return t.name
			}
		}
	}
	return ""
}

// step is one mutation with the seed it draws from, so a recipe replays deterministically
// with steps removed.
type step struct {
	m    int
	seed int64
}

// mutate applies the recipe to a copy of src; a step whose mutation finds nothing to apply
// to is skipped. It returns the target and the names of the steps that applied.
func mutate(src *pSchema, recipe []step) (*pSchema, []string) {
	s := src.clone()
	touched := map[string]bool{}
	var applied []string
	for _, st := range recipe {
		r := rand.New(rand.NewSource(st.seed))
		if mutations[st.m].apply(r, s, touched) {
			applied = append(applied, mutations[st.m].name)
		}
	}
	return s, applied
}

// ---- judging --------------------------------------------------------------------------

type verdict struct {
	kind    string // "" when the pair passes
	detail  string
	ddl     []string
	aSQL    string
	bSQL    string
	applied []string
}

// judge plans src -> target and runs the plan on a server holding src. The generator's own
// mistakes (a target the server refuses as one schema) are "generator" verdicts, kept apart
// from the planner's.
func judge(ctx context.Context, c dump.Canonicalizer, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := c.Canonical(ctx, v.aSQL)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := c.Canonical(ctx, v.bSQL)
	if err != nil {
		v.kind, v.detail = "generator (target)", err.Error()
		return v
	}
	if len(a.Problems) > 0 || len(b.Problems) > 0 {
		v.kind, v.detail = "generator (problems)", fmt.Sprint(a.Problems, b.Problems)
		return v
	}
	intents, err := ParseIntents(v.bSQL)
	if err != nil {
		v.kind, v.detail = "generator (intents)", err.Error()
		return v
	}
	ddl, err := Plan(a, b, intents)
	v.ddl = ddl
	if err != nil {
		v.kind, v.detail = "plan refused", err.Error()
		return v
	}
	got, _, err := c.Canonical(ctx, aText+"\nSET FOREIGN_KEY_CHECKS=1;\n"+strings.Join(ddl, "\n"))
	if err != nil {
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	if changes := diff.Compare(got, b); len(changes) > 0 {
		var d []string
		for _, ch := range changes {
			d = append(d, ch.String())
		}
		v.kind, v.detail = "DDL does not reach the target", strings.Join(d, "\n")
		return v
	}
	again, err := Plan(got, b, nil)
	if err != nil || len(again) > 0 {
		v.kind, v.detail = "a second plan is not empty", fmt.Sprintf("%v\n%s", err, strings.Join(again, "\n"))
		return v
	}
	return v
}

func TestMigrateProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	srv, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	} else if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	scratch, err := dump.NewScratch(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewSource(*probeSeed))
	counts := map[string]int{}
	byMutation := map[string]int{}
	var findings []verdict
	for i := 0; i < *probeN; i++ {
		src := generate(r)
		n := 1 + r.Intn(3)
		var recipe []step
		for j := 0; j < n; j++ {
			recipe = append(recipe, step{m: r.Intn(len(mutations)), seed: r.Int63()})
		}
		target, applied := mutate(src, recipe)
		if len(applied) == 0 {
			continue
		}
		v := judge(ctx, scratch, src, target, applied)
		for _, a := range applied {
			byMutation[a]++
		}
		if v.kind == "" {
			counts["pass"]++
			continue
		}
		if strings.HasPrefix(v.kind, "generator") {
			counts[v.kind]++
			if counts[v.kind] <= 3 {
				t.Logf("%s (pair %d): %s\n%s", v.kind, i, v.detail, v.bSQL)
			}
			continue
		}
		counts["finding"]++
		// minimize: drop steps while the failure stands
		for j := 0; j < len(recipe); {
			shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
			tgt, app := mutate(src, shorter)
			if len(app) == 0 {
				j++
				continue
			}
			if w := judge(ctx, scratch, src, tgt, app); w.kind == v.kind {
				recipe, v = shorter, w
				continue
			}
			j++
		}
		findings = append(findings, v)
	}

	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var summary strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&summary, "%s: %d\n", k, counts[k])
	}
	var ms []string
	for m := range byMutation {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	for _, m := range ms {
		fmt.Fprintf(&summary, "  %-45s %d\n", m, byMutation[m])
	}
	t.Logf("migrate probe, seed %d, %d pairs:\n%s", *probeSeed, *probeN, summary.String())

	// the same failure minimized from different pairs reads the same: dedupe by kind + DDL
	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe, seed %d, %d pairs\n\n%s\n", *probeSeed, *probeN, summary.String())
	distinct := 0
	for _, v := range findings {
		key := v.kind + "\n" + v.detail
		if seen[key] {
			continue
		}
		seen[key] = true
		distinct++
		fmt.Fprintf(&report, "## %d. %s\n\nmutations: %s\n\n%s\n\n### source\n\n```sql\n%s```\n\n### target\n\n```sql\n%s```\n\n### plan\n\n```sql\n%s\n```\n\n",
			distinct, v.kind, strings.Join(v.applied, "; "), v.detail, v.aSQL, v.bSQL, strings.Join(v.ddl, "\n"))
	}
	if *probeReport != "" {
		if err := os.WriteFile(*probeReport, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *probeReport)
	} else if distinct > 0 {
		t.Log(report.String())
	}
	if distinct > 0 {
		t.Errorf("%d distinct findings (%d pairs)", distinct, counts["finding"])
	}
}
