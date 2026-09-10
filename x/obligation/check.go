package obligation

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Check judges every obligation against every leaf of f, at every depth, and returns
// all applicable judgments in leaf order. Leaves inside a view's body are the view's
// business (its definition was judged when the schema loaded) and are not visited.
func Check(s Schema, decls []Obligation, f *facts.Facts, l Lowerer) []Discharge {
	if f == nil || f.Top == nil {
		return nil
	}
	c := &checker{s: s, f: f, lower: l, lowered: map[string][]facts.Pred{}, lowerErr: map[string]error{}, aggregate: map[string]string{}, mayRead: map[string]bool{}}
	bySubject := map[string][]*Obligation{}
	var sensitive []*Obligation
	for i := range decls {
		o := &decls[i]
		switch {
		case o.Body.MayRead != "":
			c.mayRead[o.Body.MayRead] = true
			continue
		case o.Body.Sensitive != nil:
			sensitive = append(sensitive, o)
			continue
		}
		bySubject[o.Subject] = append(bySubject[o.Subject], o)
		if o.Body.Alone != "" {
			c.aggregate[o.Subject] = o.Body.Alone
		}
	}
	c.collectTables(f.Top)
	c.scope(f.Top, bySubject)
	c.writes(bySubject)
	c.sensitive(sensitive)
	return c.out
}

// writes judges the obligations on what the statement does to a table -- never, paired,
// single, transitions -- once per write target, a data-modifying WITH item's as much as
// the statement's own.
func (c *checker) writes(bySubject map[string][]*Obligation) {
	for _, w := range c.f.Writes {
		var kind Kinds
		switch w.Kind {
		case facts.Insert:
			kind = OnInsert
		case facts.Update:
			kind = OnUpdate
		case facts.Delete:
			kind = OnDelete
		default:
			continue
		}
		rel := c.s.Relation(w.Table)
		if rel == nil {
			continue
		}
		sc, li := c.targetLeaf(c.f.Top, w.Table)
		leaf := facts.Leaf{Table: w.Table, Alias: rel.Name(), Kind: facts.Table, Role: facts.Target, Position: w.Position}
		if sc != nil {
			leaf = sc.Leaves[li]
		}
		main := !w.InWith
		for _, o := range bySubject[w.Table] {
			if o.Kinds&kind == 0 {
				continue
			}
			d := Discharge{Obligation: o, Leaf: leaf, Position: w.Position}
			if waived(leaf, o) {
				d.Path = Waived
				d.Message = fmt.Sprintf("%s: `%s` is waived by this statement", rel.Name(), o.Source)
				c.out = append(c.out, d)
				continue
			}
			switch {
			case o.Body.Never:
				d.Message = fmt.Sprintf("%s is declared `%s`: no statement may do this to it", rel.Name(), o.Source)
			case o.Body.Paired != "":
				paired := c.s.Relation(o.Body.Paired)
				for _, other := range c.f.Writes {
					if paired != nil && other.Table == paired.FullName() {
						d.Path = ByStatement
					}
				}
				if d.Path == 0 {
					d.Message = fmt.Sprintf("a write to %s must also write %s in the same statement (a data-modifying WITH): %s", rel.Name(), o.Body.Paired, o.Source)
				}
			case o.Body.Single:
				switch {
				case !main:
					d.Message = fmt.Sprintf("%s requires a single-row %s, which cannot be proved for a write inside WITH", rel.Name(), strings.ToUpper(w.Kind.String()))
				case c.f.AtMostOne:
					d.Path = ByStatement
				default:
					d.Message = fmt.Sprintf("%s requires a single-row %s: fix a unique key by equality (the One proof)", rel.Name(), strings.ToUpper(w.Kind.String()))
				}
			case o.Body.Transitions != nil:
				c.transition(sc, li, &w, o, rel, &d)
			case o.Body.Pinned != "" && w.Kind == facts.Update && c.f.Kind != facts.Update && assigns(w, o.Body.Pinned):
				// an ON CONFLICT DO UPDATE / MERGE branch moving the pinned column: the rows
				// it moves must still be pinned by the statement (a plain UPDATE's leaf
				// judgment already asks for the WHERE)
				if sc != nil && containsRef(sc.Fixed, facts.ColRef{Leaf: li, Column: o.Body.Pinned}) {
					d.Path = ByStatement
				} else {
					d.Message = fmt.Sprintf("%s.%s is pinned: an UPDATE assigning it must also fix %s by equality in WHERE, or the row moves to another %s", rel.Name(), o.Body.Pinned, o.Body.Pinned, o.Body.Pinned)
				}
			default:
				continue
			}
			c.out = append(c.out, d)
		}
	}
}

// assigns: the write gives col a value.
func assigns(w facts.Write, col string) bool {
	for _, a := range w.Assigned {
		if a == col {
			return true
		}
	}
	return false
}

// targetLeaf finds the level and index of the target leaf writing table, at any depth.
func (c *checker) targetLeaf(sc *facts.Scope, table string) (*facts.Scope, int) {
	for i, l := range sc.Leaves {
		if l.Table == table && l.Role == facts.Target {
			return sc, i
		}
	}
	for _, p := range sc.Preds {
		if p.Op == facts.Exists && p.Sub != nil {
			if s, i := c.targetLeaf(p.Sub, table); s != nil {
				return s, i
			}
		}
	}
	for _, ch := range sc.Children {
		if s, i := c.targetLeaf(ch, table); s != nil {
			return s, i
		}
	}
	return nil, -1
}

type checker struct {
	s        Schema
	f        *facts.Facts
	lower    Lowerer
	lowered  map[string][]facts.Pred
	lowerErr map[string]error
	out      []Discharge
	// aggregate maps a table to the root of its aggregate (from the alone obligations);
	// tables are every table the statement touches at any depth, in order
	aggregate map[string]string
	tables    []string
	// mayRead are the labels the selected context may read
	mayRead map[string]bool
}

// sensitive judges the labelled columns against every column the statement references,
// through views (a view column that passes a base column through carries its label; an
// expression column does not). A reference is a read wherever it stands: a labelled
// column in a WHERE leaks as much as one in the SELECT list.
func (c *checker) sensitive(decls []*Obligation) {
	if len(decls) == 0 {
		return
	}
	labels := map[string][]*Obligation{} // "table\x00column" -> declarations
	for _, o := range decls {
		for _, col := range o.Body.Sensitive.Columns {
			labels[o.Subject+"\x00"+col] = append(labels[o.Subject+"\x00"+col], o)
		}
	}
	for _, u := range c.f.Uses {
		if u.Assigned {
			continue // storing a labelled value is not reading it
		}
		table, col := u.Table, u.Column
		for depth := 0; depth < 16; depth++ { // follow a view column to the base column it passes through
			rel := c.s.Relation(table)
			if rel == nil || (rel.Kind() != facts.View && rel.Kind() != facts.MatView) {
				break
			}
			srcTable, srcCol, ok := rel.ViewSource(col)
			if !ok {
				table = "" // an expression column: no base column, no label
				break
			}
			table, col = srcTable, srcCol
		}
		if table == "" {
			continue
		}
		for _, o := range labels[table+"\x00"+col] {
			d := Discharge{Obligation: o, Leaf: facts.Leaf{Table: u.Table, Kind: facts.Table}, Position: u.Position}
			if c.mayRead[o.Body.Sensitive.Label] {
				d.Path = ByStatement
			} else {
				via := ""
				if u.Table != table {
					via = " (through " + u.Table + "." + u.Column + ")"
				}
				d.Message = fmt.Sprintf("%s.%s is %s%s: this context may not read it (a context with `may read %s`, or a view that masks it)", table, col, o.Body.Sensitive.Label, via, o.Body.Sensitive.Label)
			}
			c.out = append(c.out, d)
		}
	}
}

func (c *checker) collectTables(sc *facts.Scope) {
	for _, l := range sc.Leaves {
		if l.Table != "" && l.Kind == facts.Table && !contains(c.tables, l.Table) {
			c.tables = append(c.tables, l.Table)
		}
	}
	for _, p := range sc.Preds {
		if p.Op == facts.Exists && p.Sub != nil {
			c.collectTables(p.Sub)
		}
	}
	for _, ch := range sc.Children {
		c.collectTables(ch)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
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
	for _, p := range sc.Preds {
		if p.Op == facts.Exists && p.Sub != nil {
			c.scope(p.Sub, bySubject)
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
	rel := c.s.Relation(leaf.Table)
	if waived(leaf, o) {
		d.Path = Waived
		d.Message = fmt.Sprintf("%s: `%s` is waived by this statement", rel.Name(), o.Source)
		c.out = append(c.out, d)
		return
	}
	switch {
	case o.Body.Predicate != "":
		if sc.At >= 0 {
			d.Position = sc.At // the WHERE the predicate is missing from
		}
		c.predicate(sc, i, o, rel, &d)
	case o.Body.Pinned != "":
		c.pinned(sc, i, o, rel, &d)
	case o.Body.Immutable != "":
		c.immutable(o, rel, &d)
	case o.Body.Never, o.Body.Paired != "", o.Body.Single, o.Body.Transitions != nil:
		return // judged per write (writes), a WITH item's included
	case o.Body.Alone != "":
		if leaf.Kind != facts.Table {
			return
		}
		d.Path = ByStatement
		for _, t := range c.tables {
			if other := c.aggregate[t]; other != "" && other != o.Body.Alone {
				d.Path = 0
				d.Message = fmt.Sprintf("%s belongs to aggregate %s and this statement also touches %s of aggregate %s: one statement, one aggregate (read across aggregates through a view)", leaf.Table, o.Body.Alone, t, other)
				break
			}
		}
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
func (c *checker) predicate(sc *facts.Scope, i int, o *Obligation, rel Relation, d *Discharge) {
	leaf := sc.Leaves[i]
	want, err := c.lowerPredicate(o, rel)
	if err != nil {
		d.Message = fmt.Sprintf("%s: `%s` does not parse against %s: %v", leaf.Table, o.Body.Predicate, leaf.Table, err)
		return
	}
	path := ByStatement
	for _, q := range want {
		q = rebind(q, i)
		found, byPolicy := satisfied(sc, i, q)
		if byPolicy && path == ByStatement {
			path = ByPolicy
		}
		if !found {
			alias := leaf.Alias
			if alias != rel.Name() && alias != "" {
				alias = rel.Name() + " " + alias
			}
			if o.Kinds == OnRead {
				d.Message = fmt.Sprintf("rows of %s are visible where %s: add that predicate for %s, or opt out with `-- sqlshape: unfiltered %s`", rel.Name(), o.Body.Predicate, alias, rel.Name())
			} else {
				d.Message = fmt.Sprintf("%s requires %s here: add that predicate for %s, or opt out with `-- sqlshape: unfiltered %s`", rel.Name(), o.Body.Predicate, alias, rel.Name())
			}
			return
		}
	}
	d.Path = path
	if path == ByPolicy && !rel.ForceRowSecurity() {
		d.Message = fmt.Sprintf("%s satisfies `%s` by row-security policy for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner", rel.Name(), o.Body.Predicate)
	}
}

func (c *checker) lowerPredicate(o *Obligation, rel Relation) ([]facts.Pred, error) {
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
func (c *checker) pinned(sc *facts.Scope, i int, o *Obligation, rel Relation, d *Discharge) {
	leaf := sc.Leaves[i]
	col := o.Body.Pinned
	ref := facts.ColRef{Leaf: i, Column: col}
	if c.f.Kind == facts.Insert && leaf.Role == facts.Target {
		// an INSERT pins by giving the column a value (its ON CONFLICT DO UPDATE is judged
		// as a write of its own)
		if c.assignedBy(leaf.Table, col, facts.Insert) {
			d.Path = ByStatement
			return
		}
		d.Message = fmt.Sprintf("%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)", rel.Name(), col, rel.Name(), col)
		return
	}
	if c.f.Kind == facts.Merge && leaf.Role == facts.Target && c.mergeInsertOnly(leaf.Table) && c.assignedBy(leaf.Table, col, facts.Insert) {
		d.Path = ByStatement // an INSERT-only MERGE pins by assigning, like an INSERT
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
	if strings.HasPrefix(o.Source, "aggregate ") && c.viaParent(sc, i, rel, col) {
		d.Path = ByForeignKey
		return
	}
	for _, p := range sc.Preds {
		if p.Origin == facts.FromPolicy && applies(p, i) && p.Op == facts.Eq && p.Col == ref && p.Term.Kind != facts.Column {
			d.Path = ByPolicy
			if !rel.ForceRowSecurity() {
				d.Message = fmt.Sprintf("%s.%s is pinned by policy %s for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner", rel.Name(), col, p.Name)
			}
			return
		}
	}
	d.Message = fmt.Sprintf("%s.%s is not pinned: every statement on %s must fix %s by equality (or assign it)%s", rel.Name(), col, rel.Name(), col, occurrence(sc, i))
	if root, ok := strings.CutPrefix(o.Source, "aggregate "); ok {
		d.Message = fmt.Sprintf("%s is a child of aggregate %s: reach it through %s (fix %s.%s by equality, or join on %s's key)", rel.Name(), root, root, rel.Name(), col, root)
	}
}

// occurrence names the leaf by its alias when the same table appears more than once at
// the level (a self join), so the failing occurrence can be told from the others.
func occurrence(sc *facts.Scope, i int) string {
	leaf := sc.Leaves[i]
	n := 0
	for _, l := range sc.Leaves {
		if l.Table == leaf.Table {
			n++
		}
	}
	if n < 2 || leaf.Alias == "" {
		return ""
	}
	return " (the occurrence " + leaf.Alias + ")"
}

// viaForeignKey: a composite foreign key from this table whose other columns are joined
// by equality to the referenced table's columns carries the referenced column's pin
// over (the referenced key is unique, so the rows agree on every column of it).
func (c *checker) viaForeignKey(sc *facts.Scope, i int, rel Relation, col string) bool {
	for _, con := range rel.ForeignKeys() {
		if len(con.Columns) < 2 {
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
		refRel := c.s.Relation(con.RefTable)
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

// viaParent: an aggregate member's foreign-key column is joined by equality to the
// referenced column of a leaf of its parent table. The parent leaf owes the aggregate's
// obligations itself, so the chain up to the root is judged link by link.
func (c *checker) viaParent(sc *facts.Scope, i int, rel Relation, col string) bool {
	for _, con := range rel.ForeignKeys() {
		at := -1
		for k, cc := range con.Columns {
			if cc == col {
				at = k
			}
		}
		if at < 0 {
			continue
		}
		parent := c.s.Relation(con.RefTable)
		if parent == nil {
			continue
		}
		for j, other := range sc.Leaves {
			if j != i && other.Table == parent.FullName() && equalIn(sc, i, facts.ColRef{Leaf: i, Column: col}, facts.ColRef{Leaf: j, Column: con.RefColumns[at]}) {
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

// transition: an UPDATE setting the state column to a constant must fix the column, in
// its WHERE, to one of that state's predecessors (compare-and-set); a non-constant target
// is refused, since no predecessor set can be checked for it. Not assigning the column
// is not a transition.
func (c *checker) transition(sc *facts.Scope, i int, w *facts.Write, o *Obligation, rel Relation, d *Discharge) {
	tr := o.Body.Transitions
	var value *facts.Term
	for k, col := range w.Assigned {
		if col == tr.Column && k < len(w.Values) {
			v := w.Values[k]
			value = &v
		}
	}
	if value == nil {
		d.Path = ByStatement
		return
	}
	if value.Kind != facts.Const {
		d.Message = fmt.Sprintf("%s.%s is a state machine: SET it to a declared state (a literal), not to %s", rel.Name(), tr.Column, value)
		return
	}
	to := stateOf(value.Const)
	froms, ok := tr.From[to]
	if !ok {
		d.Message = fmt.Sprintf("%s.%s is a state machine: %q is not a state anything transitions to (declared: %s)", rel.Name(), tr.Column, to, strings.Join(tr.Order, ", "))
		return
	}
	if sc == nil {
		d.Message = fmt.Sprintf("%s.%s: the current state of an UPDATE inside WITH cannot be checked", rel.Name(), tr.Column)
		return
	}
	col := facts.ColRef{Leaf: i, Column: tr.Column}
	for _, p := range sc.Preds {
		if p.Col != col || !applies(p, i) {
			continue
		}
		var current []string
		switch {
		case p.Op == facts.Eq && p.Term.Kind == facts.Const:
			current = []string{stateOf(p.Term.Const)}
		case p.Op == facts.In:
			for _, t := range p.Terms {
				if t.Kind != facts.Const {
					current = nil
					break
				}
				current = append(current, stateOf(t.Const))
			}
		}
		if current == nil {
			continue
		}
		// every state the WHERE allows must be a declared predecessor
		for _, from := range current {
			declared := false
			for _, f := range froms {
				declared = declared || f == from
			}
			if !declared {
				d.Message = fmt.Sprintf("%s.%s: %s -> %s is not a declared transition (%s comes from %s)", rel.Name(), tr.Column, from, to, to, strings.Join(froms, " | "))
				return
			}
		}
		d.Path = ByStatement
		return
	}
	d.Message = fmt.Sprintf("%s.%s: SET %s = '%s' must fix the current state in WHERE (%s = '%s')", rel.Name(), tr.Column, tr.Column, to, tr.Column, strings.Join(froms, "' or '"))
}

// stateOf reads a state name out of a constant's text: the analyzer spells a constant as
// one type letter and the value ("spaid", "i2"), and a declaration writes the bare value.
func stateOf(c string) string {
	if len(c) > 1 && strings.ContainsRune("ifsbx", rune(c[0])) {
		return c[1:]
	}
	return c
}

// immutable: an UPDATE (or MERGE) may not assign the column.
func (c *checker) immutable(o *Obligation, rel Relation, d *Discharge) {
	if c.assigned(rel.FullName(), o.Body.Immutable) {
		d.Message = fmt.Sprintf("%s.%s is immutable: the statement must not assign it", rel.Name(), o.Body.Immutable)
		return
	}
	d.Path = ByStatement
}

func (c *checker) assigned(table, col string) bool {
	for _, w := range c.f.Writes {
		if w.Table == table && assigns(w, col) {
			return true
		}
	}
	return false
}

// assignedBy: a write of the given kind to table gives col a value.
func (c *checker) assignedBy(table, col string, kind facts.StmtKind) bool {
	for _, w := range c.f.Writes {
		if w.Table == table && w.Kind == kind && assigns(w, col) {
			return true
		}
	}
	return false
}

// mergeInsertOnly: every write to table is an INSERT (a MERGE with WHEN NOT MATCHED only).
func (c *checker) mergeInsertOnly(table string) bool {
	for _, w := range c.f.Writes {
		if w.Table == table && w.Kind != facts.Insert {
			return false
		}
	}
	return true
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

// satisfied: the level's facts establish the declared conjunct q (already rebound onto
// leaf i). byPolicy says the only establishing predicate came from a row-security policy.
func satisfied(sc *facts.Scope, i int, q facts.Pred) (found, byPolicy bool) {
	if q.Op == facts.Exists {
		return witness(sc, i, q), false
	}
	for _, p := range sc.Preds {
		if !applies(p, i) || !implies(p, q) {
			continue
		}
		if p.Origin == facts.FromStatement {
			return true, false
		}
		found, byPolicy = true, p.Origin == facts.FromPolicy
	}
	if found {
		return true, byPolicy
	}
	switch {
	case q.Op == facts.IsNotNull && containsRef(sc.NotNull, q.Col):
		return true, false
	case q.Op == facts.Eq && q.Term.Kind == facts.Param && containsRef(sc.Fixed, q.Col):
		// pinned through the equality closure (a = b AND b = $1)
		return true, false
	}
	return false, false
}

// witness: the rows of leaf i have a witness row for the declared EXISTS q -- either a
// leaf of the same level joined so that q's body holds (an inner join; an outer join's
// ON does not restrict i), or an EXISTS / IN subquery of the level whose body holds.
// q.Sub's leaves are matched to candidate leaves by table, in every injective way.
func witness(sc *facts.Scope, i int, q facts.Pred) bool {
	// (a) the same level: q's Outer terms name leaf i's columns as plain columns
	if assign(q.Sub.Leaves, sc.Leaves, func(j int) bool { return j != i }, func(m []int) bool {
		for _, qp := range q.Sub.Preds {
			r := rebindInner(qp, m, func(o facts.ColRef) facts.Term {
				return facts.Term{Kind: facts.Column, Col: facts.ColRef{Leaf: i, Column: o.Column}}
			})
			if ok, _ := satisfied(sc, i, r); !ok {
				return false
			}
		}
		return true
	}) {
		return true
	}
	// (b) a subquery of the level: Outer terms stay Outer, pointing at leaf i
	for _, p := range sc.Preds {
		if p.Op != facts.Exists || p.Sub == nil || !applies(p, i) {
			continue
		}
		if assign(q.Sub.Leaves, p.Sub.Leaves, func(int) bool { return true }, func(m []int) bool {
			for _, qp := range q.Sub.Preds {
				r := rebindInner(qp, m, func(o facts.ColRef) facts.Term {
					return facts.Term{Kind: facts.Outer, Col: facts.ColRef{Leaf: i, Column: o.Column}}
				})
				at := r.Col.Leaf
				if ok, _ := satisfied(p.Sub, at, r); !ok {
					return false
				}
			}
			return true
		}) {
			return true
		}
	}
	return false
}

// assign tries every injective mapping of want's leaves onto have's leaves of the same
// table that pass allow, and reports whether ok accepts one. m[k] is the index in have
// of want's k-th leaf.
func assign(want, have []facts.Leaf, allow func(int) bool, ok func(m []int) bool) bool {
	m := make([]int, len(want))
	used := map[int]bool{}
	var rec func(k int) bool
	rec = func(k int) bool {
		if k == len(want) {
			return ok(m)
		}
		for j, h := range have {
			if used[j] || !allow(j) || h.Table != want[k].Table || h.Table == "" {
				continue
			}
			used[j], m[k] = true, j
			if rec(k + 1) {
				return true
			}
			delete(used, j)
		}
		return false
	}
	return rec(0)
}

// rebindInner moves a declared body predicate onto the candidate leaves (m) and turns its
// Outer terms (about the subject, leaf 0 of the declaration) into what outer gives.
func rebindInner(q facts.Pred, m []int, outer func(facts.ColRef) facts.Term) facts.Pred {
	place := func(r facts.ColRef) facts.ColRef {
		if r.Leaf < len(m) {
			r.Leaf = m[r.Leaf]
		}
		return r
	}
	q.Col = place(q.Col)
	switch q.Term.Kind {
	case facts.Column:
		q.Term.Col = place(q.Term.Col)
	case facts.Outer:
		q.Term = outer(q.Term.Col)
	}
	cols := make([]facts.ColRef, len(q.Cols))
	for k, r := range q.Cols {
		cols[k] = place(r)
	}
	q.Cols = cols
	return q
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
	if q.Op == facts.In && p.Col == q.Col {
		// the column is one of the declared values: fixed to one of them, or held to a
		// subset of them
		switch p.Op {
		case facts.Eq:
			return p.Term.Kind != facts.Column && anyTerm(q.Terms, p.Term)
		case facts.In:
			for _, t := range p.Terms {
				if !anyTerm(q.Terms, t) {
					return false
				}
			}
			return true
		}
		return false
	}
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

// anyTerm: p establishes one of the declared terms.
func anyTerm(qs []facts.Term, p facts.Term) bool {
	for _, q := range qs {
		if sameTerm(p, q) {
			return true
		}
	}
	return false
}

// sameTerm: the statement's term p establishes the declared term q. A `$n` in a
// declaration stands for any value known before the row is examined (a parameter, a
// literal, an outer reference, a stable expression), not for that parameter number.
func sameTerm(p, q facts.Term) bool {
	if q.Kind == facts.Param {
		return p.Kind != facts.Column
	}
	if p.Kind != q.Kind {
		return false
	}
	switch p.Kind {
	case facts.Const:
		return p.Const == q.Const
	case facts.Column, facts.Outer:
		return p.Col == q.Col
	}
	return p.Text == q.Text
}

func containsRef(rs []facts.ColRef, r facts.ColRef) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}
