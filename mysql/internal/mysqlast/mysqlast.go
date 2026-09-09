// Package mysqlast folds the concrete syntax tree of package mysqlparse into an abstract
// one, following the server's own semantic actions.
//
// Every rule node of the CST knows which alternative it reduced, and parsegen recorded
// what the server's action for that alternative built (mysqlparse.Shape): a parse-tree
// class over some of the children, a list, a constant, or just one child passed up. This
// package replays those shapes generically, so the AST has the server's classes
// (PT_select_stmt, PTI_where, Item_func_eq ...) with the children in constructor-argument
// order, without a hand-written rule per construct.
//
// The actions parsegen could not read are the hand-written scope: hooks keyed by rule and
// alternative. A construct with no shape and no hook is reported as *Unsupported, so what
// the AST cannot yet express is explicit and measurable rather than silently wrong.
package mysqlast

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// Value is a node of the AST: *Node, List, Token, Const, Number, Flags, *Struct, or nil.
type Value any

// Node is a server parse-tree class built from its constructor arguments. Position
// arguments (@$) are dropped; the span is Start/End.
type Node struct {
	Class string
	Args  []Value
	// Names are the constructor's parameter names aligned with Args, when the server's
	// headers gave them (nil otherwise; hooks may set them).
	Names []string
	Start int
	End   int
	// Implicit marks a rule that had no action and more than one child: the node is named
	// after the rule and holds every child, which is bison's default only for the first.
	Implicit bool
}

// Arg returns the argument named name, or nil.
func (n *Node) Arg(name string) Value {
	for i, k := range n.Names {
		if k == name && i < len(n.Args) {
			return n.Args[i]
		}
	}
	return nil
}

// List is a server List<T> / Mem_root_array<T>: the elements in order.
type List []Value

// Token is a terminal: its kind, its text and, for identifiers and literals, the lexer's
// value (unquoted, unescaped).
type Token struct {
	Kind  mysqlparse.Kind
	Text  string
	Value string
	Start int
	End   int
}

// Const is a constant the action wrote: an enum value ("JTT_LEFT"), a literal
// ("true", "0"), or a helper expression this package does not interpret.
type Const string

// Op is a comparison operator the action passed as a creator function
// (`&comp_eq_creator`): its SQL spelling.
type Op string

// compOps maps the server's comparison creators to the operators they build
// (sql/item_cmpfunc.h: Eq_creator ... Ne_creator).
var compOps = map[string]Op{
	"&comp_eq_creator": "=", "&comp_equal_creator": "<=>", "&comp_ne_creator": "<>",
	"&comp_gt_creator": ">", "&comp_ge_creator": ">=", "&comp_lt_creator": "<", "&comp_le_creator": "<=",
}

// Number is a numeric token the action converted with my_strtoll10.
type Number int64

// Flags is a set of option constants or'ed together.
type Flags []Const

// Struct is a by-value struct the action filled field by field.
type Struct struct {
	Fields map[string]Value
	Order  []string
}

// Unsupported is a construct whose action parsegen could not read and no hook covers.
type Unsupported struct {
	Rule   string
	Alt    int
	Action string // the action as parsegen normalized it
	Start  int
	End    int
	Text   string
}

func (u *Unsupported) Error() string {
	return fmt.Sprintf("mysqlast: unsupported construct %s/%d at byte %d: %q", u.Rule, u.Alt, u.Start, u.Text)
}

// Hook builds the value of one alternative the shapes do not cover. kids are the values of
// the RHS children, in order; the hook may return nil for "no value".
type Hook func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error)

// Builder folds one statement.
type Builder struct {
	SQL   string
	hooks map[hookKey]Hook
}

type hookKey struct {
	rule string
	alt  int
}

// Build folds the CST of sql into an AST: the statement's node, without the grammar's
// start-rule wrapping and its END_OF_INPUT.
func Build(sql string, root *mysqlparse.Node) (Value, error) {
	b := &Builder{SQL: sql, hooks: hooks}
	v, err := b.Value(root)
	if err != nil {
		return nil, err
	}
	for {
		n, ok := v.(*Node)
		if !ok || !n.Implicit || len(n.Args) != 1 {
			return v, nil
		}
		v = n.Args[0]
	}
}

// Value folds one CST node.
func (b *Builder) Value(n *mysqlparse.Node) (Value, error) {
	if n.IsLeaf() {
		return Token{Kind: n.Kind, Text: n.Text(b.SQL), Value: n.Value, Start: n.Start, End: n.End}, nil
	}
	shape := n.Shape()
	kids := make([]Value, len(n.Children))
	for i, c := range n.Children {
		v, err := b.Value(c)
		if err != nil {
			return nil, err
		}
		kids[i] = v
	}
	child := func(a mysqlparse.Arg) (Value, error) {
		if a.Child == 0 {
			return b.constant(a.Text), nil
		}
		if a.Child > len(kids) && len(n.Children) == 0 && a.Child == 1 {
			return nil, nil // an empty alternative passing "$1": nothing
		}
		if a.Child > len(kids) {
			return nil, fmt.Errorf("mysqlast: %s/%d: argument $%d beyond %d children", n.Kind, n.Alt, a.Child, len(kids))
		}
		v := kids[a.Child-1]
		if a.Field != "" {
			return field(v, a.Field), nil
		}
		return v, nil
	}
	// a hand-written hook takes precedence over whatever parsegen read for the alternative
	if h := b.hooks[hookKey{n.Kind.String(), n.Alt}]; h != nil {
		return h(b, n, kids)
	}
	switch shape.Kind {
	case mysqlparse.ActDefault:
		// bison's default is $$ = $1; a rule without an action and several children keeps
		// them all, minus the end-of-input marker, which carries nothing
		var real, values []Value
		for _, k := range kids {
			if t, ok := k.(Token); ok {
				if t.Kind.String() == "END_OF_INPUT" {
					continue
				}
				if isKeyword(t.Kind) {
					real = append(real, k)
					continue
				}
			}
			real = append(real, k)
			values = append(values, k)
		}
		// `CREATE view_definition`: the keyword says nothing the child does not
		if len(values) > 0 && len(values) < len(real) {
			real = values
		}
		switch len(real) {
		case 0:
			return nil, nil
		case 1:
			return real[0], nil
		}
		return &Node{Class: n.Kind.String(), Args: real, Start: n.Start, End: n.End, Implicit: true}, nil
	case mysqlparse.ActEmpty:
		// `simple_statement: create { $$= nullptr; }`: the legacy statements build their LEX
		// in the child and hand up nothing; the AST keeps the child when it is a value
		if len(kids) == 1 {
			if _, isToken := kids[0].(Token); !isToken && kids[0] != nil {
				return kids[0], nil
			}
		}
		return nil, nil
	case mysqlparse.ActPass:
		return child(shape.Args[0])
	case mysqlparse.ActConst:
		return b.constant(shape.Const), nil
	case mysqlparse.ActNumber:
		v, err := child(shape.Args[0])
		if err != nil {
			return nil, err
		}
		if shape.Const == "16" {
			return numberBase(v, 16)
		}
		return number(v)
	case mysqlparse.ActFlags:
		var out Flags
		for _, a := range shape.Args {
			v, err := child(a)
			if err != nil {
				return nil, err
			}
			out = append(out, flags(v)...)
		}
		return out, nil
	case mysqlparse.ActNew:
		node := &Node{Class: shape.Class, Start: n.Start, End: n.End}
		for i, a := range shape.Args {
			if isPosition(a) {
				continue
			}
			v, err := child(a)
			if err != nil {
				return nil, err
			}
			node.Args = append(node.Args, v)
			if shape.Params != nil {
				name := ""
				if i < len(shape.Params) {
					name = shape.Params[i]
				}
				node.Names = append(node.Names, name)
			}
		}
		// flatten_associative_operator: `a AND b AND c` is one Item_cond_and over three
		if strings.HasPrefix(node.Class, "Item_cond_") && len(node.Args) == 2 {
			var args []Value
			for _, v := range node.Args {
				if c, ok := v.(*Node); ok && c.Class == node.Class {
					args = append(args, c.Args...)
				} else {
					args = append(args, v)
				}
			}
			node.Args = args
		}
		return node, nil
	case mysqlparse.ActListNew:
		var out List
		for _, a := range shape.Args {
			if isPosition(a) || a.Child == 0 && (a.Text == "YYMEM_ROOT" || a.Text == "YYTHD->mem_root") {
				continue
			}
			v, err := child(a)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case mysqlparse.ActListAppend:
		lst, err := child(shape.Args[0])
		if err != nil {
			return nil, err
		}
		elem, err := child(shape.Args[1])
		if err != nil {
			return nil, err
		}
		l, _ := lst.(List)
		return append(l, elem), nil
	case mysqlparse.ActStruct:
		st := &Struct{Fields: map[string]Value{}}
		for _, f := range shape.Fields {
			v, err := child(f.Arg)
			if err != nil {
				return nil, err
			}
			if f.Name == "$base" { // `$$= $N; $$.x= ...`: start from the child's struct
				if base, ok := v.(*Struct); ok {
					for _, k := range base.Order {
						st.Order = append(st.Order, k)
						st.Fields[k] = base.Fields[k]
					}
				} else if v != nil {
					st.Order = append(st.Order, "$base")
					st.Fields["$base"] = v
				}
				continue
			}
			if _, dup := st.Fields[f.Name]; !dup {
				st.Order = append(st.Order, f.Name)
			}
			st.Fields[f.Name] = v
		}
		return st, nil
	}
	return nil, &Unsupported{Rule: n.Kind.String(), Alt: n.Alt, Start: n.Start, End: n.End, Text: n.Text(b.SQL)}
}

// isKeyword reports a token that is a reserved or non-reserved word or punctuation, not
// an identifier, literal or placeholder.
func isKeyword(k mysqlparse.Kind) bool {
	switch k.String() {
	case "IDENT", "IDENT_QUOTED", "TEXT_STRING", "NCHAR_STRING", "NUM", "LONG_NUM", "ULONGLONG_NUM", "DECIMAL_NUM", "FLOAT_NUM",
		"HEX_NUM", "BIN_NUM", "LEX_HOSTNAME", "UNDERSCORE_CHARSET", "PARAM_MARKER", "DOLLAR_QUOTED_STRING_SYM":
		return false
	}
	return true
}

// constant interprets an argument the action wrote as text.
func (b *Builder) constant(text string) Value {
	switch text {
	case "nullptr", "NULL", "", "{}", "NULL_STR", "NULL_CSTR", "null_lex_str", "EMPTY_CSTR", "EMPTY_STR":
		return nil
	}
	if strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) && len(text) >= 2 {
		return Const(text[1 : len(text)-1])
	}
	if op, ok := compOps[text]; ok {
		return op
	}
	return Const(text)
}

// isPosition reports an argument that carries no meaning for the AST: a position, the
// thread, the memory arena.
func isPosition(a mysqlparse.Arg) bool {
	if a.Child != 0 {
		return false
	}
	switch a.Text {
	case "@$", "POS()", "YYTHD", "YYMEM_ROOT", "YYTHD->mem_root", "thd":
		return true
	}
	return strings.HasPrefix(a.Text, "@") && len(a.Text) > 1 && a.Text[1] >= '0' && a.Text[1] <= '9'
}

// field applies a `.str` / `.column_list` / `.flags.algo` access the action wrote on a child.
func field(v Value, f string) Value {
	f = strings.TrimLeft(f, ".->")
	if head, rest, ok := strings.Cut(f, "."); ok { // a path: one step at a time
		return field(field(v, head), rest)
	}
	switch x := v.(type) {
	case Token:
		switch f {
		case "str":
			if x.Value != "" || x.Kind.String() == "IDENT_QUOTED" || x.Kind.String() == "TEXT_STRING" {
				return x.Value
			}
			return x.Text
		case "length":
			return Number(len(x.Value))
		}
	case *Struct:
		return x.Fields[f] // a field the alternative did not set is unset (nil)
	case *Node:
		if f == "node" || f == "value" {
			return x
		}
	case List:
		return x // `&$1->value`: the row's item list is the list itself here
	}
	// a field access this package does not model: keep it visible
	return &Node{Class: "." + f, Args: []Value{v}}
}

func number(v Value) (Value, error) { return numberBase(v, 10) }

func numberBase(v Value, base int) (Value, error) {
	switch x := v.(type) {
	case Token:
		s := x.Value
		if s == "" {
			s = x.Text
		}
		n, err := strconv.ParseInt(s, base, 64)
		if err != nil {
			u, err2 := strconv.ParseUint(s, base, 64)
			if err2 != nil {
				return nil, fmt.Errorf("mysqlast: number %q: %v", s, err)
			}
			return Number(int64(u)), nil
		}
		return Number(n), nil
	case Number:
		return x, nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return nil, err
		}
		return Number(n), nil
	}
	return nil, fmt.Errorf("mysqlast: number from %T", v)
}

func flags(v Value) Flags {
	switch x := v.(type) {
	case Flags:
		return x
	case Const:
		return Flags{x}
	case nil:
		return nil
	}
	return Flags{Const(fmt.Sprint(v))}
}

// Sprint renders a value compactly for tests and debugging: Class(arg, ...), [a, b],
// tokens as their text, constants as written.
func Sprint(v Value) string {
	var b strings.Builder
	sprint(&b, v)
	return b.String()
}

func sprint(b *strings.Builder, v Value) {
	switch x := v.(type) {
	case nil:
		b.WriteString("nil")
	case *Node:
		b.WriteString(x.Class)
		b.WriteByte('(')
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			if i < len(x.Names) && x.Names[i] != "" {
				b.WriteString(x.Names[i])
				b.WriteByte('=')
			}
			sprint(b, a)
		}
		b.WriteByte(')')
	case List:
		b.WriteByte('[')
		for i, a := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			sprint(b, a)
		}
		b.WriteByte(']')
	case Token:
		if x.Value != "" && x.Value != x.Text {
			fmt.Fprintf(b, "%s=%q", x.Kind, x.Value)
		} else {
			b.WriteString(x.Text)
		}
	case Const:
		b.WriteString(string(x))
	case Op:
		b.WriteString(string(x))
	case Number:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case Flags:
		for i, f := range x {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(string(f))
		}
	case *Struct:
		b.WriteByte('{')
		for i, k := range x.Order {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteString(": ")
			sprint(b, x.Fields[k])
		}
		b.WriteByte('}')
	case string:
		fmt.Fprintf(b, "%q", x)
	default:
		fmt.Fprintf(b, "%v", x)
	}
}
