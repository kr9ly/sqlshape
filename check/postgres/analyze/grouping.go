package analyze

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// GROUP BY validity (parse_agg.c's check_ungrouped_columns): in a grouped query every
// output, HAVING and ORDER BY expression must be built from grouping expressions,
// aggregates and constants. A column that is not grouped is still fine when its table's
// primary key is entirely grouped (functional dependency). With GROUPING SETS / ROLLUP /
// CUBE the check runs against the union of every set's expressions, as PG does.

// checkGrouping reports an ungrouped column in a grouped SELECT level.
func (a *analyzer) checkGrouping(sel *pgparse.SelectStmt, sc *scope, cols []rteCol) *Error {
	if len(sel.GroupClause) == 0 && !sc.agg && sel.HavingClause == nil {
		return nil // HAVING alone makes the query grouped (one group)
	}
	var groups []*pgparse.Node
	for _, g := range groupingLeaves(sel.GroupClause) {
		resolved := a.groupExpr(g, sel, sc, cols)
		if w := windowIn(resolved); w != nil {
			return errAt(codeWindowingError, w.Location, "window functions are not allowed in GROUP BY")
		}
		groups = append(groups, resolved)
	}
	g := &grouping{a: a, p: &prover{a: a, sc: sc}, keys: map[string]bool{}, grouped: map[colKey]bool{}, usingKeys: map[string]bool{}}
	for _, n := range groups {
		if n != nil {
			if a.coercedUsingColumn(n, sc) {
				// GROUP BY f1 on a USING column coerced to another type groups by
				// t1.f1::bigint, which covers f1 but not the bare t1.f1
				g.usingKeys[n.GetColumnRef().Fields[0].GetString_().GetSval()] = true
				continue
			}
			g.keys[g.key(n)] = true
			if k, ok := g.p.resolve(n); ok {
				g.cols = append(g.cols, k)
				g.grouped[k] = true // by identity: GROUP BY t.c covers c and c covers t.c
			}
		}
	}
	for _, tn := range sel.TargetList {
		if err := g.check(tn.GetResTarget().GetVal()); err != nil {
			return err
		}
	}
	if err := g.check(sel.HavingClause); err != nil {
		return err
	}
	for _, s := range sel.SortClause {
		n := s.GetSortBy().GetNode()
		if a.outputRef(n, sel, cols) != nil {
			continue // an output-column alias or ordinal: the target was checked
		}
		if err := g.check(n); err != nil {
			return err
		}
	}
	return nil
}

// coercedUsingColumn reports whether n is an unqualified reference to a JOIN USING column
// whose merged type differs from the left input's column (the merged column is then a
// coercion of the input, not the input itself; flatten_join_alias_vars keeps them apart).
func (a *analyzer) coercedUsingColumn(n *pgparse.Node, sc *scope) bool {
	cr := n.GetColumnRef()
	if cr == nil || len(cr.Fields) != 1 || cr.Fields[0].GetString_() == nil {
		return false
	}
	name := cr.Fields[0].GetString_().GetSval()
	var coerced func(r *rte) bool
	coerced = func(r *rte) bool {
		if r == nil || r.join == nil {
			return false
		}
		for i, u := range r.join.using {
			if u == name && i < len(r.join.usingCols) {
				for _, lc := range r.join.left.find(name) {
					return lc.typ.OID != r.join.usingCols[i].typ.OID
				}
			}
		}
		return coerced(r.join.left) || coerced(r.join.right)
	}
	for _, it := range sc.items {
		if coerced(it) {
			return true
		}
	}
	return false
}

// groupExpr resolves a GROUP BY item to the expression it groups by: an ordinal or an
// output alias (when no input column has that name) stands for a target expression.
func (a *analyzer) groupExpr(n *pgparse.Node, sel *pgparse.SelectStmt, sc *scope, cols []rteCol) *pgparse.Node {
	if cr := n.GetColumnRef(); cr != nil && len(cr.Fields) == 1 {
		name := cr.Fields[0].GetString_().GetSval()
		a.probing = true
		_, err := a.resolveColumn(sc, "", name, -1)
		a.probing = false
		if err == nil {
			return n // input column wins over an output alias in GROUP BY
		}
	}
	if t := a.outputRef(n, sel, cols); t != nil {
		return t
	}
	return n
}

// outputRef returns the target expression an ORDER BY / GROUP BY item names by ordinal
// or output alias, nil otherwise.
func (a *analyzer) outputRef(n *pgparse.Node, sel *pgparse.SelectStmt, cols []rteCol) *pgparse.Node {
	if c := n.GetAConst(); c != nil {
		if v, ok := c.Val.(*pgparse.A_Const_Ival); ok {
			i := int(v.Ival.GetIval()) - 1
			if i >= 0 && i < len(sel.TargetList) {
				return sel.TargetList[i].GetResTarget().GetVal()
			}
		}
		return nil
	}
	if cr := n.GetColumnRef(); cr != nil && len(cr.Fields) == 1 {
		name := cr.Fields[0].GetString_().GetSval()
		for i, c := range cols {
			if c.name == name && i < len(sel.TargetList) {
				return sel.TargetList[i].GetResTarget().GetVal()
			}
		}
	}
	return nil
}

type grouping struct {
	a       *analyzer
	p       *prover
	keys    map[string]bool // deparsed grouping expressions
	grouped map[colKey]bool // grouped plain columns, by identity
	cols    []colKey        // grouped plain columns (for functional dependency)
	// usingKeys: unqualified names of grouped JOIN USING columns that are coercions of
	// their inputs (only the unqualified reference is covered)
	usingKeys map[string]bool
}

// check walks an expression and reports the first ungrouped column reference.
func (g *grouping) check(n *pgparse.Node) *Error {
	if n == nil {
		return nil
	}
	if g.keys[g.key(n)] {
		return nil
	}
	switch v := n.Node.(type) {
	case *pgparse.Node_AConst, *pgparse.Node_ParamRef, *pgparse.Node_SubLink, *pgparse.Node_SetToDefault:
		return nil
	case *pgparse.Node_ColumnRef:
		if len(v.ColumnRef.Fields) == 1 && g.usingKeys[v.ColumnRef.Fields[0].GetString_().GetSval()] {
			return nil
		}
		k, ok := g.p.resolve(n)
		if !ok {
			return nil // outer reference, whole-row or unresolved: not this level's problem
		}
		if g.grouped[k] || g.dependent(k) {
			return nil
		}
		return errAt(codeGroupingError, v.ColumnRef.Location, "column %q must appear in the GROUP BY clause or be used in an aggregate function", strings.Join(strs(v.ColumnRef.Fields), "."))
	case *pgparse.Node_FuncCall:
		if v.FuncCall.Over == nil && g.a.isAggregateName(strs(v.FuncCall.Funcname)) {
			if v.FuncCall.AggWithinGroup {
				// an ordered-set aggregate's direct arguments are evaluated once per group
				for _, d := range v.FuncCall.Args {
					if err := g.check(d); err != nil {
						return err
					}
				}
			}
			return nil // aggregate arguments see ungrouped rows
		}
	}
	for _, c := range children(n) {
		if err := g.check(c); err != nil {
			return err
		}
	}
	return nil
}

// dependent reports whether the column is functionally dependent on the grouped
// columns: its table's primary key is fully grouped.
func (g *grouping) dependent(k colKey) bool {
	if k.r.rel == nil {
		return false
	}
	for _, con := range k.r.rel.Constraints {
		if con.Kind != schema.PrimaryKey {
			continue
		}
		for _, name := range con.Columns {
			found := false
			for _, gk := range g.cols {
				if gk.r == k.r && k.r.cols[gk.i].name == name {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return false
}

// isAggregateName reports whether a function name denotes an aggregate in the catalog
// (by name: aggregates do not share names with plain functions in practice).
func (a *analyzer) isAggregateName(names []string) bool {
	name := names[len(names)-1]
	for _, f := range a.s.Catalog.Funcs {
		if f.Name == name && f.Kind == 'a' {
			return true
		}
	}
	for _, f := range a.s.Functions {
		if f.Name == name && f.IsAgg {
			return true
		}
	}
	return false
}

// children lists the direct child nodes of an expression node.
func children(n *pgparse.Node) []*pgparse.Node {
	var out []*pgparse.Node
	inner := n.ProtoReflect()
	inner.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		m := v.Message().Interface()
		m.ProtoReflect().Range(func(fd2 protoreflect.FieldDescriptor, v2 protoreflect.Value) bool {
			if fd2.Kind() != protoreflect.MessageKind {
				return true
			}
			if fd2.IsList() {
				l := v2.List()
				for i := 0; i < l.Len(); i++ {
					if c, ok := l.Get(i).Message().Interface().(*pgparse.Node); ok {
						out = append(out, c)
					}
				}
				return true
			}
			if c, ok := v2.Message().Interface().(*pgparse.Node); ok {
				out = append(out, c)
			}
			return true
		})
		return true
	})
	return out
}

// deparse renders an expression to SQL text for structural comparison.
func deparse(n *pgparse.Node) string {
	res := &pgparse.ParseResult{Stmts: []*pgparse.RawStmt{{Stmt: &pgparse.Node{Node: &pgparse.Node_SelectStmt{SelectStmt: &pgparse.SelectStmt{
		TargetList: []*pgparse.Node{{Node: &pgparse.Node_ResTarget{ResTarget: &pgparse.ResTarget{Val: n}}}},
	}}}}}}
	s, err := pgparse.Deparse(res)
	if err != nil {
		return ""
	}
	return s
}

// groupingLeaves flattens GROUPING SETS / ROLLUP / CUBE into the expressions they group by.
func groupingLeaves(items []*pgparse.Node) []*pgparse.Node {
	var out []*pgparse.Node
	for _, n := range items {
		if gs := n.GetGroupingSet(); gs != nil {
			out = append(out, groupingLeaves(gs.Content)...)
			continue
		}
		if l := n.GetList(); l != nil {
			out = append(out, groupingLeaves(l.Items)...)
			continue
		}
		if re := n.GetRowExpr(); re != nil && re.RowFormat == pgparse.CoercionForm_COERCE_IMPLICIT_CAST {
			out = append(out, groupingLeaves(re.Args)...)
			continue
		}
		out = append(out, n)
	}
	return out
}

// hasGroupingSets reports whether GROUP BY uses GROUPING SETS / ROLLUP / CUBE.
func hasGroupingSets(items []*pgparse.Node) bool {
	for _, n := range items {
		if n.GetGroupingSet() != nil {
			return true
		}
	}
	return false
}

// key renders an expression for grouping comparison with every column reference
// resolved, so GROUP BY t.a % 2 covers a % 2 and the other way round.
func (g *grouping) key(n *pgparse.Node) string {
	cp := proto.Clone(n).(*pgparse.Node)
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		if cr, ok := m.Interface().(*pgparse.ColumnRef); ok {
			if k, ok := g.p.resolve(&pgparse.Node{Node: &pgparse.Node_ColumnRef{ColumnRef: cr}}); ok {
				cr.Fields = []*pgparse.Node{
					{Node: &pgparse.Node_String_{String_: &pgparse.String{Sval: fmt.Sprintf("rte%p", k.r)}}},
					{Node: &pgparse.Node_String_{String_: &pgparse.String{Sval: fmt.Sprintf("c%d", k.i)}}},
				}
			}
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.IsList():
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					if fd.Kind() == protoreflect.MessageKind {
						walk(l.Get(i).Message())
					}
				}
			case fd.Kind() == protoreflect.MessageKind:
				walk(v.Message())
			}
			return true
		})
	}
	walk(cp.ProtoReflect())
	return deparse(cp)
}
