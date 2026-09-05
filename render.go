package sqlshape

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"text/template/parse"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kr9ly/sqlshape/internal/expand"
)

// Rendered is one concrete execution of a template: the SQL with $n placeholders
// and the argument values in placeholder order.
type Rendered struct {
	SQL  string
	Args []any
}

// Render evaluates the template against p. It mirrors the static expander
// exactly: the same value action yields the same $n within one rendering, and
// each range iteration gets fresh placeholders.
func (s Stmt[R, P]) Render(p P) (Rendered, error) {
	tree, err := s.tree()
	if err != nil {
		return Rendered{}, err
	}
	ev := &evaluator{vars: map[string]reflect.Value{}, keys: map[string]int{}}
	root := reflect.ValueOf(p)
	ev.vars["$"] = root
	if err := ev.list(tree.Root, root); err != nil {
		return Rendered{}, err
	}
	r := Rendered{SQL: ev.sql.String(), Args: ev.args}
	if err := s.checkAgainstExpansion(ev, r); err != nil {
		return Rendered{}, err
	}
	return r, nil
}

// checkAgainstExpansion refuses to run SQL the checker never saw. The static expander
// and this evaluator are two implementations of the template semantics; the branch
// signature of a rendering names the expansion the checker analyzed for it, and the SQL
// must be byte-identical (a range with more than two iterations has no static twin and
// is trusted by its two-iteration shape).
func (s Stmt[R, P]) checkAgainstExpansion(ev *evaluator, r Rendered) error {
	if ev.unchecked {
		return nil
	}
	exps, err := s.expansions()
	if err != nil {
		return err
	}
	if exps.Sparse {
		return nil // the checker saw a sparse set; no combination to compare against
	}
	sig := strings.TrimSpace(ev.branch.String())
	for i := range exps.Expansions {
		e := &exps.Expansions[i]
		if e.Branch != sig {
			continue
		}
		if e.SQL != r.SQL || len(e.Params) != len(r.Args) {
			return fmt.Errorf("sqlshape: rendered SQL differs from the checked expansion [%s]: the runtime evaluator and the checker disagree; please report this", sig)
		}
		return nil
	}
	return fmt.Errorf("sqlshape: rendering took a branch the checker never analyzed [%s]; please report this", sig)
}

var (
	expMu    sync.Mutex
	expCache = map[string]*expand.Result{}
)

func (s Stmt[R, P]) expansions() (*expand.Result, error) {
	expMu.Lock()
	defer expMu.Unlock()
	if r, ok := expCache[s.Template]; ok {
		return r, nil
	}
	r, err := expand.Expand(s.Template)
	if err != nil {
		return nil, fmt.Errorf("sqlshape: %w", err)
	}
	expCache[s.Template] = r
	return r, nil
}

var (
	treeMu    sync.Mutex
	treeCache = map[string]*parse.Tree{}
)

func (s Stmt[R, P]) tree() (*parse.Tree, error) {
	treeMu.Lock()
	defer treeMu.Unlock()
	if t, ok := treeCache[s.Template]; ok {
		return t, nil
	}
	trees, err := parse.Parse("q", s.Template, "{{", "}}", builtinNames)
	if err != nil {
		return nil, fmt.Errorf("sqlshape: %w", err)
	}
	treeCache[s.Template] = trees["q"]
	return trees["q"], nil
}

// builtinNames lets the parser accept the comparison / boolean helpers in conditions.
var builtinNames = map[string]any{
	"eq": stub, "ne": stub, "lt": stub, "le": stub, "gt": stub, "ge": stub,
	"and": stub, "or": stub, "not": stub, "len": stub, "index": stub,
}

type evaluator struct {
	sql  strings.Builder
	args []any
	vars map[string]reflect.Value
	keys map[string]int
	iter []int
	path []string // static path of dot, for placeholder dedupe keys
	// branch is the control-flow signature in the static expander's format; unchecked is
	// set when a range ran more than twice (no static expansion matches)
	branch    strings.Builder
	unchecked bool
}

func (ev *evaluator) list(l *parse.ListNode, dot reflect.Value) error {
	if l == nil {
		return nil
	}
	for _, n := range l.Nodes {
		if err := ev.node(n, dot); err != nil {
			return err
		}
	}
	return nil
}

func (ev *evaluator) node(n parse.Node, dot reflect.Value) error {
	switch v := n.(type) {
	case *parse.TextNode:
		ev.sql.Write(v.Text)
	case *parse.ActionNode:
		val, path, err := ev.valueAction(v.Pipe, dot)
		if err != nil {
			return err
		}
		if len(v.Pipe.Decl) > 0 {
			ev.vars[v.Pipe.Decl[0].Ident[0]] = val
			return nil
		}
		ev.param(path, val)
	case *parse.IfNode:
		cond, err := ev.pipe(v.Pipe, dot)
		if err != nil {
			return err
		}
		if isTrue(cond) {
			fmt.Fprintf(&ev.branch, " if@%d:then", v.Pos)
			return ev.list(v.List, dot)
		}
		fmt.Fprintf(&ev.branch, " if@%d:else", v.Pos)
		return ev.list(v.ElseList, dot)
	case *parse.WithNode:
		val, path, err := ev.valueAction(v.Pipe, dot)
		if err != nil {
			return err
		}
		if isTrue(val) {
			fmt.Fprintf(&ev.branch, " with@%d:then", v.Pos)
			saved := ev.path
			ev.path = path
			err := ev.list(v.List, val)
			ev.path = saved
			return err
		}
		fmt.Fprintf(&ev.branch, " with@%d:else", v.Pos)
		return ev.list(v.ElseList, dot)
	case *parse.RangeNode:
		return ev.rangeNode(v, dot)
	case *parse.CommentNode:
	default:
		return fmt.Errorf("sqlshape: unsupported template node %T", n)
	}
	return nil
}

func (ev *evaluator) rangeNode(r *parse.RangeNode, dot reflect.Value) error {
	val, path, err := ev.valueAction(r.Pipe, dot)
	if err != nil {
		return err
	}
	val = indirect(val)
	n := 0
	switch val.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		n = val.Len()
	case reflect.Invalid:
	default:
		return fmt.Errorf("sqlshape: range over %s", val.Type())
	}
	if n > 2 {
		ev.unchecked = true
	}
	fmt.Fprintf(&ev.branch, " range@%d:x%d", r.Pos, n)
	if n == 0 {
		return ev.list(r.ElseList, dot)
	}
	elemPath := append(append([]string{}, path...), "[]")
	var keys []reflect.Value
	if val.Kind() == reflect.Map {
		keys = val.MapKeys()
	}
	for i := 0; i < n; i++ {
		var elem, key reflect.Value
		if val.Kind() == reflect.Map {
			key, elem = keys[i], val.MapIndex(keys[i])
		} else {
			key, elem = reflect.ValueOf(i), val.Index(i)
		}
		switch len(r.Pipe.Decl) {
		case 1:
			ev.vars[r.Pipe.Decl[0].Ident[0]] = elem
		case 2:
			ev.vars[r.Pipe.Decl[0].Ident[0]] = key
			ev.vars[r.Pipe.Decl[1].Ident[0]] = elem
		}
		saved := ev.path
		ev.path = elemPath
		ev.iter = append(ev.iter, i)
		err := ev.list(r.List, elem)
		ev.iter = ev.iter[:len(ev.iter)-1]
		ev.path = saved
		if err != nil {
			return err
		}
	}
	return nil
}

// param appends a placeholder for val, reusing the number for the same origin
// within the same range iteration (as the static expander does).
func (ev *evaluator) param(path []string, val reflect.Value) {
	key := strings.Join(path, "/") + fmt.Sprint(ev.iter)
	n, ok := ev.keys[key]
	if !ok {
		n = len(ev.args) + 1
		ev.keys[key] = n
		ev.args = append(ev.args, normalizeArg(val))
	}
	ev.sql.WriteString("$" + strconv.Itoa(n))
}

// valueAction evaluates a pipeline that must be a plain field reference and returns
// the value with its static path (for dedupe keys).
func (ev *evaluator) valueAction(pipe *parse.PipeNode, dot reflect.Value) (reflect.Value, []string, error) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return reflect.Value{}, nil, fmt.Errorf("sqlshape: value actions must be a plain field reference")
	}
	switch a := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		v, err := fieldChain(dot, a.Ident)
		return v, append(append([]string{}, ev.path...), a.Ident...), err
	case *parse.DotNode:
		return dot, ev.path, nil
	case *parse.VariableNode:
		base, ok := ev.vars[a.Ident[0]]
		if !ok {
			return reflect.Value{}, nil, fmt.Errorf("sqlshape: undefined variable %s", a.Ident[0])
		}
		v, err := fieldChain(base, a.Ident[1:])
		return v, append([]string{a.Ident[0]}, a.Ident[1:]...), err
	}
	return reflect.Value{}, nil, fmt.Errorf("sqlshape: value actions must be a plain field reference")
}

// pipe evaluates a condition pipeline: field chains, constants, variables and the builtins.
func (ev *evaluator) pipe(pipe *parse.PipeNode, dot reflect.Value) (reflect.Value, error) {
	var prev reflect.Value
	havePrev := false
	for _, cmd := range pipe.Cmds {
		args := cmd.Args
		var fn string
		if id, ok := args[0].(*parse.IdentifierNode); ok {
			fn = id.Ident
			args = args[1:]
		}
		vals := make([]reflect.Value, 0, len(args)+1)
		for _, a := range args {
			v, err := ev.arg(a, dot)
			if err != nil {
				return reflect.Value{}, err
			}
			vals = append(vals, v)
		}
		if havePrev {
			vals = append(vals, prev)
		}
		if fn == "" {
			if len(vals) != 1 {
				return reflect.Value{}, fmt.Errorf("sqlshape: bad pipeline")
			}
			prev, havePrev = vals[0], true
			continue
		}
		v, err := callBuiltin(fn, vals)
		if err != nil {
			return reflect.Value{}, err
		}
		prev, havePrev = v, true
	}
	return prev, nil
}

func (ev *evaluator) arg(n parse.Node, dot reflect.Value) (reflect.Value, error) {
	switch a := n.(type) {
	case *parse.FieldNode:
		return fieldChain(dot, a.Ident)
	case *parse.DotNode:
		return dot, nil
	case *parse.VariableNode:
		base, ok := ev.vars[a.Ident[0]]
		if !ok {
			return reflect.Value{}, fmt.Errorf("sqlshape: undefined variable %s", a.Ident[0])
		}
		return fieldChain(base, a.Ident[1:])
	case *parse.StringNode:
		return reflect.ValueOf(a.Text), nil
	case *parse.NumberNode:
		if a.IsInt {
			return reflect.ValueOf(a.Int64), nil
		}
		return reflect.ValueOf(a.Float64), nil
	case *parse.BoolNode:
		return reflect.ValueOf(a.True), nil
	case *parse.NilNode:
		return reflect.Value{}, nil
	case *parse.PipeNode:
		return ev.pipe(a, dot)
	}
	return reflect.Value{}, fmt.Errorf("sqlshape: unsupported expression %T in condition", n)
}

func fieldChain(v reflect.Value, idents []string) (reflect.Value, error) {
	for _, name := range idents {
		v = indirect(v)
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("sqlshape: .%s on non-struct %s", name, v.Type())
		}
		f := v.FieldByName(name)
		if !f.IsValid() {
			return reflect.Value{}, fmt.Errorf("sqlshape: %s has no field %s", v.Type(), name)
		}
		v = f
	}
	return v, nil
}

func indirect(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// isTrue follows text/template: zero values, nil, empty collections are false.
func isTrue(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() > 0
	case reflect.Bool:
		return v.Bool()
	case reflect.Complex64, reflect.Complex128:
		return v.Complex() != 0
	case reflect.Chan, reflect.Func, reflect.Pointer, reflect.Interface:
		return !v.IsNil()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() != 0
	case reflect.Struct:
		return true
	}
	return false
}

func callBuiltin(name string, args []reflect.Value) (reflect.Value, error) {
	switch name {
	case "not":
		return reflect.ValueOf(!isTrue(arg0(args))), nil
	case "and":
		for _, a := range args {
			if !isTrue(a) {
				return a, nil
			}
		}
		return arg0(args), nil
	case "or":
		for _, a := range args {
			if isTrue(a) {
				return a, nil
			}
		}
		return arg0(args), nil
	case "len":
		v := indirect(arg0(args))
		if !v.IsValid() {
			return reflect.ValueOf(0), nil
		}
		return reflect.ValueOf(v.Len()), nil
	case "eq":
		if len(args) < 2 {
			return reflect.Value{}, fmt.Errorf("sqlshape: eq needs two arguments")
		}
		for _, b := range args[1:] {
			if equal(args[0], b) {
				return reflect.ValueOf(true), nil
			}
		}
		return reflect.ValueOf(false), nil
	case "ne":
		if len(args) != 2 {
			return reflect.Value{}, fmt.Errorf("sqlshape: ne needs two arguments")
		}
		return reflect.ValueOf(!equal(args[0], args[1])), nil
	case "lt", "le", "gt", "ge":
		if len(args) != 2 {
			return reflect.Value{}, fmt.Errorf("sqlshape: %s needs two arguments", name)
		}
		c, err := compare(args[0], args[1])
		if err != nil {
			return reflect.Value{}, err
		}
		switch name {
		case "lt":
			return reflect.ValueOf(c < 0), nil
		case "le":
			return reflect.ValueOf(c <= 0), nil
		case "gt":
			return reflect.ValueOf(c > 0), nil
		default:
			return reflect.ValueOf(c >= 0), nil
		}
	case "index":
		v := indirect(arg0(args))
		for _, i := range args[1:] {
			switch v.Kind() {
			case reflect.Slice, reflect.Array, reflect.String:
				v = v.Index(int(indirect(i).Int()))
			case reflect.Map:
				v = v.MapIndex(i)
			default:
				return reflect.Value{}, fmt.Errorf("sqlshape: cannot index %s", v.Type())
			}
		}
		return v, nil
	}
	return reflect.Value{}, fmt.Errorf("sqlshape: unsupported function %q in template", name)
}

func arg0(args []reflect.Value) reflect.Value {
	if len(args) == 0 {
		return reflect.Value{}
	}
	return args[0]
}

func equal(a, b reflect.Value) bool {
	a, b = indirect(a), indirect(b)
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	switch {
	case isIntKind(a) && isIntKind(b):
		return a.Int() == b.Int()
	case isUintKind(a) && isUintKind(b):
		return a.Uint() == b.Uint()
	case isIntKind(a) && isUintKind(b):
		return a.Int() >= 0 && uint64(a.Int()) == b.Uint()
	case isUintKind(a) && isIntKind(b):
		return b.Int() >= 0 && uint64(b.Int()) == a.Uint()
	case a.Kind() == reflect.String && b.Kind() == reflect.String:
		return a.String() == b.String()
	case a.Kind() == reflect.Bool && b.Kind() == reflect.Bool:
		return a.Bool() == b.Bool()
	case isFloatKind(a) && isFloatKind(b):
		return a.Float() == b.Float()
	}
	if a.Type() == b.Type() && a.Type().Comparable() {
		return a.Interface() == b.Interface()
	}
	return false
}

func compare(a, b reflect.Value) (int, error) {
	a, b = indirect(a), indirect(b)
	switch {
	case isIntKind(a) && isIntKind(b):
		return cmp(a.Int(), b.Int()), nil
	case isUintKind(a) && isUintKind(b):
		return cmp(a.Uint(), b.Uint()), nil
	case isFloatKind(a) && isFloatKind(b):
		return cmp(a.Float(), b.Float()), nil
	case a.Kind() == reflect.String && b.Kind() == reflect.String:
		return strings.Compare(a.String(), b.String()), nil
	}
	return 0, fmt.Errorf("sqlshape: incompatible types for comparison")
}

func cmp[T int64 | uint64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isIntKind(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUintKind(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

func isFloatKind(v reflect.Value) bool {
	return v.Kind() == reflect.Float32 || v.Kind() == reflect.Float64
}

// normalizeArg turns a template value into something pgx encodes for any target OID:
// named string types (enums) become string, slices of them become []string; nil
// pointers become nil; a struct that receives a composite (see isNested) becomes
// pgtype.CompositeFields in the order the checker verified (embedded structs flattened,
// `col:"-"` skipped), a slice of them a slice of those. Everything else is passed through.
func normalizeArg(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return normalizeArg(v.Elem())
	}
	t := v.Type()
	switch {
	case t.Kind() == reflect.String && t.PkgPath() != "":
		return v.String()
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String && t.Elem().PkgPath() != "":
		out := make([]string, v.Len())
		for i := range out {
			out[i] = v.Index(i).String()
		}
		return out
	case t.Kind() == reflect.Struct && isNested(t) && !v.Type().Implements(valuerType):
		return compositeArg(v)
	case t.Kind() == reflect.Slice && isNested(t) && !t.Elem().Implements(valuerType):
		if v.IsNil() {
			return nil
		}
		out := make([]pgtype.CompositeFields, v.Len())
		for i := range out {
			out[i] = compositeArg(v.Index(i))
		}
		return out
	}
	return v.Interface()
}

var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// compositeArg lays a struct (or *struct) out as the fields of a composite value.
func compositeArg(v reflect.Value) pgtype.CompositeFields {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	flat, _ := flatFields(v.Type())
	out := make(pgtype.CompositeFields, len(flat))
	for i, f := range flat {
		out[i] = normalizeArg(v.FieldByIndex(f.index))
	}
	return out
}

func stub() {}
