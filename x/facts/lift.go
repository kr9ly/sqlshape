package facts

// LiftThroughView translates the WHERE conjuncts of a view a leaf reads through (l.Body,
// the view's own defining query) onto the leaf's own columns at index at in the enclosing
// scope, via l.Outputs (the leaf's output columns that are plain columns of the body). A
// conjunct about a column the view computes (absent from Outputs) is left out: WITH CHECK
// OPTION cannot be satisfied through a computed column either, since it cannot be
// assigned. The lifted predicates carry Origin FromView.
//
// Which levels of a view chain (a view defined over another view, possibly joined to
// other relations) are lifted follows both servers' own rule for WITH CHECK OPTION,
// measured on each: the written-through view's own WHERE always; an underlying view's
// WHERE when the view above it is CASCADED (l.CheckOption, or an intermediate view's own
// CascadedCheckOption once reached), and otherwise only when that underlying view carries
// a check option of its own -- LOCAL stops the cascade but does not switch off what a
// nested view enforces by itself. Every view leaf of a body is followed, not only a sole
// one: a CASCADED view over `v1 JOIN meta` still enforces v1's WHERE (measured, MySQL 8.4).
func LiftThroughView(l Leaf, at int) []Pred {
	translate := func(col ColRef) (ColRef, bool) {
		for _, o := range l.Outputs {
			if o.Col == col {
				return ColRef{Leaf: at, Column: o.Name}, true
			}
		}
		return ColRef{}, false
	}
	return liftPreds(l.Body, translate, at, l.CheckOption == CascadedCheckOption)
}

// liftPreds walks one level of a view's own body, translating its FromStatement
// conjuncts through translate (a view leaf's own numbering onto the enclosing statement's
// leaf at) and recursing into each nested view's body that the rule above reaches, by
// composing translate with the intermediate leaf's own Outputs. cascaded: a view above
// this body's leaves is CASCADED, so every nested view is followed.
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
	for j := range body.Leaves {
		nested := body.Leaves[j]
		if nested.Body == nil || !(cascaded || nested.CheckOption != NoCheckOption) {
			continue
		}
		nestedTranslate := func(r ColRef) (ColRef, bool) {
			for _, o := range nested.Outputs {
				if o.Col == r {
					return translate(ColRef{Leaf: j, Column: o.Name})
				}
			}
			return ColRef{}, false
		}
		out = append(out, liftPreds(nested.Body, nestedTranslate, at, cascaded || nested.CheckOption == CascadedCheckOption)...)
	}
	return out
}
