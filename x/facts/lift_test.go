package facts

import "testing"

// TestLiftThroughView_DirectColumn: a view's own WHERE conjunct (Eq on a plain column it
// projects) is translated onto the writing statement's leaf, renamed through Outputs, and
// marked Origin FromView, restricted to that leaf.
func TestLiftThroughView_DirectColumn(t *testing.T) {
	l := Leaf{
		Body: &Scope{
			Preds: []Pred{
				{Op: Eq, Col: ColRef{Leaf: 0, Column: "tenant_id"}, Term: Term{Kind: Const, Const: "1"}, Origin: FromStatement},
			},
			Leaves: []Leaf{{Kind: Table}},
		},
		Outputs: []Output{{Name: "tid", Col: ColRef{Leaf: 0, Column: "tenant_id"}}},
	}
	got := LiftThroughView(l, 3, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	p := got[0]
	if p.Col != (ColRef{Leaf: 3, Column: "tid"}) || p.Origin != FromView || len(p.Restricts) != 1 || p.Restricts[0] != 3 {
		t.Errorf("got %+v", p)
	}
}

// TestLiftThroughView_UnprojectedColumnSkipped: a conjunct about a column the view does
// not project has no Output to translate through and is left out rather than guessed at.
func TestLiftThroughView_UnprojectedColumnSkipped(t *testing.T) {
	l := Leaf{
		Body: &Scope{
			Preds: []Pred{
				{Op: Eq, Col: ColRef{Leaf: 0, Column: "tenant_id"}, Term: Term{Kind: Const, Const: "1"}, Origin: FromStatement},
			},
			Leaves: []Leaf{{Kind: Table}},
		},
		Outputs: []Output{{Name: "v", Col: ColRef{Leaf: 0, Column: "v"}}}, // tenant_id not projected
	}
	if got := LiftThroughView(l, 0, false); len(got) != 0 {
		t.Errorf("got %+v, want none: tenant_id is not a projected column", got)
	}
}

// TestLiftThroughView_NonStatementOriginSkipped: only the view's own WHERE (Origin
// FromStatement in its body) is lifted -- a predicate already carried in from further down
// (FromView, FromPolicy) is not re-lifted here (liftPreds' recursion walks the nested
// scope directly instead).
func TestLiftThroughView_NonStatementOriginSkipped(t *testing.T) {
	l := Leaf{
		Body: &Scope{
			Preds: []Pred{
				{Op: Eq, Col: ColRef{Leaf: 0, Column: "tenant_id"}, Term: Term{Kind: Const, Const: "1"}, Origin: FromPolicy},
			},
			Leaves: []Leaf{{Kind: Table}},
		},
		Outputs: []Output{{Name: "tenant_id", Col: ColRef{Leaf: 0, Column: "tenant_id"}}},
	}
	if got := LiftThroughView(l, 0, false); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// TestLiftThroughView_Cascaded: a view defined over another view lifts the nested view's
// own WHERE too when cascaded, composing the two levels' Outputs; false (LOCAL) stops
// after the outer view.
func TestLiftThroughView_Cascaded(t *testing.T) {
	// v2 (outer, the one written through) is "SELECT id, tenant_id FROM v1 WHERE v > 0";
	// v1 (nested) is "SELECT id, tenant_id, v FROM t WHERE tenant_id = 1". The nested
	// leaf's own Outputs map v2's own names for v1's columns (id, tenant_id) onto v1's
	// body (leaf 0 of v1's own scope, over t).
	nestedBody := &Scope{
		Preds:  []Pred{{Op: Eq, Col: ColRef{Leaf: 0, Column: "tenant_id"}, Term: Term{Kind: Const, Const: "1"}, Origin: FromStatement}},
		Leaves: []Leaf{{Kind: Table}}, // t
	}
	outerBody := &Scope{
		Preds: []Pred{{Op: Eq, Col: ColRef{Leaf: 0, Column: "v"}, Term: Term{Kind: Const, Const: "0"}, Origin: FromStatement}},
		Leaves: []Leaf{{
			Kind: View,
			Body: nestedBody,
			Outputs: []Output{
				{Name: "id", Col: ColRef{Leaf: 0, Column: "id"}},
				{Name: "tenant_id", Col: ColRef{Leaf: 0, Column: "tenant_id"}},
				{Name: "v", Col: ColRef{Leaf: 0, Column: "v"}},
			},
		}},
	}
	l := Leaf{
		Body:    outerBody,
		Outputs: []Output{{Name: "tid", Col: ColRef{Leaf: 0, Column: "tenant_id"}}}, // v2 exposes v1's tenant_id as tid
	}

	cascaded := LiftThroughView(l, 5, true)
	found := false
	for _, p := range cascaded {
		if p.Col == (ColRef{Leaf: 5, Column: "tid"}) && p.Term.Const == "1" {
			found = true
		}
	}
	if !found {
		t.Errorf("cascaded lift did not reach v1's WHERE tenant_id = 1 through v2's own tid: %+v", cascaded)
	}

	local := LiftThroughView(l, 5, false)
	for _, p := range local {
		if p.Term.Const == "1" {
			t.Errorf("LOCAL (cascaded=false) must not reach the nested view's WHERE: %+v", local)
		}
	}
}
