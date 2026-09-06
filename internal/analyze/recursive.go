package analyze

// checkRecursiveTerm is checkWellFormedRecursion for the recursive term of a WITH
// RECURSIVE query: the query name may appear exactly once, not inside a subquery, not on
// the nullable side of an outer join, not in EXCEPT / INTERSECT, and the term may not
// aggregate.

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
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
	for _, c := range children(n) {
		if err := w.walk(c, inSub, inOuter, setop); err != nil {
			return err
		}
	}
	return nil
}

func selNode(sel *pg_query.SelectStmt) *pg_query.Node {
	if sel == nil {
		return nil
	}
	return &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: sel}}
}

// mentions reports whether the tree names the CTE anywhere.
func (w *recursionWalker) mentions(n *pg_query.Node) bool {
	if n == nil {
		return false
	}
	if rv := n.GetRangeVar(); rv != nil && rv.Schemaname == "" && rv.Relname == w.name {
		return true
	}
	for _, c := range children(n) {
		if w.mentions(c) {
			return true
		}
	}
	return false
}
