package migrate

// The migrate probe, PostgreSQL side: the same oracle check/mysql/migrate has (see its
// probe_test.go for the reasoning). Schemas are generated from the planner's vocabulary
// (column types, an ENUM type, identity and generated columns, unique constraints and
// indexes, foreign keys, CHECKs, table comments, views, functions, trigger functions with
// their triggers), mutated into a target (columns added / dropped / renamed / widened /
// re-nulled, constraints and indexes attached and detached, the primary key moved onto an
// existing or a new column, tables added / dropped / renamed, ENUM labels added and dropped,
// views, triggers and functions added / dropped / changed, generated expressions changed),
// and judged the way `sqlshape apply` runs the plan: the DDL applied to a database holding
// the source must read back as the target, column order aside (PostgreSQL cannot move a
// column; Verify reports order as notes), and a second plan from there must be empty.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// Skipped without pg_dump on PATH (nix-shell -p postgresql_17).

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- the model --------------------------------------------------------------------------

type pSchema struct {
	enums    []*pEnum
	tables   []*pTable
	views    []*pView
	triggers []*pTrigger
	funcs    []*pFunc
	intents  []string
	seq      int
}

type pEnum struct {
	name   string
	labels []string
}

type pTable struct {
	name    string
	orig    string // the source schema's name for the table (a declaration's left side)
	cols    []*pCol
	keys    []*pKey
	fks     []*pFK
	checks  []*pCheck
	comment string
}

type pCol struct {
	name     string
	typ      string // integer, bigint, text, varchar(50), numeric(10,2), ..., or an enum type's name
	notNull  bool
	def      string
	pk       bool
	identity bool
	gen      string // the column this generated (STORED) column reads
	fresh    bool   // added by a mutation: not in the source, so its drop needs no declaration
}

type pKey struct {
	name   string
	unique bool // a UNIQUE constraint; else a CREATE INDEX
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
	name  string // the trigger; its function is name + "_fn"
	table string
	col   string
	n     int
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
	for _, e := range s.enums {
		c.enums = append(c.enums, &pEnum{name: e.name, labels: append([]string(nil), e.labels...)})
	}
	for _, t := range s.tables {
		nt := &pTable{name: t.name, orig: t.orig, comment: t.comment}
		for _, col := range t.cols {
			nc := *col
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

func (s *pSchema) enum(name string) *pEnum {
	for _, e := range s.enums {
		if e.name == name {
			return e
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

func (t *pTable) pk() *pCol {
	for _, c := range t.cols {
		if c.pk {
			return c
		}
	}
	return nil
}

func (t *pTable) inFK(col string) bool {
	for _, fk := range t.fks {
		if fk.col == col {
			return true
		}
	}
	return false
}

// ---- the type vocabulary ----------------------------------------------------------------

var probeTypes = []string{"integer", "bigint", "smallint", "text", "varchar(50)", "numeric(10,2)", "double precision", "timestamptz", "date", "boolean", "jsonb", "enum"}

func isNumeric(typ string) bool {
	switch typ {
	case "integer", "bigint", "smallint", "numeric(10,2)", "numeric(14,2)", "double precision":
		return true
	}
	return false
}

func isInteger(typ string) bool { return typ == "integer" || typ == "bigint" || typ == "smallint" }

func (s *pSchema) defaultFor(c *pCol) string {
	switch c.typ {
	case "integer", "bigint", "smallint", "double precision":
		return "0"
	case "numeric(10,2)", "numeric(14,2)":
		return "0.00"
	case "text":
		return "'x'"
	case "varchar(50)", "varchar(100)":
		return "'x'"
	case "timestamptz":
		return "now()"
	case "date":
		return "'2020-01-01'"
	case "boolean":
		return "false"
	case "jsonb":
		return "'{}'"
	}
	if e := s.enum(c.typ); e != nil {
		return "'" + e.labels[0] + "'"
	}
	return ""
}

func widen(typ string) string {
	switch typ {
	case "integer":
		return "bigint"
	case "smallint":
		return "integer"
	case "varchar(50)":
		return "varchar(100)"
	case "numeric(10,2)":
		return "numeric(14,2)"
	}
	return ""
}

// ---- rendering --------------------------------------------------------------------------

func qi(name string) string { return `"` + name + `"` }

func qilist(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = qi(n)
	}
	return strings.Join(out, ", ")
}

func (c *pCol) render() string {
	var b strings.Builder
	b.WriteString(qi(c.name) + " " + c.typ)
	if c.gen != "" {
		b.WriteString(" GENERATED ALWAYS AS (" + qi(c.gen) + " + 1) STORED")
		return b.String()
	}
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.identity {
		b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
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
		parts = append(parts, "  CONSTRAINT "+qi(t.name+"_pkey")+" PRIMARY KEY ("+qi(pk.name)+")")
	}
	for _, k := range t.keys {
		if k.unique {
			parts = append(parts, "  CONSTRAINT "+qi(k.name)+" UNIQUE ("+qilist(k.cols)+")")
		}
	}
	for _, fk := range t.fks {
		s := "  CONSTRAINT " + qi(fk.name) + " FOREIGN KEY (" + qi(fk.col) + ") REFERENCES " + qi(fk.refTable) + " (" + qi(fk.refCol) + ")"
		if fk.onDelete != "" {
			s += " ON DELETE " + fk.onDelete
		}
		parts = append(parts, s)
	}
	for _, ck := range t.checks {
		c := t.col(ck.col)
		expr := "(" + qi(ck.col) + " > 0)"
		if !isNumeric(c.typ) {
			expr = "(length(" + qi(ck.col) + ") > 0)"
		}
		parts = append(parts, "  CONSTRAINT "+qi(ck.name)+" CHECK "+expr)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + qi(t.name) + " (\n" + strings.Join(parts, ",\n") + "\n);\n")
	for _, k := range t.keys {
		if !k.unique {
			b.WriteString("CREATE INDEX " + qi(k.name) + " ON " + qi(t.name) + " (" + qilist(k.cols) + ");\n")
		}
	}
	if t.comment != "" {
		b.WriteString("COMMENT ON TABLE " + qi(t.name) + " IS '" + t.comment + "';\n")
	}
	return b.String()
}

func (s *pSchema) render() string {
	var b strings.Builder
	for _, in := range s.intents {
		b.WriteString(in + "\n")
	}
	for _, e := range s.enums {
		quoted := make([]string, len(e.labels))
		for i, l := range e.labels {
			quoted[i] = "'" + l + "'"
		}
		b.WriteString("CREATE TYPE " + qi(e.name) + " AS ENUM (" + strings.Join(quoted, ", ") + ");\n")
	}
	for _, t := range s.tables {
		b.WriteString(t.render())
	}
	for _, v := range s.views {
		b.WriteString("CREATE VIEW " + qi(v.name) + " AS SELECT " + qilist(v.cols) + " FROM " + qi(v.table) + ";\n")
	}
	for _, f := range s.funcs {
		fmt.Fprintf(&b, "CREATE FUNCTION %s(a integer) RETURNS integer LANGUAGE sql IMMUTABLE RETURN a + %d;\n", qi(f.name), f.n)
	}
	for _, tr := range s.triggers {
		fmt.Fprintf(&b, "CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.%s := coalesce(NEW.%s, 0) + %d; RETURN NEW; END $$;\n",
			qi(tr.name+"_fn"), qi(tr.col), qi(tr.col), tr.n)
		fmt.Fprintf(&b, "CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s();\n", qi(tr.name), qi(tr.table), qi(tr.name+"_fn"))
	}
	return b.String()
}

// ---- generating -------------------------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "enum" {
		if len(s.enums) == 0 || r.Intn(3) == 0 {
			e := &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)]}
			s.enums = append(s.enums, e)
		}
		c.typ = pick(r, s.enums).name
	}
	c.notNull = r.Intn(2) == 0
	if c.notNull || r.Intn(3) == 0 {
		c.def = s.defaultFor(c)
	}
	return c
}

func (t *pTable) numericCols() []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if isNumeric(c.typ) && c.gen == "" && !c.identity {
			out = append(out, c)
		}
	}
	return out
}

func (s *pSchema) newTable(r *rand.Rand) *pTable {
	t := &pTable{name: s.next("t")}
	t.orig = t.name
	id := &pCol{name: "id", typ: pick(r, []string{"integer", "bigint"}), notNull: true, pk: true, identity: r.Intn(2) == 0}
	t.cols = append(t.cols, id)
	n := 2 + r.Intn(4)
	for i := 0; i < n; i++ {
		t.cols = append(t.cols, s.newCol(r))
	}
	if src := t.numericCols(); len(src) > 0 && r.Intn(3) == 0 {
		g := &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name}
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
	return t
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if c.typ != "jsonb" && !c.pk {
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
		if !c.identity && !c.pk && (isNumeric(c.typ) || c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.checks = append(t.checks, &pCheck{name: s.next("ck"), col: pick(r, cands)})
	return true
}

func (s *pSchema) addFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	ppk := parent.pk()
	c := &pCol{name: s.next("r"), typ: ppk.typ, notNull: r.Intn(2) == 0}
	t.cols = append(t.cols, c)
	fk := &pFK{name: s.next("fk"), col: c.name, refTable: parent.name, refCol: ppk.name,
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT"})}
	if fk.onDelete == "SET NULL" {
		c.notNull = false
	}
	t.fks = append(t.fks, fk)
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

// ---- mutations --------------------------------------------------------------------------

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

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func indexOfCol(t *pTable, name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// detach removes everything that reads t's column col.
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

func insertCol(r *rand.Rand, t *pTable, c *pCol) {
	at := r.Intn(len(t.cols) + 1)
	t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
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
			c = &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name}
		} else {
			c = s.newCol(r)
		}
		c.fresh = true
		insertCol(r, t, c)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add generated column reading a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "numeric(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name}
		x.fresh, g.fresh = true, true
		insertCol(r, t, g)
		insertCol(r, t, x)
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
			if widen(c.typ) != "" && c.gen == "" && !c.identity && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		w := widen(c.typ)
		if t.inFK(c.name) {
			return false // a referencing column follows its parent's key
		}
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
			c.def = s.defaultFor(c)
		}
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
		if c.identity {
			return false
		}
		for _, fk := range t.fks {
			if fk.col == c.name && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = s.defaultFor(c)
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
				// a unique constraint a foreign key references stays
				if t.keys[i].unique && len(t.keys[i].cols) == 1 && len(referencedBy(s, t, t.keys[i].cols[0])) > 0 {
					return false
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
			if !c.pk && c.gen == "" && isInteger(c.typ) && !touched[t.name+"."+c.name] && !t.inFK(c.name) {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 || touched[t.name+"."+old.name] {
			return false
		}
		c := pick(r, cands)
		c.pk, c.notNull, c.def = true, true, ""
		old.pk = false
		if len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		} else if !old.identity && r.Intn(2) == 0 {
			old.notNull = false
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
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
		c := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "bigint"}), notNull: true, pk: true, identity: r.Intn(2) == 0}
		insertCol(r, t, c)
		old.pk = false
		if len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
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
		t, parent := ts[i], ts[j]
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
				return false
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
		for i, in := range s.intents {
			s.intents[i] = strings.ReplaceAll(in, " -> "+t.name+".", " -> "+to+".")
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
		for _, e := range s.enums {
			if !touched["enum:"+e.name] {
				e.labels = append(e.labels, "z")
				touched["enum:"+e.name] = true
				return true
			}
		}
		return false
	}},
	{"drop enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range s.enums {
			if len(e.labels) > 1 && !touched["enum:"+e.name] {
				gone := e.labels[len(e.labels)-1]
				e.labels = e.labels[:len(e.labels)-1]
				s.intents = append(s.intents, fmt.Sprintf("-- @migrate enum %s: drop '%s' using '%s'", e.name, gone, e.labels[0]))
				touched["enum:"+e.name] = true
				return true
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
		pick(r, s.triggers).n += 10
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

type step struct {
	m    int
	seed int64
}

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

// ---- judging ----------------------------------------------------------------------------

type verdict struct {
	kind    string
	detail  string
	ddl     []string
	aSQL    string
	bSQL    string
	applied []string
}

func judge(ctx context.Context, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := server.Canonical(ctx, v.aSQL, nil)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := server.Canonical(ctx, v.bSQL, nil)
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
	got, _, err := server.Canonical(ctx, aText+"\nRESET search_path;\n"+strings.Join(ddl, "\n"), b)
	if err != nil {
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	var d []string
	for _, ch := range diff.Compare(got, b) {
		if !ch.OrderOnly() {
			d = append(d, ch.String())
		}
	}
	if len(d) > 0 {
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
	requirePgDump(t)
	ctx := context.Background()
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
		v := judge(ctx, src, target, applied)
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
		for j := 0; j < len(recipe); {
			shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
			tgt, app := mutate(src, shorter)
			if len(app) == 0 {
				j++
				continue
			}
			if w := judge(ctx, src, tgt, app); w.kind == v.kind {
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

	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe (postgres), seed %d, %d pairs\n\n%s\n", *probeSeed, *probeN, summary.String())
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
