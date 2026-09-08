package analyze

import (
	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/pgparse"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// AnalyzePolicy type-checks a row-level security policy's USING and WITH CHECK predicates
// the way CREATE POLICY does: an expression over the table's columns that yields boolean,
// with no aggregates, window functions or set-returning functions. Notes are the
// analyzer's own findings inside the predicates (domain mixing, and so on).
func AnalyzePolicy(s *schema.Schema, rel *schema.Relation, pol *schema.Policy) ([]Note, error) {
	var notes []Note
	for _, part := range []struct {
		what string
		expr *pgparse.Node
	}{{"USING", pol.Using}, {"WITH CHECK", pol.WithCheck}} {
		if part.expr == nil {
			continue
		}
		a := newAnalyzer(s, nil, nil)
		switch {
		case a.aggregateIn(part.expr) != nil:
			return notes, errAt(codeGroupingError, -1, "aggregate functions are not allowed in policy expressions")
		case windowIn(part.expr) != nil:
			return notes, errAt(codeWindowingError, -1, "window functions are not allowed in policy expressions")
		case a.srfIn(part.expr):
			return notes, errAt(codeFeatureNotSupported, -1, "set-returning functions are not allowed in policy expressions")
		}
		r, err := a.relationRTE(rel, nil, -1)
		if err != nil {
			return notes, err
		}
		sc := newScope(nil)
		sc.items = []*rte{r}
		e, err := a.analyzeExpr(part.expr, sc)
		if err != nil {
			return notes, err
		}
		if s.Types.BaseOf(e.typ).OID != catalog.Bool {
			return notes, errAt(codeDatatypeMismatch, -1, "argument of %s must be type boolean, not type %s", part.what, s.Types.Format(e.typ))
		}
		notes = append(notes, a.notes...)
	}
	return notes, nil
}

// SettingReads lists the current_setting(name, true) calls in a policy's predicates: a
// setting the session never set makes the predicate NULL, which hides every row without
// an error.
func SettingReads(pol *schema.Policy) []string {
	var out []string
	for _, e := range []*pgparse.Node{pol.Using, pol.WithCheck} {
		if e == nil {
			continue
		}
		schema.WalkNodes(e, func(n *pgparse.Node) {
			f := n.GetFuncCall()
			if f == nil {
				return
			}
			names := strs(f.Funcname)
			if names[len(names)-1] != "current_setting" || len(f.Args) != 2 {
				return
			}
			if c := f.Args[1].GetAConst(); c == nil || !c.GetBoolval().GetBoolval() {
				return
			}
			if c := f.Args[0].GetAConst(); c != nil {
				if sv, ok := c.Val.(*pgparse.A_Const_Sval); ok {
					out = append(out, sv.Sval.GetSval())
				}
			}
		})
	}
	return out
}
