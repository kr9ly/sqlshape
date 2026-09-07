package vet

import (
	"github.com/kr9ly/sqlshape/internal/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// policyPins returns a row-level security policy of rel whose USING predicate fixes col by
// equality to something that is not a column (a session setting, a function of the
// current user): with row security on, the database pins the column for every statement,
// so -require-columns is satisfied by the policy. Nil when no policy does.
func policyPins(rel *schema.Relation, col string) *schema.Policy {
	if !rel.RowSecurity {
		return nil
	}
	for _, p := range rel.Policies {
		if !p.Permissive || (p.Command != "all" && p.Command != "select") || p.Using == nil {
			continue
		}
		for _, c := range andConjuncts(p.Using) {
			if pinsColumn(c, col) {
				return p
			}
		}
	}
	return nil
}

// andConjuncts splits a predicate on AND.
func andConjuncts(n *pg_query.Node) []*pg_query.Node {
	if b := n.GetBoolExpr(); b != nil && b.Boolop == pg_query.BoolExprType_AND_EXPR {
		var out []*pg_query.Node
		for _, a := range b.Args {
			out = append(out, andConjuncts(a)...)
		}
		return out
	}
	return []*pg_query.Node{n}
}

// pinsColumn: `col = expr` or `expr = col` where expr reads no column.
func pinsColumn(n *pg_query.Node, col string) bool {
	x := n.GetAExpr()
	if x == nil || x.Kind != pg_query.A_Expr_Kind_AEXPR_OP || len(x.Name) != 1 || x.Name[0].GetString_().GetSval() != "=" {
		return false
	}
	isCol := func(e *pg_query.Node) bool {
		cr := e.GetColumnRef()
		if cr == nil || len(cr.Fields) == 0 {
			return false
		}
		return cr.Fields[len(cr.Fields)-1].GetString_().GetSval() == col
	}
	noCol := func(e *pg_query.Node) bool {
		found := false
		schema.WalkNodes(e, func(m *pg_query.Node) {
			if m.GetColumnRef() != nil {
				found = true
			}
		})
		return !found
	}
	return (isCol(x.Lexpr) && noCol(x.Rexpr)) || (isCol(x.Rexpr) && noCol(x.Lexpr))
}
