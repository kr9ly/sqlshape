package obligation

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/schema"
)

// Parse reads one schema.sql directive (whitespace-normalized, without the `-- sqlshape:`
// prefix) as an obligation on subject. Two grammars are accepted:
//
//	require <body> [on <kinds>]
//	visible where <expr>            (= require <expr> on read; the original spelling)
//
// where <body> is `pinned(col)`, `immutable(col)`, `via view`, or an SQL boolean
// expression, and <kinds> is a comma-separated list of select / insert / update / delete
// / read / write / all. ok is false for a directive that is not an obligation at all
// (another package's), as opposed to a malformed one, which is an error.
func Parse(subject, directive string) (o Obligation, ok bool, err error) {
	norm := strings.Join(strings.Fields(directive), " ")
	lower := strings.ToLower(norm)
	o = Obligation{Subject: subject, Source: norm}
	switch {
	case strings.HasPrefix(lower, "visible where "):
		o.Body.Predicate = strings.TrimSpace(norm[len("visible where "):])
		o.Kinds = OnRead
		return o, true, nil
	case strings.HasPrefix(lower, "require "):
	default:
		return Obligation{}, false, nil
	}
	body := strings.TrimSpace(norm[len("require "):])
	if strings.HasSuffix(strings.ToLower(body), " on") {
		return Obligation{}, true, fmt.Errorf("%s: `on` needs statement kinds (select, insert, update, delete, read, write, all)", norm)
	}
	// the `on <kinds>` suffix: the last ` on ` outside parentheses
	if i := lastTopLevel(body, " on "); i >= 0 {
		kinds, err := parseKinds(body[i+len(" on "):])
		if err != nil {
			return Obligation{}, true, fmt.Errorf("%s: %v", norm, err)
		}
		o.Kinds = kinds
		body = strings.TrimSpace(body[:i])
	}
	lb := strings.ToLower(body)
	switch {
	case body == "":
		return Obligation{}, true, fmt.Errorf("%s: nothing required", norm)
	case strings.HasPrefix(lb, "pinned(") && strings.HasSuffix(lb, ")"):
		o.Body.Pinned = strings.TrimSpace(body[len("pinned(") : len(body)-1])
		if o.Body.Pinned == "" {
			return Obligation{}, true, fmt.Errorf("%s: pinned needs a column", norm)
		}
		if o.Kinds == 0 {
			o.Kinds = OnAll
		}
	case strings.HasPrefix(lb, "immutable(") && strings.HasSuffix(lb, ")"):
		o.Body.Immutable = strings.TrimSpace(body[len("immutable(") : len(body)-1])
		if o.Body.Immutable == "" {
			return Obligation{}, true, fmt.Errorf("%s: immutable needs a column", norm)
		}
		if o.Kinds == 0 {
			o.Kinds = OnUpdate
		}
	case lb == "via view":
		o.Body.ViaView = true
		if o.Kinds == 0 {
			o.Kinds = OnRead
		}
		o.Body.IncludeWrites = o.Kinds&OnWrite != 0
	case lb == "never":
		o.Body.Never = true
		if o.Kinds == 0 {
			o.Kinds = OnWrite
		}
	case strings.HasPrefix(lb, "paired(") && strings.HasSuffix(lb, ")"):
		o.Body.Paired = strings.TrimSpace(body[len("paired(") : len(body)-1])
		if o.Body.Paired == "" {
			return Obligation{}, true, fmt.Errorf("%s: paired needs a table", norm)
		}
		if o.Kinds == 0 {
			o.Kinds = OnInsert
		}
	case lb == "single":
		o.Body.Single = true
		if o.Kinds == 0 {
			o.Kinds = OnDelete
		}
	default:
		o.Body.Predicate = body
		if o.Kinds == 0 {
			o.Kinds = OnRead
		}
	}
	return o, true, nil
}

// lastTopLevel finds the last occurrence of sep outside parentheses and quotes.
func lastTopLevel(s, sep string) int {
	depth, quote := 0, byte(0)
	last := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(strings.ToLower(s[i:]), sep):
			last = i
		}
	}
	return last
}

func parseKinds(list string) (Kinds, error) {
	var k Kinds
	for _, w := range strings.Split(list, ",") {
		switch strings.ToLower(strings.TrimSpace(w)) {
		case "select":
			k |= OnSelect
		case "insert":
			k |= OnInsert
		case "update":
			k |= OnUpdate
		case "delete":
			k |= OnDelete
		case "read":
			k |= OnRead
		case "write":
			k |= OnWrite
		case "all":
			k |= OnAll
		case "":
			return 0, fmt.Errorf("empty statement kind in %q", list)
		default:
			return 0, fmt.Errorf("unknown statement kind %q (select, insert, update, delete, read, write, all)", strings.TrimSpace(w))
		}
	}
	return k, nil
}

// Declarations collects the obligations written in schema.sql: every relation's
// `require ...` and `visible where ...` directives, and the expansion of each
// `aggregate` declaration. Malformed ones come back as Problems.
func Declarations(s *schema.Schema) ([]Obligation, []Problem) {
	var out []Obligation
	var problems []Problem
	for _, rel := range s.Relations {
		for _, d := range rel.Directives {
			lower := strings.ToLower(d)
			if strings.HasPrefix(lower, "sensitive ") || strings.HasPrefix(lower, "transitions ") {
				o, err := columnRule(rel, d)
				if err != nil {
					problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: err.Error()})
					continue
				}
				out = append(out, o)
				continue
			}
			if strings.HasPrefix(strings.ToLower(d), "context ") {
				ob, err := context(rel, d)
				if err != nil {
					problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: err.Error()})
					continue
				}
				out = append(out, ob...)
				continue
			}
			if strings.HasPrefix(strings.ToLower(d), "aggregate ") {
				ob, err := aggregate(s, rel, d)
				if err != nil {
					problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: err.Error()})
					continue
				}
				out = append(out, ob...)
				continue
			}
			o, ok, err := Parse(rel.FullName(), d)
			if !ok {
				continue
			}
			if err != nil {
				problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: err.Error()})
				continue
			}
			if col := o.Body.Pinned + o.Body.Immutable; col != "" && !hasColumn(rel, col) {
				problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: fmt.Sprintf("%s has no column %s", rel.Name, col)})
				continue
			}
			if t := o.Body.Paired; t != "" && relByFullName(s, t) == nil {
				problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: fmt.Sprintf("paired: relation %s does not exist", t)})
				continue
			}
			out = append(out, o)
		}
	}
	return out, problems
}

// hasColumn: the relation has the column (a view's are its frozen output columns).
func hasColumn(rel *schema.Relation, col string) bool {
	if rel.Column(col) != nil {
		return true
	}
	for _, vc := range rel.Frozen {
		if vc.Name == col {
			return true
		}
	}
	return false
}

// FromFlags expands vet's table-wide flags into per-table obligations, the sugar they
// always were: -require-columns=a,b is `require pinned(a)` on every table that has the
// column, -no-tables is `require via view on all`, -no-table-reads is `require via view`.
func FromFlags(s *schema.Schema, requireColumns string, noTables, noTableReads bool) []Obligation {
	var out []Obligation
	var cols []string
	for _, c := range strings.Split(requireColumns, ",") {
		if c = strings.TrimSpace(c); c != "" {
			cols = append(cols, c)
		}
	}
	for _, rel := range s.Relations {
		if rel.Kind != schema.Table {
			continue
		}
		switch {
		case noTables:
			out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnAll, Body: Body{ViaView: true, IncludeWrites: true}, Source: "-no-tables"})
		case noTableReads:
			out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnRead, Body: Body{ViaView: true}, Source: "-no-table-reads"})
		}
		for _, c := range cols {
			if rel.Column(c) != nil {
				out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnAll, Body: Body{Pinned: c}, Source: "-require-columns=" + requireColumns})
			}
		}
	}
	return out
}

// aggregate expands `-- sqlshape: aggregate <root> (<child>, <child>...)`, written above
// the root's CREATE TABLE, into the obligations DDD's aggregate rules amount to in the
// shape of a statement:
//
//   - a child is reached through its parent in the aggregate: `require pinned(<fk
//     column>) on all` on each child, the foreign key being the one to the root or, for
//     a grandchild, to the member it hangs off (a join on the parent's key pins it);
//   - one statement touches one aggregate: `alone` on the root and every child (read
//     across aggregates through a view).
func aggregate(s *schema.Schema, root *schema.Relation, directive string) ([]Obligation, error) {
	norm := strings.Join(strings.Fields(directive), " ")
	rest := strings.TrimSpace(norm[len("aggregate "):])
	name, list, ok := strings.Cut(rest, "(")
	if !ok {
		return nil, fmt.Errorf("%s: expected `aggregate <root> (<child>, ...) [lock <column>]`", norm)
	}
	list, tail, _ := strings.Cut(list, ")")
	lock := ""
	if tail = strings.TrimSpace(tail); tail != "" {
		col, ok := strings.CutPrefix(strings.ToLower(tail), "lock ")
		if !ok || strings.ContainsAny(strings.TrimSpace(col), " \t") {
			return nil, fmt.Errorf("%s: expected `lock <column>` after the children", norm)
		}
		lock = strings.TrimSpace(tail[len("lock "):])
		if root.Column(lock) == nil {
			return nil, fmt.Errorf("%s: %s has no column %s", norm, root.FullName(), lock)
		}
	}
	name = strings.TrimSpace(name)
	if name != root.Name && name != root.FullName() {
		return nil, fmt.Errorf("%s: the declaration sits above %s, not %s", norm, root.FullName(), name)
	}
	src := "aggregate " + root.FullName()
	out := []Obligation{{Subject: root.FullName(), Kinds: OnAll, Body: Body{Alone: root.FullName()}, Source: src}}
	if lock != "" {
		// the version is named in every write to the root ...
		out = append(out, Obligation{Subject: root.FullName(), Kinds: OnUpdate | OnDelete, Body: Body{Pinned: lock}, Source: src})
	}
	var children []*schema.Relation
	for _, c := range strings.Split(strings.TrimSuffix(list, ")"), ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		sch, n, _ := strings.Cut(c, ".")
		if n == "" {
			sch, n = "", sch
		}
		child := s.Relation(sch, n)
		if child == nil {
			return nil, fmt.Errorf("%s: child %s does not exist", norm, c)
		}
		children = append(children, child)
	}
	// a child hangs off the root or off another member (a grandchild off its parent): the
	// foreign key it must pin is the one into the aggregate, the root's first
	isRoot := func(t string) bool { return t == root.FullName() || t == root.Name }
	members := map[string]bool{}
	for _, ch := range children {
		members[ch.FullName()], members[ch.Name] = true, true
	}
	for _, child := range children {
		var fk *schema.Constraint
		for _, con := range child.Constraints {
			if con.Kind == schema.ForeignKey && isRoot(con.RefTable) {
				fk = con
				break
			}
		}
		if fk == nil {
			for _, con := range child.Constraints {
				if con.Kind == schema.ForeignKey && members[con.RefTable] && con.RefTable != child.FullName() && con.RefTable != child.Name {
					fk = con
					break
				}
			}
		}
		if fk == nil {
			return nil, fmt.Errorf("%s: %s has no foreign key to %s or to another member of the aggregate", norm, child.FullName(), root.FullName())
		}
		for _, col := range fk.Columns {
			out = append(out, Obligation{Subject: child.FullName(), Kinds: OnAll, Body: Body{Pinned: col}, Source: src})
		}
		out = append(out, Obligation{Subject: child.FullName(), Kinds: OnAll, Body: Body{Alone: root.FullName()}, Source: src})
		if lock != "" {
			// ... and in every write to a child: the row of the root this child hangs off
			// (through its parent, for a grandchild) must be the version the writer saw
			out = append(out, Obligation{Subject: child.FullName(), Kinds: OnUpdate | OnDelete, Body: Body{Predicate: lockPredicate(s, root, child, lock)}, Source: src})
		}
	}
	return out, nil
}

// lockPredicate spells the EXISTS a child of a locked aggregate owes on UPDATE / DELETE:
// a root row at the version the writer saw, reached through the child's foreign key and,
// for a grandchild, its parent's.
func lockPredicate(s *schema.Schema, root, child *schema.Relation, lock string) string {
	var from []string
	var where []string
	cur := child
	prev := ""
	for depth := 0; depth < 8; depth++ {
		var fk *schema.Constraint
		for _, con := range cur.Constraints {
			if con.Kind == schema.ForeignKey && (con.RefTable == root.FullName() || con.RefTable == root.Name) {
				fk = con
				break
			}
		}
		if fk == nil {
			for _, con := range cur.Constraints {
				if con.Kind == schema.ForeignKey && con.RefTable != cur.FullName() && con.RefTable != cur.Name {
					if r := relByFullName(s, con.RefTable); r != nil && r != cur {
						fk = con
						break
					}
				}
			}
		}
		if fk == nil {
			break
		}
		parent := relByFullName(s, fk.RefTable)
		alias := fmt.Sprintf("a%d", depth)
		from = append(from, parent.FullName()+" "+alias)
		for k := range fk.Columns {
			lhs := alias + "." + fk.RefColumns[k]
			rhs := fk.Columns[k]
			if prev != "" {
				rhs = prev + "." + fk.Columns[k]
			}
			where = append(where, lhs+" = "+rhs)
		}
		if parent == root {
			where = append(where, alias+"."+lock+" = $1")
			return "EXISTS (SELECT 1 FROM " + strings.Join(from, ", ") + " WHERE " + strings.Join(where, " AND ") + ")"
		}
		prev, cur = alias, parent
	}
	return "false"
}

// context reads `-- sqlshape: context <name>: <item>; <item>...` above a CREATE TABLE /
// VIEW: the obligations that hold on this relation only in the named context (`require
// ...`) and the base obligations the context lifts (`waive <body>`). An entry point
// selects one context (vet: the package's `// sqlshape: context <name>` or -context;
// check: -context); InContext applies the selection.
func context(rel *schema.Relation, directive string) ([]Obligation, error) {
	norm := strings.Join(strings.Fields(directive), " ")
	rest := strings.TrimSpace(norm[len("context "):])
	name, items, ok := strings.Cut(rest, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.ContainsAny(name, " \t") {
		return nil, fmt.Errorf("%s: expected `context <name>: require ...; waive ...`", norm)
	}
	var out []Obligation
	for _, it := range strings.Split(items, ";") {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		lower := strings.ToLower(it)
		switch {
		case strings.HasPrefix(lower, "waive "):
			o, _, err := Parse(rel.FullName(), "require "+strings.TrimSpace(it[len("waive "):]))
			if err != nil {
				return nil, fmt.Errorf("%s: %v", norm, err)
			}
			o.Context, o.Waiver, o.Source = name, true, norm
			out = append(out, o)
		case strings.HasPrefix(lower, "require "):
			o, _, err := Parse(rel.FullName(), it)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", norm, err)
			}
			o.Context, o.Source = name, norm
			out = append(out, o)
		case strings.HasPrefix(lower, "may read "):
			label := strings.TrimSpace(it[len("may read "):])
			if label == "" || strings.ContainsAny(label, " \t") {
				return nil, fmt.Errorf("%s: `may read` names one label", norm)
			}
			out = append(out, Obligation{Subject: rel.FullName(), Context: name, Body: Body{MayRead: label}, Source: norm})
		default:
			return nil, fmt.Errorf("%s: %q is neither `require ...` nor `waive ...`", norm, it)
		}
	}
	return out, nil
}

// InContext is the set of obligations in force under the named context ("" for none):
// the base obligations minus those the context waives, plus the context's own. The
// result carries no waivers and no other context's obligations, so it can be judged.
func InContext(decls []Obligation, name string) []Obligation {
	// a waiver lifts the statement kinds it names (`waive pinned(x) on select` leaves the
	// writes bound); without `on` it lifts them all
	waived := map[string]Kinds{}
	for _, o := range decls {
		if o.Waiver && o.Context == name {
			waived[o.Subject+"\x00"+o.Body.Spec()] |= o.Kinds
		}
	}
	var out []Obligation
	for _, o := range decls {
		switch {
		case o.Waiver:
		case o.Context == "":
			if rest := o.Kinds &^ waived[o.Subject+"\x00"+o.Body.Spec()]; rest != 0 {
				o.Kinds = rest
				out = append(out, o)
			}
		case o.Context == name:
			out = append(out, o)
		}
	}
	return out
}

// Contexts lists the context names the declarations mention, in order of first mention.
func Contexts(decls []Obligation) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range decls {
		if o.Context != "" && !seen[o.Context] {
			seen[o.Context] = true
			out = append(out, o.Context)
		}
	}
	return out
}

// columnRule reads the two column-level declarations written above a CREATE TABLE:
//
//	sensitive <label>: col, col        the columns carry the label
//	transitions <col>: a -> b, b -> c | d   the column is a state machine
func columnRule(rel *schema.Relation, directive string) (Obligation, error) {
	norm := strings.Join(strings.Fields(directive), " ")
	o := Obligation{Subject: rel.FullName(), Kinds: OnAll, Source: norm}
	head, body, ok := strings.Cut(norm, ":")
	if !ok {
		return Obligation{}, fmt.Errorf("%s: expected `<keyword> <name>: ...`", norm)
	}
	kw, name, _ := strings.Cut(strings.TrimSpace(head), " ")
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return Obligation{}, fmt.Errorf("%s: %s names one %s", norm, kw, map[string]string{"sensitive": "label", "transitions": "column"}[strings.ToLower(kw)])
	}
	switch strings.ToLower(kw) {
	case "sensitive":
		sens := &Sensitive{Label: name}
		for _, c := range strings.Split(body, ",") {
			if c = strings.TrimSpace(c); c != "" {
				if !hasColumn(rel, c) {
					return Obligation{}, fmt.Errorf("%s: %s has no column %s", norm, rel.Name, c)
				}
				sens.Columns = append(sens.Columns, c)
			}
		}
		if len(sens.Columns) == 0 {
			return Obligation{}, fmt.Errorf("%s: no columns", norm)
		}
		o.Body.Sensitive = sens
	case "transitions":
		if !hasColumn(rel, name) {
			return Obligation{}, fmt.Errorf("%s: %s has no column %s", norm, rel.Name, name)
		}
		tr := &Transitions{Column: name, From: map[string][]string{}}
		for _, edge := range strings.Split(body, ",") {
			lhs, rhs, ok := strings.Cut(edge, "->")
			if !ok {
				return Obligation{}, fmt.Errorf("%s: %q is not `from -> to`", norm, strings.TrimSpace(edge))
			}
			froms := states(lhs)
			tos := states(rhs)
			if len(froms) == 0 || len(tos) == 0 {
				return Obligation{}, fmt.Errorf("%s: %q is not `from -> to`", norm, strings.TrimSpace(edge))
			}
			for _, to := range tos {
				if _, seen := tr.From[to]; !seen {
					tr.Order = append(tr.Order, to)
				}
				tr.From[to] = append(tr.From[to], froms...)
			}
		}
		o.Kinds = OnUpdate
		o.Body.Transitions = tr
	}
	return o, nil
}

// states splits `a | b` into its state names, quotes stripped.
func states(list string) []string {
	var out []string
	for _, st := range strings.Split(list, "|") {
		st = strings.Trim(strings.TrimSpace(st), "'\"")
		if st != "" {
			out = append(out, st)
		}
	}
	return out
}
