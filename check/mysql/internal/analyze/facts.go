package analyze

import (
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// The statement's facts (x/facts): what one block provably does, for the contracts that are
// written on facts rather than on a dialect's tree — the One proof (x/cardinality) and the
// obligations. Everything recorded holds for every surviving row: a conjunct of WHERE or of
// an inner join's ON, and for an outer join only what its ON says about the nullable side; a
// disjunction contributes nothing, a function call is opaque.

// block records one SELECT block (or the target list of a write) after its clauses are typed.
func (a *analyzer) block(sc *scope, body *mysqlast.Node) *facts.Scope {
	fs := &facts.Scope{At: -1}
	for _, r := range sc.rels {
		fs.Leaves = append(fs.Leaves, a.leafFacts(r))
	}
	if w := body.Arg("opt_where_clause"); w != nil {
		if n, ok := w.(*mysqlast.Node); ok {
			fs.At = int32(a.ph.Back(n.Start))
			if n.Class == "PTI_where" {
				w = n.Arg("expr")
			}
		}
		for _, c := range conjuncts(w) {
			a.predFacts(sc, fs, c, nil)
		}
	}
	for _, j := range sc.joins {
		var restrict []int // nil: the conjunct holds for every row of the block
		switch {
		case strings.Contains(j.kind, "LEFT"):
			restrict = j.right
		case strings.Contains(j.kind, "RIGHT"):
			restrict = j.left
		}
		if j.on != nil {
			for _, c := range conjuncts(j.on) {
				a.predFacts(sc, fs, c, restrict)
			}
		}
		for _, name := range j.using {
			l, okl := a.colIn(sc, j.left, name)
			r, okr := a.colIn(sc, j.right, name)
			if okl && okr {
				a.equalityFacts(fs, l, r, restrict)
			}
		}
	}
	closeFixed(fs)
	if sc.kids != nil {
		fs.Children = append(fs.Children, *sc.kids...)
	}
	a.shape(sc, fs, body)
	return fs
}

// shape writes down what a SELECT block says about its own row count: its GROUP BY
// expressions (each a leaf column, a known value, or an expression; ROLLUP is one row per
// set), or an aggregate-only select list without GROUP BY (one row).
func (a *analyzer) shape(sc *scope, fs *facts.Scope, body *mysqlast.Node) {
	if body.Class != "PT_query_specification" {
		return
	}
	items, _ := body.Arg("item_list").(mysqlast.List)
	g, ok := body.Arg("opt_group_clause").(*mysqlast.Node)
	if !ok {
		if len(items) > 0 && a.allAggregates(items) {
			fs.Single = true
		}
		return
	}
	fs.Groups = []facts.Term{}
	fs.GroupingSets = str(g.Arg("olap")) != "" && str(g.Arg("olap")) != "UNSPECIFIED_OLAP_TYPE"
	list, _ := g.Arg("group_list").(mysqlast.List)
	for _, it := range list {
		item := it
		if oe, ok := it.(*mysqlast.Node); ok && oe.Class == "PT_order_expr" {
			item = oe.Arg("item")
		}
		fs.Groups = append(fs.Groups, a.groupTerm(sc, items, item))
	}
}

// groupTerm classifies one GROUP BY item the way the server resolves it: an integer is a
// select-list position, a bare name a select-list alias first, then a column.
func (a *analyzer) groupTerm(sc *scope, items mysqlast.List, item mysqlast.Value) facts.Term {
	if n, ok := item.(*mysqlast.Node); ok {
		switch n.Class {
		case "Item_int", "Item_uint":
			i := intOr(n.Arg("i"), intOr(n.Arg("str"), 0)) - 1
			if i >= 0 && i < len(items) {
				if ewa, ok := items[i].(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" {
					return a.groupTerm(sc, nil, ewa.Arg("expr"))
				}
			}
		case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
			name := str(n.Arg("ident"))
			for _, it := range items {
				if ewa, ok := it.(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" && strings.EqualFold(str(ewa.Arg("alias")), name) {
					if _, isCol := a.colFact(sc, n); !isCol {
						return a.groupTerm(sc, nil, ewa.Arg("expr"))
					}
				}
			}
		}
	}
	if col, ok := a.colFact(sc, item); ok {
		return facts.Term{Kind: facts.Column, Col: col}
	}
	if t, ok := a.termFacts(sc, item); ok {
		return t
	}
	text := ""
	if n, ok := item.(*mysqlast.Node); ok {
		text = a.textOf(n)
	}
	return facts.Term{Kind: facts.Expr, Text: text}
}

// leafFacts describes one relation of the block: its table and the unique keys the proof
// may fix a row by (the primary key and the UNIQUE keys over whole columns).
func (a *analyzer) leafFacts(r relation) facts.Leaf {
	lf := facts.Leaf{Alias: r.alias, Kind: facts.Derived, Role: facts.Read, Position: int32(a.ph.Back(r.pos))}
	if r.target {
		lf.Role = facts.Target
	}
	if r.cte {
		lf.Kind = facts.CTE
	}
	if r.table != nil {
		lf.Table, lf.Kind = r.table.Name, facts.Table
		for _, k := range r.table.Keys {
			if k.Kind != schema.Primary && k.Kind != schema.Unique || len(k.Parts) == 0 {
				continue
			}
			cols := make([]string, 0, len(k.Parts))
			for _, p := range k.Parts {
				if p.Expr != nil || p.Length != 0 || p.Column == "" {
					cols = nil
					break
				}
				cols = append(cols, p.Column)
			}
			if cols != nil {
				lf.Keys = append(lf.Keys, facts.Key{Columns: cols})
			}
		}
	} else if r.view != "" {
		lf.Table, lf.Kind = r.view, facts.View
	}
	if r.table == nil && r.body != nil {
		lf.Body = r.body
	}
	if lf.Table != "" {
		// the opt-outs: the statement's, or inside a view's body the view's own directives
		waived := a.waived
		if len(a.viewWaived) > 0 {
			waived = a.viewWaived[len(a.viewWaived)-1]
		}
		lf.Waived = append([]string{}, waived[lf.Table]...)
	}
	return lf
}

// conjuncts flattens the ANDs of a condition.
func conjuncts(v mysqlast.Value) []mysqlast.Value {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	if n.Class == "Item_cond_and" {
		var out []mysqlast.Value
		for _, arg := range n.Args {
			out = append(out, conjuncts(arg)...)
		}
		return out
	}
	return []mysqlast.Value{v}
}

// predFacts records one conjunct. restrict, when not nil, lists the leaves the conjunct is
// allowed to say something about (the nullable side of an outer join).
func (a *analyzer) predFacts(sc *scope, fs *facts.Scope, c mysqlast.Value, restrict []int) {
	n, ok := c.(*mysqlast.Node)
	if !ok {
		return
	}
	text, cols := a.opaqueText(sc, n)
	pr := facts.Pred{Op: facts.Opaque, Text: text, Cols: cols, Origin: facts.FromStatement}
	if restrict != nil {
		pr.Restricts = append([]int{}, restrict...)
	}
	switch n.Class {
	case "PTI_comp_op":
		if op, _ := n.Arg("boolfunc2creator").(mysqlast.Op); op == "=" {
			l, okl := a.colFact(sc, n.Arg("left"))
			r, okr := a.colFact(sc, n.Arg("right"))
			switch {
			case okl && okr:
				a.equalityFacts(fs, l, r, restrict)
				return
			case okl || okr:
				col, other := l, n.Arg("right")
				if okr {
					col, other = r, n.Arg("left")
				}
				if term, ok := a.termFacts(sc, other); ok && allowed(col, restrict) {
					fs.Preds = append(fs.Preds, facts.Pred{Op: facts.Eq, Col: col, Term: term, Text: pr.Text, Restricts: pr.Restricts, Origin: facts.FromStatement})
					fs.Fixed = append(fs.Fixed, col)
					return
				}
			}
		}
	case "Item_func_isnull", "Item_func_isnotnull":
		if col, ok := a.colFact(sc, n.Arg("a")); ok && allowed(col, restrict) {
			pr.Op, pr.Col = facts.IsNull, col
			if n.Class == "Item_func_isnotnull" {
				pr.Op = facts.IsNotNull
				fs.NotNull = append(fs.NotNull, col)
			}
			fs.Preds = append(fs.Preds, pr)
			return
		}
	case "Item_func_in":
		list, _ := n.Arg("list").(mysqlast.List)
		if !isTrue(n.Arg("is_negation")) && len(list) > 1 {
			if col, ok := a.colFact(sc, list[0]); ok && allowed(col, restrict) {
				terms := make([]facts.Term, 0, len(list)-1)
				for _, v := range list[1:] {
					t, ok := a.termFacts(sc, v)
					if !ok {
						terms = nil
						break
					}
					terms = append(terms, t)
				}
				if terms != nil {
					pr.Op, pr.Col, pr.Terms = facts.In, col, terms
					fs.Preds = append(fs.Preds, pr)
					return
				}
			}
		}
	}
	fs.Preds = append(fs.Preds, pr)
}

// opaqueText renders a conjunct the language does not decompose, with every column
// reference that resolves to a leaf of the block reduced to its bare column name (so the
// same predicate reads the same whatever the alias), and lists those columns.
func (a *analyzer) opaqueText(sc *scope, n *mysqlast.Node) (string, []facts.ColRef) {
	type cut struct {
		start, end int
		name       string
	}
	var cuts []cut
	var cols []facts.ColRef
	seen := map[facts.ColRef]bool{}
	var walk func(v mysqlast.Value)
	walk = func(v mysqlast.Value) {
		switch x := v.(type) {
		case *mysqlast.Node:
			switch x.Class {
			case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
				if ref, ok := a.colFact(sc, x); ok {
					if !seen[ref] {
						seen[ref] = true
						cols = append(cols, ref)
					}
					if x.Class == "PTI_simple_ident_q_2d" || x.Class == "PTI_simple_ident_q_3d" {
						cuts = append(cuts, cut{x.Start, x.End, ref.Column})
					}
				}
				return
			}
			for _, arg := range x.Args {
				walk(arg)
			}
		case mysqlast.List:
			for _, e := range x {
				walk(e)
			}
		case *mysqlast.Struct:
			for _, k := range x.Order {
				walk(x.Fields[k])
			}
		}
	}
	walk(n)
	text := a.textOf(n)
	if len(cuts) > 0 && n.Start >= 0 && n.End <= len(a.text) {
		sort.Slice(cuts, func(i, j int) bool { return cuts[i].start < cuts[j].start })
		var b strings.Builder
		pos := n.Start
		for _, c := range cuts {
			if c.start < pos || c.end > n.End {
				continue
			}
			b.WriteString(a.text[pos:c.start])
			b.WriteString(c.name)
			pos = c.end
		}
		b.WriteString(a.text[pos:n.End])
		text = b.String()
	}
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Leaf != cols[j].Leaf {
			return cols[i].Leaf < cols[j].Leaf
		}
		return cols[i].Column < cols[j].Column
	})
	return text, cols
}

// equalityFacts records col = col: a predicate, and the edges knowledge travels along —
// both ways for an inner join or WHERE, only into the nullable side for an outer join's ON.
func (a *analyzer) equalityFacts(fs *facts.Scope, l, r facts.ColRef, restrict []int) {
	fs.Preds = append(fs.Preds, facts.Pred{Op: facts.Eq, Col: l, Term: facts.Term{Kind: facts.Column, Col: r}, Origin: facts.FromStatement, Restricts: append([]int(nil), restrict...)})
	if restrict == nil || allowed(r, restrict) {
		fs.Edges = append(fs.Edges, facts.Edge{From: l, To: r})
	}
	if restrict == nil || allowed(l, restrict) {
		fs.Edges = append(fs.Edges, facts.Edge{From: r, To: l})
	}
}

// allowed reports whether a conjunct may speak about the column's leaf.
func allowed(c facts.ColRef, restrict []int) bool {
	if restrict == nil {
		return true
	}
	for _, i := range restrict {
		if i == c.Leaf {
			return true
		}
	}
	return false
}

// colFact resolves a column reference to a leaf of this block (an outer query's column is
// not a fact of this block; it is a Known term instead, see termFacts).
func (a *analyzer) colFact(sc *scope, v mysqlast.Value) (facts.ColRef, bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return facts.ColRef{}, false
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
	default:
		return facts.ColRef{}, false
	}
	own := scope{rels: sc.rels} // this level only
	ref, err := a.column(own, n, "where clause")
	if err != nil || ref.rel == nil {
		return facts.ColRef{}, false
	}
	for i := range sc.rels {
		if &sc.rels[i] == ref.rel || sc.rels[i].alias == ref.rel.alias {
			return facts.ColRef{Leaf: i, Column: ref.c.Name}, true
		}
	}
	return facts.ColRef{}, false
}

// colIn resolves a USING column within the given leaves.
func (a *analyzer) colIn(sc *scope, leaves []int, name string) (facts.ColRef, bool) {
	for _, i := range leaves {
		if _, ok := sc.rels[i].column(name); ok {
			return facts.ColRef{Leaf: i, Column: name}, true
		}
	}
	return facts.ColRef{}, false
}

// termFacts classifies the known side of an equality: a parameter, a literal, or an
// expression that reads no column of this block (an outer reference, a function of
// constants). An expression reading this block's columns is not a term.
func (a *analyzer) termFacts(sc *scope, v mysqlast.Value) (facts.Term, bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return facts.Term{}, false
	}
	switch n.Class {
	case "Item_param":
		return facts.Term{Kind: facts.Param, Param: int32(a.ph.Number(n.Start))}, true
	case "Item_int", "Item_uint", "Item_decimal", "Item_float", "PTI_text_literal_text_string", "PTI_text_literal_nchar_string",
		"PTI_text_literal_underscore_charset", "Item_hex_string", "Item_bin_string", "Item_null", "Item_func_true", "Item_func_false":
		return facts.Term{Kind: facts.Const, Const: a.textOf(n)}, true
	}
	if a.readsBlock(sc, v) {
		return facts.Term{}, false
	}
	if _, isSub := v.(*mysqlast.Node); isSub && containsClass(v, "PT_subquery") {
		return facts.Term{}, false // a subquery's value is not known before the statement runs
	}
	return facts.Term{Kind: facts.Known, Text: a.textOf(n)}, true
}

// readsBlock reports whether v references a column of this block.
func (a *analyzer) readsBlock(sc *scope, v mysqlast.Value) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		if _, ok := a.colFact(sc, x); ok {
			return true
		}
		for _, arg := range x.Args {
			if a.readsBlock(sc, arg) {
				return true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if a.readsBlock(sc, e) {
				return true
			}
		}
	case *mysqlast.Struct:
		for _, e := range x.Fields {
			if a.readsBlock(sc, e) {
				return true
			}
		}
	}
	return false
}

// textOf is the source text of a node, in the statement's own spelling ($n).
func (a *analyzer) textOf(n *mysqlast.Node) string {
	if n.Start < 0 || n.End > len(a.text) || n.Start >= n.End {
		return ""
	}
	return a.text[n.Start:n.End]
}

// closeFixed closes Fixed over Edges (From fixed implies To fixed) and sorts it.
func closeFixed(fs *facts.Scope) {
	known := map[facts.ColRef]bool{}
	for _, c := range fs.Fixed {
		known[c] = true
	}
	for changed := true; changed; {
		changed = false
		for _, e := range fs.Edges {
			if known[e.From] && !known[e.To] {
				known[e.To] = true
				changed = true
			}
		}
	}
	fs.Fixed = fs.Fixed[:0]
	for c := range known {
		fs.Fixed = append(fs.Fixed, c)
	}
	sort.Slice(fs.Fixed, func(i, j int) bool {
		if fs.Fixed[i].Leaf != fs.Fixed[j].Leaf {
			return fs.Fixed[i].Leaf < fs.Fixed[j].Leaf
		}
		return fs.Fixed[i].Column < fs.Fixed[j].Column
	})
}

// writeFacts is the record of a write: its target, the columns it assigns and the terms
// stored in them (parallel to assigned; the first row's for a multi-row INSERT).
func (a *analyzer) writeFacts(kind facts.StmtKind, rel *relation, assigned []*schema.Column, values []facts.Term) facts.Write {
	w := facts.Write{Table: rel.table.Name, Kind: kind, Position: int32(a.ph.Back(rel.pos))}
	for i, c := range assigned {
		dup := false
		for _, name := range w.Assigned {
			dup = dup || name == c.Name
		}
		if dup {
			continue
		}
		w.Assigned = append(w.Assigned, c.Name)
		v := facts.Term{Kind: facts.Known, Text: "?"}
		if i < len(values) {
			v = values[i]
		}
		w.Values = append(w.Values, v)
	}
	return w
}

// limitOne reports a literal LIMIT 1 (or 0).
func limitOne(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	lim := mysqlast.Value(n)
	if opts, ok := n.Arg("limit_options").(*mysqlast.Struct); ok {
		lim = opts.Fields["limit"]
	}
	ln, ok := lim.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch ln.Class {
	case "Item_int":
		return intOr(ln.Arg("i"), -1) <= 1 && intOr(ln.Arg("i"), -1) >= 0
	case "Item_uint":
		return intOr(ln.Arg("str"), -1) <= 1 && intOr(ln.Arg("str"), -1) >= 0
	}
	return false
}

// allAggregates reports a select list made only of aggregate calls (over the whole input,
// without GROUP BY, that is one row).
func (a *analyzer) allAggregates(items mysqlast.Value) bool {
	list, _ := items.(mysqlast.List)
	if len(list) == 0 {
		return false
	}
	for _, it := range list {
		n, ok := it.(*mysqlast.Node)
		if !ok || n.Class != "PTI_expr_with_alias" {
			return false
		}
		if !isAggregate(n.Arg("expr")) {
			return false
		}
	}
	return true
}

// isAggregate reports an aggregate function call (COUNT, SUM, MIN, MAX, AVG, GROUP_CONCAT,
// BIT_*, STD, ...): the Item_sum classes and COUNT(*)'s own node. A window function (an
// aggregate with OVER) is not one.
func isAggregate(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	if n.Class == "PTI_count_sym" {
		return n.Arg("w") == nil
	}
	if !isA(n.Class, "Item_sum") {
		return false
	}
	return n.Arg("w") == nil && n.Arg("window") == nil // with OVER it is a window function
}
