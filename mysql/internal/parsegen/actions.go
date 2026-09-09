package parsegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The semantic actions StripGrammar throws away are the map from the concrete syntax tree
// to the server's parse tree: `$$= NEW_PTN PT_select_stmt(@$, $1, $2)` says which class an
// alternative builds and which children feed which constructor argument. This file reads
// them back so that the AST layer can be generated rather than written.

// ActionKind classifies what an alternative's action does with its children.
type ActionKind int

const (
	// ActUnknown: something this reader does not model (legacy LEX mutation, helpers).
	ActUnknown ActionKind = iota
	// ActDefault: no action; bison's default `$$= $1` applies (keyword lists, mostly).
	ActDefault
	// ActEmpty: `{}` or `$$= nullptr`: no value.
	ActEmpty
	// ActPass: `$$= $N` — the alternative is one child.
	ActPass
	// ActConst: `$$= SOME_CONSTANT` (an enum or literal).
	ActConst
	// ActNew: `$$= NEW_PTN Class(args)`, all args resolved.
	ActNew
	// ActListNew: a fresh list holding one child (`$$= NEW_PTN List(...); $$->push_back($N)`).
	ActListNew
	// ActListAppend: `$$= $A; $$->push_back($B)` — left recursion growing a list.
	ActListAppend
	// ActFlags: `$$= $A | $B` — option bit sets combined.
	ActFlags
	// ActNumber: `$$= my_strtoll10($N.str, ...)` — a numeric token read as a number.
	ActNumber
	// ActStruct: `$$.a= $N; $$.b= $M;` — a by-value struct filled from children/constants.
	ActStruct
)

func (k ActionKind) String() string {
	return [...]string{"unknown", "default", "empty", "pass", "const", "new", "list-new", "list-append", "flags", "number", "struct"}[k]
}

// Arg is one constructor argument as the action wrote it.
type Arg struct {
	Child int    // 1-based RHS position, 0 when the argument is not a child
	Field string // ".str", ".column_list", ... when a field of the child is passed
	Text  string // the argument as written when it is neither (a constant, a helper call)
}

// Alt is one grammar alternative with its action read.
type Alt struct {
	Rule   string
	Index  int      // alternative number within the rule
	Syms   []string // RHS symbols (terminals and nonterminals; mid-rule actions dropped)
	Kind   ActionKind
	Class  string  // ActNew / ActListNew: the PT class
	Args   []Arg   // ActNew: constructor arguments; ActPass: the one child; ActListAppend: list, element
	Const  string  // ActConst
	Fields []Field // ActStruct
	Action string  // the action text, normalized
}

// Field is one `$$.name= value` assignment of a struct-valued action.
type Field struct {
	Name string
	Arg  Arg
}

// ReadActions parses the rules section of sql_yacc.yy and classifies every alternative.
func ReadActions(src string) ([]Alt, error) {
	sections := reSectionSep.FindAllStringIndex(src, -1)
	if len(sections) < 1 {
		return nil, fmt.Errorf("grammar: no %%%% section separator")
	}
	rulesEnd := len(src)
	if len(sections) > 1 {
		rulesEnd = sections[1][0]
	}
	toks, err := lexRules(src[sections[0][1]:rulesEnd])
	if err != nil {
		return nil, err
	}
	var alts []Alt
	i := 0
	for i < len(toks) {
		lhs := toks[i]
		if lhs.kind != tkID || i+1 >= len(toks) || toks[i+1].kind != tkColon {
			return nil, fmt.Errorf("grammar: expected rule head near %q", lhs.text)
		}
		i += 2
		idx := 0
		for {
			var syms []string
			action := ""
			for i < len(toks) {
				t := toks[i]
				if t.kind == tkBar || t.kind == tkSemi {
					break
				}
				if t.kind == tkID && i+1 < len(toks) && toks[i+1].kind != tkColon || t.kind == tkLit {
					i++
					syms = append(syms, t.text)
					continue
				}
				if t.kind == tkID { // next rule head
					break
				}
				i++
				switch t.kind {
				case tkPrec:
					i++
				case tkAction:
					final := i >= len(toks) || toks[i].kind == tkBar || toks[i].kind == tkSemi ||
						(toks[i].kind == tkID && i+1 < len(toks) && toks[i+1].kind == tkColon)
					if final {
						action = t.text
					}
				}
			}
			a := classify(lhs.text, idx, syms, action)
			alts = append(alts, a)
			idx++
			if i < len(toks) && toks[i].kind == tkBar {
				i++
				continue
			}
			if i < len(toks) && toks[i].kind == tkSemi {
				i++
			}
			break
		}
	}
	return alts, nil
}

var (
	reCComment    = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reLineCmt     = regexp.MustCompile(`//[^\n]*`)
	reSpace       = regexp.MustCompile(`\s+`)
	reNullCheck   = regexp.MustCompile(`if\(\$\$==(nullptr|NULL)\)MYSQL_YYABORT;`)
	rePass        = regexp.MustCompile(`^\$\$=\$(\d+);$`)
	reConst       = regexp.MustCompile(`^\$\$=(-?\d+|true|false|[A-Z][A-Za-z_0-9]*(::[A-Za-z_0-9]+)*);$`)
	reNew         = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)(<[^>]*>)?\((.*)\);$`)
	reListAppend  = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reListAppend2 = regexp.MustCompile(`^\$(\d+)->push_(back|front)\(\$(\d+)\);\$\$=\$(\d+);$`)
	reListNew     = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)(<[^>]*>)?\((.*?)\);if\(\$\$==(nullptr|NULL)\|\|\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	rePassPos     = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$!=nullptr\)\$\$->m_pos=@\$;$`)
	rePassField   = regexp.MustCompile(`^\$\$=(to_lex_cstring\()?\$(\d+)(\.str|\.node)?\)?;$`)
	reItemize     = regexp.MustCompile(`^ITEMIZE\(\$(\d+),&\$\$\);$`)
	reFlags       = regexp.MustCompile(`^\$\$=\$(\d+)\|\$(\d+);$`)
	reNumber      = regexp.MustCompile(`^interror;\$\$=\((ulong|ulonglong|int|uint|longlong)\)my_strtoll10\(\$(\d+)\.str,nullptr,&error\);(if\(error!=0\)\{[^}]*\})?$`)
	reAppend3     = regexp.MustCompile(`^(if\(\$(\d+)==nullptr\|\|\$(\d+)->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;|if\(\$(\d+)->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;|\$(\d+)->push_(back|front)\(\$(\d+)\);)\$\$=\$(\d+);(\$\$->m_pos=@\$;)?$`)
	reAppendVal   = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$\.push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reAppendNull  = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$==nullptr\|\|\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reListInitVal = regexp.MustCompile(`^\$\$\.init\(YYMEM_ROOT\);(if\(\$\$\.push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;)?$`)
	reStruct      = regexp.MustCompile(`^(\$\$\.[A-Za-z_0-9]+=[^;]+;)+$`)
	reStructField = regexp.MustCompile(`\$\$\.([A-Za-z_0-9]+)=([^;]+);`)
	reArgChild    = regexp.MustCompile(`^\$(\d+)$`)
	reArgField    = regexp.MustCompile(`^\$(\d+)((\.|->)[A-Za-z_0-9.]+)$`)
)

// normalize strips comments and whitespace so that the shapes can be matched textually.
func normalize(action string) string {
	a := strings.TrimSpace(action)
	a = strings.TrimPrefix(a, "{")
	a = strings.TrimSuffix(a, "}")
	a = reCComment.ReplaceAllString(a, "")
	a = reLineCmt.ReplaceAllString(a, "")
	a = reSpace.ReplaceAllString(a, "")
	a = reNullCheck.ReplaceAllString(a, "")
	return a
}

func classify(rule string, idx int, syms []string, action string) Alt {
	a := Alt{Rule: rule, Index: idx, Syms: syms, Action: normalize(action)}
	n := a.Action
	switch {
	case action == "":
		a.Kind = ActDefault
	case n == "" || n == "$$=nullptr;" || n == "$$=NULL;" || n == "$$={};":
		a.Kind = ActEmpty
	case rePass.MatchString(n):
		m := rePass.FindStringSubmatch(n)
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(m[1])}}
	case rePassPos.MatchString(n):
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(rePassPos.FindStringSubmatch(n)[1])}}
	case rePassField.MatchString(n):
		m := rePassField.FindStringSubmatch(n)
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(m[2]), Field: m[3]}}
	case reItemize.MatchString(n):
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(reItemize.FindStringSubmatch(n)[1])}}
	case reFlags.MatchString(n):
		m := reFlags.FindStringSubmatch(n)
		a.Kind = ActFlags
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[2])}}
	case reNumber.MatchString(n):
		a.Kind = ActNumber
		a.Args = []Arg{{Child: atoi(reNumber.FindStringSubmatch(n)[2])}}
	case reAppendVal.MatchString(n):
		m := reAppendVal.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[3])}}
	case reAppendNull.MatchString(n):
		m := reAppendNull.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[3])}}
	case reAppend3.MatchString(n):
		m := reAppend3.FindStringSubmatch(n)
		list, elem := firstNonEmpty(m[3], m[6], m[9]), firstNonEmpty(m[5], m[8], m[11])
		if list == m[12] {
			a.Kind = ActListAppend
			a.Args = []Arg{{Child: atoi(list)}, {Child: atoi(elem)}}
		}
	case reListInitVal.MatchString(n):
		m := reListInitVal.FindStringSubmatch(n)
		a.Kind = ActListNew
		if m[3] != "" {
			a.Args = []Arg{{Child: atoi(m[3])}}
		}
	case reStruct.MatchString(n):
		a.Kind = ActStruct
		for _, f := range reStructField.FindAllStringSubmatch(n, -1) {
			a.Fields = append(a.Fields, Field{Name: f[1], Arg: parseArgs(f[2])[0]})
		}
	case reConst.MatchString(n):
		a.Kind = ActConst
		a.Const = reConst.FindStringSubmatch(n)[1]
	case reListAppend.MatchString(n):
		m := reListAppend.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[3])}}
	case reListAppend2.MatchString(n):
		m := reListAppend2.FindStringSubmatch(n)
		if m[1] == m[4] {
			a.Kind = ActListAppend
			a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[3])}}
		}
	case reListNew.MatchString(n):
		m := reListNew.FindStringSubmatch(n)
		a.Kind = ActListNew
		a.Class = m[1]
		a.Args = append(parseArgs(m[3]), Arg{Child: atoi(m[6])})
	case reNew.MatchString(n):
		m := reNew.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = parseArgs(m[3])
	}
	return a
}

func parseArgs(s string) []Arg {
	var out []Arg
	for _, x := range splitTopLevel(s) {
		switch {
		case reArgChild.MatchString(x):
			out = append(out, Arg{Child: atoi(reArgChild.FindStringSubmatch(x)[1])})
		case reArgField.MatchString(x):
			m := reArgField.FindStringSubmatch(x)
			out = append(out, Arg{Child: atoi(m[1]), Field: m[2]})
		default:
			out = append(out, Arg{Text: x})
		}
	}
	return out
}

// splitTopLevel splits a C argument list on the commas outside parentheses/brackets.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '<', '{':
			depth++
		case ')', ']', '>', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// Reach returns the rules reachable from roots, following RHS nonterminals.
func Reach(alts []Alt, roots []string) map[string]bool {
	byRule := map[string][]Alt{}
	for _, a := range alts {
		byRule[a.Rule] = append(byRule[a.Rule], a)
	}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(r string) {
		if seen[r] {
			return
		}
		if _, ok := byRule[r]; !ok {
			return
		}
		seen[r] = true
		for _, a := range byRule[r] {
			for _, s := range a.Syms {
				visit(s)
			}
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return seen
}

// ReachReport lists, for the rules reachable from roots, the alternatives whose actions
// are not read: the hand-written scope.
func ReachReport(alts []Alt, roots []string) string {
	reach := Reach(alts, roots)
	var b strings.Builder
	total, unknown := 0, 0
	byRule := map[string]int{}
	for _, a := range alts {
		if !reach[a.Rule] {
			continue
		}
		total++
		if a.Kind == ActUnknown {
			unknown++
			byRule[a.Rule]++
		}
	}
	fmt.Fprintf(&b, "reachable from %s: rules=%d alternatives=%d unknown=%d\n", strings.Join(roots, ","), len(reach), total, unknown)
	for _, kv := range top(byRule, 80) {
		var shape string
		for _, a := range alts {
			if a.Rule == kv.k && a.Kind == ActUnknown {
				shape = a.Action
				break
			}
		}
		if len(shape) > 90 {
			shape = shape[:90]
		}
		fmt.Fprintf(&b, "  %3d  %-36s %s\n", kv.n, kv.k, shape)
	}
	return b.String()
}

// ActionReport summarizes how far the actions are read: counts per kind, the NEW_PTN
// arguments that are not plain children, and the unknown actions grouped by shape.
func ActionReport(alts []Alt) string {
	var b strings.Builder
	counts := map[ActionKind]int{}
	argPlain, argOther := 0, 0
	otherArgs := map[string]int{}
	unknown := map[string][]string{}
	for _, a := range alts {
		counts[a.Kind]++
		if a.Kind == ActNew || a.Kind == ActListNew {
			for _, g := range a.Args {
				if g.Text == "" || g.Text == "@$" || g.Text == "nullptr" || g.Text == "NULL" || regexp.MustCompile(`^(@\d+|-?\d+|true|false|[A-Z][A-Za-z_0-9]*(::[A-Za-z_0-9]+)*)$`).MatchString(g.Text) {
					argPlain++
				} else {
					argOther++
					otherArgs[regexp.MustCompile(`\d+`).ReplaceAllLiteralString(g.Text, "N")]++
				}
			}
		}
		if a.Kind == ActUnknown {
			shape := regexp.MustCompile(`\$\d+`).ReplaceAllLiteralString(a.Action, "$N")
			if len(shape) > 70 {
				shape = shape[:70]
			}
			unknown[shape] = append(unknown[shape], a.Rule)
		}
	}
	fmt.Fprintf(&b, "alternatives=%d\n", len(alts))
	for k := ActUnknown; k <= ActStruct; k++ {
		fmt.Fprintf(&b, "  %-12s %d\n", k, counts[k])
	}
	fmt.Fprintf(&b, "NEW_PTN args: plain=%d other=%d\n", argPlain, argOther)
	for _, kv := range top(otherArgs, 15) {
		fmt.Fprintf(&b, "  %4d  %s\n", kv.n, kv.k)
	}
	fmt.Fprintf(&b, "unknown shapes (rules):\n")
	shapes := map[string]int{}
	for s, rs := range unknown {
		shapes[s] = len(rs)
	}
	for _, kv := range top(shapes, 40) {
		rs := unknown[kv.k]
		sort.Strings(rs)
		fmt.Fprintf(&b, "  %4d  %-70s  %s\n", kv.n, kv.k, strings.Join(uniq(rs)[:min(3, len(uniq(rs)))], ","))
	}
	return b.String()
}

type kv struct {
	k string
	n int
}

func top(m map[string]int, n int) []kv {
	var out []kv
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n > out[j].n || out[i].n == out[j].n && out[i].k < out[j].k })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func uniq(s []string) []string {
	var out []string
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
