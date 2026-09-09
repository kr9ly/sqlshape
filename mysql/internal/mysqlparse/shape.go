package mysqlparse

// Shape is what the server's semantic action did for one alternative of a rule: which
// parse-tree class it built and which children fed which constructor argument. parsegen
// reads it out of sql_yacc.yy (shapes.go); it is the map from the CST to an AST.
type Shape struct {
	Kind   ActKind
	Class  string  // ActNew, ActListNew: the server's parse-tree class
	Const  string  // ActConst: the constant as written (an enum value, a literal)
	Args   []Arg   // ActNew: constructor arguments; ActPass: the child; ActListAppend: list, element; ActFlags: the two operands; ActNumber: the token
	Fields []Field // ActStruct: field assignments
}

// ActKind is the form of a semantic action.
type ActKind uint8

const (
	ActUnknown    ActKind = iota // not read: the hand-written scope
	ActDefault                   // no action; the value is the first child (keyword lists)
	ActEmpty                     // no value
	ActPass                      // the value is one child (or a field of it)
	ActConst                     // a constant
	ActNew                       // a parse-tree class built from the arguments
	ActListNew                   // a list holding the given child
	ActListAppend                // a list (first arg) with a child (second arg) appended
	ActFlags                     // two option sets or'ed
	ActNumber                    // a numeric token read as a number
	ActStruct                    // a by-value struct with the named fields
)

func (k ActKind) String() string {
	return [...]string{"unknown", "default", "empty", "pass", "const", "new", "list-new", "list-append", "flags", "number", "struct"}[k]
}

// Arg is one argument as the action wrote it: a child by 1-based RHS position, possibly a
// field of it (".str", ".column_list"), or a constant / expression as text.
type Arg struct {
	Child int
	Field string
	Text  string
}

// Field is one assignment of an ActStruct action.
type Field struct {
	Name string
	Arg  Arg
}

// Shape returns the action shape of a rule node; leaves have no shape.
func (n *Node) Shape() Shape {
	if n.IsLeaf() || int(n.Kind) >= len(shapes) || n.Alt >= len(shapes[n.Kind]) {
		return Shape{}
	}
	return shapes[n.Kind][n.Alt]
}
