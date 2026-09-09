package analyze

import (
	"github.com/kr9ly/sqlshape/mysql/internal/mysqlast"
	"github.com/kr9ly/sqlshape/mysql/internal/schema"
)

// typed is what the analyzer knows of an expression's value.
type typed struct {
	typ      schema.Type
	known    bool
	nullable bool
}

var unknown = typed{nullable: true}

func known(name string, nullable bool) typed {
	return typed{typ: schema.Type{Name: name, Length: -1, Dec: -1}, known: true, nullable: nullable}
}

func isParam(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	return ok && n.Class == "Item_param"
}

// setParam records the type context gives a placeholder.
func (a *analyzer) setParam(v mysqlast.Value, t schema.Type) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return
	}
	i := a.ph.number(n.Start) - 1
	if i < 0 || i >= len(a.params) {
		return
	}
	// the first context wins: the same $n in two places keeps its first type
	if !a.params[i].Known {
		a.params[i] = Param{Type: t, Known: true}
	}
}

// expr types an expression. The rules here are the skeleton — column references,
// literals, placeholders, comparisons and logic, COUNT — and every other construct is
// walked for its errors and parameters but left untyped; the type rules proper (the
// server's resolve_type facts, aggregate_type) come next.
func (a *analyzer) expr(sc scope, v mysqlast.Value, where string) (typed, error) {
	switch x := v.(type) {
	case nil:
		return unknown, nil
	case mysqlast.List:
		for _, e := range x {
			if _, err := a.expr(sc, e, where); err != nil {
				return unknown, err
			}
		}
		return unknown, nil
	case *mysqlast.Struct:
		for _, k := range x.Order {
			if _, err := a.expr(sc, x.Fields[k], where); err != nil {
				return unknown, err
			}
		}
		return unknown, nil
	case *mysqlast.Node:
		return a.node(sc, x, where)
	}
	return unknown, nil
}

func (a *analyzer) node(sc scope, n *mysqlast.Node, where string) (typed, error) {
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
		ref, err := a.column(sc, n, where)
		if err != nil {
			return unknown, err
		}
		return typed{typ: ref.col.Type, known: true, nullable: !ref.col.NotNull || ref.rel.nullable}, nil
	case "Item_param":
		return unknown, nil
	case "Item_int", "Item_uint":
		return known("bigint", false), nil
	case "Item_decimal":
		return known("decimal", false), nil
	case "Item_float":
		return known("double", false), nil
	case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset", "PTI_text_literal_concat":
		return known("varchar", false), nil
	case "Item_null":
		return typed{typ: schema.Type{Name: "null", Length: -1, Dec: -1}, known: true, nullable: true}, nil
	case "Item_func_true", "Item_func_false":
		return known("bigint", false), nil
	case "PTI_comp_op":
		left, err := a.expr(sc, n.Arg("left"), where)
		if err != nil {
			return unknown, err
		}
		right, err := a.expr(sc, n.Arg("right"), where)
		if err != nil {
			return unknown, err
		}
		// a placeholder compared with a typed operand takes its type
		if isParam(n.Arg("right")) && left.known {
			a.setParam(n.Arg("right"), left.typ)
		}
		if isParam(n.Arg("left")) && right.known {
			a.setParam(n.Arg("left"), right.typ)
		}
		return known("bigint", left.nullable || right.nullable), nil
	case "Item_func_in", "Item_func_between", "Item_func_like":
		// list-shaped comparisons: the first operand's type reaches the placeholders
		var first typed
		var err error
		args := n.Args
		if n.Class == "Item_func_in" {
			list, _ := n.Arg("list").(mysqlast.List)
			args = list
		}
		nullable := false
		for i, arg := range args {
			var t typed
			if i == 0 {
				first, err = a.expr(sc, arg, where)
				t = first
			} else {
				t, err = a.expr(sc, arg, where)
				if isParam(arg) && first.known {
					a.setParam(arg, first.typ)
				}
			}
			if err != nil {
				return unknown, err
			}
			nullable = nullable || t.nullable
		}
		return known("bigint", nullable), nil
	case "Item_cond_and", "Item_cond_or", "Item_func_xor", "PTI_truth_transform", "Item_func_isnull", "Item_func_isnotnull":
		nullable := false
		for _, arg := range n.Args {
			t, err := a.expr(sc, arg, where)
			if err != nil {
				return unknown, err
			}
			nullable = nullable || t.nullable
		}
		if n.Class == "Item_func_isnull" || n.Class == "Item_func_isnotnull" {
			nullable = false
		}
		return known("bigint", nullable), nil
	case "PTI_count_sym", "Item_sum_count":
		if _, err := a.expr(sc, n.Args, where); err != nil {
			return unknown, err
		}
		return known("bigint", false), nil
	}
	// anything else: walk it for its errors and parameters, and leave it untyped
	if _, err := a.expr(sc, mysqlast.List(n.Args), where); err != nil {
		return unknown, err
	}
	return unknown, nil
}
