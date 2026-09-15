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
	events   []*pEvent
	intents  []string // `-- @migrate` lines the mutations declared (target side only)
	seq      int      // name counter
	mutating bool     // set on a clone: the tables that exist are the source's, and hold rows
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
	// collate: a varchar / text column declared COLLATE utf8mb4_bin; invisible: INVISIBLE;
	// onUpdate: a datetime(6) column with ON UPDATE CURRENT_TIMESTAMP(6)
	collate   bool
	invisible bool
	onUpdate  bool
	fresh     bool // added by a mutation: not in the source, so its drop needs no declaration
}

type pKey struct {
	name   string
	unique bool
	cols   []string
	prefix []int // one per column: a prefix length, 0 for the whole column
}

type pFK struct {
	name     string
	cols     []string
	refTable string
	refCols  []string
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

// pEvent is a CREATE EVENT with a schedule of n hours and a body that sets a user variable.
type pEvent struct {
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
			nk.prefix = append([]int(nil), k.prefix...)
			nt.keys = append(nt.keys, &nk)
		}
		for _, fk := range t.fks {
			nfk := *fk
			nfk.cols = append([]string(nil), fk.cols...)
			nfk.refCols = append([]string(nil), fk.refCols...)
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
	for _, e := range s.events {
		ne := *e
		c.events = append(c.events, &ne)
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
		if indexOf(fk.cols, col) >= 0 {
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
		return "'1'"
	case strings.HasPrefix(c.typ, "decimal"):
		return "'1.00'"
	case c.typ == "double":
		return "'1'"
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
	if c.collate {
		b.WriteString(" COLLATE utf8mb4_bin")
	}
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
	if c.onUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP(6)")
	}
	if c.invisible {
		b.WriteString(" INVISIBLE")
	}
	return b.String()
}

// keyCols renders a key's column list with its prefix lengths.
func (k *pKey) keyCols() string {
	parts := make([]string, len(k.cols))
	for i, c := range k.cols {
		parts[i] = q(c)
		if i < len(k.prefix) && k.prefix[i] > 0 {
			parts[i] += fmt.Sprintf("(%d)", k.prefix[i])
		}
	}
	return strings.Join(parts, ",")
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
		parts = append(parts, "  "+kind+" "+q(k.name)+" ("+k.keyCols()+")")
	}
	for _, fk := range t.fks {
		s := "  CONSTRAINT " + q(fk.name) + " FOREIGN KEY (" + qlist(fk.cols) + ") REFERENCES " + q(fk.refTable) + " (" + qlist(fk.refCols) + ")"
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
	for _, e := range s.events {
		fmt.Fprintf(&b, "CREATE EVENT %s ON SCHEDULE EVERY %d HOUR DO SET @probe = %d;\n", q(e.name), e.n, e.n)
	}
	return b.String()
}

// rows renders three rows per table, parents first (the tables are declared in that order):
// integer columns carry the row number (distinct, positive, a valid reference to a parent's
// id), a nullable column is NULL in the third row, an AUTO_INCREMENT id is set explicitly so
// children can reference it, generated columns are left to the server.
func (s *pSchema) rows() string {
	var b strings.Builder
	for _, t := range s.tables {
		var names []string
		var cols []*pCol
		for _, c := range t.cols {
			if c.gen == "" {
				names = append(names, q(c.name))
				cols = append(cols, c)
			}
		}
		var rows []string
		for i := 1; i <= 3; i++ {
			var vals []string
			for _, c := range cols {
				if i == 3 && !c.notNull && !c.pk {
					vals = append(vals, "NULL")
					continue
				}
				vals = append(vals, value(c, i))
			}
			rows = append(rows, "("+strings.Join(vals, ", ")+")")
		}
		fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES %s;\n", q(t.name), strings.Join(names, ", "), strings.Join(rows, ", "))
	}
	return b.String()
}

// value is row i's value for column c.
func value(c *pCol, i int) string {
	switch {
	case isInteger(c.typ), c.typ == "double", strings.HasPrefix(c.typ, "decimal"):
		return fmt.Sprint(i)
	case strings.HasPrefix(c.typ, "varchar"), c.typ == "text":
		return fmt.Sprintf("'v%d'", i)
	case c.typ == "datetime(6)":
		return fmt.Sprintf("'2024-01-%02d 00:00:00'", i)
	case c.typ == "date":
		return fmt.Sprintf("'2024-01-%02d'", i)
	case c.typ == "enum":
		return "'" + c.labels[(i-1)%len(c.labels)] + "'"
	case c.typ == "json":
		return fmt.Sprintf(`'{"i": %d}'`, i)
	}
	return "NULL"
}

// fill is the literal a backfill gives a column that turns NOT NULL under existing rows.
func fill(c *pCol) string {
	switch {
	case isNumeric(c.typ):
		return "9"
	case c.typ == "enum":
		return "'" + c.labels[0] + "'"
	case c.typ == "json":
		return "'{}'"
	case c.typ == "datetime(6)":
		return "'2024-02-01 00:00:00'"
	case c.typ == "date":
		return "'2024-02-01'"
	}
	return "'fill'"
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
	if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && r.Intn(4) == 0 {
		c.collate = true
	}
	if c.typ == "datetime(6)" && c.def != "" && r.Intn(2) == 0 {
		c.onUpdate = true
	}
	if r.Intn(8) == 0 {
		c.invisible = true
	}
	return c
}

// numericCols: the plain (not generated, not AUTO_INCREMENT, not a foreign key's -- MySQL
// refuses a generated column over a referencing column with a referential action, 1215)
// numeric columns a generated column or a trigger body may read.
func (t *pTable) numericCols() []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if isNumeric(c.typ) && c.gen == "" && !c.auto && !t.inFK(c.name) {
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
		if r.Intn(4) == 0 {
			s.addCompositeFK(r, t, pick(r, s.tables))
		} else {
			s.addFK(r, t, pick(r, s.tables))
		}
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
	unique := r.Intn(2) == 0
	var cands []string
	for _, c := range t.cols {
		// (a column a mutation added holds one default in every row: no unique key over it;
		// a text column takes a prefix length below, a json column no key at all)
		// (an ON UPDATE timestamp is one value in every row a backfill touches: no unique key)
		if c.typ != "json" && !c.pk && (c.gen == "" || c.stored) && !(unique && (c.typ == "enum" || c.fresh || c.onUpdate)) {
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
	k := &pKey{name: s.next("k"), unique: unique, cols: cands[:n], prefix: make([]int, n)}
	for i, name := range k.cols {
		c := t.col(name)
		if c.typ == "text" || (strings.HasPrefix(c.typ, "varchar") && r.Intn(3) == 0) {
			k.prefix[i] = 10
		}
	}
	t.keys = append(t.keys, k)
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
	t.fks = append(t.fks, &pFK{name: s.next("fk"), cols: []string{c.name}, refTable: parent.name, refCols: []string{ppk.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT"})})
	if t.fks[len(t.fks)-1].onDelete == "SET NULL" {
		c.notNull = false
	}
	return true
}

// addCompositeFK gives t two new columns referencing a two-column UNIQUE key that parent
// gains over two new NOT NULL integer columns of its own (rows carry the row number in
// every integer column, so every (i, i) pair exists in the parent).
func (s *pSchema) addCompositeFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	a := &pCol{name: s.next("a"), typ: "int", notNull: true, def: "0"}
	b := &pCol{name: s.next("b"), typ: "int", notNull: true, def: "0"}
	parent.cols = append(parent.cols, a, b)
	if s.mutating {
		// the parent holds rows: the new key columns need distinct values (the key's)
		a.fresh, b.fresh = true, true
		pk := parent.pk().name
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.name, a.name, q(pk)),
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.name, b.name, q(pk)))
	}
	parent.keys = append(parent.keys, &pKey{name: s.next("k"), unique: true, cols: []string{a.name, b.name}})
	ra := &pCol{name: s.next("r"), typ: "int", fresh: s.mutating}
	rb := &pCol{name: s.next("r"), typ: "int", fresh: s.mutating}
	t.cols = append(t.cols, ra, rb)
	t.fks = append(t.fks, &pFK{name: s.next("fk"), cols: []string{ra.name, rb.name}, refTable: parent.name, refCols: []string{a.name, b.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL"})})
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
	var src []*pCol
	for _, c := range t.numericCols() {
		// the body adds to the column: not a reference, not a key, not a referenced column
		if !t.inFK(c.name) && !c.pk && len(referencedBy(s, t, c.name)) == 0 {
			src = append(src, c)
		}
	}
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
	if r.Intn(3) == 0 {
		s.events = append(s.events, &pEvent{name: s.next("ev"), n: 1 + r.Intn(9)})
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
		if !c.pk && c.gen == "" && !c.fresh && !touched[t.name+"."+c.name] {
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
			if fk.refTable == t.name && indexOf(fk.refCols, col) >= 0 {
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
		if indexOf(fk.cols, col) < 0 {
			fks = append(fks, fk)
		}
	}
	t.fks = fks
	var cols []*pCol
	for _, c := range t.cols {
		if c.gen == col {
			if !c.fresh {
				s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
			}
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
		if i := indexOf(fk.cols, from); i >= 0 {
			fk.cols[i] = to
		}
	}
	for _, c := range t.cols {
		if c.gen == from {
			c.gen = to
		}
	}
	for _, fk := range referencedBy(s, t, from) {
		fk.refCols[indexOf(fk.refCols, from)] = to
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
		c.fresh = true
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
		x.fresh, g.fresh = true, true
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
		for _, g := range t.cols {
			if g.gen == c.name && touched[t.name+"."+g.name] {
				return false // the generated column reading it was moved by another step already
			}
		}
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
		// a backfill expression written earlier names the column as the target spells it
		for i, in := range s.intents {
			if strings.HasPrefix(in, "-- @migrate backfill "+t.name+".") {
				s.intents[i] = strings.ReplaceAll(in, " = "+q(c.name), " = "+q(to))
			}
		}
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
		if t.inFK(c.name) {
			return false // a referencing column follows its parent's key (widened below)
		}
		// a referenced key and every column referencing it widen together
		for _, fk := range referencedBy(s, t, c.name) {
			child := s.table(fkOwner(s, fk))
			cc := child.col(fk.cols[indexOf(fk.refCols, c.name)])
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
			if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = defaultFor(c)
		} else {
			c.def = ""
		}
		if c.notNull {
			f := fill(c)
			if t.inFK(c.name) {
				f = "1" // a parent every table has
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.name, c.name, f, q(c.name)))
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
					if lead == fk.cols[0] {
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
		if t.inFK(c.name) {
			return false
		}
		if !c.notNull {
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 9 where %s is null", t.name, c.name, q(c.name)))
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
		c := t.cols[len(t.cols)-1]
		if c.notNull {
			// existing rows need a parent before the column turns NOT NULL
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 1", t.name, c.name))
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add composite foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
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
		t, parent := ts[i], ts[j]
		if !s.addCompositeFK(r, t, parent) {
			return false
		}
		for _, c := range parent.cols[len(parent.cols)-2:] {
			c.fresh = true
			touched[parent.name+"."+c.name] = true
		}
		for _, c := range t.cols[len(t.cols)-2:] {
			c.fresh = true
			touched[t.name+"."+c.name] = true
		}
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
			in = strings.ReplaceAll(in, " -> "+t.name+".", " -> "+to+".")
			in = strings.ReplaceAll(in, "enum "+t.name+".", "enum "+to+".")
			in = strings.ReplaceAll(in, "backfill "+t.name+".", "backfill "+to+".")
			s.intents[i] = in
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
		if !old.auto && r.Intn(2) == 0 {
			c.auto = true // the server numbers the existing rows
		} else {
			// the rows need distinct values before the key goes on: the old key's
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.name, c.name, q(old.name)))
		}
		old.pk = false
		if old.auto || len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
	}},
	{"toggle collation", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && c.gen == "" && !touched[t.name+"."+c.name] {
					c.collate = !c.collate
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"toggle invisible", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if !c.pk && c.gen == "" && !touched[t.name+"."+c.name] {
					c.invisible = !c.invisible
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"toggle on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "datetime(6)" && c.gen == "" && !touched[t.name+"."+c.name] {
					c.onUpdate = !c.onUpdate
					if c.onUpdate && c.def == "" {
						c.def = defaultFor(c)
					}
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"add event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.events = append(s.events, &pEvent{name: s.next("ev"), n: 1 + r.Intn(9)})
		return true
	}},
	{"drop event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.events) == 0 {
			return false
		}
		i := r.Intn(len(s.events))
		s.events = append(s.events[:i], s.events[i+1:]...)
		return true
	}},
	{"change event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.events) == 0 {
			return false
		}
		pick(r, s.events).n += 10
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
	s.mutating = true
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
	// the source database holds rows: what apply runs against is never empty
	got, _, err := c.Canonical(ctx, aText+"\n"+src.rows()+"\nSET FOREIGN_KEY_CHECKS=1;\n"+strings.Join(ddl, "\n"))
	if err != nil {
		if strings.Contains(err.Error(), "Error 1062") || strings.Contains(err.Error(), "Error 1452") || strings.Contains(err.Error(), "Error 3819") {
			// the rows themselves (a duplicate, a dangling reference, a CHECK): a rows()
			// mistake unless the plan's own statement raised it -- told apart below by
			// loading the rows alone
			if _, _, rerr := c.Canonical(ctx, aText+"\n"+src.rows()); rerr != nil {
				v.kind, v.detail = "generator (rows)", rerr.Error()+"\n"+src.rows()
				return v
			}
		}
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
		n := 1 + r.Intn(5)
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
				t.Logf("%s (pair %d, %s): %s\n%s", v.kind, i, strings.Join(applied, "; "), v.detail, v.aSQL)
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
