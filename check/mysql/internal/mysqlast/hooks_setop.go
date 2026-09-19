package mysqlast

import (
	"fmt"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
)

// Set operations and quantified comparisons: actions parsegen reads as calls it does not
// interpret (flatten_equal_set_ops, PTI_comp_op_all with a manual syntax error).
func init() {
	// query_expression_body: query_expression_body UNION_SYM union_option query_expression_body
	// -> {flatten_equal_set_ops<PT_union>(lhs.body, distinct, rhs.body, rhs.is_parenthesized), false}
	// (likewise EXCEPT / INTERSECT). flatten_equal_set_ops appends the right operand to the
	// left one's list when it is the same operation with the same DISTINCT / ALL
	// (X op Y op Z ==> op(X, Y, Z)); otherwise it makes a new node over the two.
	for _, op := range []struct{ sym, class string }{{"UNION_SYM", "PT_union"}, {"EXCEPT_SYM", "PT_except"}, {"INTERSECT_SYM", "PT_intersect"}} {
		class := op.class
		register("query_expression_body", "query_expression_body "+op.sym+" union_option query_expression_body", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			left, right := field(kids[0], "body"), field(kids[3], "body")
			distinct := Const("true")
			if s := fmt.Sprint(kids[2]); s == "0" {
				distinct = Const("false")
			}
			rightParens := field(kids[3], "is_parenthesized")
			var node *Node
			if l, ok := left.(*Node); ok && l.Class == class && l.Arg("is_distinct") == distinct {
				list, _ := l.Arg("list").(List)
				node = &Node{Class: class, Names: l.Names, Args: []Value{append(append(List{}, list...), right), distinct, rightParens}, Start: n.Start, End: n.End}
			} else {
				node = &Node{Class: class, Names: []string{"list", "is_distinct", "is_rhs_in_parentheses"}, Args: []Value{List{left, right}, distinct, rightParens}, Start: n.Start, End: n.End}
			}
			return &Struct{Fields: map[string]Value{"body": node, "is_parenthesized": Const("false")}, Order: []string{"body", "is_parenthesized"}}, nil
		})
	}
	// with_list: with_list ',' common_table_expr -> the list with the CTE appended; the
	// single-element form parsegen reads as a PT_with_list it builds, which comes out a List
	register("with_list", "with_list ',' common_table_expr", appendTo(1, 3))
	// bool_pri: bool_pri comp_op all_or_any table_subquery -> PTI_comp_op_all(left, comp_op, is_all, subquery);
	// the server rejects <=> here with a manual syntax error
	register("bool_pri", "bool_pri comp_op all_or_any table_subquery", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		if op, ok := kids[1].(Op); ok && op == "<=>" {
			return nil, &Unsupported{Rule: "bool_pri", Start: n.Start, End: n.End, Text: "<=> ALL / ANY is a syntax error"}
		}
		all := Const("false")
		if s := fmt.Sprint(kids[2]); s == "1" {
			all = Const("true")
		}
		return &Node{Class: "PTI_comp_op_all", Names: []string{"left", "comp_op", "is_all", "subselect"}, Args: []Value{kids[0], kids[1], all, kids[3]}, Start: n.Start, End: n.End}, nil
	})
}
