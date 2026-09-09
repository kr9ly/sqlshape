package mysqlast

import (
	"fmt"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// hooks are the alternatives whose server action parsegen could not read, written by
// hand against what sql_yacc.yy does. They are registered by rule name and right-hand
// side, so that a grammar version that reorders alternatives fails at init rather than
// binding a hook to the wrong one. `go run ./internal/parsegen/cmd/parsegen -actions
// -roots ...` lists the candidates.
var hooks = map[hookKey]Hook{}

// register adds a hook for the alternative of rule whose right-hand side is syms.
func register(rule, syms string, h Hook) {
	alt, ok := mysqlparse.Alternative(rule, syms)
	if !ok {
		panic(fmt.Sprintf("mysqlast: no alternative %s: %s in this grammar", rule, syms))
	}
	hooks[hookKey{rule, alt}] = h
}

// pass returns child i (1-based).
func pass(i int) Hook {
	return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) { return kids[i-1], nil }
}

// build returns a Node of class over the given children (1-based positions).
func build(class string, args ...int) Hook {
	return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		node := &Node{Class: class, Start: n.Start, End: n.End}
		for _, i := range args {
			node.Args = append(node.Args, kids[i-1])
		}
		return node, nil
	}
}

// appendTo returns child list (1-based) with child elem appended.
func appendTo(list, elem int) Hook {
	return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		l, _ := kids[list-1].(List)
		return append(l, kids[elem-1]), nil
	}
}

// listOf returns a List of the given children.
func listOf(elems ...int) Hook {
	return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		var l List
		for _, i := range elems {
			l = append(l, kids[i-1])
		}
		return l, nil
	}
}

// constant returns a fixed constant.
func constant(c string) Hook {
	return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) { return Const(c), nil }
}
