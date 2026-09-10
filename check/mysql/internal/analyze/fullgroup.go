package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// The server's checks on a grouped, aggregated or DISTINCT block, as sql_mode
// ONLY_FULL_GROUP_BY (the default) runs them after resolution:
//
//   - Group_check (aggregate_check.cc): every expression of the select list, the ORDER BY,
//     the HAVING and the windows' PARTITION BY / ORDER BY must be group-invariant -- equal
//     to a GROUP BY expression, an aggregate, or made of columns functionally dependent on
//     the group columns. The dependencies the server recognizes: the group columns; a
//     table's columns once a PRIMARY / UNIQUE key of it is dependent (a nullable key column
//     only where a conjunct rejects its NULL); WHERE and inner-join equalities col = col
//     (both ways) and col = constant (a literal or an outer reference, not a parameter); an
//     outer join's ON equalities into its nullable side; and the dependencies inside a
//     derived table's, view's or CTE's body, seen through its output columns. ROLLUP allows
//     no dependency beyond the group expressions themselves. Error 1055 with GROUP BY, 1140
//     for an aggregated query without it (whose ORDER BY the server drops unchecked).
//   - The HAVING resolution rule the check relies on: a column named outside an aggregate
//     in HAVING must be a select-list column or alias, or a GROUP BY column (1054).
//   - Distinct_check: with DISTINCT, an ORDER BY expression not in the select list may
//     read only select-list columns (3065).
//   - An aggregate in the ORDER BY of a query that aggregates nowhere else (3029), or of a
//     set operation (3028).

// outerRef is a column reference a nested query resolved in an enclosing block.
type outerRef struct {
	block *relation // the block resolved into: the address of its first relation
	leaf  int       // index of the relation in that block
	col   string
	at    int    // offset of the reference in a.text
	where string // the clause the reference was typed in
}

// blockInfo is what the checks need of a typed SELECT block.
type blockInfo struct {
	sc         scope
	body       *mysqlast.Node // the PT_query_specification
	items      mysqlast.List  // the select items (PTI_expr_with_alias / Item_asterisk)
	explicit   bool           // GROUP BY
	aggregated bool           // GROUP BY, or an aggregate of this block in the select list or HAVING
	rollup     bool
	distinct   bool
	groupCols  []facts.ColRef   // the GROUP BY items that are columns of the block
	groupExprs []mysqlast.Value // every GROUP BY item, positions and aliases resolved to the select expression
	groupColAt []int            // parallel to groupExprs: index into groupCols of the column an item is, -1 otherwise
	ancestors  []*relation      // the enclosing blocks' identities
	fd         *fdSet           // the dependencies, once computed
}

// blockID is the identity a block is known by in outerRefs: its first relation's address.
func blockID(sc *scope) *relation {
	if sc == nil || len(sc.rels) == 0 {
		return nil
	}
	return &sc.rels[0]
}

// groupCheck records what the checks need of a block whose select list, GROUP BY and
// HAVING are typed, and runs the checks on the select list and HAVING (the ORDER BY's wait
// for the query expression: orderCheck).
func (a *analyzer) groupCheck(sc *scope, body *mysqlast.Node) error {
	info := &blockInfo{sc: *sc, body: body}
	info.items, _ = body.Arg("item_list").(mysqlast.List)
	info.distinct = strings.Contains(fmt.Sprint(body.Arg("options")), "SELECT_DISTINCT")
	for o := sc.outer; o != nil; o = o.outer {
		if id := blockID(o); id != nil {
			info.ancestors = append(info.ancestors, id)
		}
	}
	if g, ok := body.Arg("opt_group_clause").(*mysqlast.Node); ok {
		info.explicit = true
		olap := str(g.Arg("olap"))
		info.rollup = olap != "" && olap != "UNSPECIFIED_OLAP_TYPE"
		list, _ := g.Arg("group_list").(mysqlast.List)
		for _, it := range list {
			item := it
			if oe, ok := it.(*mysqlast.Node); ok && oe.Class == "PT_order_expr" {
				item = oe.Arg("item")
			}
			item = a.groupItem(sc, info.items, item)
			info.groupExprs = append(info.groupExprs, item)
			if col, ok := a.colFact(sc, item); ok {
				info.groupCols = append(info.groupCols, col)
				info.groupColAt = append(info.groupColAt, len(info.groupCols)-1)
			} else {
				info.groupColAt = append(info.groupColAt, -1)
			}
		}
	}
	having := havingExpr(body.Arg("opt_having_clause"))
	info.aggregated = info.explicit
	if !info.aggregated {
		for _, it := range info.items {
			if n, ok := it.(*mysqlast.Node); ok && n.Class == "PTI_expr_with_alias" && a.hasOwnAggregate(info, n.Arg("expr"), 0) {
				info.aggregated = true
			}
		}
		if having != nil && a.hasOwnAggregate(info, having, 0) {
			info.aggregated = true
		}
	}
	sc.info = info
	if sc.facts != nil {
		if a.blocks == nil {
			a.blocks = map[*facts.Scope]*blockInfo{}
		}
		a.blocks[sc.facts] = info
	}
	if having != nil {
		if err := a.havingResolution(info, having); err != nil {
			return err
		}
	}
	if !info.aggregated {
		return nil
	}
	fd := a.fdClosure(info, true, nil, nil, nil)
	info.fd = fd
	num := 1
	for _, it := range info.items {
		n, ok := it.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch n.Class {
		case "Item_asterisk":
			table := str(arg(n, "opt_table_name", 1))
			for i := range sc.rels {
				rel := &sc.rels[i]
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				for _, c := range rel.columns() {
					if !fd.isFD(facts.ColRef{Leaf: i, Column: c.Name}) {
						return a.groupError(info, num, "SELECT list", rel.alias+"."+c.Name, n.Start)
					}
					num++
				}
			}
		case "PTI_expr_with_alias":
			expr := n.Arg("expr")
			if !info.matchesGroup(expr) {
				if f := a.fdWalk(fd, expr, 0, false); f != nil {
					return a.groupError(info, num, "SELECT list", f.name, f.at)
				}
			}
			num++
		}
	}
	if having != nil && !info.matchesGroup(having) {
		if f := a.fdWalk(fd, having, 0, true); f != nil {
			return a.groupError(info, 1, "HAVING clause", f.name, f.at)
		}
	}
	// the windows: those of the select items, then the WINDOW clause's
	var windows []*mysqlast.Node
	for _, it := range info.items {
		windows = collectClass(it, "PT_window", windows)
	}
	if list, ok := body.Arg("opt_window_clause").(mysqlast.List); ok {
		for _, w := range list {
			if n, ok := w.(*mysqlast.Node); ok && n.Class == "PT_window" {
				windows = append(windows, n)
			}
		}
	}
	for _, w := range windows {
		for _, key := range []string{"partition_by", "order_by"} {
			list, _ := w.Arg(key).(mysqlast.List)
			for k, it := range list {
				item := it
				if oe, ok := it.(*mysqlast.Node); ok && oe.Class == "PT_order_expr" {
					item = oe.Arg("item")
				}
				if info.matchesGroup(item) {
					continue
				}
				if f := a.fdWalk(fd, item, 0, false); f != nil {
					return a.groupError(info, k+1, fmt.Sprintf("PARTITION BY or ORDER BY clause of window '%s'", windowName(w)), f.name, f.at)
				}
			}
		}
	}
	return nil
}

// orderCheck runs the checks on a block's ORDER BY: an aggregate where the query does not
// aggregate (3029), group invariance (1055; an aggregated query without GROUP BY has its
// ORDER BY dropped), and DISTINCT (3065).
func (a *analyzer) orderCheck(block *scope, order mysqlast.Value) error {
	info := block.info
	n, ok := order.(*mysqlast.Node)
	if info == nil || !ok {
		return nil
	}
	list, _ := n.Arg("order_list").(mysqlast.List)
	if len(list) == 0 {
		return nil
	}
	items := make([]mysqlast.Value, len(list))
	for i, it := range list {
		items[i] = it
		if oe, ok := it.(*mysqlast.Node); ok && oe.Class == "PT_order_expr" {
			items[i] = oe.Arg("item")
		}
	}
	if !info.aggregated {
		for i, item := range items {
			if a.hasOwnAggregate(info, item, 0) {
				return &Error{Message: fmt.Sprintf("Expression #%d of ORDER BY contains aggregate function and applies to the result of a non-aggregated query", i+1), Code: 3029, Position: a.ph.Back(nodeStart(item))}
			}
		}
	}
	if info.explicit {
		for i, item := range items {
			if a.inSelectList(info, item) || info.matchesGroup(item) {
				continue
			}
			if f := a.fdWalk(info.fd, item, 0, false); f != nil {
				return a.groupError(info, i+1, "ORDER BY clause", f.name, f.at)
			}
		}
	}
	if info.distinct {
		for i, item := range items {
			if a.inSelectList(info, item) {
				continue
			}
			if f := a.distinctWalk(info, item, 0); f != nil {
				return &Error{Message: fmt.Sprintf("Expression #%d of ORDER BY clause is not in SELECT list, references column '%s' which is not in SELECT list; this is incompatible with DISTINCT", i+1, f.name), Code: 3065, Position: a.ph.Back(f.at)}
			}
		}
	}
	return nil
}

func (a *analyzer) groupError(info *blockInfo, num int, place, col string, at int) error {
	if info.explicit {
		return &Error{Message: fmt.Sprintf("Expression #%d of %s is not in GROUP BY clause and contains nonaggregated column '%s' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by", num, place, col), Code: 1055, Position: a.ph.Back(at)}
	}
	return &Error{Message: fmt.Sprintf("In aggregated query without GROUP BY, expression #%d of %s contains nonaggregated column '%s'; this is incompatible with sql_mode=only_full_group_by", num, place, col), Code: 1140, Position: a.ph.Back(at)}
}

// havingExpr is the HAVING condition under its PTI_having wrapper.
func havingExpr(v mysqlast.Value) mysqlast.Value {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	if n.Class == "PTI_having" {
		return n.Arg("expr")
	}
	return n
}

// groupItem resolves a GROUP BY item the way the server does: a position or an alias
// (that no table column shadows) names a select expression; anything else is itself.
func (a *analyzer) groupItem(sc *scope, items mysqlast.List, item mysqlast.Value) mysqlast.Value {
	n, ok := item.(*mysqlast.Node)
	if !ok {
		return item
	}
	switch n.Class {
	case "Item_int", "Item_uint":
		i := intOr(n.Arg("i"), intOr(n.Arg("str"), 0)) - 1
		if i >= 0 && i < len(items) {
			if ewa, ok := items[i].(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" {
				return ewa.Arg("expr")
			}
		}
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		if _, isCol := a.colFact(sc, n); isCol {
			return item
		}
		name := str(n.Arg("ident"))
		for _, it := range items {
			if ewa, ok := it.(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" && strings.EqualFold(str(ewa.Arg("alias")), name) {
				return ewa.Arg("expr")
			}
		}
	}
	return item
}

// matchesGroup reports an expression equal to a GROUP BY expression (Item::eq: the same
// tree, names compared without case).
func (info *blockInfo) matchesGroup(v mysqlast.Value) bool {
	if !info.explicit {
		return false
	}
	text := mysqlast.Sprint(v)
	for _, g := range info.groupExprs {
		if strings.EqualFold(mysqlast.Sprint(g), text) {
			return true
		}
	}
	return false
}

// inSelectList reports an ORDER BY item the select list already has: a position, a name
// that is a select column's, or the same expression.
func (a *analyzer) inSelectList(info *blockInfo, item mysqlast.Value) bool {
	n, ok := item.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "Item_int", "Item_uint":
		return true // validated by orderBy
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		name := str(n.Arg("ident"))
		for _, c := range info.sc.items {
			if strings.EqualFold(c.Name, name) {
				return true
			}
		}
	}
	text := mysqlast.Sprint(n)
	for _, it := range info.items {
		if ewa, ok := it.(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" && strings.EqualFold(mysqlast.Sprint(ewa.Arg("expr")), text) {
			return true
		}
	}
	return false
}

// windowName is the name a window's error names it by.
func windowName(w *mysqlast.Node) string {
	if n, ok := w.Arg("name").(*mysqlast.Node); ok {
		if s := str(n.Arg("str")); s != "" {
			return s
		}
	}
	if s := str(w.Arg("name")); s != "" {
		return s
	}
	return "<unnamed window>"
}

// collectClass appends the nodes of the class in v, in order.
func collectClass(v mysqlast.Value, class string, out []*mysqlast.Node) []*mysqlast.Node {
	switch x := v.(type) {
	case *mysqlast.Node:
		if x.Class == class {
			return append(out, x)
		}
		for _, arg := range x.Args {
			out = collectClass(arg, class, out)
		}
	case mysqlast.List:
		for _, e := range x {
			out = collectClass(e, class, out)
		}
	}
	return out
}

// --- the walks ---

// fdFail is a column that is not group-invariant: where it is and how the error spells it.
type fdFail struct {
	at   int
	name string
}

// isColumnRef reports a bare column reference node.
func isColumnRef(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
		return true
	}
	return false
}

// localColumn resolves a column reference against the block's own relations only.
func (a *analyzer) localColumn(info *blockInfo, n *mysqlast.Node) (facts.ColRef, bool) {
	own := scope{rels: info.sc.rels}
	ref, err := a.column(own, n, "field list")
	if err != nil || ref.rel == nil {
		return facts.ColRef{}, false
	}
	for i := range info.sc.rels {
		if &info.sc.rels[i] == ref.rel || info.sc.rels[i].alias == ref.rel.alias {
			return facts.ColRef{Leaf: i, Column: ref.c.Name}, true
		}
	}
	return facts.ColRef{}, false
}

// outerRefTo finds the reference at offset at that a nested query resolved in the block.
func (a *analyzer) outerRefTo(info *blockInfo, at int) *outerRef {
	id := blockID(&info.sc)
	for i := range a.outerRefs {
		r := &a.outerRefs[i]
		if r.at == at && r.block == id {
			return r
		}
	}
	return nil
}

// outerRefAbove reports a reference at offset at resolved in a block enclosing this one.
func (a *analyzer) outerRefAbove(info *blockInfo, at int) bool {
	for i := range a.outerRefs {
		r := &a.outerRefs[i]
		if r.at != at {
			continue
		}
		for _, anc := range info.ancestors {
			if r.block == anc {
				return true
			}
		}
	}
	return false
}

// spell writes a column of the block the way the error names it: alias.column.
func (info *blockInfo) spell(c facts.ColRef) string {
	if c.Leaf >= 0 && c.Leaf < len(info.sc.rels) {
		return info.sc.rels[c.Leaf].alias + "." + c.Column
	}
	return c.Column
}

// isAggregateLike: an aggregate function of some block (not a window function), or
// GROUPING(), which is one for the check.
func isAggregateLike(n *mysqlast.Node) bool {
	return isAggregate(n) || n.Class == "Item_func_grouping"
}

// isAnyValue: ANY_VALUE(x), which the check does not look into.
func isAnyValue(n *mysqlast.Node) bool {
	return n.Class == "PTI_function_call_generic_ident_sys" && strings.EqualFold(str(n.Arg("ident")), "ANY_VALUE")
}

// aggregateOwner says which block an aggregate found sub levels below the block
// aggregates in: the block (own), one enclosing it (outer), or a nested one (inner). It
// is the innermost block a column of its arguments belongs to; an aggregate without
// columns belongs to the block it is written in.
type owner byte

const (
	ownInner owner = iota
	ownOwn
	ownOuter
)

func (a *analyzer) aggregateOwner(info *blockInfo, agg *mysqlast.Node, sub int) owner {
	if sub == 0 {
		return ownOwn
	}
	var cols []*mysqlast.Node
	for _, arg := range agg.Args {
		cols = collectColumns(arg, cols)
	}
	if len(cols) == 0 {
		return ownInner
	}
	own := false
	for _, c := range cols {
		switch {
		case a.outerRefTo(info, c.Start) != nil:
			own = true
		case a.outerRefAbove(info, c.Start):
		default:
			return ownInner
		}
	}
	if own {
		return ownOwn
	}
	return ownOuter
}

// collectColumns appends the column reference nodes in v.
func collectColumns(v mysqlast.Value, out []*mysqlast.Node) []*mysqlast.Node {
	switch x := v.(type) {
	case *mysqlast.Node:
		if isColumnRef(x) {
			return append(out, x)
		}
		for _, arg := range x.Args {
			out = collectColumns(arg, out)
		}
	case mysqlast.List:
		for _, e := range x {
			out = collectColumns(e, out)
		}
	}
	return out
}

// hasOwnAggregate reports an aggregate of the block in v (sub: how many query levels
// below the block v is).
func (a *analyzer) hasOwnAggregate(info *blockInfo, v mysqlast.Value, sub int) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		if isAggregateLike(x) {
			if a.aggregateOwner(info, x, sub) == ownOwn {
				return true
			}
		}
		if x.Class == "PT_subquery" {
			sub++
		}
		for _, arg := range x.Args {
			if a.hasOwnAggregate(info, arg, sub) {
				return true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if a.hasOwnAggregate(info, e, sub) {
				return true
			}
		}
	}
	return false
}

// fdWalk finds, in an expression of the block (sub levels below it), a column of the
// block that is not functionally dependent on the group columns: a column named directly,
// or through a nested query as an outer reference. An aggregate of the block (or of an
// enclosing one) is one value per group; a nested query's own aggregate is looked into for
// the block's columns. In HAVING an unqualified name that is a select alias is the
// validated select expression.
func (a *analyzer) fdWalk(fd *fdSet, v mysqlast.Value, sub int, having bool) *fdFail {
	info := fd.info
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if f := a.fdWalk(fd, e, sub, having); f != nil {
				return f
			}
		}
	case *mysqlast.Node:
		switch {
		case isAggregateLike(x):
			if a.aggregateOwner(info, x, sub) != ownInner {
				if fd.groupsKnown {
					return nil
				}
				return &fdFail{at: x.Start, name: a.textOf(x)}
			}
		case isAnyValue(x):
			return nil
		case x.Class == "PT_window":
			return nil // checked apart, with the window's place
		case isColumnRef(x):
			if sub == 0 {
				if having && (x.Class == "PTI_simple_ident_ident" || x.Class == "PTI_simple_ident_nospvar_ident") {
					if _, ok := a.lookupItem(info.sc, str(x.Arg("ident"))); ok {
						return nil
					}
				}
				col, ok := a.localColumn(info, x)
				if !ok {
					return nil
				}
				if !fd.isFD(col) {
					return &fdFail{at: x.Start, name: info.spell(col)}
				}
				return nil
			}
			if r := a.outerRefTo(info, x.Start); r != nil {
				col := facts.ColRef{Leaf: r.leaf, Column: r.col}
				if !fd.isFD(col) {
					return &fdFail{at: x.Start, name: info.spell(col)}
				}
			}
			return nil
		case x.Class == "PT_subquery":
			sub++
		}
		for _, arg := range x.Args {
			if f := a.fdWalk(fd, arg, sub, having); f != nil {
				return f
			}
		}
	}
	return nil
}

// havingResolution applies the server's rule for names in HAVING: outside an aggregate
// of the block, a column must be a select-list column or alias or a GROUP BY column; the
// tables are not consulted (1054). Through a nested query the same holds for a reference
// resolved in the block.
func (a *analyzer) havingResolution(info *blockInfo, v mysqlast.Value) error {
	return a.havingWalk(info, v, 0)
}

func (a *analyzer) havingWalk(info *blockInfo, v mysqlast.Value, sub int) error {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if err := a.havingWalk(info, e, sub); err != nil {
				return err
			}
		}
	case *mysqlast.Node:
		switch {
		case isAggregateLike(x):
			if a.aggregateOwner(info, x, sub) != ownInner {
				return nil // its arguments read the tables
			}
		case isColumnRef(x):
			if sub == 0 {
				if x.Class == "PTI_simple_ident_ident" || x.Class == "PTI_simple_ident_nospvar_ident" {
					if _, ok := a.lookupItem(info.sc, str(x.Arg("ident"))); ok {
						return nil
					}
				}
				col, ok := a.localColumn(info, x)
				if !ok || info.namesColumn(col) {
					return nil
				}
				return &Error{Message: fmt.Sprintf("Unknown column '%s' in 'having clause'", a.textOf(x)), Code: 1054, Position: a.ph.Back(x.Start)}
			}
			if r := a.outerRefTo(info, x.Start); r != nil {
				col := facts.ColRef{Leaf: r.leaf, Column: r.col}
				if !info.namesColumn(col) {
					return &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", info.spell(col), r.where), Code: 1054, Position: a.ph.Back(x.Start)}
				}
			}
			return nil
		case x.Class == "PT_subquery":
			sub++
		}
		for _, arg := range x.Args {
			if err := a.havingWalk(info, arg, sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// namesColumn reports a column the select list has as a plain column or the GROUP BY names.
func (info *blockInfo) namesColumn(col facts.ColRef) bool {
	if info.selectsColumn(col) {
		return true
	}
	for _, g := range info.groupCols {
		if g.Leaf == col.Leaf && strings.EqualFold(g.Column, col.Column) {
			return true
		}
	}
	return false
}

// selectsColumn reports a column the select list has as a plain column.
func (info *blockInfo) selectsColumn(col facts.ColRef) bool {
	for _, c := range info.sc.items {
		if c.leaf1 == col.Leaf+1 && strings.EqualFold(c.leafCol, col.Column) {
			return true
		}
	}
	return false
}

// distinctWalk finds, in an ORDER BY expression of a DISTINCT block, a column of the block
// the select list does not have (Distinct_check): aggregates and window functions are one
// value, and a nested query is looked into for outer references.
func (a *analyzer) distinctWalk(info *blockInfo, v mysqlast.Value, sub int) *fdFail {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if f := a.distinctWalk(info, e, sub); f != nil {
				return f
			}
		}
	case *mysqlast.Node:
		switch {
		case isAggregateLike(x), isWindowFunction(x):
			return nil
		case isColumnRef(x):
			var col facts.ColRef
			if sub == 0 {
				c, ok := a.localColumn(info, x)
				if !ok {
					return nil
				}
				col = c
			} else {
				r := a.outerRefTo(info, x.Start)
				if r == nil {
					return nil
				}
				col = facts.ColRef{Leaf: r.leaf, Column: r.col}
			}
			if !info.selectsColumn(col) {
				return &fdFail{at: x.Start, name: info.spell(col)}
			}
			return nil
		case x.Class == "PT_subquery":
			sub++
		}
		for _, arg := range x.Args {
			if f := a.distinctWalk(info, arg, sub); f != nil {
				return f
			}
		}
	}
	return nil
}

// isWindowFunction: a function with an OVER clause.
func isWindowFunction(n *mysqlast.Node) bool {
	for _, key := range []string{"w", "window"} {
		if w, ok := n.Arg(key).(*mysqlast.Node); ok && w.Class == "PT_window" {
			return true
		}
	}
	return false
}

// ndJoin reports an outer join's nullable side whose ON is not deterministic.
func (sc *scope) ndJoin(restrict []int) bool {
	for _, nd := range sc.ndJoins {
		if sameInts(nd, restrict) {
			return true
		}
	}
	return false
}

// simplified reports an outer join the WHERE turns into an inner join: it rejects NULL
// for a column of the nullable side (simplify_joins).
func (sc *scope) simplified(restrict []int) bool {
	for _, m := range sc.nnMarks {
		if m.restrict != nil {
			continue
		}
		for _, i := range restrict {
			if m.col.Leaf == i {
				return true
			}
		}
	}
	return false
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// deterministic reports an expression free of functions bound to the execution (RAND,
// NOW, UUID, user variables, ...).
func deterministic(v mysqlast.Value) bool {
	switch x := v.(type) {
	case *mysqlast.Node:
		if ndNode(x) {
			return false
		}
		for _, arg := range x.Args {
			if !deterministic(arg) {
				return false
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if !deterministic(e) {
				return false
			}
		}
	}
	return true
}

// ndNode: a node whose value is bound to the execution -- a parameter, a variable, a
// function of the clock, the session or chance.
func ndNode(x *mysqlast.Node) bool {
	switch x.Class {
	case "Item_param", "PTI_user_variable", "PTI_variable_aux_set_var", "PTI_variable_aux_ident_or_text", "PTI_variable_aux_3d",
		"PTI_get_system_variable", "Item_func_get_user_var", "Item_func_get_system_var", "Item_func_set_user_var":
		return true
	case "PTI_function_call_generic_ident_sys":
		return nondeterministic[strings.ToUpper(str(x.Arg("ident")))]
	}
	for _, prefix := range ndPrefixes {
		if strings.HasPrefix(x.Class, prefix) {
			return true
		}
	}
	return false
}

var ndPrefixes = []string{"Item_func_now", "Item_func_curdate", "Item_func_curtime", "Item_func_sysdate", "Item_func_rand", "Item_func_uuid",
	"Item_func_connection_id", "Item_func_user", "Item_func_current_user", "Item_func_current_role", "Item_func_database", "Item_func_version",
	"Item_func_found_rows", "Item_func_row_count", "Item_func_last_insert_id", "Item_func_sleep", "Item_func_benchmark", "Item_func_get_lock",
	"Item_func_release_lock", "Item_func_release_all_locks", "Item_func_is_free_lock", "Item_func_is_used_lock", "Item_func_unix_timestamp",
	"Item_func_random_bytes", "Item_func_sp", "Item_func_udf", "Item_func_master_pos_wait", "Item_func_source_pos_wait", "Item_func_wait_for_executed_gtid_set",
	"PTI_function_call_nonkeyword_now", "PTI_function_call_nonkeyword_sysdate", "PTI_function_call_nonkeyword_curdate", "PTI_function_call_nonkeyword_curtime",
	"PTI_function_call_nonkeyword_utc"}

// --- the dependencies ---

// fdSet is the closure of the functional dependencies of one block: the columns known
// from the group columns (or, for a derived table's body, from the outputs the enclosing
// block knows), the relations known whole, and whether every group expression is known
// (then the block's aggregates are one value).
type fdSet struct {
	info        *blockInfo
	known       map[facts.ColRef]bool
	nonNull     map[facts.ColRef]bool
	whole       []bool
	groupsKnown bool
	exprKnown   []string // group expressions known, as printed
}

func (fd *fdSet) isFD(c facts.ColRef) bool {
	return fd.known[c] || (c.Leaf >= 0 && c.Leaf < len(fd.whole) && fd.whole[c.Leaf])
}

// fdClosure computes the block's dependencies. For the block being checked (top) the
// source is its GROUP BY; for a derived table's body the source is what the enclosing
// block knows of its outputs: seeds (plain column outputs, those among them a conjunct
// makes non-NULL in seedNonNull) and seedExprs (expression outputs, as printed).
func (a *analyzer) fdClosure(info *blockInfo, top bool, seeds, seedNonNull []facts.ColRef, seedExprs []string) *fdSet {
	sc := &info.sc
	fs := sc.facts
	fd := &fdSet{info: info, known: map[facts.ColRef]bool{}, nonNull: map[facts.ColRef]bool{}, whole: make([]bool, len(sc.rels))}
	if top {
		for _, c := range info.groupCols {
			fd.known[c] = true
		}
		for _, g := range info.groupExprs {
			fd.exprKnown = append(fd.exprKnown, mysqlast.Sprint(g))
		}
		fd.groupsKnown = true
	} else {
		for _, c := range seeds {
			fd.known[c] = true
		}
		for _, c := range seedNonNull {
			fd.nonNull[c] = true
		}
		fd.exprKnown = seedExprs
	}
	if info.rollup || fs == nil {
		// ROLLUP: the group expressions and nothing derived from them
		if !top {
			fd.groupsKnown = fd.allGroupsKnown()
		}
		return fd
	}
	for _, m := range sc.nnMarks {
		// an ON marks its nullable side only, unless the WHERE makes the join inner
		if m.restrict == nil || allowed(m.col, m.restrict) || sc.simplified(m.restrict) {
			fd.nonNull[m.col] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, pr := range fs.Preds {
			if pr.Op != facts.Eq || (pr.Restricts != nil && sc.ndJoin(pr.Restricts)) {
				continue
			}
			switch pr.Term.Kind {
			case facts.Const:
				if !fd.known[pr.Col] {
					fd.known[pr.Col] = true
					changed = true
				}
			case facts.Known:
				if !fd.known[pr.Col] && a.fdConst[pr.Text+"\x00"+pr.Term.Text] {
					fd.known[pr.Col] = true
					changed = true
				}
			case facts.Column:
				// col = col: both ways in WHERE or an inner join (an outer join the WHERE
				// makes inner included), else only into the nullable side
				l, r := pr.Col, pr.Term.Col
				both := pr.Restricts == nil || sc.simplified(pr.Restricts)
				if (both || allowed(r, pr.Restricts)) && !fd.known[r] && fd.isFD(l) {
					fd.known[r] = true
					changed = true
				}
				if (both || allowed(l, pr.Restricts)) && !fd.known[l] && fd.isFD(r) {
					fd.known[l] = true
					changed = true
				}
			}
		}
		for i := range sc.rels {
			if fd.whole[i] {
				continue
			}
			if a.relWhole(fd, i) {
				fd.whole[i] = true
				changed = true
			}
		}
	}
	if !top {
		fd.groupsKnown = fd.allGroupsKnown()
	}
	return fd
}

// allGroupsKnown: every GROUP BY expression of the block is known -- a column that is, a
// constant, or an expression the source names.
func (fd *fdSet) allGroupsKnown() bool {
	if !fd.info.explicit {
		return false
	}
	for k, g := range fd.info.groupExprs {
		if !fd.groupExprKnown(fd.info, g, k) {
			return false
		}
	}
	return true
}

// groupExprKnown: the k-th GROUP BY expression of info is known.
func (fd *fdSet) groupExprKnown(info *blockInfo, g mysqlast.Value, k int) bool {
	if at := info.groupColAt[k]; at >= 0 {
		return fd.isFD(info.groupCols[at])
	}
	if constItem(g) {
		return true
	}
	return fd.exprKnownHas(mysqlast.Sprint(g))
}

// relWhole decides whether the known columns determine every column of the relation: a
// PRIMARY / UNIQUE key of a table is known (a nullable key column only where a conjunct
// rejects its NULL); a derived relation's body determines every output from what the block
// knows of its outputs.
func (a *analyzer) relWhole(fd *fdSet, i int) bool {
	rel := &fd.info.sc.rels[i]
	fs := fd.info.sc.facts
	if rel.table != nil {
		if i >= len(fs.Leaves) {
			return false
		}
		for _, key := range fs.Leaves[i].Keys {
			ok := len(key.Columns) > 0
			for _, name := range key.Columns {
				c := facts.ColRef{Leaf: i, Column: name}
				if !fd.isFD(c) {
					ok = false
					break
				}
				if col := rel.table.Column(name); col != nil && !col.NotNull && !fd.nonNull[c] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	if rel.body == nil {
		return false
	}
	body := a.blocks[rel.body]
	if body == nil {
		return false // a set operation: no dependency crosses it
	}
	// what the block knows of the outputs
	var seeds, seedNonNull []facts.ColRef
	var seedExprs []string
	anyNotNull := false
	for j, c := range rel.cols {
		out := facts.ColRef{Leaf: i, Column: c.Name}
		if !fd.known[out] {
			continue
		}
		if c.leaf1 > 0 {
			in := facts.ColRef{Leaf: c.leaf1 - 1, Column: c.leafCol}
			seeds = append(seeds, in)
			if rel.merged && fd.nonNull[out] {
				seedNonNull = append(seedNonNull, in) // the server sees the merged table's column itself
			}
		} else if e := body.itemExpr(j); e != nil {
			seedExprs = append(seedExprs, mysqlast.Sprint(e))
		}
		if !c.Nullable {
			anyNotNull = true
		}
	}
	if rel.nullable && !rel.merged && !anyNotNull && !fd.info.sc.simplified([]int{i}) {
		// a materialized table on the nullable side of an outer join: the dependency
		// propagates only from a non-nullable output (Group_check::non_null_in_source)
		return false
	}
	child := a.fdClosure(body, false, seeds, seedNonNull, seedExprs)
	if child.allOutputs() {
		return true
	}
	// the outputs known one by one
	for j, c := range rel.cols {
		out := facts.ColRef{Leaf: i, Column: c.Name}
		if !fd.known[out] && child.outputFD(a, body, j, c) {
			fd.known[out] = true
		}
	}
	return false
}

// allOutputs: the body is one row per known values -- every group expression known for a
// grouped body, every relation whole otherwise (a body without FROM is one row).
func (fd *fdSet) allOutputs() bool {
	if fd.info.explicit {
		return !fd.info.rollup && fd.groupsKnown
	}
	for _, w := range fd.whole {
		if !w {
			return false
		}
	}
	return true
}

// outputFD: output j of the body is known -- a plain column that is, a group expression
// that is, or an expression of known columns and aggregates.
func (fd *fdSet) outputFD(a *analyzer, body *blockInfo, j int, c Column) bool {
	if c.leaf1 > 0 {
		return fd.isFD(facts.ColRef{Leaf: c.leaf1 - 1, Column: c.leafCol})
	}
	e := body.itemExpr(j)
	if e == nil {
		return false
	}
	text := mysqlast.Sprint(e)
	for k, g := range body.groupExprs {
		if strings.EqualFold(mysqlast.Sprint(g), text) {
			return fd.groupExprKnown(body, g, k)
		}
	}
	if body.rollup {
		return false
	}
	return a.fdWalk(fd, e, 0, false) == nil
}

func (fd *fdSet) exprKnownHas(text string) bool {
	for _, k := range fd.exprKnown {
		if strings.EqualFold(k, text) {
			return true
		}
	}
	return false
}

// itemExpr is the expression of the body's j-th output column (nil for a column of a
// `*` expansion, which is a plain column anyway).
func (info *blockInfo) itemExpr(j int) mysqlast.Value {
	k := 0
	for _, it := range info.items {
		n, ok := it.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch n.Class {
		case "PTI_expr_with_alias":
			if k == j {
				return n.Arg("expr")
			}
			k++
		case "Item_asterisk":
			table := str(arg(n, "opt_table_name", 1))
			for i := range info.sc.rels {
				rel := &info.sc.rels[i]
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				k += len(rel.columns())
			}
			if k > j {
				return nil
			}
		}
	}
	return nil
}

// constItem reports an expression the server evaluates to a constant before the statement
// runs (Item::const_item): literals and deterministic functions of them, a subquery over
// no table; not a parameter, a user or system variable, a column, or a function bound to
// the execution (NOW, RAND, UUID, ...).
func constItem(v mysqlast.Value) bool {
	switch x := v.(type) {
	case nil, mysqlast.Token, mysqlast.Const, mysqlast.Op, mysqlast.Number, mysqlast.Flags, *mysqlast.Struct:
		return true
	case mysqlast.List:
		for _, e := range x {
			if !constItem(e) {
				return false
			}
		}
		return true
	case *mysqlast.Node:
		if ndNode(x) || isColumnRef(x) || isAggregateLike(x) || isWindowFunction(x) {
			return false
		}
		if x.Class == "PT_subquery" {
			qe, ok := x.Arg("query_expression").(*mysqlast.Node)
			if !ok {
				return false
			}
			body, ok := qe.Arg("body").(*mysqlast.Node)
			if !ok || body.Class != "PT_query_specification" {
				return false
			}
			from, _ := body.Arg("from_clause").(mysqlast.List)
			if len(from) > 0 || body.Arg("opt_where_clause") != nil || body.Arg("opt_having_clause") != nil || body.Arg("opt_group_clause") != nil {
				return false
			}
			return constItem(body.Arg("item_list"))
		}
		for _, arg := range x.Args {
			if !constItem(arg) {
				return false
			}
		}
		return true
	}
	return false
}

// nondeterministic are the generic-call functions whose value is bound to the execution.
var nondeterministic = map[string]bool{
	"UUID": true, "UUID_SHORT": true, "RANDOM_BYTES": true, "CONNECTION_ID": true, "VERSION": true, "DATABASE": true, "SCHEMA": true,
	"USER": true, "CURRENT_USER": true, "SESSION_USER": true, "SYSTEM_USER": true, "SLEEP": true, "GET_LOCK": true, "RELEASE_LOCK": true,
	"RELEASE_ALL_LOCKS": true, "IS_FREE_LOCK": true, "IS_USED_LOCK": true, "FOUND_ROWS": true, "ROW_COUNT": true, "LAST_INSERT_ID": true,
	"RAND": true, "NOW": true, "SYSDATE": true, "CURDATE": true, "CURTIME": true, "CURRENT_TIMESTAMP": true, "CURRENT_DATE": true, "CURRENT_TIME": true,
	"LOCALTIME": true, "LOCALTIMESTAMP": true, "UTC_DATE": true, "UTC_TIME": true, "UTC_TIMESTAMP": true, "UNIX_TIMESTAMP": true, "CURRENT_ROLE": true,
	"ICU_VERSION": true, "PS_CURRENT_THREAD_ID": true, "PS_THREAD_ID": true, "ROLES_GRAPHML": true, "BENCHMARK": true, "SOURCE_POS_WAIT": true,
	"MASTER_POS_WAIT": true, "WAIT_FOR_EXECUTED_GTID_SET": true, "STATEMENT_DIGEST": true, "STATEMENT_DIGEST_TEXT": true,
}
