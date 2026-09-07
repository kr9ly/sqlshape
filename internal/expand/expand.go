// Package expand enumerates the finite set of SQL strings a query template can
// produce. Control flow (if / else / with / range) is expanded exhaustively;
// every value action `{{.Field}}` becomes a `$n` placeholder whose Go-side
// origin is recorded as a field path on the parameter struct.
package expand

import (
	"fmt"
	"strconv"
	"strings"
	"text/template/parse"
)

// MaxExpansions caps the full branch product. Beyond it the expansion falls back to a
// sparse set (Result.Sparse): every branch off, every branch on, and each branch on
// alone. Independent AND predicates are then still fully type-checked; a fragment that
// depends on another (a JOIN one `if` introduces, a column another `if` uses) shows up
// as an error in its "alone" form, which is the cue to restructure the template.
const MaxExpansions = 256

// Path is a field path from the parameter struct root: each element is a field
// name, or "[]" for "element of the preceding slice / array / map".
type Path []string

func (p Path) String() string {
	var b strings.Builder
	for _, e := range p {
		if e == "[]" {
			b.WriteString("[]")
		} else {
			b.WriteString(".")
			b.WriteString(e)
		}
	}
	if b.Len() == 0 {
		return "."
	}
	return b.String()
}

// Param is one `$n` in an expansion.
type Param struct {
	N    int  // 1-based placeholder number
	Path Path // origin on P
	Pos  int  // byte offset of the action in the template text
}

// Expansion is one concrete SQL string.
type Expansion struct {
	SQL    string
	Params []Param
	// Branch names the control-flow choices that produced this expansion, e.g. "if@31:then range@114:x2".
	Branch string
	segs   []segment
}

// segment maps a run of expanded bytes back to the template.
type segment struct {
	expStart, tmplStart, n int
}

// TemplatePos maps a byte offset in SQL back to a template offset (best effort).
func (e *Expansion) TemplatePos(sqlOff int) int {
	for _, s := range e.segs {
		if sqlOff >= s.expStart && sqlOff < s.expStart+s.n {
			return s.tmplStart + (sqlOff - s.expStart)
		}
	}
	if len(e.segs) > 0 {
		last := e.segs[len(e.segs)-1]
		return last.tmplStart + last.n
	}
	return 0
}

// Result of expanding one template.
type Result struct {
	Expansions []Expansion
	// Controls are the paths read by if / with / range conditions (they must exist on P).
	Controls []Path
	// Sparse is set when the branch product exceeded MaxExpansions and Expansions holds
	// the sparse set instead of every combination (see MaxExpansions).
	Sparse bool
	// Combinations is the size of the full branch product.
	Combinations int
}

// Error is a template problem with a template byte offset.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("template: %s (at %d)", e.Msg, e.Pos) }

// Expand parses and expands tmpl.
func Expand(tmpl string) (*Result, error) {
	trees, err := parse.Parse("q", tmpl, "{{", "}}", builtins)
	if err != nil {
		return nil, &Error{Pos: 0, Msg: strings.TrimPrefix(err.Error(), "template: q:")}
	}
	root := trees["q"].Root
	branches := branchNodes(root)
	combos := 1
	for _, b := range branches {
		if b.kind == 'r' {
			combos *= 3
		} else {
			combos *= 2
		}
		if combos > MaxExpansions {
			break
		}
	}
	res := &Result{Combinations: combos}
	if combos <= MaxExpansions {
		x := &expander{res: res}
		out, err := x.list(root, []*state{{dot: Path{}, vars: map[string]Path{"$": {}}}})
		if err != nil {
			return nil, err
		}
		x.emit(out)
		return res, nil
	}
	// sparse: all off, all on, each on alone
	res.Sparse = true
	policies := []map[parse.Pos]int{{}, {}}
	for _, b := range branches {
		on := 1
		if b.kind == 'r' {
			on = 2
		}
		policies[1][b.pos] = on
		policies = append(policies, map[parse.Pos]int{b.pos: 1})
	}
	for _, pol := range policies {
		x := &expander{res: res, choose: func(pos parse.Pos) int { return pol[pos] }}
		out, err := x.list(root, []*state{{dot: Path{}, vars: map[string]Path{"$": {}}}})
		if err != nil {
			return nil, err
		}
		x.emit(out)
	}
	return res, nil
}

func (x *expander) emit(out []*state) {
	for _, st := range out {
		x.res.Expansions = append(x.res.Expansions, Expansion{
			SQL: st.sql.String(), Params: st.params, Branch: strings.TrimSpace(st.branch), segs: st.segs,
		})
	}
}

type branchNode struct {
	pos  parse.Pos
	kind byte // 'i' if, 'w' with, 'r' range
}

// branchNodes lists the control nodes of a tree in source order.
func branchNodes(l *parse.ListNode) []branchNode {
	var out []branchNode
	var walk func(l *parse.ListNode)
	walk = func(l *parse.ListNode) {
		if l == nil {
			return
		}
		for _, n := range l.Nodes {
			switch v := n.(type) {
			case *parse.IfNode:
				out = append(out, branchNode{v.Pos, 'i'})
				walk(v.List)
				walk(v.ElseList)
			case *parse.WithNode:
				out = append(out, branchNode{v.Pos, 'w'})
				walk(v.List)
				walk(v.ElseList)
			case *parse.RangeNode:
				out = append(out, branchNode{v.Pos, 'r'})
				walk(v.List)
				walk(v.ElseList)
			}
		}
	}
	walk(l)
	return out
}

// builtins lets conditions use the usual comparison / boolean helpers. They are never evaluated.
var builtins = map[string]any{
	"eq": func() {}, "ne": func() {}, "lt": func() {}, "le": func() {}, "gt": func() {}, "ge": func() {},
	"and": func() {}, "or": func() {}, "not": func() {}, "len": func() {}, "index": func() {},
}

type expander struct {
	res *Result
	// choose, when set (sparse mode), picks the single alternative to take at a control
	// node: 0 else / no iteration, 1 then / one iteration, 2 two iterations
	choose func(pos parse.Pos) int
}

// state is one partial expansion being built.
type state struct {
	sql    strings.Builder
	params []Param
	segs   []segment
	branch string
	dot    Path
	vars   map[string]Path
	// paramKeys dedupes identical origins within one expansion so the same value gets one $n
	paramKeys map[string]int
	// iter is the stack of active range iteration indexes (disambiguates $n across iterations)
	iter []int
}

func (s *state) clone() *state {
	c := &state{dot: s.dot, branch: s.branch}
	c.sql.WriteString(s.sql.String())
	c.params = append([]Param{}, s.params...)
	c.segs = append([]segment{}, s.segs...)
	c.vars = map[string]Path{}
	for k, v := range s.vars {
		c.vars[k] = v
	}
	c.paramKeys = map[string]int{}
	for k, v := range s.paramKeys {
		c.paramKeys[k] = v
	}
	c.iter = append([]int{}, s.iter...)
	return c
}

func (s *state) text(t string, tmplPos int) {
	s.segs = append(s.segs, segment{expStart: s.sql.Len(), tmplStart: tmplPos, n: len(t)})
	s.sql.WriteString(t)
}

func (s *state) param(p Path, tmplPos int) {
	key := p.String() + fmt.Sprint(s.iter)
	if s.paramKeys == nil {
		s.paramKeys = map[string]int{}
	}
	n, ok := s.paramKeys[key]
	if !ok {
		n = len(s.params) + 1
		s.paramKeys[key] = n
		s.params = append(s.params, Param{N: n, Path: p, Pos: tmplPos})
	}
	s.text("$"+strconv.Itoa(n), tmplPos)
}

func (x *expander) list(l *parse.ListNode, states []*state) ([]*state, error) {
	if l == nil || len(states) == 0 {
		return states, nil
	}
	var err error
	for _, n := range l.Nodes {
		states, err = x.node(n, states)
		if err != nil {
			return nil, err
		}
		if len(states) > MaxExpansions {
			return nil, &Error{Pos: int(n.Position()), Msg: fmt.Sprintf("more than %d expansions; split the template", MaxExpansions)}
		}
	}
	return states, nil
}

func (x *expander) node(n parse.Node, states []*state) ([]*state, error) {
	switch v := n.(type) {
	case *parse.TextNode:
		for _, s := range states {
			s.text(string(v.Text), int(v.Pos))
		}
		return states, nil
	case *parse.ActionNode:
		if len(v.Pipe.Decl) > 0 {
			// {{$x := .Y}}
			p, err := x.pipePath(v.Pipe, states[0], int(v.Pos))
			if err != nil {
				return nil, err
			}
			for _, s := range states {
				s.vars[v.Pipe.Decl[0].Ident[0]] = p
			}
			return states, nil
		}
		p, err := x.pipePath(v.Pipe, states[0], int(v.Pos))
		if err != nil {
			return nil, err
		}
		for _, s := range states {
			s.param(x.rebase(p, s), int(v.Pos))
		}
		return states, nil
	case *parse.IfNode:
		x.control(v.Pipe, states[0])
		return x.branches(&v.BranchNode, states, "if", nil)
	case *parse.WithNode:
		p, err := x.pipePath(v.Pipe, states[0], int(v.Pos))
		if err != nil {
			return nil, err
		}
		x.res.Controls = append(x.res.Controls, p)
		if len(v.Pipe.Decl) > 0 {
			// {{with $y := .X}}: $y is .X inside
			for _, s := range states {
				s.vars[v.Pipe.Decl[0].Ident[0]] = p
			}
		}
		return x.branches(&v.BranchNode, states, "with", &p)
	case *parse.RangeNode:
		return x.rangeNode(v, states)
	case *parse.CommentNode:
		return states, nil
	case *parse.TemplateNode:
		return nil, &Error{Pos: int(v.Pos), Msg: "{{template}} is not supported"}
	}
	return nil, &Error{Pos: int(n.Position()), Msg: fmt.Sprintf("unsupported template node %T", n)}
}

// branches expands then/else lists. withDot, if set, becomes dot inside the then-branch.
func (x *expander) branches(b *parse.BranchNode, states []*state, kind string, withDot *Path) ([]*state, error) {
	var out []*state
	takeThen, takeElse := true, true
	if x.choose != nil {
		takeThen = x.choose(b.Pos) >= 1
		takeElse = !takeThen
	}
	then := make([]*state, 0, len(states))
	for _, s := range states {
		c := s.clone()
		c.branch += fmt.Sprintf(" %s@%d:then", kind, b.Pos)
		if withDot != nil {
			c.dot = x.rebase(*withDot, s)
		}
		then = append(then, c)
	}
	if !takeThen {
		then = nil
	}
	then, err := x.list(b.List, then)
	if err != nil {
		return nil, err
	}
	for _, s := range then {
		if withDot != nil {
			s.dot = states[0].dot
		}
	}
	out = append(out, then...)
	if !takeElse {
		return out, nil
	}
	els := make([]*state, 0, len(states))
	for _, s := range states {
		c := s.clone()
		c.branch += fmt.Sprintf(" %s@%d:else", kind, b.Pos)
		els = append(els, c)
	}
	els, err = x.list(b.ElseList, els)
	if err != nil {
		return nil, err
	}
	return append(out, els...), nil
}

// rangeNode expands {{range}} as 0, 1 and 2 iterations.
func (x *expander) rangeNode(r *parse.RangeNode, states []*state) ([]*state, error) {
	p, err := x.pipePath(r.Pipe, states[0], int(r.Pos))
	if err != nil {
		return nil, err
	}
	x.res.Controls = append(x.res.Controls, p)
	var out []*state
	counts := []int{0, 1, 2}
	if x.choose != nil {
		counts = []int{x.choose(r.Pos)}
	}
	for _, count := range counts {
		batch := make([]*state, 0, len(states))
		for _, s := range states {
			c := s.clone()
			c.branch += fmt.Sprintf(" range@%d:x%d", r.Pos, count)
			batch = append(batch, c)
		}
		if count == 0 {
			batch, err = x.list(r.ElseList, batch)
			if err != nil {
				return nil, err
			}
			out = append(out, batch...)
			continue
		}
		for i := 0; i < count; i++ {
			for _, c := range batch {
				elem := append(append(Path{}, x.rebase(p, c)...), "[]")
				c.dot = elem
				c.iter = append(c.iter, i)
				// {{range $i, $v := .X}} / {{range $v := .X}}
				switch len(r.Pipe.Decl) {
				case 1:
					c.vars[r.Pipe.Decl[0].Ident[0]] = elem
				case 2:
					c.vars[r.Pipe.Decl[0].Ident[0]] = Path{"#index"}
					c.vars[r.Pipe.Decl[1].Ident[0]] = elem
				}
			}
			batch, err = x.list(r.List, batch)
			if err != nil {
				return nil, err
			}
			for _, c := range batch {
				c.iter = c.iter[:len(c.iter)-1]
			}
		}
		for _, c := range batch {
			c.dot = states[0].dot
		}
		out = append(out, batch...)
	}
	return out, nil
}

// control records paths read by a condition (no expansion effect).
func (x *expander) control(pipe *parse.PipeNode, s *state) {
	for _, cmd := range pipe.Cmds {
		for _, arg := range cmd.Args {
			switch a := arg.(type) {
			case *parse.FieldNode:
				x.res.Controls = append(x.res.Controls, x.rebase(append(append(Path{}, s.dot...), a.Ident...), s))
			case *parse.DotNode:
				x.res.Controls = append(x.res.Controls, s.dot)
			}
		}
	}
}

// pipePath extracts the single field-chain a value action must be. Relative paths (from dot)
// are returned with dot prepended; variables resolve through s.vars.
func (x *expander) pipePath(pipe *parse.PipeNode, s *state, pos int) (Path, error) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return nil, &Error{Pos: pos, Msg: "value actions must be a plain field reference like {{.Field}}"}
	}
	switch a := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		return append(append(Path{}, s.dot...), a.Ident...), nil
	case *parse.DotNode:
		return append(Path{}, s.dot...), nil
	case *parse.VariableNode:
		base, ok := s.vars[a.Ident[0]]
		if !ok {
			return nil, &Error{Pos: pos, Msg: fmt.Sprintf("undefined variable %s", a.Ident[0])}
		}
		return append(append(Path{}, base...), a.Ident[1:]...), nil
	}
	return nil, &Error{Pos: pos, Msg: "value actions must be a plain field reference like {{.Field}}"}
}

// rebase is the identity today; paths are already absolute. Kept as the single place to
// change if relative resolution is needed.
func (x *expander) rebase(p Path, s *state) Path { return p }
