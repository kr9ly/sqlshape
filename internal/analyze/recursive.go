package analyze

// checkRecursiveTerm is checkWellFormedRecursion for the recursive term of a WITH
// RECURSIVE query: the query name may appear exactly once, not inside a subquery, not on
// the nullable side of an outer join, not in EXCEPT / INTERSECT, and the term may not
// aggregate.

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (a *analyzer) checkRecursiveTerm(name string, term *pg_query.Node) *Error {
	w := &recursionWalker{a: a, name: name}
	if err := w.walk(term, false, false, ""); err != nil {
		return err
	}
	if w.refs == 0 {
		return nil // not actually recursive
	}
	if w.refs > 1 {
		return errAt(codeInvalidRecursion, w.loc, "recursive reference to query %q must not appear more than once", name)
	}
	if sel := term.GetSelectStmt(); sel != nil && sel.Op == pg_query.SetOperation_SETOP_NONE {
		for _, tn := range sel.TargetList {
			if agg := a.aggregateIn(tn.GetResTarget().GetVal()); agg != nil {
				return errAt(codeInvalidRecursion, agg.Location, "aggregate functions are not allowed in a recursive query's recursive term")
			}
		}
		if agg := a.aggregateIn(sel.HavingClause); agg != nil {
			return errAt(codeInvalidRecursion, agg.Location, "aggregate functions are not allowed in a recursive query's recursive term")
		}
	}
	return nil
}

type recursionWalker struct {
	a    *analyzer
	name string
	refs int
	loc  int32
}

// walk descends the parse tree; inSub / inOuter say the current position is inside a
// subquery / on the nullable side of an outer join, setop names the set operation whose
// forbidden arm we are in.
func (w *recursionWalker) walk(n *pg_query.Node, inSub, inOuter bool, setop string) *Error {
	if n == nil {
		return nil
	}
	switch v := n.Node.(type) {
	case *pg_query.Node_RangeVar:
		if v.RangeVar.Schemaname == "" && v.RangeVar.Relname == w.name {
			switch {
			case inSub:
				return errAt(codeInvalidRecursion, v.RangeVar.Location, "recursive reference to query %q must not appear within a subquery", w.name)
			case inOuter:
				return errAt(codeInvalidRecursion, v.RangeVar.Location, "recursive reference to query %q must not appear within an outer join", w.name)
			case setop != "":
				return errAt(codeInvalidRecursion, v.RangeVar.Location, "recursive reference to query %q must not appear within %s", w.name, setop)
			}
			w.refs++
			w.loc = v.RangeVar.Location
		}
		return nil
	case *pg_query.Node_SubLink, *pg_query.Node_RangeSubselect:
		for _, c := range children(n) {
			if err := w.walk(c, true, inOuter, setop); err != nil {
				return err
			}
		}
		return nil
	case *pg_query.Node_JoinExpr:
		j := v.JoinExpr
		lOuter := inOuter || j.Jointype == pg_query.JoinType_JOIN_RIGHT || j.Jointype == pg_query.JoinType_JOIN_FULL
		rOuter := inOuter || j.Jointype == pg_query.JoinType_JOIN_LEFT || j.Jointype == pg_query.JoinType_JOIN_FULL
		if err := w.walk(j.Larg, inSub, lOuter, setop); err != nil {
			return err
		}
		if err := w.walk(j.Rarg, inSub, rOuter, setop); err != nil {
			return err
		}
		return w.walk(j.Quals, true, inOuter, setop)
	case *pg_query.Node_SelectStmt:
		sel := v.SelectStmt
		if wc := sel.WithClause; wc != nil {
			// a nested WITH: its items are walked in the same context; one defining the
			// name hides the outer query from what can see it (a RECURSIVE WITH's names
			// are visible throughout, a plain WITH's to the items after it and the query)
			for i, cn := range wc.Ctes {
				c := cn.GetCommonTableExpr()
				if c.Ctename == w.name {
					if wc.Recursive {
						return nil
					}
					for _, prev := range wc.Ctes[:i+1] {
						if err := w.walk(prev.GetCommonTableExpr().Ctequery, inSub, inOuter, setop); err != nil {
							return err
						}
					}
					return nil
				}
			}
			for _, cn := range wc.Ctes {
				if err := w.walk(cn.GetCommonTableExpr().Ctequery, inSub, inOuter, setop); err != nil {
					return err
				}
			}
		}
		switch sel.Op {
		case pg_query.SetOperation_SETOP_EXCEPT:
			if err := w.walk(selNode(sel.Larg), inSub, inOuter, setop); err != nil {
				return err
			}
			return w.walk(selNode(sel.Rarg), inSub, inOuter, "EXCEPT")
		case pg_query.SetOperation_SETOP_INTERSECT:
			if err := w.walk(selNode(sel.Larg), inSub, inOuter, "INTERSECT"); err != nil {
				return err
			}
			return w.walk(selNode(sel.Rarg), inSub, inOuter, "INTERSECT")
		case pg_query.SetOperation_SETOP_UNION:
			if err := w.walk(selNode(sel.Larg), inSub, inOuter, setop); err != nil {
				return err
			}
			return w.walk(selNode(sel.Rarg), inSub, inOuter, setop)
		}
		for _, item := range sel.FromClause {
			if err := w.walk(item, inSub, inOuter, setop); err != nil {
				return err
			}
		}
		// anything outside FROM (target list, WHERE, ...) can only hold the name inside a subquery
		for _, c := range children(n) {
			if c.GetRangeVar() != nil || c.GetJoinExpr() != nil || c.GetRangeSubselect() != nil {
				continue
			}
			if err := w.walk(c, true, inOuter, setop); err != nil {
				return err
			}
		}
		return nil
	}
	for _, c := range allNodes(n) {
		if err := w.walk(c, inSub, inOuter, setop); err != nil {
			return err
		}
	}
	return nil
}

// allNodes is children() that also looks through message fields that are not Nodes
// themselves (WithClause, IntoClause, OnConflictClause, ...).
func allNodes(n *pg_query.Node) []*pg_query.Node {
	var out []*pg_query.Node
	var descend func(m protoreflect.Message)
	descend = func(m protoreflect.Message) {
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.Kind() != protoreflect.MessageKind {
				return true
			}
			visit := func(mv protoreflect.Message) {
				if c, ok := mv.Interface().(*pg_query.Node); ok {
					out = append(out, c)
					return
				}
				descend(mv)
			}
			if fd.IsList() {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					visit(l.Get(i).Message())
				}
				return true
			}
			visit(v.Message())
			return true
		})
	}
	// the Node's single oneof payload
	n.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind {
			descend(v.Message())
		}
		return true
	})
	return out
}

func selNode(sel *pg_query.SelectStmt) *pg_query.Node {
	if sel == nil {
		return nil
	}
	return &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: sel}}
}

// topLevelRef reports whether the recursive term names the CTE directly in its own FROM
// list (through joins, not inside a subquery or a nested WITH): where a SEARCH / CYCLE
// clause needs it (analyzeCTE).
func (w *recursionWalker) topLevelRef(sel *pg_query.SelectStmt) bool {
	if sel == nil {
		return false
	}
	if wc := sel.WithClause; wc != nil && !wc.Recursive {
		for _, cn := range wc.Ctes {
			if cn.GetCommonTableExpr().Ctename == w.name {
				return false
			}
		}
	}
	var inFrom func(n *pg_query.Node) bool
	inFrom = func(n *pg_query.Node) bool {
		if rv := n.GetRangeVar(); rv != nil {
			return rv.Schemaname == "" && rv.Relname == w.name
		}
		if j := n.GetJoinExpr(); j != nil {
			return inFrom(j.Larg) || inFrom(j.Rarg)
		}
		return false
	}
	for _, item := range sel.FromClause {
		if inFrom(item) {
			return true
		}
	}
	return false
}

// mentions reports whether the tree names the CTE anywhere.
func (w *recursionWalker) mentions(n *pg_query.Node) bool {
	if n == nil {
		return false
	}
	if rv := n.GetRangeVar(); rv != nil && rv.Schemaname == "" && rv.Relname == w.name {
		return true
	}
	if sel := n.GetSelectStmt(); sel != nil {
		if wc := sel.WithClause; wc != nil {
			// a nested WITH defining the name hides the outer query (see walk)
			for i, cn := range wc.Ctes {
				if cn.GetCommonTableExpr().Ctename == w.name {
					if wc.Recursive {
						return false
					}
					for _, prev := range wc.Ctes[:i+1] {
						if w.mentions(prev.GetCommonTableExpr().Ctequery) {
							return true
						}
					}
					return false
				}
			}
		}
		if w.mentions(selNode(sel.Larg)) || w.mentions(selNode(sel.Rarg)) {
			return true
		}
	}
	for _, c := range allNodes(n) {
		if w.mentions(c) {
			return true
		}
	}
	return false
}
