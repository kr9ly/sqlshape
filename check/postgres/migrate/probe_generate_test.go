package migrate

// Split from probe_test.go (see its header): generating schemas from the vocabulary --
// pick(), the newX() constructors, addX() helpers that attach constraints/indexes/views/
// triggers to an already-generated table, and generate() itself. Pure move.

import (
	"fmt"
	"math/rand"
	"strings"
)

// ---- generating -------------------------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "domain" {
		if len(s.domains) == 0 || r.Intn(3) == 0 {
			s.domains = append(s.domains, &pDomain{name: s.next("num")})
		}
		c.typ = pick(r, s.domains).name
	}
	if c.typ == "enum" {
		if len(s.enums) == 0 || r.Intn(3) == 0 {
			e := &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)], fresh: s.mutating}
			s.enums = append(s.enums, e)
		}
		c.typ = pick(r, s.enums).name
	}
	if c.typ == "composite" {
		if len(s.composites) == 0 || r.Intn(3) == 0 {
			s.composites = append(s.composites, s.newComposite(r))
		}
		c.typ = pick(r, s.composites).name
	}
	c.notNull = r.Intn(2) == 0
	if c.notNull || r.Intn(3) == 0 {
		c.def = s.defaultFor(c)
	}
	if r.Intn(5) == 0 {
		c.comment = "about " + c.name
	}
	return c
}

// newComposite is a two-attribute composite type, one numeric and one text-like: attrValue
// gives both a value that varies by row, so a unique key or a plain comparison over a
// composite column holds like it would over a plain one.
// ensureExtension adds name to the schema's declared extensions unless already there.
func (s *pSchema) ensureExtension(name string) {
	for _, e := range s.extensions {
		if e == name {
			return
		}
	}
	s.extensions = append(s.extensions, name)
}

func (s *pSchema) newComposite(r *rand.Rand) *pComposite {
	return &pComposite{name: s.next("ct"), attrs: []*pAttr{
		{name: "n", typ: "integer"},
		{name: "w", typ: "text"},
	}}
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
	t := &pTable{name: s.next("t"), schema: pick(r, []string{"public", "public", "app"})}
	t.orig = t.full()
	id := &pCol{name: "id", typ: pick(r, []string{"integer", "bigint"}), notNull: true, pk: true}
	switch r.Intn(3) {
	case 0:
		id.identity = true
	case 1:
		id.typ, id.serial = "bigint", true
	}
	t.cols = append(t.cols, id)
	n := 2 + r.Intn(4)
	for i := 0; i < n; i++ {
		t.cols = append(t.cols, s.newCol(r))
	}
	if src := t.numericCols(); len(src) > 0 && r.Intn(2) == 0 {
		g := &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name}
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
	}
	s.addKey(r, t)
	if r.Intn(3) == 0 {
		s.addCheck(r, t)
	}
	if r.Intn(2) == 0 {
		s.addExclude(r, t)
	}
	if r.Intn(4) == 0 {
		s.addPartialIndex(r, t)
	}
	if len(s.tables) > 0 {
		if r.Intn(4) == 0 {
			s.addCompositeFK(r, t, pick(r, s.tables))
		} else {
			s.addFK(r, t, pick(r, s.tables))
		}
	}
	if r.Intn(4) == 0 {
		t.comment = "about " + t.name
	}
	return t
}

// newPartTable makes a RANGE or LIST partitioned table with three partitions: two
// ordinary ones and a DEFAULT.
func (s *pSchema) newPartTable(r *rand.Rand) *pPartTable {
	name := s.next("part")
	pt := &pPartTable{name: name, orig: "public." + name, strategy: "RANGE"}
	if r.Intn(2) == 0 {
		pt.strategy = "LIST"
	}
	// pt.id: plain integer most of the time, or a column whose own sequence a later
	// repartition (change partition strategy / departition table) has to carry across the
	// rebuild -- bigserial, or IDENTITY (BY DEFAULT / ALWAYS), exercising
	// detectRepartitions/repartitionTable's own vocabulary (migrate.go,
	// brief-pg-repartition-seq.md) through the generator instead of only by hand.
	pt.id = &pCol{name: "id", typ: "integer", notNull: true}
	switch r.Intn(4) {
	case 1:
		pt.id.serial = true
	case 2:
		pt.id.identity = true
	case 3:
		pt.id.identity = true
		pt.id.idAlways = true
	}
	c1, c2 := s.next(name+"_"), s.next(name+"_")
	def := s.next(name + "_")
	if pt.strategy == "LIST" {
		pt.parts = []*pPartChild{
			{name: c1, orig: "public." + c1, values: []string{"a", "b"}},
			{name: c2, orig: "public." + c2, values: []string{"c"}},
			{name: def, orig: "public." + def, isDefault: true},
		}
	} else {
		pt.parts = []*pPartChild{
			{name: c1, orig: "public." + c1, lo: 1, hi: 10},
			{name: c2, orig: "public." + c2, lo: 10, hi: 20},
			{name: def, orig: "public." + def, isDefault: true},
		}
	}
	return pt
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	unique := r.Intn(2) == 0
	var cands []string
	for _, c := range t.cols {
		// three rows: a boolean or an enum column repeats a value, so no unique key over it
		// (a column a mutation added holds one default in every row: no unique key over it)
		if c.typ != "jsonb" && !c.pk && !(unique && (c.typ == "boolean" || s.enum(c.typ) != nil || c.fresh)) {
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
	t.keys = append(t.keys, &pKey{name: s.next("k"), unique: unique, cols: cands[:n]})
	return true
}

// addPartialIndex is a plain CREATE INDEX WHERE (...): "col IS NOT NULL" over any column, or
// "col > 0" over a numeric one -- both true for every row a table holds, so the index never
// needs to exclude a row the probe wrote.
func (s *pSchema) addPartialIndex(r *rand.Rand, t *pTable) bool {
	var cands []*pCol
	for _, c := range t.cols {
		if !c.pk && c.typ != "jsonb" {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return false
	}
	c := pick(r, cands)
	where := "notnull"
	if isNumeric(c.typ) && r.Intn(2) == 0 {
		where = "positive"
	}
	t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, where: where})
	return true
}

// addExprIndex is a plain CREATE INDEX over a single expression: lower(col) for a text-like
// column, (col + 1) for a numeric one.
func (s *pSchema) addExprIndex(r *rand.Rand, t *pTable) bool {
	var textCands, numCands []*pCol
	for _, c := range t.cols {
		if c.pk || c.gen != "" {
			continue
		}
		switch {
		case c.typ == "text" || strings.HasPrefix(c.typ, "varchar"):
			textCands = append(textCands, c)
		case isNumeric(c.typ):
			numCands = append(numCands, c)
		}
	}
	switch {
	case len(textCands) > 0 && (len(numCands) == 0 || r.Intn(2) == 0):
		c := pick(r, textCands)
		t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, expr: "lower"})
	case len(numCands) > 0:
		c := pick(r, numCands)
		t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, expr: "plus1"})
	default:
		return false
	}
	return true
}

// addExclude is CONSTRAINT ... EXCLUDE USING btree (col WITH =) over an integer column: a
// row's own value is always distinct from every other row's (the row number), so it holds
// under the fixture the same way a unique key would -- a fresh column excluded for the same
// reason (every row shares its one default).
func (s *pSchema) addExclude(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if isInteger(c.typ) && !c.pk && !c.fresh {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{pick(r, cands)}, exclude: true})
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
	fk := &pFK{name: s.next("fk"), cols: []string{c.name}, refTable: parent.name, refCols: []string{ppk.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT"}),
		onUpdate: pick(r, []string{"", "CASCADE", "RESTRICT", "NO ACTION"})}
	if fk.onDelete == "SET NULL" {
		c.notNull = false
	}
	t.fks = append(t.fks, fk)
	return true
}

// addCompositeFK gives t two new columns referencing a two-column UNIQUE key that parent
// gains over two new NOT NULL integer columns of its own (rows carry the row number in
// every integer column, so every (i, i) pair exists in the parent).
func (s *pSchema) addCompositeFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	a := &pCol{name: s.next("a"), typ: "integer", notNull: true, def: "0"}
	b := &pCol{name: s.next("b"), typ: "integer", notNull: true, def: "0"}
	parent.cols = append(parent.cols, a, b)
	if s.mutating {
		// the parent holds rows: the new key columns need distinct values (the key's)
		a.fresh, b.fresh = true, true
		pk := parent.pk().name
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.full(), a.name, qi(pk)),
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.full(), b.name, qi(pk)))
	}
	parent.keys = append(parent.keys, &pKey{name: s.next("k"), unique: true, cols: []string{a.name, b.name}})
	ra := &pCol{name: s.next("r"), typ: "integer", fresh: s.mutating}
	rb := &pCol{name: s.next("r"), typ: "integer", fresh: s.mutating}
	t.cols = append(t.cols, ra, rb)
	t.fks = append(t.fks, &pFK{name: s.next("fk"), cols: []string{ra.name, rb.name}, refTable: parent.name, refCols: []string{a.name, b.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL"})})
	return true
}

func (s *pSchema) addView(r *rand.Rand) bool { return s.addViewKind(r, r.Intn(3) == 0) }

func (s *pSchema) addViewKind(r *rand.Rand, mat bool) bool {
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
	s.views = append(s.views, &pView{name: s.next("v"), table: t.name, cols: cols, mat: mat})
	return true
}

func (s *pSchema) addTrigger(r *rand.Rand) bool { return s.addTriggerOn(r, pick(r, s.tables)) }

func (s *pSchema) addTriggerOn(r *rand.Rand, t *pTable) bool {
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
	name := s.next("tr")
	col, n := pick(r, src).name, 1+r.Intn(9)
	s.triggerFns = append(s.triggerFns, &pTriggerFn{name: name, col: col, n: n})
	s.triggers = append(s.triggers, &pTrigger{name: name, table: t.name, col: col, n: n, callName: name})
	return true
}

func generate(r *rand.Rand, pg18 bool) *pSchema {
	s := &pSchema{pg18: pg18}
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		s.tables = append(s.tables, s.newTable(r))
	}
	// a partitioned table from pair 0 too (see untouched-style helpers below for why): a
	// spare "drop partition" / "detach partition" needs a candidate that isn't only one a
	// same-recipe "add partitioned table" step happened to leave behind
	s.partTables = append(s.partTables, s.newPartTable(r))
	// pt0 keeps newPartTable's own random id draw (plain / bigserial / IDENTITY ALWAYS / BY
	// DEFAULT): it is also the pre-detached spare right below, and "detach partition" /
	// "attach partition" no longer refuse a non-plain pt (brief-pg-attach-seq.md --
	// migrate.go's partitionAttach and detachedIDCol, just below, now know what each one
	// does across a DETACH / ATTACH), so this spare exercises every id kind exactly as any
	// other pt does.
	// a spare already-detached partition from pair 0 too: "attach partition" alone
	// (directed coverage) needs one already standing apart, not only one a same-recipe
	// "detach partition" step happened to leave behind. Bound-picking mirrors "add
	// partition"'s own (a LIST value / RANGE bound the other children don't already use).
	{
		pt0 := s.partTables[0]
		name := s.next(pt0.name + "_")
		if pt0.strategy == "LIST" {
			used := map[string]bool{}
			for _, c := range pt0.parts {
				for _, v := range c.values {
					used[v] = true
				}
			}
			letter := "d"
			for _, cand := range []string{"d", "e", "f", "g", "h"} {
				if !used[cand] {
					letter = cand
					break
				}
			}
			pt0.parts = append(pt0.parts, &pPartChild{name: name, orig: "public." + name, values: []string{letter}, detached: true})
		} else {
			hi := 0
			for _, c := range pt0.parts {
				if !c.isDefault && c.hi > hi {
					hi = c.hi
				}
			}
			pt0.parts = append(pt0.parts, &pPartChild{name: name, orig: "public." + name, lo: hi, hi: hi + 10, detached: true})
		}
	}
	if r.Intn(2) == 0 {
		s.addViewKind(r, false) // a spare plain view, so "drop view" has one from pair 0
	}
	if r.Intn(3) == 0 {
		s.addViewKind(r, true) // a spare materialized view, so "drop materialized view" has one from pair 0
	}
	if r.Intn(2) == 0 {
		tt := pick(r, s.tables)
		s.addTriggerOn(r, tt)
		s.addTriggerOn(r, tt) // a second one on the same table, so "change trigger function" has a pair from pair 0
	}
	s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9), retType: "integer", volatility: "IMMUTABLE", lang: "sql"})
	// spares with no column using them: "drop domain" / "drop enum" / "drop range type"
	// need a candidate from pair 0, not just one a same-recipe "add" step happened to
	// leave behind
	s.domains = append(s.domains, &pDomain{name: s.next("num")})
	s.enums = append(s.enums, &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)]})
	s.composites = append(s.composites, s.newComposite(r))
	s.ranges = append(s.ranges, &pRange{name: s.next("rg")})
	s.extensions = append(s.extensions, "pgcrypto")
	// a policy / rule from pair 0 too, for the same reason as the spares above
	t := pick(r, s.tables)
	t.rowSec = true
	command := pick(r, []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"})
	using, withCheck := policyPredicates(command)
	s.policies = append(s.policies, &pPolicy{name: s.next("pol"), table: t.name, command: command,
		permissive: r.Intn(4) != 0, using: using, withCheck: withCheck})
	t2 := pick(r, s.tables)
	s.rules = append(s.rules, &pRule{name: s.next("ru"), table: t2.name, where: r.Intn(2) == 0, enabled: true})
	// a standalone sequence, owned by some table's plain integer column, from pair 0 too
	sq := &pSeq{name: s.next("sq")}
	s.seqs = append(s.seqs, sq)
	for _, t3 := range s.tables {
		for _, c := range t3.cols {
			if isInteger(c.typ) && !c.pk && !c.identity && !c.serial && c.gen == "" {
				sq.table, sq.col = t3.name, c.name
			}
		}
	}
	// a second, unowned standalone sequence from pair 0 too: "set sequence owner" alone
	// (directed coverage) needs one with no owner yet, not only one a same-recipe "clear
	// sequence owner" step happened to leave behind.
	s.seqs = append(s.seqs, &pSeq{name: s.next("sq")})
	// a seed table from pair 0 too, so "change" / "drop seed row" have rows to act on
	s.seedTable = &pSeedTable{name: s.next("lk"), rows: []pSeedRow{
		{code: s.next("code"), label: "a"},
		{code: s.next("code"), label: "b"},
	}}
	if pg18 {
		// a temporal (WITHOUT OVERLAPS) unique key and an ordinary (not yet PERIOD)
		// foreign key resting on it, from pair 0 too: "toggle key without overlaps" /
		// "toggle foreign key period" alone (directed coverage) need a candidate that
		// already carries the shape they flip, not only one a same-recipe "add without
		// overlaps key" step happened to leave behind -- mutations18's own vocabulary
		// otherwise only ever adds a brand new temporal key (+ constraint), never changes
		// an existing one's own WITHOUT OVERLAPS / PERIOD flag (the ~ field diff.Alphabet
		// mines).
		tp := &pTable{name: s.next("tp"), schema: "public"}
		tp.orig = tp.full()
		tid := &pCol{name: "id", typ: "integer", notNull: true, pk: true}
		span := &pCol{name: "span", typ: "daterange", notNull: true}
		tp.cols = []*pCol{tid, span}
		tp.keys = []*pKey{{name: s.next("k"), unique: true, cols: []string{tid.name, span.name}, withoutOverlaps: true}}
		s.tables = append(s.tables, tp)
		s.ensureExtension("btree_gist")

		tc := &pTable{name: s.next("tp"), schema: "public"}
		tc.orig = tc.full()
		cid := &pCol{name: "id", typ: "integer", notNull: true, pk: true}
		pid := &pCol{name: "pid", typ: "integer", notNull: true}
		valid := &pCol{name: "valid", typ: "daterange", notNull: true}
		tc.cols = []*pCol{cid, pid, valid}
		// plain (not yet PERIOD) FK, resting on tp's own plain PK alone: "toggle foreign
		// key period" widens it onto tp's temporal key instead (pid, valid) -> (id, span),
		// PERIOD.
		tc.fks = []*pFK{{name: s.next("fk"), cols: []string{pid.name}, refTable: tp.name, refCols: []string{tid.name}}}
		s.tables = append(s.tables, tc)
	}
	return s
}
