package facts

// LiftThroughView translates the WHERE conjuncts of a view a leaf reads through (l.Body,
// the view's own defining query) onto the leaf's own columns at index at in the enclosing
// scope, via l.Outputs (the leaf's output columns that are plain columns of the body). A
// conjunct about a column the view computes (absent from Outputs) is left out: WITH CHECK
// OPTION cannot be satisfied through a computed column either, since it cannot be
// assigned. The lifted predicates carry Origin FromView.
//
// cascaded also lifts a nested view's own conjuncts (a view defined over another view),
// the way PostgreSQL's CASCADED check option and MySQL's default (plain WITH CHECK OPTION,
// or WITH CASCADED CHECK OPTION) follow the whole chain down to the base table; false
// (PostgreSQL's WITH LOCAL CHECK OPTION) stops after this level.
func LiftThroughView(l Leaf, at int, cascaded bool) []Pred {
	translate := func(col ColRef) (ColRef, bool) {
		for _, o := range l.Outputs {
			if o.Col == col {
				return ColRef{Leaf: at, Column: o.Name}, true
			}
		}
		return ColRef{}, false
	}
	return liftPreds(l.Body, translate, at, cascaded)
}

// liftPreds walks one level of a view's own body, translating its FromStatement
// conjuncts through translate (a view leaf's own numbering onto the enclosing statement's
// leaf at) and, when cascaded, recursing into a nested view's body one level further down
// by composing translate with the intermediate leaf's own Outputs.
func liftPreds(body *Scope, translate func(ColRef) (ColRef, bool), at int, cascaded bool) []Pred {
	if body == nil {
		return nil
	}
	var out []Pred
	for _, p := range body.Preds {
		if p.Origin != FromStatement {
			continue
		}
		col, ok := translate(p.Col)
		if !ok {
			continue
		}
		np := p
		np.Col = col
		if p.Term.Kind == Column {
			tc, ok2 := translate(p.Term.Col)
			if !ok2 {
				continue
			}
			np.Term.Col = tc
		}
		if len(p.Cols) > 0 {
			cols := make([]ColRef, 0, len(p.Cols))
			bad := false
			for _, c := range p.Cols {
				tc, ok3 := translate(c)
				if !ok3 {
					bad = true
					break
				}
				cols = append(cols, tc)
			}
			if bad {
				continue
			}
			np.Cols = cols
		}
		np.Restricts = []int{at}
		np.Origin = FromView
		out = append(out, np)
	}
	if cascaded && len(body.Leaves) == 1 && body.Leaves[0].Body != nil {
		nested := body.Leaves[0]
		nestedTranslate := func(r ColRef) (ColRef, bool) {
			for _, o := range nested.Outputs {
				if o.Col == r {
					return translate(ColRef{Leaf: 0, Column: o.Name})
				}
			}
			return ColRef{}, false
		}
		out = append(out, liftPreds(nested.Body, nestedTranslate, at, cascaded)...)
	}
	return out
}
