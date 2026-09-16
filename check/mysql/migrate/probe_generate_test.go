package migrate

import (
	"fmt"
	"math/rand"
	"strings"
)

// ---- generating a source schema -------------------------------------------------------

// ---- generating a source schema -------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "enum" {
		c.labels = []string{"a", "b", "c"}[:2+r.Intn(2)]
	}
	if c.typ == "point" {
		c.notNull = true // a SPATIAL index (addSpatialKey) needs it; simplest to always have it
	} else {
		c.notNull = r.Intn(2) == 0
	}
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
	if r.Intn(5) == 0 {
		c.comment = "note " + c.name
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
	if r.Intn(5) == 0 {
		s.addFulltextKey(r, t)
	}
	if r.Intn(5) == 0 {
		s.addSpatialKey(r, t)
	}
	if r.Intn(5) == 0 {
		s.addFunctionalKey(r, t)
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
	if r.Intn(5) == 0 {
		t.charset, t.collation = "utf8mb4", "utf8mb4_bin"
	}
	if r.Intn(5) == 0 {
		t.rowFormat = pick(r, []string{"DYNAMIC", "COMPRESSED"})
	}
	return t
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	unique := r.Intn(2) == 0
	var cands []string
	for _, c := range t.cols {
		// (a column a mutation added holds one default in every row: no unique key over it;
		// a text column takes a prefix length below, a json column no key at all; a point
		// column takes only a SPATIAL index, addSpatialKey's job, never this plain kind)
		// (an ON UPDATE timestamp is one value in every row a backfill touches: no unique key)
		if c.typ != "json" && c.typ != "point" && !c.pk && (c.gen == "" || c.stored) && !(unique && (c.typ == "enum" || c.fresh || c.onUpdate)) {
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
	k := &pKey{name: s.next("k"), unique: unique, cols: cands[:n], prefix: make([]int, n), desc: make([]bool, n)}
	for i, name := range k.cols {
		c := t.col(name)
		if c.typ == "text" || (strings.HasPrefix(c.typ, "varchar") && r.Intn(3) == 0) {
			k.prefix[i] = 10
		}
		if r.Intn(4) == 0 {
			k.desc[i] = true
		}
	}
	if r.Intn(6) == 0 {
		k.invisible = true
	}
	t.keys = append(t.keys, k)
	return true
}

// addFunctionalKey gives t an index over an expression, not a plain column: `(col + 1)` for
// a numeric source or `(lower(col))` for a text one (MySQL implements a functional key part
// as a hidden generated column reading the real one -- cols still names it, so every
// mutation that keeps a key's columns in step by name, rename included, already does).
func (s *pSchema) addFunctionalKey(r *rand.Rand, t *pTable) bool {
	type cand struct {
		name, kind string
	}
	var cands []cand
	for _, c := range t.numericCols() {
		cands = append(cands, cand{c.name, "plus1"})
	}
	for _, c := range t.cols {
		if strings.HasPrefix(c.typ, "varchar") && c.gen == "" && !t.inFK(c.name) {
			cands = append(cands, cand{c.name, "lower"})
		}
	}
	if len(cands) == 0 {
		return false
	}
	picked := pick(r, cands)
	k := &pKey{name: s.next("k"), cols: []string{picked.name}, prefix: []int{0}, expr: []string{picked.kind}, desc: []bool{r.Intn(3) == 0}}
	t.keys = append(t.keys, k)
	return true
}

// addFulltextKey gives t a FULLTEXT index over one of its text / varchar columns.
func (s *pSchema) addFulltextKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && (c.gen == "" || c.stored) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	name := pick(r, cands)
	t.keys = append(t.keys, &pKey{name: s.next("k"), special: "FULLTEXT", cols: []string{name}, prefix: []int{0}})
	return true
}

// addSpatialKey gives t a SPATIAL index over one of its point columns (always NOT NULL --
// see newCol -- which a SPATIAL index requires).
func (s *pSchema) addSpatialKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if c.typ == "point" {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	name := pick(r, cands)
	t.keys = append(t.keys, &pKey{name: s.next("k"), special: "SPATIAL", cols: []string{name}, prefix: []int{0}})
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
	t.checks = append(t.checks, &pCheck{name: s.next("ck"), col: pick(r, cands), enforced: true})
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
	fk := &pFK{name: s.next("fk"), cols: []string{c.name}, refTable: parent.name, refCols: []string{ppk.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"}),
		onUpdate: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})}
	t.fks = append(t.fks, fk)
	// a column an action may SET NULL cannot be declared NOT NULL
	if fk.onDelete == "SET NULL" || fk.onUpdate == "SET NULL" {
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
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL"}), onUpdate: pick(r, []string{"", "CASCADE", "RESTRICT"})})
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
	if r.Intn(2) == 0 {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9)})
	}
	if r.Intn(2) == 0 {
		s.procs = append(s.procs, &pProc{name: s.next("p"), n: 1 + r.Intn(9)})
	}
	if r.Intn(3) == 0 {
		s.events = append(s.events, s.newEvent(r))
	}
	return s
}

// newEvent gives about half its events some of the optional schedule / option vocabulary,
// so the source side already carries some of it for a mutation to toggle off, not only on.
func (s *pSchema) newEvent(r *rand.Rand) *pEvent {
	e := &pEvent{name: s.next("ev"), n: 1 + r.Intn(9)}
	if r.Intn(2) == 0 {
		e.starts, e.ends = "2099-01-01 00:00:00", "2099-06-01 00:00:00"
	}
	if r.Intn(3) == 0 {
		e.completion = "PRESERVE"
	}
	if r.Intn(2) == 0 {
		e.status = "DISABLE"
	}
	if r.Intn(3) == 0 {
		e.comment = "about " + e.name
	}
	return e
}
