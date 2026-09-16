package factsprobe

import (
	"fmt"
	"math/rand"
	"strings"
)

// ---- the statement model -------------------------------------------------------------
//
// A statement is a scope: leaves (table aliases joined together), conjuncts over them, and
// optionally LIMIT 1. Every conjunct carries its own three-valued evaluation over a row of
// the model, so the probe can (1) compute the rows the server should return, as a check on
// the generator itself, and (2) evaluate an Opaque predicate the facts only spell as text.

// leaf is one relation occurrence under an alias: a table, a view (a Table the model
// materialized from its base, rendered as CREATE VIEW), a derived table or a CTE (both
// rendered from src, the model's rows in table).
type leaf struct {
	alias string
	table *Table
	// src is the defining query of a derived table or CTE leaf (nil for a table or view):
	// the base table under alias "d" and the conjuncts over it
	src *derived
	cte bool // src is written as a WITH item named table.Name, referenced by name
	// outer: joined with LEFT JOIN (its columns may be NULL in a row); on is its join
	// condition, over this and earlier leaves
	outer bool
	on    []conj
}

// derived is the body of a view, derived table or CTE: SELECT every column of base (as
// d) WHERE the conjuncts hold.
type derived struct {
	base  *Table
	where []conj
}

func (d *derived) sql() string {
	var items []string
	for _, c := range d.base.Cols {
		items = append(items, "d."+c.Name)
	}
	return "SELECT " + strings.Join(items, ", ") + " FROM " + d.base.Name + " AS d WHERE " + conjSQL(d.where)
}

// rows evaluates the body over the model.
func (d *derived) rows(m *Schema, params []Value) [][]Value {
	var out [][]Value
	for _, tr := range d.base.Rows {
		r := row{}.with(leaf{alias: "d", table: d.base}, tr)
		ok := true
		for _, c := range d.where {
			if c.eval(r, m, params) != yes {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, tr)
		}
	}
	return out
}

// assign is one SET item of an UPDATE, or one column of an INSERT row: a literal or a
// parameter.
type assign struct {
	col   Col
	sql   string
	value Value
}

// tri is a three-valued truth.
type tri byte

const (
	unknown tri = iota
	yes
	no
)

func bool3(b bool) tri {
	if b {
		return yes
	}
	return no
}

// conj is one conjunct: its SQL, the facts shape the generator expects it to produce (an
// alphabet entry), and its evaluation.
type conj struct {
	sql  string
	kind string // the alphabet entry this conjunct is meant to exercise
	eval func(r row, m *Schema, params []Value) tri
	// cols are the alias.column references the conjunct reads (for minimization notes)
}

// query is a generated statement: a SELECT over its leaves, or a single-table UPDATE /
// DELETE (leaves[0] is the target) or an INSERT of one row (leaves[0] the table).
type query struct {
	kind   string // "select", "update", "delete", "insert"
	leaves []leaf
	where  []conj
	limit1 bool
	params []Value // bound values of $1..$n
	// group: GROUP BY this column of leaves[0], with COUNT(*) beside it (the select list
	// is then that column alone)
	group *Col
	// union: UNION ALL with a second branch selecting leaves[0]'s "a" from another table
	union *leaf
	// set are UPDATE's assignments, or INSERT's row (every column, in table order)
	set []assign
	// known are the values of the uncorrelated scalar subqueries the statement spells,
	// by their normalized text, for a Known term the facts carry
	known map[string]Value
}

// normText lowercases a SQL fragment and drops its spaces and AS keywords, so a producer's
// rendering of a subquery compares with the generator's.
func normText(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " as ", " ")
	return strings.Trim(strings.ReplaceAll(s, " ", ""), "()")
}

// sql renders the statement. A SELECT projects every column of every leaf, aliased
// alias__col, so the rows returned hold what the facts speak about; under GROUP BY only
// the grouping column (and COUNT(*) AS n); a UNION ALL branch projects "a" alone.
func (q *query) sql() string {
	switch q.kind {
	case "update":
		var sets []string
		for _, a := range q.set {
			sets = append(sets, a.col.Name+" = "+a.sql)
		}
		return "UPDATE " + q.leaves[0].table.Name + " AS " + q.leaves[0].alias + " SET " + strings.Join(sets, ", ") + "\nWHERE " + conjSQL(q.where)
	case "delete":
		return "DELETE FROM " + q.leaves[0].table.Name + " AS " + q.leaves[0].alias + "\nWHERE " + conjSQL(q.where)
	case "insert":
		var cols, vals []string
		for _, a := range q.set {
			cols = append(cols, a.col.Name)
			vals = append(vals, a.sql)
		}
		return "INSERT INTO " + q.leaves[0].table.Name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
	}
	var b strings.Builder
	var ctes []string
	for _, l := range q.leaves {
		if l.cte {
			ctes = append(ctes, l.table.Name+" AS ("+l.src.sql()+")")
		}
	}
	if len(ctes) > 0 {
		b.WriteString("WITH " + strings.Join(ctes, ",\n     ") + "\n")
	}
	b.WriteString("SELECT ")
	var items []string
	if q.group != nil {
		// GROUP BY t0.id: every column of t0 is functionally dependent on it, so all
		// of them are projected (both dialects accept that); the other leaf's are not
		for _, c := range q.leaves[0].table.Cols {
			items = append(items, "t0."+c.Name+" AS t0__"+c.Name)
		}
		items = append(items, "COUNT(*) AS n")
	} else if q.union != nil {
		items = append(items, "t0.a AS t0__a")
	} else {
		for _, l := range q.leaves {
			for _, c := range l.table.Cols {
				items = append(items, l.alias+"."+c.Name+" AS "+l.alias+"__"+c.Name)
			}
		}
	}
	b.WriteString(strings.Join(items, ", "))
	b.WriteString("\nFROM ")
	for i, l := range q.leaves {
		if i > 0 {
			if l.outer {
				b.WriteString("\n  LEFT JOIN ")
			} else {
				b.WriteString("\n  JOIN ")
			}
		}
		switch {
		case l.src != nil && !l.cte:
			b.WriteString("(" + l.src.sql() + ") AS " + l.alias)
		default:
			b.WriteString(l.table.Name + " AS " + l.alias)
		}
		if i > 0 {
			b.WriteString(" ON " + conjSQL(l.on))
		}
	}
	if len(q.where) > 0 {
		b.WriteString("\nWHERE " + conjSQL(q.where))
	}
	if q.group != nil {
		b.WriteString("\nGROUP BY t0." + q.group.Name)
	}
	if q.union != nil {
		b.WriteString("\nUNION ALL SELECT u.a FROM " + q.union.table.Name + " AS u")
	}
	if q.limit1 {
		b.WriteString("\nLIMIT 1")
	}
	return b.String()
}

// witness is the SELECT that returns the rows a write touches: its target's columns under
// the same WHERE.
func (q *query) witness() string {
	sel := &query{kind: "select", leaves: q.leaves[:1], where: q.where, params: q.params}
	return sel.sql()
}

func conjSQL(cs []conj) string {
	if len(cs) == 0 {
		return "TRUE"
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.sql
	}
	return strings.Join(parts, " AND ")
}

// rows evaluates the query over the model: the rows the server should return (order
// aside; LIMIT 1 is checked by count, not by which row).
func (q *query) rows(m *Schema) []row {
	var out []row
	var walk func(i int, r row)
	walk = func(i int, r row) {
		if i == len(q.leaves) {
			for _, c := range q.where {
				if c.eval(r, m, q.params) != yes {
					return
				}
			}
			out = append(out, r)
			return
		}
		l := q.leaves[i]
		matched := false
		for _, tr := range l.table.Rows {
			nr := r.with(l, tr)
			ok := true
			for _, c := range l.on {
				if c.eval(nr, m, q.params) != yes {
					ok = false
					break
				}
			}
			if ok {
				matched = true
				walk(i+1, nr)
			}
		}
		if l.outer && !matched {
			walk(i+1, r.with(l, nil))
		}
	}
	walk(0, row{})
	if q.group != nil {
		groups := map[string]int{}
		first := map[string]row{}
		var order []string
		for _, r := range out {
			k := r.get("t0", "id").String()
			if _, ok := groups[k]; !ok {
				order = append(order, k)
				first[k] = r
			}
			groups[k]++
		}
		out = nil
		for _, k := range order {
			g := row{"n": intValue(groups[k])}
			for _, c := range q.leaves[0].table.Cols {
				g["t0."+c.Name] = first[k].get("t0", c.Name)
			}
			out = append(out, g)
		}
	}
	if q.union != nil {
		var projected []row
		for _, r := range out {
			projected = append(projected, row{"t0.a": r.get("t0", "a")})
		}
		for _, tr := range q.union.table.Rows {
			projected = append(projected, row{"t0.a": tr[q.union.table.col("a")]})
		}
		out = projected
	}
	return out
}

// with extends r with a leaf's row (nil: every column NULL).
func (r row) with(l leaf, tr []Value) row {
	nr := make(row, len(r)+len(l.table.Cols))
	for k, v := range r {
		nr[k] = v
	}
	for i, c := range l.table.Cols {
		if tr == nil {
			nr[l.alias+"."+c.Name] = null
		} else {
			nr[l.alias+"."+c.Name] = tr[i]
		}
	}
	return nr
}

// ---- generation -----------------------------------------------------------------------

// genSchema draws 2 or 3 tables of 4 columns (id INT PRIMARY KEY, a INT, b INT NOT NULL
// UNIQUE or not, s VARCHAR) with 3 to 6 rows over a small domain (1..3, 'x'..'z', NULL),
// so predicates match several rows and NULLs reach every comparison.
func genSchema(r *rand.Rand, prefix string) *Schema {
	s := &Schema{}
	n := 2 + r.Intn(2)
	for i := 0; i < n; i++ {
		t := &Table{Name: fmt.Sprintf("%st%d", prefix, i)}
		t.Cols = []Col{{Name: "id", Kind: Int, NotNull: true}, {Name: "a", Kind: Int}, {Name: "b", Kind: Int, NotNull: true}, {Name: "s", Kind: Str}}
		t.PK = []string{"id"}
		unique := r.Intn(2) == 0
		if unique {
			t.Uniques = [][]string{{"b"}}
		}
		rows := 3 + r.Intn(4)
		seenB := map[string]bool{}
		for k := 0; k < rows; k++ {
			b := intValue(1 + r.Intn(6))
			for unique && seenB[b.S] {
				b = intValue(1 + r.Intn(6))
			}
			seenB[b.S] = true
			t.Rows = append(t.Rows, []Value{intValue(k + 1), genInt(r, true), b, genStr(r, true)})
		}
		s.Tables = append(s.Tables, t)
	}
	// a view over one of the tables, with a conjunct of its own (a fixed column, a
	// NOT NULL, an opaque comparison), materialized in the model from the base's rows
	if r.Intn(2) == 0 {
		base := s.Tables[r.Intn(len(s.Tables))]
		v := &Table{Name: prefix + "v0", Cols: base.Cols, view: &derived{base: base}}
		for {
			// a view body takes no parameter
			c := genConj(r, &query{}, []leaf{{alias: "d", table: base}}, s)
			if !strings.Contains(c.sql, "$") {
				v.view.where = []conj{c}
				break
			}
		}
		v.Rows = v.view.rows(s, nil)
		s.Tables = append(s.Tables, v)
	}
	return s
}

func genInt(r *rand.Rand, nullable bool) Value {
	if nullable && r.Intn(4) == 0 {
		return null
	}
	return intValue(1 + r.Intn(3))
}

func genStr(r *rand.Rand, nullable bool) Value {
	if nullable && r.Intn(4) == 0 {
		return null
	}
	return strValue(string(rune('x' + r.Intn(3))))
}

// genQuery draws a statement over the schema. A SELECT: one leaf (a table, the view, a
// derived table or a CTE over a table), or two joined (inner or left), one to three
// WHERE conjuncts, sometimes GROUP BY, UNION ALL or LIMIT 1. Or a single-table UPDATE /
// DELETE under the same WHERE vocabulary, or an INSERT of one row.
func genQuery(r *rand.Rand, m *Schema) *query {
	q := &query{kind: "select"}
	var bases []*Table
	for _, t := range m.Tables {
		if t.view == nil {
			bases = append(bases, t)
		}
	}
	switch r.Intn(10) {
	case 0:
		q.kind = "update"
	case 1:
		q.kind = "delete"
	case 2:
		q.kind = "insert"
	}
	if q.kind != "select" {
		t := bases[r.Intn(len(bases))]
		q.leaves = []leaf{{alias: "t0", table: t}}
		if q.kind == "insert" {
			id := len(t.Rows) + 1 + r.Intn(3)
			for _, c := range t.Cols {
				var v Value
				switch {
				case c.Name == "id":
					v = intValue(id)
				case c.Kind == Str:
					v = genStr(r, !c.NotNull)
				default:
					v = genInt(r, !c.NotNull)
				}
				q.set = append(q.set, genAssign(r, q, c, v))
			}
			return q
		}
		if q.kind == "update" {
			n := 1 + r.Intn(2)
			used := map[string]bool{}
			for i := 0; i < n; i++ {
				c := t.Cols[1+r.Intn(len(t.Cols)-1)] // not the primary key
				if used[c.Name] {
					continue
				}
				used[c.Name] = true
				var v Value
				if c.Kind == Str {
					v = genStr(r, !c.NotNull)
				} else {
					v = genInt(r, !c.NotNull)
				}
				q.set = append(q.set, genAssign(r, q, c, v))
			}
		}
		n := 1 + r.Intn(3)
		for i := 0; i < n; i++ {
			q.where = append(q.where, genConj(r, q, q.leaves, m))
		}
		return q
	}
	q.leaves = append(q.leaves, genLeaf(r, q, m, "t0", bases))
	if r.Intn(2) == 0 {
		l := genLeaf(r, q, m, "t1", bases)
		l.outer = r.Intn(3) == 0
		// the ON: an equality between the two, sometimes a further conjunct on the new leaf
		l.on = append(l.on, eqCols(q.leaves[0], l, r))
		if r.Intn(3) == 0 {
			l.on = append(l.on, genConj(r, q, []leaf{l}, m))
		}
		q.leaves = append(q.leaves, l)
	}
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		q.where = append(q.where, genConj(r, q, q.leaves, m))
	}
	switch r.Intn(8) {
	case 0:
		if q.leaves[0].src == nil && q.leaves[0].table.view == nil {
			// GROUP BY the primary key, which only a base table leaf carries for the
			// functional dependency both dialects accept in the select list
			c := q.leaves[0].table.Cols[0] // id
			q.group = &c
		}
	case 1:
		u := bases[r.Intn(len(bases))]
		q.union = &leaf{alias: "u", table: u}
	}
	q.limit1 = r.Intn(6) == 0
	return q
}

// genLeaf draws a relation for alias: a table, the view when the schema has one, a
// derived table or a CTE over a table (each with a conjunct of its own).
func genLeaf(r *rand.Rand, q *query, m *Schema, alias string, bases []*Table) leaf {
	switch r.Intn(6) {
	case 0, 1:
		if v := m.view(); v != nil {
			return leaf{alias: alias, table: v}
		}
	case 2, 3:
		base := bases[r.Intn(len(bases))]
		d := &derived{base: base, where: []conj{genConj(r, q, []leaf{{alias: "d", table: base}}, m)}}
		t := &Table{Name: alias + "_d", Cols: base.Cols, Rows: d.rows(m, q.params)}
		return leaf{alias: alias, table: t, src: d, cte: r.Intn(2) == 0}
	}
	return leaf{alias: alias, table: bases[r.Intn(len(bases))]}
}

// genAssign is `col = literal` or `col = $n` for a write; an UPDATE's integer column
// sometimes takes `b + 1` (an expression: a Known value the probe cannot check).
func genAssign(r *rand.Rand, q *query, c Col, v Value) assign {
	if q.kind == "update" && c.Kind == Int && r.Intn(5) == 0 {
		return assign{col: c, sql: "b + 1", value: Value{S: "?"}}
	}
	if r.Intn(3) == 0 {
		q.params = append(q.params, v)
		return assign{col: c, sql: fmt.Sprintf("$%d", len(q.params)), value: v}
	}
	return assign{col: c, sql: literal(c.Kind, v), value: v}
}

// pick draws a leaf and one of its columns.
func pick(r *rand.Rand, leaves []leaf) (leaf, Col) {
	l := leaves[r.Intn(len(leaves))]
	return l, l.table.Cols[r.Intn(len(l.table.Cols))]
}

// eqCols is `b.col = a.col` between two leaves, over columns of one kind.
func eqCols(a, b leaf, r *rand.Rand) conj {
	ca := a.table.Cols[r.Intn(len(a.table.Cols))]
	var cands []Col
	for _, c := range b.table.Cols {
		if c.Kind == ca.Kind {
			cands = append(cands, c)
		}
	}
	cb := cands[r.Intn(len(cands))]
	return conj{sql: b.alias + "." + cb.Name + " = " + a.alias + "." + ca.Name, kind: "pred eq column",
		eval: func(rw row, _ *Schema, _ []Value) tri {
			return eqValues(rw.get(b.alias, cb.Name), rw.get(a.alias, ca.Name))
		}}
}

// eqValues is SQL's `=`: unknown when either side is NULL.
func eqValues(a, b Value) tri {
	if a.Null || b.Null {
		return unknown
	}
	return bool3(a.S == b.S)
}

// genConj draws one conjunct over the given leaves (the columns it may read); q supplies
// the parameter list to extend and m the other tables a subquery may use.
func genConj(r *rand.Rand, q *query, leaves []leaf, m *Schema) conj {
	l, c := pick(r, leaves)
	ref := l.alias + "." + c.Name
	lit := func() Value {
		if c.Kind == Str {
			return genStr(r, false)
		}
		return genInt(r, false)
	}
	litSQL := func(v Value) string { return literal(c.Kind, v) }
	switch r.Intn(13) {
	case 12: // col = <constant expression>: a value known before the statement runs
		expr, v := "2 + 1", intValue(3)
		if c.Kind == Str {
			expr, v = "LOWER('X')", strValue("x")
		}
		if q.known == nil {
			q.known = map[string]Value{}
		}
		q.known[normText(expr)] = v
		return conj{sql: ref + " = " + expr, kind: "pred eq known", eval: func(rw row, _ *Schema, _ []Value) tri { return eqValues(rw.get(l.alias, c.Name), v) }}
	case 0, 1: // col = literal
		v := lit()
		return conj{sql: ref + " = " + litSQL(v), kind: "pred eq const", eval: func(rw row, _ *Schema, _ []Value) tri { return eqValues(rw.get(l.alias, c.Name), v) }}
	case 2, 3: // col = $n
		q.params = append(q.params, lit())
		n := len(q.params)
		return conj{sql: fmt.Sprintf("%s = $%d", ref, n), kind: "pred eq param", eval: func(rw row, _ *Schema, ps []Value) tri { return eqValues(rw.get(l.alias, c.Name), ps[n-1]) }}
	case 4: // col = other column of the same leaf (same kind)
		var cands []Col
		for _, o := range l.table.Cols {
			if o.Kind == c.Kind && o.Name != c.Name {
				cands = append(cands, o)
			}
		}
		if len(cands) == 0 {
			return genConj(r, q, leaves, m)
		}
		o := cands[r.Intn(len(cands))]
		return conj{sql: ref + " = " + l.alias + "." + o.Name, kind: "pred eq column", eval: func(rw row, _ *Schema, _ []Value) tri {
			return eqValues(rw.get(l.alias, c.Name), rw.get(l.alias, o.Name))
		}}
	case 5: // col IN (a, b[, $n])
		vals := []Value{lit(), lit()}
		var parts []string
		for _, v := range vals {
			parts = append(parts, litSQL(v))
		}
		var pn int
		if r.Intn(2) == 0 {
			q.params = append(q.params, lit())
			pn = len(q.params)
			parts = append(parts, fmt.Sprintf("$%d", pn))
		}
		return conj{sql: ref + " IN (" + strings.Join(parts, ", ") + ")", kind: "pred in", eval: func(rw row, _ *Schema, ps []Value) tri {
			set := vals
			if pn > 0 {
				set = append(append([]Value(nil), vals...), ps[pn-1])
			}
			return inValues(rw.get(l.alias, c.Name), set)
		}}
	case 6: // col = a OR col = b
		v1, v2 := lit(), lit()
		return conj{sql: "(" + ref + " = " + litSQL(v1) + " OR " + ref + " = " + litSQL(v2) + ")", kind: "pred in", eval: func(rw row, _ *Schema, _ []Value) tri {
			return inValues(rw.get(l.alias, c.Name), []Value{v1, v2})
		}}
	case 7: // IS NULL
		return conj{sql: ref + " IS NULL", kind: "pred isnull", eval: func(rw row, _ *Schema, _ []Value) tri { return bool3(rw.get(l.alias, c.Name).Null) }}
	case 8: // IS NOT NULL
		return conj{sql: ref + " IS NOT NULL", kind: "pred isnotnull", eval: func(rw row, _ *Schema, _ []Value) tri { return bool3(!rw.get(l.alias, c.Name).Null) }}
	case 9: // col <op> literal: opaque
		v := lit()
		ops := []string{"<>", "<", ">", "<=", ">="}
		op := ops[r.Intn(len(ops))]
		return conj{sql: ref + " " + op + " " + litSQL(v), kind: "pred opaque", eval: func(rw row, _ *Schema, _ []Value) tri { return compare(rw.get(l.alias, c.Name), op, v, c.Kind) }}
	case 10: // EXISTS (SELECT 1 FROM u WHERE u.x = outer.col [AND u.y = lit])
		return genExists(r, q, l, c, m, false)
	case 11: // col = (SELECT MAX(u.x) FROM u): a value known before the statement runs
		return genScalar(r, q, l, c, m)
	default: // col IN (SELECT u.x FROM u WHERE ...)
		return genExists(r, q, l, c, m, true)
	}
}

// genScalar is `<ref> = (SELECT MAX(s0.x) FROM u AS s0)`, an uncorrelated scalar subquery
// (a Known term). u is never the write's own target (MySQL's 1093).
func genScalar(r *rand.Rand, q *query, l leaf, c Col, m *Schema) conj {
	u := otherTable(r, q, m)
	var cands []Col
	for _, o := range u.Cols {
		if o.Kind == c.Kind {
			cands = append(cands, o)
		}
	}
	x := cands[r.Intn(len(cands))]
	ref := l.alias + "." + c.Name
	sub := "(SELECT MAX(s0." + x.Name + ") FROM " + u.Name + " AS s0)"
	if q.known == nil {
		q.known = map[string]Value{}
	}
	q.known[normText(sub)] = maxOf(u, x)
	return conj{sql: ref + " = " + sub, kind: "pred eq known", eval: func(rw row, m *Schema, _ []Value) tri {
		return eqValues(rw.get(l.alias, c.Name), maxOf(u, x))
	}}
}

// maxOf is MAX(col) over the table's rows (NULL when no row has a value).
func maxOf(u *Table, x Col) Value {
	best := null
	for _, tr := range u.Rows {
		v := tr[u.col(x.Name)]
		if v.Null {
			continue
		}
		if best.Null || compare(v, ">", best, x.Kind) == yes {
			best = v
		}
	}
	return best
}

// otherTable draws a table for a subquery: any for a SELECT; for a write, one other than
// the target (MySQL refuses a subquery reading the table an UPDATE / DELETE writes, 1093,
// and one reading a view over it, 1443 -- a separate face, out of this probe's scope).
func otherTable(r *rand.Rand, q *query, m *Schema) *Table {
	for {
		u := m.Tables[r.Intn(len(m.Tables))]
		if q.kind == "select" || q.kind == "" || len(q.leaves) == 0 {
			return u
		}
		target := q.leaves[0].table
		if u == target || (u.view != nil && u.view.base == target) {
			continue
		}
		return u
	}
}

func inValues(v Value, set []Value) tri {
	if v.Null {
		return unknown
	}
	sawNull := false
	for _, s := range set {
		if s.Null {
			sawNull = true
			continue
		}
		if s.S == v.S {
			return yes
		}
	}
	if sawNull {
		return unknown
	}
	return no
}

// compare is SQL's ordering comparison over one kind (integers by value, strings by
// byte order -- the generator's strings are single lower-case letters, which every
// collation orders the same way).
func compare(a Value, op string, b Value, k ColKind) tri {
	if a.Null || b.Null {
		return unknown
	}
	var cmp int
	if k == Int {
		var x, y int
		fmt.Sscan(a.S, &x)
		fmt.Sscan(b.S, &y)
		cmp = x - y
	} else {
		cmp = strings.Compare(a.S, b.S)
	}
	switch op {
	case "<>":
		return bool3(cmp != 0)
	case "<":
		return bool3(cmp < 0)
	case ">":
		return bool3(cmp > 0)
	case "<=":
		return bool3(cmp <= 0)
	case ">=":
		return bool3(cmp >= 0)
	}
	return bool3(cmp == 0)
}

// genExists draws a correlated subquery over another table: `EXISTS (SELECT 1 FROM u AS
// s0 WHERE s0.x = <outer ref> [AND s0.y = lit])`, or the IN form `<outer ref> IN (SELECT
// s0.x FROM u AS s0 WHERE ...)`.
func genExists(r *rand.Rand, q *query, l leaf, c Col, m *Schema, in bool) conj {
	u := otherTable(r, q, m)
	var cands []Col
	for _, o := range u.Cols {
		if o.Kind == c.Kind {
			cands = append(cands, o)
		}
	}
	x := cands[r.Intn(len(cands))]
	ref := l.alias + "." + c.Name
	var extra *conj
	if r.Intn(2) == 0 {
		y := u.Cols[r.Intn(len(u.Cols))]
		var v Value
		if y.Kind == Str {
			v = genStr(r, false)
		} else {
			v = genInt(r, false)
		}
		e := conj{sql: "s0." + y.Name + " = " + literal(y.Kind, v), eval: func(rw row, _ *Schema, _ []Value) tri { return eqValues(rw.get("s0", y.Name), v) }}
		extra = &e
	}
	where := "s0." + x.Name + " = " + ref
	if extra != nil {
		where += " AND " + extra.sql
	}
	var sql, kind string
	if in {
		w := ""
		if extra != nil {
			w = " WHERE " + extra.sql
		}
		sql = ref + " IN (SELECT s0." + x.Name + " FROM " + u.Name + " AS s0" + w + ")"
		kind = "pred exists (in subquery)"
	} else {
		sql = "EXISTS (SELECT 1 FROM " + u.Name + " AS s0 WHERE " + where + ")"
		kind = "pred exists"
	}
	return conj{sql: sql, kind: kind, eval: func(rw row, m *Schema, ps []Value) tri {
		outer := rw.get(l.alias, c.Name)
		result := no
		for _, ur := range u.Rows {
			sr := row{}
			for i, uc := range u.Cols {
				sr["s0."+uc.Name] = ur[i]
			}
			if extra != nil && extra.eval(sr, m, ps) != yes {
				continue
			}
			switch eqValues(sr.get("s0", x.Name), outer) {
			case yes:
				return yes
			case unknown:
				if in {
					result = unknown // x IN (..., NULL, ...) with no match is NULL
				}
			}
		}
		return result
	}}
}
