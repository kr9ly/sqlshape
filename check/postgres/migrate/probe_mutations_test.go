package migrate

// Split from probe_test.go (see its header): the mutation vocabulary. mutations (and its
// PostgreSQL-18-only extension mutations18) stay a single var each -- the PRNG picks a
// mutation by its index into this slice, so a mutation's position controls what seed 1
// draws; spreading the literal across files via per-file init() append would leave the
// final order to Go's (file-alphabetical) init sequencing instead of this expression, a
// silent way to change what every existing seed reproduces. So despite the vocabulary
// spanning columns, keys, foreign keys, tables, schemas, sequences, seed rows, functions,
// triggers, rules, policies, composite types, identity and partitions, it is one file, one
// var. Pure move -- no mutation's body, ordering or selection guard changed here.

import (
	"fmt"
	"math/rand"
	"strings"
)

// ---- mutations --------------------------------------------------------------------------

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

// keyIsReferenced reports whether any column of k is referenced by another table's foreign
// key (a composite unique key rests under a composite foreign key column by column, so any
// one of its columns being referenced is enough).
func keyIsReferenced(s *pSchema, t *pTable, k *pKey) bool {
	for _, col := range k.cols {
		if len(referencedBy(s, t, col)) > 0 {
			return true
		}
	}
	return false
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
	for _, sq := range s.seqs {
		if sq.table == t.name && sq.col == from {
			sq.col = to
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
	var policies []*pPolicy
	for _, pol := range s.policies {
		if pol.table != t.name {
			policies = append(policies, pol)
		}
	}
	s.policies = policies
	var rules []*pRule
	for _, ru := range s.rules {
		if ru.table != t.name {
			rules = append(rules, ru)
		}
	}
	s.rules = rules
	for _, sq := range s.seqs {
		if sq.table == t.name {
			sq.table, sq.col = "", ""
		}
	}
}

func insertCol(r *rand.Rand, t *pTable, c *pCol) {
	at := r.Intn(len(t.cols) + 1)
	t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
}

// excludeChangeColumn and excludeToggleOperator are registered twice in mutations (a
// second entry point, same body): see "toggle trigger fires on update "'s comment for why.

func excludeChangeColumn(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
	for _, t := range untouched(s, touched) {
		var cands []*pKey
		for _, k := range t.keys {
			if k.exclude && !k.opNE { // opNE needs the shared-value column it has
				cands = append(cands, k)
			}
		}
		if len(cands) == 0 {
			continue
		}
		k := pick(r, cands)
		var others []string
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.fresh && c.name != k.cols[0] {
				others = append(others, c.name)
			}
		}
		if len(others) == 0 {
			continue
		}
		k.cols[0] = pick(r, others)
		return true
	}
	return false
}

func excludeToggleOperator(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
	for _, t := range untouched(s, touched) {
		var cands []*pKey
		for _, k := range t.keys {
			if k.exclude {
				cands = append(cands, k)
			}
		}
		if len(cands) == 0 {
			continue
		}
		k := pick(r, cands)
		if !k.opNE {
			// WITH <> forbids any two rows differing: a fresh column, sharing one
			// default value in every row, always holds. It also needs GIST (btree's
			// integer operator family has no "<>" member, 42809, measured), so
			// btree_gist too.
			c := &pCol{name: s.next("ne"), typ: "integer", notNull: true, def: "1", fresh: true}
			t.cols = append(t.cols, c)
			k.cols[0], k.opNE = c.name, true
			s.ensureExtension("btree_gist")
			return true
		}
		var ints []string
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.fresh && c.name != k.cols[0] {
				ints = append(ints, c.name)
			}
		}
		if len(ints) == 0 {
			continue
		}
		k.cols[0], k.opNE = pick(r, ints), false
		return true
	}
	return false
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
		for _, g := range t.cols {
			if g.gen == c.name && touched[t.name+"."+g.name] {
				return false // the generated column reading it was moved by another step already
			}
		}
		for _, sq := range s.seqs {
			if sq.table == t.name && sq.col == c.name {
				return false // a standalone sequence is OWNED BY this column
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
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
		// a backfill expression written earlier names the column as the target spells it
		for i, in := range s.intents {
			if strings.HasPrefix(in, "-- @migrate backfill "+t.full()+".") {
				s.intents[i] = strings.ReplaceAll(in, " = "+qi(c.name), " = "+qi(to))
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
			if widen(c.typ) != "" && c.gen == "" && !c.identity && !c.serial && !touched[t.name+"."+c.name] {
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
			cc := child.col(fk.cols[indexOf(fk.refCols, c.name)])
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
		if c.identity || c.serial {
			return false
		}
		for _, fk := range t.fks {
			if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = s.defaultFor(c)
		} else {
			c.def = ""
		}
		if c.notNull {
			f := s.fill(c)
			if t.inFK(c.name) {
				// a parent every table has (rows() gives every table a row 3), and never
				// row 1's or row 2's own FK value: rows() only ever nulls a nullable
				// column's third row, so this backfill only ever reaches row 3 -- reusing
				// its own row number rather than a fixed "1" cannot duplicate row 1's or
				// row 2's, which an EXCLUDE (WITH =) over this column would otherwise
				// refuse as a duplicate (23P01, measured)
				f = "3"
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.full(), c.name, f, qi(c.name)))
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
				if t.keys[i].unique && len(referencedBy(s, t, t.keys[i].cols[0])) > 0 {
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
			// c.fresh is excluded: a column another mutation just added carries whatever
			// single literal DEFAULT that mutation gave it (shared by every row, not the
			// base generator's per-row-distinct value), so becoming the primary key
			// without its own backfill risks a duplicate key on apply (measured: "toggle
			// exclude constraint operator"'s "ne" column, shared default 1 so its WITH <>
			// exclude always holds, moved onto as a PK loses that assumption -- !c.notNull
			// alone missed it, since that column was already NOT NULL)
			if !c.pk && c.gen == "" && isInteger(c.typ) && !c.fresh && !touched[t.name+"."+c.name] && !t.inFK(c.name) {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 || touched[t.name+"."+old.name] {
			return false
		}
		c := pick(r, cands)
		if !c.notNull {
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 9 where %s is null", t.full(), c.name, qi(c.name)))
		}
		c.pk, c.notNull, c.def = true, true, ""
		old.pk = false
		if len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		} else if !old.identity && !old.serial && r.Intn(2) == 0 {
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
		if !c.identity {
			// the rows need distinct values before the key goes on: the old key's
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.full(), c.name, qi(old.name)))
		}
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
		c := t.cols[len(t.cols)-1]
		if c.notNull {
			// existing rows need a parent before the column turns NOT NULL
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 1", t.full(), c.name))
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
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> "+t.schema+"."+to)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> "+t.schema+"."+to+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill "+t.schema+"."+to+".")
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
		for _, pol := range s.policies {
			if pol.table == t.name {
				pol.table = to
			}
		}
		for _, ru := range s.rules {
			if ru.table == t.name {
				ru.table = to
			}
		}
		for _, sq := range s.seqs {
			if sq.table == t.name {
				sq.table = to
			}
		}
		touched[t.name], touched[to] = true, true
		t.name = to
		return true
	}},
	{"move table to public schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema != "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> public."+t.name)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> public."+t.name+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill public."+t.name+".")
			s.intents[i] = in
		}
		touched[t.name] = true
		t.schema = "public"
		return true
	}},
	{"move table to app schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema == "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> app."+t.name)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> app."+t.name+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill app."+t.name+".")
			s.intents[i] = in
		}
		touched[t.name] = true
		t.schema = "app"
		return true
	}},
	{"collapse app schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// every remaining "app" table at once, so the schema itself goes: leaving even
		// one behind (as "move table to public schema" alone might, one at a time)
		// keeps "app" declared
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema != "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		for _, t := range cands {
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> public."+t.name)
			for i, in := range s.intents {
				in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> public."+t.name+".")
				in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill public."+t.name+".")
				s.intents[i] = in
			}
			touched[t.name] = true
			t.schema = "public"
		}
		return true
	}},
	{"change domain check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, d := range s.domains {
			if !touched["domain:"+d.name] {
				d.ge = !d.ge
				touched["domain:"+d.name] = true
				return true
			}
		}
		return false
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
			// a fresh enum (minted by an earlier "add table" / "add column" step, not in the
			// source) has no from-side to declare the drop against: 42704 "no such enum"
			if e.fresh {
				continue
			}
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
	{"toggle domain not null", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a column of the domain may already hold NULL: keep this off a domain any
		// column uses (existing NULLs would refuse the ALTER DOMAIN SET NOT NULL, 23502,
		// the same way an ungueded "add identity" once did -- see its mutation)
		for _, d := range s.domains {
			if !typeInUse(s, d.name) && !touched["domain:"+d.name] {
				d.notNull = !d.notNull
				touched["domain:"+d.name] = true
				return true
			}
		}
		return false
	}},
	{"drop domain", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, d := range s.domains {
			if !typeInUse(s, d.name) && !touched["domain:"+d.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["domain:"+s.domains[i].name] = true
		s.domains = append(s.domains[:i], s.domains[i+1:]...)
		return true
	}},
	{"drop enum type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, e := range s.enums {
			if !typeInUse(s, e.name) && !touched["enum:"+e.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["enum:"+s.enums[i].name] = true
		s.enums = append(s.enums[:i], s.enums[i+1:]...)
		return true
	}},
	{"add range type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.ranges = append(s.ranges, &pRange{name: s.next("rg")})
		return true
	}},
	{"add composite type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.composites = append(s.composites, s.newComposite(r))
		return true
	}},
	{"add enum type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// fresh: true, or a same-recipe "drop enum label" (unaware this enum has no
		// from-side of its own) can pick it and declare `-- @migrate enum <new>: drop ...`
		// against an enum "no such enum in the current schema" refuses (42704, measured).
		s.enums = append(s.enums, &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)], fresh: true})
		return true
	}},
	{"add domain type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.domains = append(s.domains, &pDomain{name: s.next("num")})
		return true
	}},
	{"drop range type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.ranges) == 0 {
			return false
		}
		i := r.Intn(len(s.ranges))
		s.ranges = append(s.ranges[:i], s.ranges[i+1:]...)
		return true
	}},
	{"add extension", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		have := map[string]bool{}
		for _, e := range s.extensions {
			have[e] = true
		}
		var cands []string
		for _, e := range []string{"pgcrypto", "citext"} {
			if !have[e] {
				cands = append(cands, e)
			}
		}
		if len(cands) == 0 {
			return false
		}
		s.extensions = append(s.extensions, pick(r, cands))
		return true
	}},
	{"drop extension", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		gistInUse := false
		for _, t := range s.tables {
			for _, k := range t.keys {
				if k.exclude && (k.usingGist || k.opNE) {
					gistInUse = true
				}
			}
		}
		var cands []int
		for i, e := range s.extensions {
			if e == "btree_gist" && gistInUse {
				continue // a gist EXCLUDE constraint depends on it
			}
			cands = append(cands, i)
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		s.extensions = append(s.extensions[:i], s.extensions[i+1:]...)
		return true
	}},
	{"add rule", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		s.rules = append(s.rules, &pRule{name: s.next("ru"), table: t.name, where: r.Intn(2) == 0, enabled: true})
		return true
	}},
	{"add sequence", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.seqs = append(s.seqs, &pSeq{name: s.next("sq")})
		return true
	}},
	{"drop sequence", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.seqs) == 0 {
			return false
		}
		i := r.Intn(len(s.seqs))
		s.seqs = append(s.seqs[:i], s.seqs[i+1:]...)
		return true
	}},
	{"set sequence owner", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pSeq
		for _, sq := range s.seqs {
			if sq.table == "" {
				cands = append(cands, sq)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		owned := map[string]bool{} // a column two sequences both claim OWNED BY is not
		// a shape any real schema author would declare, and adds()/renames() only expect
		// one from-side sequence to ever forward onto a given owner (sequenceFollowsRename,
		// migrate.go): picking a column another sequence already owns can match this
		// mutation's own new sequence to that unrelated one by coincidence and lose its
		// CREATE SEQUENCE entirely (measured).
		for _, sq := range s.seqs {
			if sq.table != "" {
				owned[sq.table+"."+sq.col] = true
			}
		}
		var cols []*pCol
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.identity && !c.serial && !c.fresh && c.gen == "" && !owned[t.name+"."+c.name] {
				cols = append(cols, c)
			}
		}
		if len(cols) == 0 {
			return false
		}
		sq := pick(r, cands)
		c := pick(r, cols)
		sq.table, sq.col = t.name, c.name
		return true
	}},
	{"clear sequence owner", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pSeq
		for _, sq := range s.seqs {
			if sq.table != "" {
				cands = append(cands, sq)
			}
		}
		if len(cands) == 0 {
			return false
		}
		sq := pick(r, cands)
		sq.table, sq.col = "", ""
		return true
	}},
	{"add seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if s.seedTable == nil || touched["seed"] {
			return false
		}
		st := s.seedTable
		st.rows = append(st.rows, pSeedRow{code: s.next("code"), label: "z"})
		return true
	}},
	{"drop seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		st := s.seedTable
		if st == nil || st.additive || len(st.rows) == 0 || touched["seed"] {
			return false
		}
		i := r.Intn(len(st.rows))
		st.rows = append(st.rows[:i], st.rows[i+1:]...)
		touched["seed"] = true
		return true
	}},
	{"change seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		st := s.seedTable
		if st == nil || len(st.rows) == 0 || touched["seed"] {
			return false
		}
		row := &st.rows[r.Intn(len(st.rows))]
		row.label += "!"
		touched["seed"] = true
		return true
	}},
	{"toggle seed table additive", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if s.seedTable == nil {
			return false
		}
		s.seedTable.additive = !s.seedTable.additive
		return true
	}},
	{"drop rule", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["rule:"+s.rules[i].name] = true
		s.rules = append(s.rules[:i], s.rules[i+1:]...)
		return true
	}},
	{"toggle rule enabled", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pRule
		for _, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, ru)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ru := pick(r, cands)
		ru.enabled = !ru.enabled
		touched["rule:"+ru.name] = true
		return true
	}},
	{"toggle rule where", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pRule
		for _, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, ru)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ru := pick(r, cands)
		ru.where = !ru.where
		touched["rule:"+ru.name] = true
		return true
	}},
	{"reword table comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.comment != "" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.comment += "!"
		return true
	}},
	{"reword column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		var owner []*pTable
		for _, t := range s.tables {
			for _, c := range t.cols {
				if c.comment != "" && !touched[t.name+".comment."+c.name] {
					cands = append(cands, c)
					owner = append(owner, t)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := r.Intn(len(cands))
		cands[i].comment += "!"
		touched[owner[i].name+".comment."+cands[i].name] = true
		return true
	}},
	{"change function return type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.retType == "integer" {
			f.retType = "bigint"
		} else {
			f.retType = "integer"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function strict", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.strict = !f.strict
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function volatility", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.volatility == "IMMUTABLE" {
			f.volatility = "STABLE"
		} else {
			f.volatility = "IMMUTABLE"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function argument default", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.argDefault = !f.argDefault
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function language", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.lang == "plpgsql" {
			f.lang = "sql"
		} else {
			f.lang = "plpgsql"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "procedure"
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to aggregate", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && f.retType == "integer" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "aggregate"
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to window", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// migrate.go's functions() only drops a LANGUAGE internal function that is a
		// range/multirange constructor (isRangeConstructor); a user-declared internal
		// WINDOW function like this one is tracked and its "window" property diffed
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "window"
		touched["function:"+f.name] = true
		return true
	}},
	{"change exclude constraint column", excludeChangeColumn},
	{"change exclude constraint column ", excludeChangeColumn}, // a second entry point (same body): see "toggle trigger fires on update "
	{"toggle exclude constraint where", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			if k.where == "positive" {
				k.where = ""
			} else {
				k.where = "positive"
			}
			return true
		}
		return false
	}},
	{"toggle exclude constraint operator", excludeToggleOperator},
	{"toggle exclude constraint operator ", excludeToggleOperator}, // a second entry point (same body): see "toggle trigger fires on update "
	{"toggle exclude constraint using", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude && !k.opNE { // opNE always renders as gist regardless
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			if !k.usingGist {
				s.ensureExtension("btree_gist")
			}
			k.usingGist = !k.usingGist
			return true
		}
		return false
	}},
	{"rename foreign key column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pFK
			for _, fk := range t.fks {
				if len(fk.cols) == 1 && !touched[t.name+"."+fk.cols[0]] {
					cands = append(cands, fk)
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := t.col(pick(r, cands).cols[0])
			to := c.name + "_new"
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
			renameRefs(s, t, c.name, to)
			touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
			c.name = to
			return true
		}
		return false
	}},
	{"rename unique key column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []string
			for _, k := range t.keys {
				if !k.unique {
					continue
				}
				for _, cn := range k.cols {
					if !touched[t.name+"."+cn] {
						cands = append(cands, cn)
					}
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := t.col(pick(r, cands))
			to := c.name + "_new"
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
			renameRefs(s, t, c.name, to)
			touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
			c.name = to
			return true
		}
		return false
	}},
	{"change foreign key on delete", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pFK
			for _, fk := range t.fks {
				if len(fk.cols) == 1 {
					cands = append(cands, fk)
				}
			}
			if len(cands) == 0 {
				continue
			}
			fk := pick(r, cands)
			c := t.col(fk.cols[0])
			opts := []string{"", "CASCADE", "RESTRICT"}
			if !c.notNull {
				opts = append(opts, "SET NULL")
			}
			var filtered []string
			for _, o := range opts {
				if o != fk.onDelete {
					filtered = append(filtered, o)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			next := pick(r, filtered)
			if next == "SET NULL" {
				c.notNull = false
			}
			fk.onDelete = next
			return true
		}
		return false
	}},
	{"toggle key unique", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude || k.where != "" || k.expr != "" {
					continue
				}
				if k.unique {
					// a foreign key elsewhere may rest on this unique key (same guard as
					// "drop key"): going plain drops the index a REFERENCES needs (42830,
					// measured)
					if !keyIsReferenced(s, t, k) {
						cands = append(cands, k)
					}
					continue
				}
				// going plain -> unique: every column needs a unique key's usual
				// safety (see addKey's comment: no repeats, no shared fresh default)
				safe := true
				for _, cn := range k.cols {
					c := t.col(cn)
					if c.typ == "jsonb" || c.typ == "boolean" || s.enum(c.typ) != nil || c.fresh {
						safe = false
					}
				}
				if safe {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.unique = !k.unique
			if !k.unique {
				k.notDist, k.defer_ = false, false
			}
			return true
		}
		return false
	}},
	{"change index columns", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if !k.unique && !k.exclude && k.expr == "" {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			var others []string
			for _, c := range t.cols {
				if c.typ == "jsonb" || c.name == k.cols[0] {
					continue
				}
				if k.where == "positive" && !isNumeric(c.typ) {
					continue // WHERE (col > 0) needs a numeric column
				}
				others = append(others, c.name)
			}
			if len(others) == 0 {
				continue
			}
			k.cols[0] = pick(r, others)
			return true
		}
		return false
	}},
	{"change policy command", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		var opts []string
		for _, c := range []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"} {
			if c != pol.command {
				opts = append(opts, c)
			}
		}
		pol.command = pick(r, opts)
		pol.using, pol.withCheck = policyPredicates(pol.command)
		touched["policy:"+pol.name] = true
		return true
	}},
	{"toggle policy role", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		pol.role = !pol.role
		touched["policy:"+pol.name] = true
		return true
	}},
	{"toggle view check option", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pView
		for _, v := range s.views {
			if !v.mat {
				cands = append(cands, v)
			}
		}
		if len(cands) == 0 {
			return false
		}
		v := pick(r, cands)
		if v.checkOption == "" {
			v.checkOption = pick(r, []string{"LOCAL", "CASCADED"})
		} else {
			v.checkOption = ""
		}
		return true
	}},
	{"toggle trigger fires on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.onUpdate = !tr.onUpdate
		return true
	}},
	{"toggle trigger fires on update ", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a second entry point (same body): raises this comparatively rare field's
		// selection odds against the growing mutation list, same reasoning as the
		// generate()-time spares elsewhere in this file
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.onUpdate = !tr.onUpdate
		return true
	}},
	{"add second trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addTrigger(r) }},
	{"change trigger function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// deterministically ordered (s.triggers is a slice, not a map): a repointed
		// trigger's own column still exists (same table as the one it now calls)
		var pairs [][2]*pTrigger
		for i, a := range s.triggers {
			for j, b := range s.triggers {
				if i != j && a.table == b.table {
					pairs = append(pairs, [2]*pTrigger{a, b})
				}
			}
		}
		if len(pairs) == 0 {
			return false
		}
		p := pick(r, pairs)
		p[0].callName = p[1].callName
		return true
	}},
	{"change trigger function ", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a second entry point (same body): raises this field's selection odds against
		// the growing mutation list, same reasoning as "toggle trigger fires on update "
		var pairs [][2]*pTrigger
		for i, a := range s.triggers {
			for j, b := range s.triggers {
				if i != j && a.table == b.table {
					pairs = append(pairs, [2]*pTrigger{a, b})
				}
			}
		}
		if len(pairs) == 0 {
			return false
		}
		p := pick(r, pairs)
		p[0].callName = p[1].callName
		return true
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
	{"add view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addViewKind(r, false) }},
	{"add materialized view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addViewKind(r, true) }},
	{"drop view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// plain views only: a shared pool with materialized ones (the original form of this
		// mutation, r.Intn(len(s.views)) over both) let directedCoverage's "- matview" entry
		// depend on which kind the draw happened to land on, and its own retry loop (30
		// attempts) stops at the first *successful* application regardless of which kind was
		// dropped -- a schema with only a plain view spare (generate()'s r.Intn(2)==0, no
		// materialized one, r.Intn(3)==0) applies this mutation just fine and never comes
		// back for another try, so a seed unlucky enough to draw only-plain across all 30
		// attempts leaves "- matview" unhit (measured: -migrate-probe-seed 2 -migrate-probe-n
		// 100). Splitting into two mutations, one per kind (mirroring "add view" / "add
		// materialized view" already being separate), gives directedCoverage its own
		// dedicated draw for each: it can only report "- matview" stuck if no materialized
		// view spare was generated in 30 fresh schemas, (2/3)^30 short of ever observed.
		var idx []int
		for i, v := range s.views {
			if !v.mat {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return false
		}
		i := pick(r, idx)
		s.views = append(s.views[:i], s.views[i+1:]...)
		return true
	}},
	{"drop materialized view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// the materialized half of "drop view" (see its own comment): kept as its own
		// candidate pool so directedCoverage always gets a materialized view to drop when
		// generate() seeded one, instead of sharing a draw with plain views.
		var idx []int
		for i, v := range s.views {
			if v.mat {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return false
		}
		i := pick(r, idx)
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
		// its pTriggerFn is not tied to the trigger's own lifetime (see pTrigger's doc
		// comment), so this never needs to check who else might call it
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
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9), retType: "integer", volatility: "IMMUTABLE", lang: "sql"})
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
	{"column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		var owner []*pTable
		for _, t := range s.tables {
			for _, c := range t.cols {
				if c.gen == "" && !touched[t.name+".comment."+c.name] {
					cands = append(cands, c)
					owner = append(owner, t)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := r.Intn(len(cands))
		c := cands[i]
		if c.comment == "" {
			c.comment = "about " + owner[i].name + "." + c.name
		} else {
			c.comment = ""
		}
		touched[owner[i].name+".comment."+c.name] = true
		return true
	}},
	{"change check operator", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pCheck
			for _, ck := range t.checks {
				if isNumeric(t.col(ck.col).typ) {
					cands = append(cands, ck)
				}
			}
			if len(cands) == 0 {
				continue
			}
			ck := pick(r, cands)
			ck.ge = !ck.ge
			return true
		}
		return false
	}},
	{"toggle unique nulls distinct", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.unique {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.notDist = !k.notDist
			return true
		}
		return false
	}},
	{"toggle unique deferrable", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				// a foreign key elsewhere may rest on this unique constraint: PostgreSQL
				// refuses a deferrable one there (55000 "cannot use a deferrable unique
				// constraint for referenced table", measured)
				if k.unique && !keyIsReferenced(s, t, k) {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.defer_ = !k.defer_
			return true
		}
		return false
	}},
	{"toggle foreign key deferrable", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			pick(r, t.fks).defer_ = !pick(r, t.fks).defer_
			return true
		}
		return false
	}},
	{"change foreign key on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// "NO ACTION" dropped from the option list (unlike "change foreign key on delete",
		// which never had it): confupdtype's own default byte is 'a' (no action), which
		// diff.Props (diff.go) only ever records once it differs from 'a' -- so "" and
		// explicit "NO ACTION" render as the very same absent "on update" property, and a
		// draw landing on one in place of the other applies cleanly but changes nothing a
		// diff can see. directedCoverage's retry loop only checks that a mutation *applied*
		// (len(applied) > 0), not that it changed anything -- a seed unlucky enough to only
		// ever draw that no-op transition across all 30 attempts left "~ constraint on
		// update" unhit (measured: -migrate-probe-seed 2 -migrate-probe-n 100, exposed once
		// "drop view" / "drop materialized view" split shifted this mutation's own draw).
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			fk := pick(r, t.fks)
			var opts []string
			for _, o := range []string{"", "CASCADE", "RESTRICT"} {
				if o != fk.onUpdate {
					opts = append(opts, o)
				}
			}
			fk.onUpdate = pick(r, opts)
			return true
		}
		return false
	}},
	{"add partial index", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addPartialIndex(r, pick(r, ts))
	}},
	{"add expression index", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addExprIndex(r, pick(r, ts))
	}},
	{"add exclude constraint", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addExclude(r, pick(r, ts))
	}},
	{"change index predicate", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if !k.unique && k.where != "" {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			c := t.col(k.cols[0])
			if k.where == "notnull" && isNumeric(c.typ) {
				k.where = "positive"
			} else {
				k.where = "notnull"
			}
			return true
		}
		return false
	}},
	{"toggle row security", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		t.rowSec = !t.rowSec
		if !t.rowSec {
			t.forceRowSec = false
		}
		return true
	}},
	{"toggle force row security", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.rowSec {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.forceRowSec = !t.forceRowSec
		return true
	}},
	{"add policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		command := pick(r, []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"})
		using, withCheck := policyPredicates(command)
		s.policies = append(s.policies, &pPolicy{name: s.next("pol"), table: t.name, command: command,
			permissive: r.Intn(4) != 0, using: using, withCheck: withCheck})
		return true
	}},
	{"drop policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		s.policies = append(s.policies[:i], s.policies[i+1:]...)
		return true
	}},
	{"change policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		pol.permissive = !pol.permissive
		touched["policy:"+pol.name] = true
		return true
	}},
	{"add composite attribute", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// ADD ATTRIBUTE leaves a using column's stored values alone (measured); a table
		// using the type is not excluded here -- except one whose column default is a
		// frozen ROW(...) literal, which the new attribute count would leave stale
		var cands []*pComposite
		for _, co := range s.composites {
			if !compositeArityFrozen(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		typ := "integer"
		if r.Intn(2) == 0 {
			typ = "text"
		}
		co.attrs = append(co.attrs, &pAttr{name: s.next("a"), typ: typ})
		touched["composite:"+co.name] = true
		return true
	}},
	{"drop composite attribute", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// DROP ATTRIBUTE leaves a using column's stored values alone (measured); a table
		// using the type is not excluded here -- except one whose column default is a
		// frozen ROW(...) literal, same reason as "add composite attribute"
		var cands []*pComposite
		for _, co := range s.composites {
			if len(co.attrs) > 1 && !compositeArityFrozen(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		co.attrs = co.attrs[:len(co.attrs)-1]
		touched["composite:"+co.name] = true
		return true
	}},
	{"change composite attribute type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// ALTER ATTRIBUTE TYPE, unlike ADD / DROP, is refused while a table uses the type
		// (0A000, measured; the planner turns it into a problem -- see alterType's
		// "composite" case): keep this one off a composite any column still carries.
		var cands []*pComposite
		for _, co := range s.composites {
			if !compositeInUse(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		a := pick(r, co.attrs)
		switch a.typ {
		case "integer":
			a.typ = "bigint"
		case "bigint":
			a.typ = "integer"
		case "text":
			a.typ = "varchar(50)"
		default:
			a.typ = "text"
		}
		touched["composite:"+co.name] = true
		return true
	}},
	{"drop composite type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, co := range s.composites {
			if !compositeInUse(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["composite:"+s.composites[i].name] = true
		s.composites = append(s.composites[:i], s.composites[i+1:]...)
		return true
	}},
	{"toggle identity kind", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.identity && !touched[t.name+"."+c.name] {
					cands = append(cands, c)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		c.idAlways = !c.idAlways
		return true
	}},
	{"add identity", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.plainCols(touched) {
			if !isInteger(c.typ) || c.serial {
				continue
			}
			// ON DELETE SET NULL needs the column nullable; leave it alone (same guard as
			// "change nullability and default")
			settable := true
			for _, fk := range t.fks {
				if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
					settable = false
				}
			}
			if settable {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		c.identity, c.def = true, ""
		if c.notNull {
			c.idAlways = r.Intn(2) == 0
		} else {
			// IDENTITY implies NOT NULL: existing NULLs need a backfill before the server's
			// own SET NOT NULL (23502 "contains null values" otherwise, measured). A backfill
			// UPDATE assigns the column a plain value, which GENERATED ALWAYS forbids by
			// declaration (428C9 "can only be updated to DEFAULT") even though the UPDATE
			// runs before the column turns identity: keep the column BY DEFAULT, which an
			// explicit value is always allowed to override.
			f := s.fill(c)
			if t.inFK(c.name) {
				// a parent every table has (rows() gives every table a row 3), and never
				// row 1's or row 2's own FK value: rows() only ever nulls a nullable
				// column's third row, so this backfill only ever reaches row 3 -- reusing
				// its own row number rather than a fixed "1" cannot duplicate row 1's or
				// row 2's, which an EXCLUDE (WITH =) over this column would otherwise
				// refuse as a duplicate (23P01, measured)
				f = "3"
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.full(), c.name, f, qi(c.name)))
			c.notNull, c.idAlways = true, false
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"drop identity", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.identity && !touched[t.name+"."+c.name] {
					c.identity = false
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"add partitioned table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		pt := s.newPartTable(r)
		s.partTables = append(s.partTables, pt)
		touched[pt.name] = true
		return true
	}},
	{"add partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		pts := untouchedPartTables(s, touched)
		if len(pts) == 0 {
			return false
		}
		pt := pick(r, pts)
		name := s.next(pt.name + "_")
		if pt.strategy == "LIST" {
			used := map[string]bool{}
			for _, c := range pt.parts {
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
			pt.parts = append(pt.parts, &pPartChild{name: name, orig: "public." + name, values: []string{letter}})
		} else {
			hi := 0
			for _, c := range pt.parts {
				if !c.isDefault && !c.gone && c.hi > hi {
					hi = c.hi
				}
			}
			pt.parts = append(pt.parts, &pPartChild{name: name, orig: "public." + name, lo: hi, hi: hi + 10})
		}
		touched[pt.name] = true
		return true
	}},
	{"drop partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) < 2 {
				continue // keep at least one ordinary partition besides DEFAULT
			}
			c := pick(r, live)
			c.gone = true
			s.intents = append(s.intents, "-- @migrate drop "+c.orig)
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"detach partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			if pt.idCol().serial || pt.idCol().identity {
				// a detached child keeps pt's own id column, bigserial / IDENTITY included
				// (render()'s own comment) -- consistent on its own, but "attach partition"
				// reattaching it would have to carry its now-independent owned sequence back
				// across the ATTACH, which detectRepartitions/repartitionTable's own widening
				// (brief-pg-repartition-seq.md) never reaches: a standalone table joining a
				// partitioned one, not a whole table's own partition key changing. Left to a
				// plain-id pt so "attach partition" (the only side that would actually need
				// this) never has to.
				continue
			}
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) < 2 {
				continue // keep at least one ordinary partition besides DEFAULT
			}
			c := pick(r, live)
			c.detached = true
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"attach partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			for _, c := range pt.parts {
				if c.detached && !c.gone {
					c.detached = false
					touched[pt.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"move partition bound", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			var last *pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					last = c // the highest RANGE bound / most recently added LIST child
				}
			}
			if last == nil {
				continue
			}
			if pt.strategy == "LIST" {
				last.values = append(last.values, "w")
			} else {
				last.hi += 5
			}
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"shrink partition bound", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// unlike "move partition bound" (which only ever widens, so the row already
		// sitting at a child's own bound -- see pPartTable.rows -- always keeps fitting),
		// this one narrows a child's bound past that very row, so it no longer belongs and
		// the plan (partitionMovePredicate, migrate.go) has to move it back to the parent
		// before DETACH + ATTACH runs, into the DEFAULT partition (brief-holes-pg.md item
		// C). The row is not lost -- appends a DO $$ check confirming the parent's total
		// count still holds it, since diff.Compare (judge's usual check) never looks at
		// data.
		for _, pt := range untouchedPartTables(s, touched) {
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) == 0 {
				continue
			}
			c := pick(r, live)
			switch pt.strategy {
			case "LIST":
				used := map[string]bool{}
				for _, o := range pt.parts {
					for _, v := range o.values {
						used[v] = true
					}
				}
				letter := ""
				for _, cand := range []string{"m", "n", "o", "p", "q"} {
					if !used[cand] {
						letter = cand
						break
					}
				}
				if letter == "" {
					continue
				}
				c.values = []string{letter} // the row's own kind (c's former values[0]) no longer matches
			default: // RANGE
				if c.hi-c.lo < 2 {
					continue // no room to raise lo past the row sitting at the old lo and stay a valid bound
				}
				c.lo++ // the row sits exactly at the old lo (pPartTable.rows), now excluded
			}
			total := 0 // one row per part from pt.rows() that is neither gone nor detached
			for _, o := range pt.parts {
				if !o.gone && !o.detached {
					total++
				}
			}
			s.checks = append(s.checks, fmt.Sprintf(
				"DO $$ BEGIN IF (SELECT count(*) FROM %s) <> %d THEN RAISE EXCEPTION 'partition %s row count: %%', (SELECT count(*) FROM %s); END IF; END $$;",
				qi(pt.name), total, pt.name, qi(pt.name)))
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"change partition strategy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// Rebuilds pt under the other strategy (RANGE <-> LIST), reaching "~ table
		// partition key" (diff.Alphabet) through the generator instead of only by hand
		// (TestProbeRepartitionRangeToList, migrate_test.go): migrate.go's repartitionTable
		// (detectRepartitions / repartitionEligible) renames the source table aside,
		// creates the target's own shape fresh under the old name and lets PostgreSQL's
		// partition router place every existing row -- refusing (23514) if one fits
		// nowhere, which a DEFAULT partition on both the old and the new side always rules
		// out. pPartTable is already narrow enough (no key, no index, no foreign key, and
		// -- unlike pTable -- no policy / rule / trigger / comment of its own; see its doc
		// comment) to sit inside repartitionEligible's scope unconditionally, so this
		// mutation never needs to check eligibility itself the way a pTable-based one
		// would have to.
		pts := untouchedPartTables(s, touched)
		if len(pts) == 0 {
			return false
		}
		pt := pick(r, pts)
		oldMax := pt.maxRowID()
		// a child pt already carries detached (not gone) is its own independent table by
		// now (detectRepartitions only folds a *currently attached* child into its old
		// parent's demolition -- schema.Relation.Parents is empty the moment DETACH runs),
		// so replacing pt.parts wholesale would silently drop it out of the target with no
		// @migrate declaring so (measured): carried forward unchanged, same as
		// pTableFromPart / "departition table" have to.
		var carry []*pPartChild
		for _, c := range pt.parts {
			if c.detached && !c.gone {
				carry = append(carry, c)
			}
		}
		newStrategy := "LIST"
		if pt.strategy == "LIST" {
			newStrategy = "RANGE"
		}
		pt.strategy = newStrategy
		c1, c2 := s.next(pt.name+"_"), s.next(pt.name+"_")
		def := s.next(pt.name + "_")
		if newStrategy == "LIST" {
			pt.parts = []*pPartChild{
				{name: c1, orig: pt.orig, values: []string{"a", "b"}},
				{name: c2, orig: pt.orig, values: []string{"c"}},
				{name: def, orig: pt.orig, isDefault: true},
			}
		} else {
			pt.parts = []*pPartChild{
				{name: c1, orig: pt.orig, lo: 1, hi: 10},
				{name: c2, orig: pt.orig, lo: 10, hi: 20},
				{name: def, orig: pt.orig, isDefault: true},
			}
		}
		pt.parts = append(pt.parts, carry...)
		repartitionSeqCheck(s, pt, oldMax)
		touched[pt.name] = true
		return true
	}},
	{"departition table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// The reverse direction of "add partitioned table": an existing partitioned pt
		// loses its PARTITION BY entirely and becomes one plain table under its own name,
		// its rows routed back by repartitionTable/pendingRepartition the same way. Uses
		// pTableFromPart to get a pTable that renders as an ordinary table (see its doc
		// comment); the old pt is removed from s.partTables so render()/rows() only ever
		// describe it once, and the new plain pTable is otherwise indistinguishable from
		// one that had started out unpartitioned. Any child pt already carries detached
		// (not gone) is its own independent table by now, unaffected by departitioning its
		// former parent -- carried forward as its own plain pTable, same reasoning as
		// "change partition strategy" (measured: dropping it silently otherwise).
		pts := untouchedPartTables(s, touched)
		if len(pts) == 0 {
			return false
		}
		pt := pick(r, pts)
		oldMax := pt.maxRowID()
		var kept []*pPartTable
		for _, other := range s.partTables {
			if other != pt {
				kept = append(kept, other)
			}
		}
		s.partTables = kept
		s.tables = append(s.tables, pTableFromPart(pt))
		repartitionSeqCheck(s, pt, oldMax)
		for _, c := range pt.parts {
			if c.detached && !c.gone {
				// render() gave this already-detached child pt's own id column (see its own
				// comment) -- carried through here too, or departitioning pt would silently
				// make this unrelated table's id column look changed (measured).
				s.tables = append(s.tables, pTableFromPart(&pPartTable{name: c.name, orig: c.orig, id: pt.idCol()}))
			}
		}
		touched[pt.name] = true
		return true
	}},
}

// mutations18 is PostgreSQL-18-only vocabulary (brief-holes-pg.md item E), added to
// mutations for TestMigrateProbe18 alone: VIRTUAL generated columns, and NOT ENFORCED on
// a CHECK or a foreign key. WITHOUT OVERLAPS (a temporal PRIMARY KEY / UNIQUE, needing a
// range-typed column pTable's own vocabulary does not otherwise carry) and PERIOD foreign
// keys are measured by hand instead (see alphabetKnownUnreached's own entries for why).
var mutations18 = []mutation{
	{"add virtual generated column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "numeric(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name, genVirtual: true}
		x.fresh, g.fresh = true, true
		insertCol(r, t, g)
		insertCol(r, t, x)
		touched[t.name+"."+x.name], touched[t.name+"."+g.name] = true, true
		return true
	}},
	{"toggle generated kind", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pCol
			for _, c := range t.cols {
				if c.gen != "" && !c.fresh && !touched[t.name+"."+c.name] {
					cands = append(cands, c)
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := pick(r, cands)
			c.genVirtual = !c.genVirtual
			touched[t.name+"."+c.name] = true
			return true
		}
		return false
	}},
	{"toggle check not enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.checks) == 0 {
				continue
			}
			ck := pick(r, t.checks)
			ck.notEnforced = !ck.notEnforced
			touched[t.name] = true
			return true
		}
		return false
	}},
	{"toggle foreign key not enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			fk := pick(r, t.fks)
			fk.notEnforced = !fk.notEnforced
			touched[t.name] = true
			return true
		}
		return false
	}},
	{"add without overlaps key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		pk := t.pk()
		if pk == nil || !isInteger(pk.typ) {
			return false
		}
		// a fresh integer scalar column and a fresh daterange column, the range last
		// (WITHOUT OVERLAPS requires it there); both backfilled from the table's own
		// primary key so every row's span is [pk, pk+1) -- distinct integers make
		// distinct, non-overlapping ranges, whatever pk's own values are.
		scalar := &pCol{name: s.next("c"), typ: pk.typ, notNull: true, fresh: true}
		span := &pCol{name: s.next("span"), typ: "daterange", notNull: true, fresh: true}
		insertCol(r, t, scalar)
		insertCol(r, t, span)
		s.intents = append(s.intents,
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.full(), scalar.name, qi(pk.name)),
			fmt.Sprintf("-- @migrate backfill %s.%s = daterange('2024-01-01'::date + %s::integer, '2024-01-02'::date + %s::integer)",
				t.full(), span.name, qi(pk.name), qi(pk.name)))
		t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{scalar.name, span.name}, withoutOverlaps: true})
		s.ensureExtension("btree_gist")
		touched[t.name+"."+scalar.name], touched[t.name+"."+span.name] = true, true
		return true
	}},
	{"toggle key without overlaps", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// Flips an *existing* key's own WITHOUT OVERLAPS flag -- unlike "add without
		// overlaps key" above (always a brand new constraint, + constraint), this reaches
		// "~ constraint without overlaps" (diff.Alphabet): needs a candidate that already
		// carries the flag, seeded by generate() itself (its own doc comment) since no
		// in-recipe mutation adds one to an existing key in place. A plain UNIQUE over the
		// same columns is valid regardless of the range column's type (ordinary btree
		// equality), so turning the flag off never needs a column or shape change; turning
		// it on for an arbitrary key elsewhere would (its last column would have to be a
		// range type first), so this only ever finds the generate()-seeded temporal key in
		// practice.
		for _, t := range untouched(s, touched) {
			for _, k := range t.keys {
				if !k.unique || !k.withoutOverlaps {
					continue
				}
				// a PERIOD foreign key elsewhere may already rest on this key (same
				// guard "drop key" uses for an ordinary key an FK references): turning
				// WITHOUT OVERLAPS off here would leave that FK referencing a key that no
				// longer qualifies as temporal (measured).
				rested := false
				for _, other := range s.tables {
					for _, fk := range other.fks {
						if fk.withPeriod && fk.refTable == t.name {
							rested = true
						}
					}
				}
				if rested {
					continue
				}
				k.withoutOverlaps = !k.withoutOverlaps
				touched[t.name] = true
				return true
			}
		}
		return false
	}},
	{"toggle foreign key period", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// Widens an *existing*, ordinary foreign key onto the referenced table's own
		// WITHOUT OVERLAPS key instead, PERIOD -- reaches "~ constraint period"
		// (diff.Alphabet) the same way "toggle key without overlaps" reaches its own
		// field: a candidate needs a plain FK resting on a table that also carries a
		// temporal key over one more column than the FK currently uses, which only
		// generate()'s own seeded pair (its doc comment) provides.
		for _, t := range untouched(s, touched) {
			for _, fk := range t.fks {
				if fk.withPeriod {
					continue
				}
				parent := s.table(fk.refTable)
				if parent == nil {
					continue
				}
				var tk *pKey
				for _, k := range parent.keys {
					if k.unique && k.withoutOverlaps && len(k.cols) == len(fk.refCols)+1 {
						tk = k
						break
					}
				}
				if tk == nil {
					continue
				}
				rangeCol := tk.cols[len(tk.cols)-1]
				var ownRange *pCol
				for _, c := range t.cols {
					if c.typ == "daterange" && indexOf(fk.cols, c.name) < 0 {
						ownRange = c
						break
					}
				}
				if ownRange == nil {
					continue
				}
				fk.cols = append(append([]string(nil), fk.cols...), ownRange.name)
				fk.refCols = append(append([]string(nil), fk.refCols...), rangeCol)
				fk.withPeriod = true
				touched[t.name] = true
				return true
			}
		}
		return false
	}},
}

// pTableFromPart is pt rendered as one plain (unpartitioned) table: the same name, schema
// and two columns pPartTable.render() always uses (id integer NOT NULL, kind text NOT
// NULL), no PARTITION BY and no children -- "departition table"'s target side, so that
// side's diff.Props sees "partition key" go from pt's own RANGE/LIST text to empty (the
// same "~ table partition key" alphabet entry a strategy change produces, the other
// direction). No key, index or foreign key either, matching pt's own scope exactly so
// nothing about repartitionEligible needs to be reconsidered for it.
func pTableFromPart(pt *pPartTable) *pTable {
	id := *pt.idCol() // pt's own id column (plain / bigserial / IDENTITY), copied so mutating it here never reaches back into pt
	return &pTable{
		schema: "public",
		name:   pt.name,
		orig:   pt.orig,
		cols: []*pCol{
			&id,
			{name: "kind", typ: "text", notNull: true},
		},
	}
}

// repartitionSeqCheck appends a check (run, like "shrink partition bound"'s own, after the
// plan's DDL and before judge() ever compares schemas) that a fresh row inserted with no
// explicit id continues pt's sequence from where the repartition (change partition strategy
// / departition table) found it, rather than colliding back at 1 the way a brand new
// IDENTITY sequence otherwise would (migrate.go's pendingRepartition is what is actually
// under test here: OVERRIDING SYSTEM VALUE on the carried-over rows, then setval()) --
// 'zz' never matches any LIST child's own values and any id an auto-generated one produces
// is comfortably past 100000, so the new row always lands in whichever side's DEFAULT
// partition (RANGE or LIST), or the one remaining plain table once departitioned. A plain
// integer id has no default to insert without one at all, so only a bigserial or IDENTITY
// pt needs this.
func repartitionSeqCheck(s *pSchema, pt *pPartTable, oldMax int) {
	if !pt.idCol().serial && !pt.idCol().identity {
		return
	}
	s.checks = append(s.checks, fmt.Sprintf(
		"INSERT INTO %s (kind) VALUES ('zz');\n"+
			"DO $$ BEGIN IF (SELECT max(id) FROM %s) <> %d THEN RAISE EXCEPTION 'partition %s id continuation: %%', (SELECT max(id) FROM %s); END IF; END $$;",
		qi(pt.name), qi(pt.name), oldMax+1, pt.name, qi(pt.name)))
}

// untouchedPartTables is untouched's counterpart for pPartTable: candidates a mutation may
// still act on, keyed the same way (touched[pt.name]) so a recipe never applies two
// partition mutations to the same partitioned table in one pair (matching how pTable
// mutations use touched[t.name] to keep a pair's changes disjoint and so unambiguous to
// re-derive @migrate declarations for).
func untouchedPartTables(s *pSchema, touched map[string]bool) []*pPartTable {
	var out []*pPartTable
	for _, pt := range s.partTables {
		if !touched[pt.name] {
			out = append(out, pt)
		}
	}
	return out
}

// policyPredicates is which of USING / WITH CHECK a policy for command needs: SELECT and
// DELETE read only (USING), INSERT writes only (WITH CHECK), ALL and UPDATE both.
func policyPredicates(command string) (using, withCheck bool) {
	switch command {
	case "SELECT", "DELETE":
		return true, false
	case "INSERT":
		return false, true
	}
	return true, true
}

type step struct {
	m    int
	seed int64
}

func mutate(muts []mutation, src *pSchema, recipe []step) (*pSchema, []string) {
	s := src.clone()
	s.mutating = true
	touched := map[string]bool{}
	var applied []string
	for _, st := range recipe {
		r := rand.New(rand.NewSource(st.seed))
		if muts[st.m].apply(r, s, touched) {
			applied = append(applied, muts[st.m].name)
		}
	}
	return s, applied
}
