package obligation

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Check judges every obligation against every leaf of f, at every depth, and returns
// all applicable judgments in leaf order. Leaves inside a view's body are the view's
// business (its definition was judged when the schema loaded) and are not visited.
func Check(s *schema.Schema, decls []Obligation, f *facts.Facts, l Lowerer) []Discharge {
	if f == nil || f.Top == nil {
		return nil
	}
	c := &checker{s: s, f: f, lower: l, lowered: map[string][]facts.Pred{}, lowerErr: map[string]error{}}
	bySubject := map[string][]*Obligation{}
	for i := range decls {
		o := &decls[i]
		bySubject[o.Subject] = append(bySubject[o.Subject], o)
	}
	c.scope(f.Top, bySubject)
	return c.out
}

type checker struct {
	s        *schema.Schema
	f        *facts.Facts
	lower    Lowerer
	lowered  map[string][]facts.Pred
	lowerErr map[string]error
	out      []Discharge
}

func (c *checker) scope(sc *facts.Scope, bySubject map[string][]*Obligation) {
	if !sc.Returning {
		for i, leaf := range sc.Leaves {
			if leaf.Table == "" {
				continue
			}
			for _, o := range bySubject[leaf.Table] {
				if o.Kinds&c.kindAt(leaf) == 0 {
					continue
				}
				c.judge(sc, i, o)
			}
		}
	}
	for _, ch := range sc.Children {
		c.scope(ch, bySubject)
	}
}

// kindAt is the statement class a leaf sees: what the statement does to it. The target
// of an UPDATE / DELETE / MERGE is read as well as written (its WHERE scans the rows), so
// `on read` obligations apply there too; only an INSERT target is not read.
func (c *checker) kindAt(leaf facts.Leaf) Kinds {
	if leaf.Role != facts.Target {
		return OnSelect
	}
	switch c.f.Kind {
	case facts.Insert:
		return OnInsert
	case facts.Update:
		return OnSelect | OnUpdate
	case facts.Delete:
		return OnSelect | OnDelete
	case facts.Merge:
		return OnSelect | OnWrite
	}
	return OnSelect
}

func (c *checker) judge(sc *facts.Scope, i int, o *Obligation) {
	leaf := sc.Leaves[i]
	d := Discharge{Obligation: o, Leaf: leaf, Position: leaf.Position}
	rel := relByFullName(c.s, leaf.Table)
	if waived(leaf, o) {
		d.Path = Waived
		d.Message = fmt.Sprintf("%s: `%s` is waived by this statement", rel.Name, o.Source)
		c.out = append(c.out, d)
		return
	}
	switch {
	case o.Body.Predicate != "":
		d.Position = sc.At
		c.predicate(sc, i, o, rel, &d)
	case o.Body.Pinned != "":
		c.pinned(sc, i, o, rel, &d)
	case o.Body.Immutable != "":
		c.immutable(o, rel, &d)
	case o.Body.ViaView:
		if leaf.Kind != facts.Table {
			return
		}
		if leaf.Role == facts.Target && !o.Body.IncludeWrites {
			return
		}
		switch o.Source {
		case "-no-tables":
			d.Message = fmt.Sprintf("table %s is referenced directly; with -no-tables application code reads views and calls functions only", leaf.Table)
		case "-no-table-reads":
			d.Message = fmt.Sprintf("table %s is read directly; with -no-table-reads application code reads views (tables are written, not read)", leaf.Table)
		default:
			d.Message = fmt.Sprintf("table %s is referenced directly: it is declared `%s`, read it through a view", leaf.Table, o.Source)
		}
	default:
		return
	}
	c.out = append(c.out, d)
}

// predicate: every conjunct of the declared expression must be implied by the level's
// predicates about this leaf.
func (c *checker) predicate(sc *facts.Scope, i int, o *Obligation, rel *schema.Relation, d *Discharge) {
	leaf := sc.Leaves[i]
	want, err := c.lowerPredicate(o, rel)
	if err != nil {
		d.Message = fmt.Sprintf("%s: `%s` does not parse against %s: %v", leaf.Table, o.Body.Predicate, leaf.Table, err)
		return
	}
	path := ByStatement
	for _, q := range want {
		q = rebind(q, i)
		found := false
		for _, p := range sc.Preds {
			if !applies(p, i) || !implies(p, q) {
				continue
			}
			found = true
			if p.Origin == facts.FromPolicy && path == ByStatement {
				path = ByPolicy
			}
			if p.Origin == facts.FromStatement {
				break
			}
		}
		if !found && q.Op == facts.IsNotNull && containsRef(sc.NotNull, q.Col) {
			found = true
		}
		if !found {
			alias := leaf.Alias
			if alias != rel.Name && alias != "" {
				alias = rel.Name + " " + alias
			}
			if o.Kinds == OnRead {
				d.Message = fmt.Sprintf("rows of %s are visible where %s: add that predicate for %s, or opt out with `-- sqlshape: unfiltered %s`", rel.Name, o.Body.Predicate, alias, rel.Name)
			} else {
				d.Message = fmt.Sprintf("%s requires %s here: add that predicate for %s, or opt out with `-- sqlshape: unfiltered %s`", rel.Name, o.Body.Predicate, alias, rel.Name)
			}
			return
		}
	}
	d.Path = path
	if path == ByPolicy && !rel.ForceRowSecurity {
		d.Message = fmt.Sprintf("%s satisfies `%s` by row-security policy for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner", rel.Name, o.Body.Predicate)
	}
}

func (c *checker) lowerPredicate(o *Obligation, rel *schema.Relation) ([]facts.Pred, error) {
	key := o.Subject + "\x00" + o.Body.Predicate
	if p, ok := c.lowered[key]; ok {
		return p, c.lowerErr[key]
	}
	if c.lower == nil {
		return nil, fmt.Errorf("no lowerer")
	}
	p, err := c.lower.Lower(o.Body.Predicate, rel)
	c.lowered[key], c.lowerErr[key] = p, err
	return p, err
}

// pinned: the column is equal to a known value (or, on a write target, assigned).
func (c *checker) pinned(sc *facts.Scope, i int, o *Obligation, rel *schema.Relation, d *Discharge) {
	leaf := sc.Leaves[i]
	col := o.Body.Pinned
	ref := facts.ColRef{Leaf: i, Column: col}
	if leaf.Role == facts.Target && c.assigned(leaf.Table, col) {
		d.Path = ByStatement
		return
	}
	if c.f.Kind == facts.Insert && leaf.Role == facts.Target {
		d.Message = fmt.Sprintf("%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)", rel.Name, col, rel.Name, col)
		return
	}
	if containsRef(sc.Fixed, ref) {
		d.Path = ByStatement
		return
	}
	if c.viaForeignKey(sc, i, rel, col) {
		d.Path = ByForeignKey
		return
	}
	for _, p := range sc.Preds {
		if p.Origin == facts.FromPolicy && applies(p, i) && p.Op == facts.Eq && p.Col == ref && p.Term.Kind != facts.Column {
			d.Path = ByPolicy
			if !rel.ForceRowSecurity {
				d.Message = fmt.Sprintf("%s.%s is pinned by policy %s for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner", rel.Name, col, p.Name)
			}
			return
		}
	}
	d.Message = fmt.Sprintf("%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)", rel.Name, col, rel.Name, col)
}

// viaForeignKey: a composite foreign key from this table whose other columns are joined
// by equality to the referenced table's columns carries the referenced column's pin
// over (the referenced key is unique, so the rows agree on every column of it).
func (c *checker) viaForeignKey(sc *facts.Scope, i int, rel *schema.Relation, col string) bool {
	for _, con := range rel.Constraints {
		if con.Kind != schema.ForeignKey || len(con.Columns) < 2 {
			continue
		}
		at := -1
		for k, cc := range con.Columns {
			if cc == col {
				at = k
			}
		}
		if at < 0 {
			continue
		}
		refRel := relByFullName(c.s, con.RefTable)
		if refRel == nil {
			continue
		}
		for j, other := range sc.Leaves {
			if j == i || other.Table != refRel.FullName() {
				continue
			}
			joined := true
			for k := range con.Columns {
				if k == at {
					continue
				}
				if !equalIn(sc, i, facts.ColRef{Leaf: i, Column: con.Columns[k]}, facts.ColRef{Leaf: j, Column: con.RefColumns[k]}) {
					joined = false
					break
				}
			}
			if joined && containsRef(sc.Fixed, facts.ColRef{Leaf: j, Column: con.RefColumns[at]}) {
				return true
			}
		}
	}
	return false
}

// equalIn: a and b are equated by a predicate of the level that restricts leaf i.
func equalIn(sc *facts.Scope, i int, a, b facts.ColRef) bool {
	for _, p := range sc.Preds {
		if p.Op != facts.Eq || p.Term.Kind != facts.Column || !applies(p, i) {
			continue
		}
		if (p.Col == a && p.Term.Col == b) || (p.Col == b && p.Term.Col == a) {
			return true
		}
	}
	return false
}

// immutable: an UPDATE (or MERGE) may not assign the column.
func (c *checker) immutable(o *Obligation, rel *schema.Relation, d *Discharge) {
	if c.assigned(rel.FullName(), o.Body.Immutable) {
		d.Message = fmt.Sprintf("%s.%s is immutable: the statement must not assign it", rel.Name, o.Body.Immutable)
		return
	}
	d.Path = ByStatement
}

func (c *checker) assigned(table, col string) bool {
	for _, w := range c.f.Writes {
		if w.Table != table {
			continue
		}
		for _, a := range w.Assigned {
			if a == col {
				return true
			}
		}
	}
	return false
}

// waived: the statement (or the view definition the leaf sits in) opted out of o at this
// leaf: "*" waives everything on the table, "unfiltered" the predicate obligations, and
// a body spelled as declared waives that one.
func waived(leaf facts.Leaf, o *Obligation) bool {
	spec := o.Body.Spec()
	for _, w := range leaf.Waived {
		switch {
		case w == "*":
			return true
		case w == "unfiltered" && o.Body.Predicate != "":
			return true
		case strings.EqualFold(strings.Join(strings.Fields(w), " "), spec):
			return true
		}
	}
	return false
}

// applies: the predicate restricts leaf i (an outer join's ON restricts only one side).
func applies(p facts.Pred, i int) bool {
	if p.Restricts == nil {
		return true
	}
	for _, r := range p.Restricts {
		if r == i {
			return true
		}
	}
	return false
}

// rebind moves a lowered predicate (about leaf 0, the subject) onto leaf i.
func rebind(q facts.Pred, i int) facts.Pred {
	q.Col.Leaf = i
	if q.Term.Kind == facts.Column {
		q.Term.Col.Leaf = i
	}
	cols := make([]facts.ColRef, len(q.Cols))
	for k, r := range q.Cols {
		r.Leaf = i
		cols[k] = r
	}
	q.Cols = cols
	return q
}

// implies: predicate p of the statement establishes the declared conjunct q.
func implies(p, q facts.Pred) bool {
	if p.Op != q.Op {
		return false
	}
	switch q.Op {
	case facts.Eq:
		if p.Col == q.Col && sameTerm(p.Term, q.Term) {
			return true
		}
		// a column equality reads the same either way round
		return q.Term.Kind == facts.Column && p.Term.Kind == facts.Column && p.Col == q.Term.Col && p.Term.Col == q.Col
	case facts.IsNull, facts.IsNotNull:
		return p.Col == q.Col
	case facts.Opaque:
		if p.Text != q.Text || len(p.Cols) != len(q.Cols) {
			return false
		}
		for k := range p.Cols {
			if p.Cols[k] != q.Cols[k] {
				return false
			}
		}
		return true
	}
	return false
}

func sameTerm(a, b facts.Term) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case facts.Param:
		return a.Param == b.Param
	case facts.Const:
		return a.Const == b.Const
	case facts.Column:
		return a.Col == b.Col
	}
	return a.Text == b.Text
}

func containsRef(rs []facts.ColRef, r facts.ColRef) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// relByFullName resolves the schema-qualified-unless-public name the facts use.
func relByFullName(s *schema.Schema, name string) *schema.Relation {
	sch, n, ok := strings.Cut(name, ".")
	if !ok {
		return s.Relation("", name)
	}
	if rel := s.Relation(sch, n); rel != nil {
		return rel
	}
	return s.Relation("", name)
}
