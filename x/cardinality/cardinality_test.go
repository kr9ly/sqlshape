package cardinality

import (
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

func TestAtMostOne(t *testing.T) {
	users := facts.Leaf{Table: "users", Alias: "u", Kind: facts.Table, Keys: []facts.Key{{Columns: []string{"id"}}, {Columns: []string{"email"}}}}
	orders := facts.Leaf{Table: "orders", Alias: "o", Kind: facts.Table, Keys: []facts.Key{{Columns: []string{"id"}}, {Columns: []string{"user_id", "seq"}}}}
	col := func(leaf int, c string) facts.ColRef { return facts.ColRef{Leaf: leaf, Column: c} }
	cases := []struct {
		name string
		f    *facts.Facts
		want bool
	}{
		{"nil", nil, false},
		{"producer proved", &facts.Facts{AtMostOne: true}, true},
		{"no from", &facts.Facts{Kind: facts.Select, Top: &facts.Scope{}}, true},
		{"pk fixed", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users}, Fixed: []facts.ColRef{col(0, "id")}}}, true},
		{"non-key fixed", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users}, Fixed: []facts.ColRef{col(0, "name")}}}, false},
		{"half of a composite key", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{orders}, Fixed: []facts.ColRef{col(0, "user_id")}}}, false},
		{"composite key", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{orders}, Fixed: []facts.ColRef{col(0, "user_id"), col(0, "seq")}}}, true},
		{"join carried by an edge", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users, orders}, Fixed: []facts.ColRef{col(1, "id")},
			Edges: []facts.Edge{{From: col(1, "user_id"), To: col(0, "id")}, {From: col(0, "id"), To: col(1, "user_id")}}}}, true},
		{"join not carried", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users, orders}, Fixed: []facts.ColRef{col(0, "id")},
			Edges: []facts.Edge{{From: col(1, "user_id"), To: col(0, "id")}, {From: col(0, "id"), To: col(1, "user_id")}}}}, false},
		{"derived not proved", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Alias: "d", Kind: facts.Derived}}}}, false},
		{"derived proved inside", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Alias: "d", Kind: facts.Derived,
			Body: &facts.Scope{Leaves: []facts.Leaf{users}, Fixed: []facts.ColRef{col(0, "email")}}}}}}, true},
		{"derived seeded by the outer level", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Alias: "d", Kind: facts.Derived,
			Outputs: []facts.Output{{Name: "mail", Col: col(0, "email")}}, Body: &facts.Scope{Leaves: []facts.Leaf{users}}}}, Fixed: []facts.ColRef{col(0, "mail")}}}, true},
		{"single level", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users}, Single: true}}, true},
		{"many level", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{users}, Fixed: []facts.ColRef{col(0, "id")}, Many: "UNION may combine rows"}}, false},
		{"group pinned", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{orders}, Fixed: []facts.ColRef{col(0, "user_id")}, Groups: []facts.Term{{Kind: facts.Column, Col: col(0, "user_id")}}}}, true},
		{"group not pinned", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{orders}, Fixed: []facts.ColRef{col(0, "note")}, Groups: []facts.Term{{Kind: facts.Column, Col: col(0, "user_id")}}}}, false},
		{"partial key repeated", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Table: "t", Alias: "t", Kind: facts.Table, Keys: []facts.Key{{Columns: []string{"uid"}, Where: []facts.Pred{{Op: facts.IsNull, Col: col(0, "deleted_at")}}}}}},
			Fixed: []facts.ColRef{col(0, "uid")}, Preds: []facts.Pred{{Op: facts.IsNull, Col: col(0, "deleted_at")}}}}, true},
		{"partial key not repeated", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Table: "t", Alias: "t", Kind: facts.Table, Keys: []facts.Key{{Columns: []string{"uid"}, Where: []facts.Pred{{Op: facts.IsNull, Col: col(0, "deleted_at")}}}}}},
			Fixed: []facts.ColRef{col(0, "uid")}}}, false},
		{"temporal key by a contained point", &facts.Facts{Top: &facts.Scope{Leaves: []facts.Leaf{{Table: "t", Alias: "t", Kind: facts.Table, Keys: []facts.Key{{Columns: []string{"id", "valid_at"}, Temporal: true}}}},
			Fixed: []facts.ColRef{col(0, "id")}, Preds: []facts.Pred{{Op: facts.Contains, Col: col(0, "valid_at"), Term: facts.Term{Kind: facts.Param, Param: 2}}}}}, true},
		{"insert reads its source", &facts.Facts{Kind: facts.Insert, Top: &facts.Scope{Leaves: []facts.Leaf{{Table: "orders", Alias: "orders", Kind: facts.Table, Role: facts.Target}}},
			Source: &facts.Scope{Leaves: []facts.Leaf{users}, Fixed: []facts.ColRef{col(0, "id")}}}, true},
	}
	for _, c := range cases {
		got, why := AtMostOne(c.f)
		if got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
		if !got && why == "" {
			t.Errorf("%s: no reason given", c.name)
		}
	}
}
