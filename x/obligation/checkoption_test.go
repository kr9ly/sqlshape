package obligation

import (
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// fakeRel is the minimal Relation a handwritten Facts needs: a name and nothing else
// pinned() reads (no foreign keys, no row security).
type fakeRel string

func (r fakeRel) Name() string                                 { return string(r) }
func (r fakeRel) FullName() string                             { return string(r) }
func (r fakeRel) Kind() facts.RelKind                          { return facts.Table }
func (r fakeRel) HasColumn(col string) bool                    { return true }
func (r fakeRel) Directives() []string                         { return nil }
func (r fakeRel) ForeignKeys() []ForeignKey                    { return nil }
func (r fakeRel) ViewSource(col string) (string, string, bool) { return "", "", false }
func (r fakeRel) ForceRowSecurity() bool                       { return false }

type fakeSchema map[string]Relation

func (s fakeSchema) Relations() []Relation {
	var out []Relation
	for _, r := range s {
		out = append(out, r)
	}
	return out
}
func (s fakeSchema) Relation(name string) Relation { return s[name] }

// TestPinnedByView: a hand-built Facts.Write against table "t", with the target leaf's
// scope carrying an Eq(tenant_id, const) predicate of Origin FromView (as
// facts.LiftThroughView would produce from a WITH CHECK OPTION view's WHERE) discharges
// `require pinned(tenant_id)` as ByView, the same way a row-security policy's Eq discharges
// it as ByPolicy. Without that predicate (or with it there but of a different Origin /
// referencing another column via a Column term, which pins nothing absolute) the
// obligation fails.
func TestPinnedByView(t *testing.T) {
	s := fakeSchema{"t": fakeRel("t")}
	decls, ok, err := Parse("t", "require pinned(tenant_id)")
	if !ok || err != nil {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}

	build := func(preds []facts.Pred) *facts.Facts {
		return &facts.Facts{
			Kind: facts.Update,
			Top: &facts.Scope{
				Leaves: []facts.Leaf{{Table: "t", Alias: "t", Kind: facts.Table, Role: facts.Target}},
				Preds:  preds,
			},
			Writes: []facts.Write{{Table: "t", Kind: facts.Update, Assigned: []string{"v"}}},
		}
	}

	cases := []struct {
		name  string
		preds []facts.Pred
		want  Path
	}{
		{
			name: "FromView Eq const pins",
			preds: []facts.Pred{
				{Op: facts.Eq, Col: facts.ColRef{Leaf: 0, Column: "tenant_id"}, Term: facts.Term{Kind: facts.Const, Const: "1"}, Origin: facts.FromView},
			},
			want: ByView,
		},
		{
			name:  "no predicate at all",
			preds: nil,
			want:  0,
		},
		{
			name: "FromStatement (not FromView) is judged by the statement's own WHERE closure, not this loop",
			preds: []facts.Pred{
				{Op: facts.Eq, Col: facts.ColRef{Leaf: 0, Column: "tenant_id"}, Term: facts.Term{Kind: facts.Const, Const: "1"}, Origin: facts.FromStatement},
			},
			want: 0, // FromStatement equalities are read from sc.Fixed (via closeFixed), not sc.Preds; a
			// handwritten Facts with no Fixed entry does not pin, showing the FromView loop is not a
			// backdoor for every origin
		},
		{
			name: "FromView but a column term (a join equality, not an absolute value) pins nothing",
			preds: []facts.Pred{
				{Op: facts.Eq, Col: facts.ColRef{Leaf: 0, Column: "tenant_id"}, Term: facts.Term{Kind: facts.Column, Col: facts.ColRef{Leaf: 0, Column: "other"}}, Origin: facts.FromView},
			},
			want: 0,
		},
		{
			name: "FromView restricted to a different leaf does not apply here",
			preds: []facts.Pred{
				{Op: facts.Eq, Col: facts.ColRef{Leaf: 0, Column: "tenant_id"}, Term: facts.Term{Kind: facts.Const, Const: "1"}, Origin: facts.FromView, Restricts: []int{1}},
			},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := build(c.preds)
			var got Path
			for _, d := range Check(s, []Obligation{decls}, f, nil) {
				got = d.Path
			}
			if got != c.want {
				t.Errorf("path = %v, want %v", got, c.want)
			}
		})
	}
}
