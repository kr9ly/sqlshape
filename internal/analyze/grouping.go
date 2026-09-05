package analyze

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// GROUP BY validity (parse_agg.c's check_ungrouped_columns): in a grouped query every
// output, HAVING and ORDER BY expression must be built from grouping expressions,
// aggregates and constants. A column that is not grouped is still fine when its table's
// primary key is entirely grouped (functional dependency). With GROUPING SETS / ROLLUP /
// CUBE the check runs against the union of every set's expressions, as PG does.

// checkGrouping reports an ungrouped column in a grouped SELECT level.
func (a *analyzer) checkGrouping(sel *pg_query.SelectStmt, sc *scope, cols []rteCol) *Error {
	if len(sel.GroupClause) == 0 && !sc.agg {
		return nil
	}
	var groups []*pg_query.Node
	for _, g := range groupingLeaves(sel.GroupClause) {
		groups = append(groups, a.groupExpr(g, sel, sc, cols))
	}
	g := &grouping{a: a, p: &prover{a: a, sc: sc}, keys: map[string]bool{}}
	for _, n := range groups {
		if n != nil {
			g.keys[deparse(n)] = true
			if k, ok := g.p.resolve(n); ok {
				g.cols = append(g.cols, k)
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

// groupExpr resolves a GROUP BY item to the expression it groups by: an ordinal or an
// output alias (when no input column has that name) stands for a target expression.
func (a *analyzer) groupExpr(n *pg_query.Node, sel *pg_query.SelectStmt, sc *scope, cols []rteCol) *pg_query.Node {
	if cr := n.GetColumnRef(); cr != nil && len(cr.Fields) == 1 {
		name := cr.Fields[0].GetString_().GetSval()
		if _, err := a.resolveColumn(sc, "", name, -1); err == nil {
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
func (a *analyzer) outputRef(n *pg_query.Node, sel *pg_query.SelectStmt, cols []rteCol) *pg_query.Node {
	if c := n.GetAConst(); c != nil {
		if v, ok := c.Val.(*pg_query.A_Const_Ival); ok {
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
	a    *analyzer
	p    *prover
	keys map[string]bool // deparsed grouping expressions
	cols []colKey        // grouped plain columns (for functional dependency)
}

// check walks an expression and reports the first ungrouped column reference.
func (g *grouping) check(n *pg_query.Node) *Error {
	if n == nil {
		return nil
	}
	if g.keys[deparse(n)] {
		return nil
	}
	switch v := n.Node.(type) {
	case *pg_query.Node_AConst, *pg_query.Node_ParamRef, *pg_query.Node_SubLink, *pg_query.Node_SetToDefault:
		return nil
	case *pg_query.Node_ColumnRef:
		k, ok := g.p.resolve(n)
		if !ok {
			return nil // outer reference, whole-row or unresolved: not this level's problem
		}
		if g.dependent(k) {
			return nil
		}
		return errAt(codeGroupingError, v.ColumnRef.Location, "column %q must appear in the GROUP BY clause or be used in an aggregate function", strings.Join(strs(v.ColumnRef.Fields), "."))
	case *pg_query.Node_FuncCall:
		if v.FuncCall.Over == nil && g.a.isAggregateName(strs(v.FuncCall.Funcname)) {
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
func children(n *pg_query.Node) []*pg_query.Node {
	var out []*pg_query.Node
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
					if c, ok := l.Get(i).Message().Interface().(*pg_query.Node); ok {
						out = append(out, c)
					}
				}
				return true
			}
			if c, ok := v2.Message().Interface().(*pg_query.Node); ok {
				out = append(out, c)
			}
			return true
		})
		return true
	})
	return out
}

// deparse renders an expression to SQL text for structural comparison.
func deparse(n *pg_query.Node) string {
	res := &pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: &pg_query.SelectStmt{
		TargetList: []*pg_query.Node{{Node: &pg_query.Node_ResTarget{ResTarget: &pg_query.ResTarget{Val: n}}}},
	}}}}}}
	s, err := pg_query.Deparse(res)
	if err != nil {
		return ""
	}
	return s
}

// groupingLeaves flattens GROUPING SETS / ROLLUP / CUBE into the expressions they group by.
func groupingLeaves(items []*pg_query.Node) []*pg_query.Node {
	var out []*pg_query.Node
	for _, n := range items {
		if gs := n.GetGroupingSet(); gs != nil {
			out = append(out, groupingLeaves(gs.Content)...)
			continue
		}
		if l := n.GetList(); l != nil {
			out = append(out, groupingLeaves(l.Items)...)
			continue
		}
		out = append(out, n)
	}
	return out
}

// hasGroupingSets reports whether GROUP BY uses GROUPING SETS / ROLLUP / CUBE.
func hasGroupingSets(items []*pg_query.Node) bool {
	for _, n := range items {
		if n.GetGroupingSet() != nil {
			return true
		}
	}
	return false
}
