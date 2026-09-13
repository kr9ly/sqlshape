// Package expand enumerates the finite set of SQL strings a query template can
// produce. Control flow (if / else / with / range) is expanded exhaustively;
// every value action `{{.Field}}` becomes a `$n` placeholder whose Go-side
// origin is recorded as a field path on the parameter struct.
package expand

import (
	"fmt"
	"sort"
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
	// GuardedTrue lists the paths a plain {{if .X}} or {{with .X}} in this expansion
	// decided true (the then branch was taken reading X itself, not a computed
	// expression like `gt .X 0`). Reading X this way proves X was not the zero value at
	// that point: for a pointer field, that means non-nil for the rest of the
	// expansion's lineage. See Guarded.
	GuardedTrue []Path
	segs        []segment
}

// Guarded reports whether p is exactly one of e.GuardedTrue's paths. It is deliberately
// not a prefix match: {{with .Order}} proves .Order itself non-nil, but says nothing
// about .Order.ID's own nilness when ID is itself a pointer field -- that field can still
// be its own nil regardless of Order's. Only a condition reading that exact path (e.g.
// nested as {{if .ID}} inside the with) proves it.
func (e *Expansion) Guarded(p Path) bool {
	for _, g := range e.GuardedTrue {
		if len(p) != len(g) {
			continue
		}
		match := true
		for i, el := range g {
			if p[i] != el {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
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
	// branchNodes already collapses repeated occurrences of the same control path (see
	// staticKey / addBranch): a condition read twice, e.g. an {{if .Status}} in a column
	// list and again in the matching VALUES clause, is one branch, not two, since the two
	// occurrences always agree (they read the same value). Only the *count* here is
	// deduped; the actual tie enforcement happens per-expansion in branches / rangeNode via
	// state.decisions, which is exact (dot/var-aware) where this static, syntax-only key is
	// just an approximation used to size the branch product / build sparse policies.
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
		out, err := x.list(root, []*state{newRootState()})
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
		out, err := x.list(root, []*state{newRootState()})
		if err != nil {
			return nil, err
		}
		x.emit(out)
	}
	return res, nil
}

func newRootState() *state {
	return &state{dot: Path{}, vars: map[string]Path{"$": {}}, decisions: map[string]int{}}
}

func (x *expander) emit(out []*state) {
	for _, st := range out {
		var keys []string
		for k := range st.guardedTrue {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var guarded []Path
		for _, k := range keys {
			guarded = append(guarded, st.guardedTrue[k])
		}
		x.res.Expansions = append(x.res.Expansions, Expansion{
			SQL: st.sql.String(), Params: st.params, Branch: strings.TrimSpace(st.branch),
			GuardedTrue: guarded, segs: st.segs,
		})
	}
}

type branchNode struct {
	pos  parse.Pos
	kind byte // 'i' if, 'w' with, 'r' range
}

// branchNodes lists the control nodes of a tree in source order. A control whose condition
// is a plain field/dot/variable reference (the same shape a value action {{.Field}} must
// have -- see pipePath) is deduped against any earlier control reading the same reference:
// repeated reads of one path always agree, so they count, and later get tied, as one branch
// rather than one per occurrence. Conditions that are not a plain reference (e.g. `gt .X 0`)
// cannot be tied this way (their value isn't known without evaluating them) and are always
// counted independently, as before.
func branchNodes(l *parse.ListNode) []branchNode {
	var out []branchNode
	seen := map[string]bool{}
	var walk func(l *parse.ListNode)
	walk = func(l *parse.ListNode) {
		if l == nil {
			return
		}
		for _, n := range l.Nodes {
			switch v := n.(type) {
			case *parse.IfNode:
				addBranch(&out, seen, v.Pos, 'i', v.Pipe)
				walk(v.List)
				walk(v.ElseList)
			case *parse.WithNode:
				addBranch(&out, seen, v.Pos, 'w', v.Pipe)
				walk(v.List)
				walk(v.ElseList)
			case *parse.RangeNode:
				addBranch(&out, seen, v.Pos, 'r', v.Pipe)
				walk(v.List)
				walk(v.ElseList)
			}
		}
	}
	walk(l)
	return out
}

func addBranch(out *[]branchNode, seen map[string]bool, pos parse.Pos, kind byte, pipe *parse.PipeNode) {
	if key, ok := staticKey(pipe); ok {
		full := string(kind) + key
		if seen[full] {
			return
		}
		seen[full] = true
	}
	*out = append(*out, branchNode{pos, kind})
}

// staticKey renders the syntactic shape of a plain field/dot/variable reference pipe, the
// same shape pipePath accepts, for use as an approximate (dot/var-unaware) tie key. It is
// only used to size the branch product and build sparse policies; the exact, scope-correct
// tie key used to actually collapse expansions is computed live per state in pipePath.
func staticKey(pipe *parse.PipeNode) (string, bool) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", false
	}
	switch a := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		return "." + strings.Join(a.Ident, "."), true
	case *parse.DotNode:
		return ".", true
	case *parse.VariableNode:
		return "$" + strings.Join(a.Ident, "."), true
	}
	return "", false
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
	// decisions ties repeated reads of the same control path to the same outcome within one
	// expansion: keyed "if:<path>" / "with:<path>" (1 = then, 0 = else) or "range:<path>"
	// (the iteration count, 0/1/2). Set the first time a path is read, consulted (instead of
	// branching again) on every later read of the same path in this lineage.
	decisions map[string]int
	// guardedTrue records, keyed by Path.String(), every path a plain {{if}}/{{with}} in
	// this lineage has decided true (see Expansion.GuardedTrue).
	guardedTrue map[string]Path
}

// guard records that p was read true by a plain if/with condition in this lineage.
func (s *state) guard(p Path) {
	if s.guardedTrue == nil {
		s.guardedTrue = map[string]Path{}
	}
	s.guardedTrue[p.String()] = p
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
	c.decisions = map[string]int{}
	for k, v := range s.decisions {
		c.decisions[k] = v
	}
	c.guardedTrue = map[string]Path{}
	for k, v := range s.guardedTrue {
		c.guardedTrue[k] = v
	}
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
		return x.branches(&v.BranchNode, states, "if", false, func(s *state) (string, Path, bool) {
			p, err := x.pipePath(v.Pipe, s, int(v.Pos))
			if err != nil {
				return "", nil, false
			}
			return "if:" + p.String(), p, true
		})
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
		return x.branches(&v.BranchNode, states, "with", true, func(s *state) (string, Path, bool) {
			p, err := x.pipePath(v.Pipe, s, int(v.Pos))
			if err != nil {
				return "", nil, false
			}
			return "with:" + p.String(), p, true
		})
	case *parse.RangeNode:
		return x.rangeNode(v, states)
	case *parse.CommentNode:
		return states, nil
	case *parse.TemplateNode:
		return nil, &Error{Pos: int(v.Pos), Msg: "{{template}} is not supported"}
	}
	return nil, &Error{Pos: int(n.Position()), Msg: fmt.Sprintf("unsupported template node %T", n)}
}

// branchKey resolves, for one state, the tie key ("if:<path>" / "with:<path>") and the
// resolved path a control's condition reads, when that condition is a plain field/dot/
// variable reference (ok=false otherwise, e.g. `gt .X 0`: not tie-able, always branches).
type branchKey func(s *state) (key string, path Path, ok bool)

// branches expands then/else lists. withDot requests that the resolved path (from kf)
// become dot inside the then-branch ({{with}}); kf may be nil for controls that decline to
// resolve to a tie-able path in a given state (branches independently, as before).
//
// A state that already has a decision recorded for kf's key (because an earlier control in
// this same expansion read the identical path -- e.g. the same {{if .Status}} appearing
// twice) is not branched again: it is routed straight down the branch that decision already
// picked. This is what ties two occurrences of one condition to the same outcome instead of
// letting them vary independently (which would multiply the expansion count and could even
// produce impossible combinations, like a column list and its VALUES clause disagreeing on
// whether a column is present).
func (x *expander) branches(b *parse.BranchNode, states []*state, kind string, withDot bool, kf branchKey) ([]*state, error) {
	takeThen, takeElse := true, true
	if x.choose != nil {
		takeThen = x.choose(b.Pos) >= 1
		takeElse = !takeThen
	}

	var thenIn, elseIn []*state
	for _, s := range states {
		var key string
		var path Path
		var ok bool
		if kf != nil {
			key, path, ok = kf(s)
		}
		if ok {
			if d, seen := s.decisions[key]; seen {
				// Already decided by an earlier occurrence of the same path: follow it,
				// don't re-branch this state.
				s.branch += fmt.Sprintf(" %s@%d:%s", kind, b.Pos, thenElse(d == 1))
				if d == 1 {
					s.guard(path)
					if withDot {
						s.dot = x.rebase(path, s)
					}
					thenIn = append(thenIn, s)
				} else {
					elseIn = append(elseIn, s)
				}
				continue
			}
		}
		if takeThen {
			c := s.clone()
			if ok {
				c.decisions[key] = 1
				c.guard(path)
			}
			if withDot {
				c.dot = x.rebase(path, s)
			}
			c.branch += fmt.Sprintf(" %s@%d:then", kind, b.Pos)
			thenIn = append(thenIn, c)
		}
		if takeElse {
			c := s.clone()
			if ok {
				c.decisions[key] = 0
			}
			c.branch += fmt.Sprintf(" %s@%d:else", kind, b.Pos)
			elseIn = append(elseIn, c)
		}
	}

	then, err := x.list(b.List, thenIn)
	if err != nil {
		return nil, err
	}
	if withDot {
		for _, s := range then {
			s.dot = states[0].dot
		}
	}
	els, err := x.list(b.ElseList, elseIn)
	if err != nil {
		return nil, err
	}
	return append(then, els...), nil
}

func thenElse(then bool) string {
	if then {
		return "then"
	}
	return "else"
}

// rangeNode expands {{range}} as 0, 1 and 2 iterations. States that range over the same
// resolved path as an earlier {{range}} in this expansion (state.decisions["range:<path>"])
// are tied to that earlier occurrence's iteration count instead of being re-branched into
// their own independent 0/1/2, for the same reason if/with ties repeated conditions: two
// reads of one path always agree.
func (x *expander) rangeNode(r *parse.RangeNode, states []*state) ([]*state, error) {
	type group struct {
		path   Path
		states []*state
	}
	byKey := map[string]*group{}
	var order []string
	for _, s := range states {
		p, err := x.pipePath(r.Pipe, s, int(r.Pos))
		if err != nil {
			return nil, err
		}
		key := p.String()
		g, ok := byKey[key]
		if !ok {
			g = &group{path: p}
			byKey[key] = g
			order = append(order, key)
		}
		g.states = append(g.states, s)
	}

	var out []*state
	for _, key := range order {
		g := byKey[key]
		x.res.Controls = append(x.res.Controls, g.path)
		tieKey := "range:" + key

		byCount := map[int][]*state{}
		var undecided []*state
		for _, s := range g.states {
			if c, ok := s.decisions[tieKey]; ok {
				byCount[c] = append(byCount[c], s)
			} else {
				undecided = append(undecided, s)
			}
		}
		for _, count := range []int{0, 1, 2} {
			ss, ok := byCount[count]
			if !ok {
				continue
			}
			batch, err := x.runRange(r, g.path, ss, count, false, "")
			if err != nil {
				return nil, err
			}
			out = append(out, batch...)
		}
		if len(undecided) > 0 {
			counts := []int{0, 1, 2}
			if x.choose != nil {
				counts = []int{x.choose(r.Pos)}
			}
			for _, count := range counts {
				batch, err := x.runRange(r, g.path, undecided, count, true, tieKey)
				if err != nil {
					return nil, err
				}
				out = append(out, batch...)
			}
		}
	}
	return out, nil
}

// runRange runs count iterations of r.List (or r.ElseList when count == 0) over states,
// cloning each first. path is the range's resolved path (computed once, before any dot
// mutation -- recomputing it after the first iteration would resolve against the element
// dot instead of the collection). When record is true, the chosen count is recorded under
// tieKey so a later {{range}} over the identical path reuses it instead of branching again.
func (x *expander) runRange(r *parse.RangeNode, path Path, states []*state, count int, record bool, tieKey string) ([]*state, error) {
	parentDot := states[0].dot
	batch := make([]*state, 0, len(states))
	for _, s := range states {
		c := s.clone()
		c.branch += fmt.Sprintf(" range@%d:x%d", r.Pos, count)
		if record {
			c.decisions[tieKey] = count
		}
		batch = append(batch, c)
	}
	var err error
	if count == 0 {
		batch, err = x.list(r.ElseList, batch)
		if err != nil {
			return nil, err
		}
		return batch, nil
	}
	for i := 0; i < count; i++ {
		for _, c := range batch {
			elem := append(append(Path{}, x.rebase(path, c)...), "[]")
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
		c.dot = parentDot
	}
	return batch, nil
}

// control records paths read by a condition (no expansion effect). A condition argument
// may itself be a nested pipeline -- a parenthesized call like `(len .Xz)` in
// `{{if gt (len .Xz) 0}}` -- so field/dot references are hunted recursively through
// nested pipelines, not just directly on the top-level command.
func (x *expander) control(pipe *parse.PipeNode, s *state) {
	for _, cmd := range pipe.Cmds {
		for _, arg := range cmd.Args {
			x.controlArg(arg, s)
		}
	}
}

// controlArg records the paths read by one condition argument, recursing into a nested
// pipeline's own commands and arguments.
func (x *expander) controlArg(arg parse.Node, s *state) {
	switch a := arg.(type) {
	case *parse.FieldNode:
		x.res.Controls = append(x.res.Controls, x.rebase(append(append(Path{}, s.dot...), a.Ident...), s))
	case *parse.DotNode:
		x.res.Controls = append(x.res.Controls, s.dot)
	case *parse.PipeNode:
		x.control(a, s)
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
