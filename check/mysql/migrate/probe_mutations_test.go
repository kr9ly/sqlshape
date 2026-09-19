package migrate

import (
	"fmt"
	"math/rand"
	"strings"
)

// ---- mutations ------------------------------------------------------------------------

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

// referencedByAny: the foreign keys (in any table) that reference table t at all, on any
// of its columns.
// nonFreshEvents are s's events that already existed in the source (not one an earlier
// step in the same recipe just added): a step that only sets one of their optional
// fields, on one of those, produces a whole "+ event" (diff.Compare never sees the from
// side at all), not the per-field "~ event ..." the event mutations below exist for.
func nonFreshEvents(s *pSchema) []*pEvent {
	var out []*pEvent
	for _, e := range s.events {
		if !e.fresh {
			out = append(out, e)
		}
	}
	return out
}

func referencedByAny(s *pSchema, t *pTable) []*pFK {
	var out []*pFK
	for _, other := range s.tables {
		for _, fk := range other.fks {
			if fk.refTable == t.name {
				out = append(out, fk)
			}
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
		if c.typ == "point" && s.mutating {
			// t already holds rows (this is a mutation, not a fresh table): unlike every
			// other type here, MySQL has no implicit "zero value" for a NOT NULL geometry
			// column, and even an expression DEFAULT does not save it (Error 1138,
			// "Invalid use of NULL value", measured both ways) -- a point column added
			// under rows can only be nullable.
			c.notNull, c.def = false, ""
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
		var cands []*pCol
		for _, c := range t.plainCols(touched) {
			// point is always NOT NULL (a SPATIAL index needs it, and it takes no plain
			// literal DEFAULT the way defaultFor spells one)
			if c.typ != "point" {
				cands = append(cands, c)
			}
		}
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
	{"add fulltext key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addFulltextKey(r, pick(r, ts))
	}},
	{"add spatial key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addSpatialKey(r, pick(r, ts))
	}},
	{"add functional key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addFunctionalKey(r, pick(r, ts))
	}},
	{"toggle key invisible", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.keys) > 0 {
				k := pick(r, t.keys)
				k.invisible = !k.invisible
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
	{"change foreign key actions", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, fk := range t.fks {
				key := t.name + ".fk:" + fk.name
				if touched[key] {
					continue
				}
				fk.onDelete = pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})
				fk.onUpdate = pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})
				// SET NULL on either action refuses a NOT NULL referencing column (1216-class)
				if fk.onDelete == "SET NULL" || fk.onUpdate == "SET NULL" {
					for _, name := range fk.cols {
						t.col(name).notNull = false
					}
				}
				touched[key] = true
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
	{"change check expression", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, ck := range t.checks {
				key := t.name + ".check:" + ck.name
				if touched[key] {
					continue
				}
				if ck.op == ">=" {
					ck.op = ">"
				} else {
					ck.op = ">="
				}
				touched[key] = true
				return true
			}
		}
		return false
	}},
	// CheckProps compares expression and enforced together (diff.CheckProps), so the planner
	// redoes the constraint as a DROP + ADD rather than ALTER TABLE ... ALTER CHECK ...
	// [NOT] ENFORCED; both a real mysqld accepts, and this exercises the DROP + ADD path.
	{"toggle check enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, ck := range t.checks {
				key := t.name + ".check:" + ck.name
				if touched[key] {
					continue
				}
				ck.enforced = !ck.enforced
				touched[key] = true
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
		s.events = append(s.events, &pEvent{name: s.next("ev"), n: 1 + r.Intn(9), fresh: true})
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
	{"table charset", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a plain (no explicit per-column COLLATE) text / varchar / enum column implicitly
		// takes the table's own default charset at CREATE time; migrate.go's alterTable
		// re-issues such a column's own MODIFY COLUMN once the table's own default has
		// moved out from under it (see its own doc comment for why: ALTER TABLE ... DEFAULT
		// CHARSET= alone never retroactively converts it), so both a table where every
		// string column already spells its own COLLATE explicitly (this generator's
		// `collate` flag) and one where none do are candidates now.
		cands := untouched(s, touched)
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.charset == "latin1" {
			// back to undeclared (the server default, utf8mb4 family): toggling between
			// "" and "utf8mb4"/"utf8mb4_bin" alone never actually changes the charset
			// (only the collation -- undeclared still canonicalizes to the utf8mb4
			// family, measured), so this needs a genuinely different charset to ever
			// exercise "~ table charset" itself, not only "~ table collation"
			t.charset, t.collation = "", ""
		} else {
			t.charset, t.collation = "latin1", "latin1_swedish_ci"
		}
		return true
	}},
	{"table row format", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		switch t.rowFormat {
		case "":
			t.rowFormat = "DYNAMIC"
		case "DYNAMIC":
			t.rowFormat = "COMPRESSED"
		default:
			t.rowFormat = ""
		}
		return true
	}},
	{"table engine", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			// MyISAM takes neither this table's own foreign keys nor another's
			// referencing it (Error 1215, "Cannot add foreign key constraint",
			// measured); touched below keeps a later step from adding either.
			if len(t.fks) > 0 || len(referencedByAny(s, t)) > 0 {
				continue
			}
			// nor a SRID-bound spatial column (Error 1178, "The storage engine for
			// the table doesn't support geographic spatial reference systems",
			// measured): only InnoDB does. Nor a DESC key part (same Error 1178,
			// "...doesn't support descending indexes", measured): MyISAM key parts are
			// always ascending.
			unfit := false
			for _, c := range t.cols {
				if c.typ == "point" {
					unfit = true
				}
			}
			for _, k := range t.keys {
				for _, d := range k.desc {
					if d {
						unfit = true
					}
				}
				for _, e := range k.expr {
					if e != "" {
						unfit = true // a functional key part: not risking MyISAM support
					}
				}
			}
			if unfit {
				continue
			}
			cands = append(cands, t)
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.engine == "MyISAM" {
			t.engine = ""
		} else {
			t.engine = "MyISAM"
		}
		touched[t.name] = true
		return true
	}},
	{"column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
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
		if c.comment == "" {
			c.comment = "now about " + c.name
		} else {
			c.comment = ""
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	// table auto_increment: a live counter, not data the diff reports -- the design keeps
	// dump.normalizeTable from seeing an existing table's AUTO_INCREMENT at all, so a plan
	// touching only this is empty and a second Plan is empty too (the server may coerce a
	// requested value below the rows' own counter upward; that is not a difference either).
	{"table auto_increment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if pk := t.pk(); pk != nil && pk.auto {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.autoInc > 0 && r.Intn(2) == 0 {
			t.autoInc = 0
		} else {
			t.autoInc = 100 + r.Intn(900)
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
	{"add procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.procs = append(s.procs, &pProc{name: s.next("p"), n: 1 + r.Intn(9)})
		return true
	}},
	{"drop procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.procs) == 0 {
			return false
		}
		i := r.Intn(len(s.procs))
		s.procs = append(s.procs[:i], s.procs[i+1:]...)
		return true
	}},
	{"change procedure body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.procs) == 0 {
			return false
		}
		pick(r, s.procs).n += 10
		return true
	}},
	{"toggle event at", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.at != "" {
			e.at = ""
		} else {
			e.at, e.starts, e.ends = "2099-03-01 00:00:00", "", "" // AT excludes STARTS/ENDS
		}
		return true
	}},
	// "toggle event bounds" used to cover both directions in one mutation, deterministic on
	// each candidate event's own current state; directed coverage only asks that a mutation
	// applies at all, not that it lands on a particular direction, so it never guaranteed
	// this alphabet gate's own "~ event starts" / "~ event ends" the way it looked like it
	// did (measured: adding this package's own new partitioning mutations shifted the random
	// pairs' draws enough that neither direction landed at seed 1 anymore). Split in two, so
	// each direction is its own candidate set directed coverage (and the random pairs) can
	// draw independently.
	{"set event bounds", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range nonFreshEvents(s) {
			if e.at != "" || e.starts != "" {
				continue // STARTS/ENDS belong to the EVERY form only, and only when unset
			}
			e.starts, e.ends = "2099-01-01 00:00:00", "2099-06-01 00:00:00"
			return true
		}
		return false
	}},
	{"clear event bounds", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range nonFreshEvents(s) {
			if e.starts == "" {
				continue
			}
			e.starts, e.ends = "", ""
			return true
		}
		return false
	}},
	{"toggle event completion", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.completion == "" {
			e.completion = "PRESERVE"
		} else {
			e.completion = ""
		}
		// status rides along: this mutation is the only reliable way seed 1's specific
		// draws exercise "~ event status" (a dedicated "toggle event status" mutation
		// exists but this recipe's random walk never happens to select its own index in
		// 200 pairs, measured -- an artifact of a fixed seed, not a real gap).
		if e.status == "" {
			e.status = "DISABLE"
		} else {
			e.status = ""
		}
		return true
	}},
	{"toggle event status", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.status == "" {
			e.status = "DISABLE"
		} else {
			e.status = ""
		}
		return true
	}},
	{"event comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.comment == "" {
			e.comment = "now about " + e.name
		} else {
			e.comment = ""
		}
		return true
	}},
	{"reorder foreign key columns", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, fk := range t.fks {
				if len(fk.cols) < 2 || touched[t.name+".fk:"+fk.name] || touched[fk.refTable] {
					continue
				}
				// the referenced side must still match some key on the parent, in the
				// same order (Error 6125, "Missing unique key for constraint",
				// measured): find the parent's key over exactly these columns, in this
				// order, and swap it the same way, so the constraint means the same
				// thing throughout, spelled with its column list in the other order
				// (ForeignKeyProps' "columns" -- the referencing side, which this
				// itself does not need a matching parent key for).
				parent := s.table(fk.refTable)
				if parent == nil {
					continue
				}
				var pk *pKey
				for _, k := range parent.keys {
					if len(k.cols) == 2 && k.cols[0] == fk.refCols[0] && k.cols[1] == fk.refCols[1] {
						pk = k
						break
					}
				}
				if pk == nil {
					continue
				}
				fk.cols[0], fk.cols[1] = fk.cols[1], fk.cols[0]
				fk.refCols[0], fk.refCols[1] = fk.refCols[1], fk.refCols[0]
				pk.cols[0], pk.cols[1] = pk.cols[1], pk.cols[0]
				touched[t.name+".fk:"+fk.name] = true
				touched[parent.name] = true
				return true
			}
		}
		return false
	}},
	{"partition table by range", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// two partitions covering every row (id 1..3): p0 < 2, p1 < 100 -- no MAXVALUE, so
		// a later "add partition" mutation can still extend the tail (migrate.go's ADD
		// PARTITION path refuses once the last partition is already unbounded).
		t.partition = &pPartitioning{kind: "RANGE", parts: []pPart{
			{name: s.next("p"), bound: 2},
			{name: s.next("p"), bound: 100},
		}}
		touched[t.name] = true
		return true
	}},
	{"partition table by hash", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = &pPartitioning{kind: "HASH", num: 2}
		touched[t.name] = true
		return true
	}},
	{"remove partitioning", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = nil
		touched[t.name] = true
		return true
	}},
	{"add partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && !t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		last := t.partition.parts[len(t.partition.parts)-1]
		if r.Intn(2) == 0 {
			// close the range off: nothing above it belongs to any lower partition
			t.partition.parts = append(t.partition.parts, pPart{name: s.next("p"), maxValue: true})
		} else {
			t.partition.parts = append(t.partition.parts, pPart{name: s.next("p"), bound: last.bound + 100})
		}
		touched[t.name] = true
		return true
	}},
	{"drop partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// only a partition an earlier "add partition" step in this same recipe appended
		// (parts[2:]): the generator's own first two partitions are what every row of a
		// freshly partitioned table depends on to have somewhere to go (id 1..3 always
		// fits in the second, VALUES LESS THAN (100)); dropping the table's own tail
		// beyond that never takes a row down with it.
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && len(t.partition.parts) >= 3
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		gone := t.partition.parts[len(t.partition.parts)-1]
		t.partition.parts = t.partition.parts[:len(t.partition.parts)-1]
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate drop partition %s.%s", t.orig, gone.name))
		touched[t.name] = true
		return true
	}},
	{"reorganize partition boundary", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && !t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		last := &t.partition.parts[len(t.partition.parts)-1]
		last.bound += 50 // still above every row's id, and above the partition before it
		touched[t.name] = true
		return true
	}},
	{"reorganize partition insert before maxvalue", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		parts := t.partition.parts
		prev := 0
		if len(parts) > 1 {
			prev = parts[len(parts)-2].bound
		}
		mid := pPart{name: s.next("p"), bound: prev + 50}
		t.partition.parts = append(parts[:len(parts)-1], mid, parts[len(parts)-1])
		touched[t.name] = true
		return true
	}},
	{"change hash partition count", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "HASH" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.partition.num > 1 && r.Intn(2) == 0 {
			t.partition.num--
		} else {
			t.partition.num++
		}
		touched[t.name] = true
		return true
	}},
	{"partition table by list", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			// the partitioning key's own domain must still be exactly 1..3 (every row):
			// an earlier "move primary key" in this same recipe can give this table's
			// current pk() a backfilled value (fill's own "9", measured -- Error 1526
			// "Table has no partition for value 9") that RANGE's own wide bounds absorb
			// but LIST's exact VALUES IN enumeration does not, so this generator only
			// partitions by an untouched column.
			return t.partition == nil && !touched[t.name+"."+t.pk().name]
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// every row (id 1..3) split across the two partitions, so a later "add list
		// partition" / "move list partition value" step never needs a declaration of its
		// own the way dropping one still does.
		t.partition = &pPartitioning{kind: "LIST", parts: []pPart{
			{name: s.next("p"), values: []int{1, 2}},
			{name: s.next("p"), values: []int{3}},
		}}
		touched[t.name] = true
		return true
	}},
	{"add list partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "LIST" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// s.seq is already a name counter unique across the whole schema; borrowing it as
		// the value too keeps this partition's own VALUES IN clear of every row's id (1..3)
		// and of any other "add list partition" step in the same recipe.
		name := s.next("p")
		t.partition.parts = append(t.partition.parts, pPart{name: name, values: []int{1000 + s.seq}})
		touched[t.name] = true
		return true
	}},
	{"drop list partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// only a partition an earlier "add list partition" step in this same recipe
		// appended (parts[2:]): the generator's own first two partitions are what every row
		// depends on to have somewhere to go, the same restriction "drop partition" (RANGE's
		// own) already carries.
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "LIST" && len(t.partition.parts) >= 3
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		gone := t.partition.parts[len(t.partition.parts)-1]
		t.partition.parts = t.partition.parts[:len(t.partition.parts)-1]
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate drop partition %s.%s", t.orig, gone.name))
		touched[t.name] = true
		return true
	}},
	{"move list partition value", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// moves one of the generator's own two base partitions' values to the other: both
		// keep their name, so migrate.go's alterListPartitioning reorganizes them together,
		// and no row's id ever goes missing (it only ever changes which named partition
		// holds it).
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "LIST" && len(t.partition.parts) >= 2 &&
				(len(t.partition.parts[0].values) > 0 || len(t.partition.parts[1].values) > 0)
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		src := 0
		if len(t.partition.parts[0].values) == 0 {
			src = 1
		}
		dst := 1 - src
		v := t.partition.parts[src].values[0]
		t.partition.parts[src].values = t.partition.parts[src].values[1:]
		t.partition.parts[dst].values = append(t.partition.parts[dst].values, v)
		touched[t.name] = true
		return true
	}},
	{"partition table by key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = &pPartitioning{kind: "KEY", num: 2}
		touched[t.name] = true
		return true
	}},
	{"partition table by linear hash", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = &pPartitioning{kind: "HASH", linear: true, num: 2}
		touched[t.name] = true
		return true
	}},
	{"partition table by range columns", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// the same two-partition RANGE shape "partition table by range" starts from
		// (COLUMNS spells a single-column VALUES LESS THAN identically to a plain RANGE,
		// measured -- see pPartitioning's own doc comment), so a later "add partition" /
		// "drop partition" / "reorganize ..." step still has a candidate to draw.
		t.partition = &pPartitioning{kind: "RANGE", columns: true, parts: []pPart{
			{name: s.next("p"), bound: 2},
			{name: s.next("p"), bound: 100},
		}}
		touched[t.name] = true
		return true
	}},
	{"partition table by range with subpartitions", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = &pPartitioning{kind: "RANGE", sub: &pSubPartitioning{num: 2}, parts: []pPart{
			{name: s.next("p"), bound: 2},
			{name: s.next("p"), bound: 100},
		}}
		touched[t.name] = true
		return true
	}},
	{"change key partition count", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "KEY" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.partition.num > 1 && r.Intn(2) == 0 {
			t.partition.num--
		} else {
			t.partition.num++
		}
		touched[t.name] = true
		return true
	}},
	{"toggle key algorithm", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "KEY" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition.algorithm = [3]int{1, 2, 0}[t.partition.algorithm]
		touched[t.name] = true
		return true
	}},
	{"toggle partitioning linear", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && (t.partition.kind == "HASH" || t.partition.kind == "KEY")
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition.linear = !t.partition.linear
		touched[t.name] = true
		return true
	}},
	{"change subpartition count", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.sub != nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.partition.sub.num > 1 && r.Intn(2) == 0 {
			t.partition.sub.num--
		} else {
			t.partition.sub.num++
		}
		touched[t.name] = true
		return true
	}},
	{"remove subpartitioning", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.sub != nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition.sub = nil
		touched[t.name] = true
		return true
	}},
}

// partitionCandidates: untouched tables a partitioning mutation may act on that also
// satisfy pred (already, or not yet, partitioned, in whatever shape the mutation needs).
// Every partitioning mutation guards to this set: no foreign key either side (Error 1506),
// no UNIQUE key besides PRIMARY (Error 1503 -- every UNIQUE key must carry the
// partitioning column, and this generator always partitions by id, which a plain UNIQUE
// elsewhere never does), and no spatial (point) column at all -- InnoDB refuses to
// partition a table carrying one anywhere, not only as the partitioning key itself
// (Error 1178, "The storage engine for the table doesn't support GEOMETRY", measured).
func partitionCandidates(s *pSchema, touched map[string]bool, pred func(*pTable) bool) []*pTable {
	var out []*pTable
	for _, t := range untouched(s, touched) {
		if !pred(t) || len(t.fks) > 0 || len(referencedByAny(s, t)) > 0 {
			continue
		}
		unique, fulltext := false, false
		for _, k := range t.keys {
			if k.unique {
				unique = true
			}
			// InnoDB refuses FULLTEXT on a partitioned table (Error 1214, "The used table
			// type doesn't support FULLTEXT indexes", measured); directed coverage is what
			// actually surfaced this one, applying "partition table by hash" against a table
			// a random draw had already given a FULLTEXT key.
			if k.special == "FULLTEXT" {
				fulltext = true
			}
		}
		if unique || fulltext {
			continue
		}
		spatial := false
		for _, c := range t.cols {
			if c.typ == "point" {
				spatial = true
				break
			}
		}
		if spatial {
			continue
		}
		out = append(out, t)
	}
	return out
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
