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
	// Guards are the checks the server's action made and this reader dropped (`if (cond)
	// MYSQL_YYABORT`, `if (cond) my_error(...)`): errors a parse-time check layer must
	// reproduce.
	Guards []string
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
			// $N in the action counts mid-rule actions as symbols; the CST has no node for
			// them, so posMap[N] is the child index (1-based) or 0 for a mid-rule action.
			posMap := []int{0}
			for i < len(toks) {
				t := toks[i]
				if t.kind == tkBar || t.kind == tkSemi {
					break
				}
				if t.kind == tkID && i+1 < len(toks) && toks[i+1].kind != tkColon || t.kind == tkLit {
					i++
					syms = append(syms, t.text)
					posMap = append(posMap, len(syms))
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
					} else {
						posMap = append(posMap, 0)
					}
				}
			}
			a := classify(lhs.text, idx, syms, action)
			if len(posMap) > len(syms)+1 && a.Kind != ActUnknown && a.Kind != ActDefault {
				a = remap(a, posMap)
			}
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
	reCComment       = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reLineCmt        = regexp.MustCompile(`//[^\n]*`)
	reSpace          = regexp.MustCompile(`\s+`)
	reNullCheck      = regexp.MustCompile(`if\(\$\$==(nullptr|NULL)\)MYSQL_YYABORT;`)
	reDigest         = regexp.MustCompile(`Lex_input_stream\*lip=YYLIP;lip->reduce_digest_token\([^)]*\);`)
	reFoundSemicolon = regexp.MustCompile(`YYLIP->found_semicolon=nullptr;`)
	// charset conversion of a token: the value is the token
	reConvert       = regexp.MustCompile(`^THD\*thd=YYTHD;if\(thd->charset_is_\w+\)(\{.*)?\$\$=\$1;.*convert_string\(&\$\$,.*MYSQL_YYABORT;\}$`)
	reStrmake       = regexp.MustCompile(`^(THD\*thd=YYTHD;)?\$\$\.str=(thd|YYTHD)->strmake\(\$(\d+)\.str,\$\d+\.length\);if\(\$\$\.str==nullptr\)MYSQL_YYABORT;\$\$\.length=\$\d+\.length;$`)
	reParseTree     = regexp.MustCompile(`^\*parse_tree=\$(\d+);$`)
	reContextualize = regexp.MustCompile(`^\$\$=nullptr;CONTEXTUALIZE\(\$(\d+)\);$`)
	reHelperNew     = regexp.MustCompile(`^\$\$=([a-z][a-z_0-9]*)\((.*)\);$`)
	reBraceNew      = regexp.MustCompile(`^\$\$=([A-Z][A-Za-z_0-9]*)\{(.*)\};$`)
	reDequeNew      = regexp.MustCompile(`^\$\$=new\(YYMEM_ROOT\)[A-Za-z_0-9<>*]+\(YYMEM_ROOT\);((\$\$->push_back\(\$\d+\);)+)$`)
	reLexSet        = regexp.MustCompile(`^(LEX\*lex=Lex;|THD\*thd=YYTHD;LEX\*lex=thd->lex;)?((lex|Lex|YYTHD->lex)->[A-Za-z_.]+\|?=[^;]+;)+$`)
	reLexSetField   = regexp.MustCompile(`(?:lex|Lex|YYTHD->lex)->([A-Za-z_.]+)\|?=([^;]+);`)
	reBraceAnon     = regexp.MustCompile(`^\$\$=\{(.*)\};$`)
	reNewList       = regexp.MustCompile(`^\$\$=(?:NEW_PTN|new\(YYMEM_ROOT\))(?:List|Mem_root_array|mem_root_deque)<[^>]*>(?:\(YYMEM_ROOT\))?;((?:if\((?:\$\$==nullptr\|\|)?)?\$\$->push_(?:back|front)\(\$\d+(?:\.\w+)?\)\)?(?:MYSQL_YYABORT)?;)+$`)
	reNewListPB     = regexp.MustCompile(`\$\$->push_(back|front)\(\$(\d+)(\.\w+)?\)`)
	reNewListVal    = regexp.MustCompile(`^\$\$\.init\((?:YYMEM_ROOT|YYTHD->mem_root)\);(?:if\(\$\$\.push_(?:back|front)\((?:to_lex_cstring\()?\$(\d+)\)?\)\)MYSQL_YYABORT;)?$`)
	reFlatten       = regexp.MustCompile(`^\$\$=flatten_associative_operator<(Item_cond_[a-z]+),[^>]*>\(YYMEM_ROOT,@\$,\$(\d+),\$(\d+)\);(if\(\$\$!=nullptr\)\$\$->m_pos=@\$;)?$`)
	reNewPlain      = regexp.MustCompile(`^\$\$=new(?:\(YYMEM_ROOT\))?([A-Z][A-Za-z_0-9]*)\((.*)\);(\$\$->m_pos=@\$;)?$`)
	reNewFieldList  = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)\(@\$(?:,YYMEM_ROOT)?\);if\(\$\$==nullptr\|\|\$\$->push_back\(&?\$(\d+)(->[a-z_]+)?\)\)MYSQL_YYABORT;$`)
	reSpGuard       = regexp.MustCompile(`if\(lex->sphead\)\{[^{}]*MYSQL_YYABORT;\}`)
	reMsgGuard      = regexp.MustCompile(`if\([^{};]*\)my_(message|error)\([^;]*\);`)
	reStructExt     = regexp.MustCompile(`^\$\$=\$(\d+);((\$\$\.\w+=[^;]+;)+)$`)
	reAllocNew      = regexp.MustCompile(`^if\(!\(\$\$=([A-Za-z_0-9]+)::alloc\((.*)\)\)\)MYSQL_YYABORT;$`)
	reCastWrap      = regexp.MustCompile(`(?:to_lex_cstring|static_cast<[a-z_0-9:]+>)\((\$\d+(?:\.str)?)\)`)
	reGuardBlock    = regexp.MustCompile(`if\([^{};]*\)\{[^{}]*MYSQL_YYABORT;\}`)
	reGuardStmt     = regexp.MustCompile(`if\([^{};]*\)MYSQL_YYABORT;`)
	reNewListPlain  = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)\(@\$(?:,YYMEM_ROOT)?\);(?:if\((?:\$\$==nullptr\|\|)?)?\$\$->push_back\(&?\$(\d+)(->[a-z_]+)?\)\)?(?:MYSQL_YYABORT)?;$`)
	reAppendOnly    = regexp.MustCompile(`^if\(\$\$->push_(back|front)\(&?\$(\d+)(?:->\w+)?\)\)MYSQL_YYABORT;(\$\$->m_pos=@\$;)?$`)
	reSetCall       = regexp.MustCompile(`\$\$\.([\w.]+)\.set\(([^()]*)\);`)
	reInitCall      = regexp.MustCompile(`\$\$(\.[\w.]+)?\.init\(\);`)
	reNumberHex     = regexp.MustCompile(`^\$\$=\((?:ulong|ulonglong|int|uint|longlong)\)my_strtoll\(\$(\d+)\.str,nullptr,16\);$`)
	reAtol          = regexp.MustCompile(`^\$\$=atol\(\$(\d+)\.str\);$`)
	reNullStruct    = regexp.MustCompile(`^\$\$=(?:LEX_STRING|LEX_CSTRING)\{nullptr,0\};$`)
	reAppendValPre  = regexp.MustCompile(`^if\(\$(\d+)\.push_back\(\$(\d+)\)\)MYSQL_YYABORT;\$\$=\$(\d+);$`)
	reAppendPlain   = regexp.MustCompile(`^\$\$=\$(\d+);\$\$->push_(back|front)\(\$(\d+)\);(\$\$->m_pos=@\$;)?$`)
	reNullValue     = regexp.MustCompile(`^\$\$=(null_lex_str|NULL_STR|NULL_CSTR|EMPTY_CSTR|EMPTY_STR);$`)
	rePass          = regexp.MustCompile(`^\$\$=\$(\d+);$`)
	reConst         = regexp.MustCompile(`^\$\$=(-?\d+|true|false|&?[A-Za-z][A-Za-z_0-9]*(::[A-Za-z_0-9]+)*);$`)
	reNew           = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)(<[^>]*>)?\(([^;]*)\);$`)
	reListAppend    = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reListAppend2   = regexp.MustCompile(`^\$(\d+)->push_(back|front)\(\$(\d+)\);\$\$=\$(\d+);$`)
	reListNew       = regexp.MustCompile(`^\$\$=NEW_PTN([A-Za-z_0-9]+)(<[^>]*>)?\((.*?)\);if\(\$\$==(nullptr|NULL)\|\|\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	rePassPos       = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$!=nullptr\)\$\$->m_pos=@\$;$`)
	rePassField     = regexp.MustCompile(`^\$\$=(to_lex_cstring\()?\$(\d+)(\.str|\.node)?\)?;$`)
	reItemize       = regexp.MustCompile(`^ITEMIZE\(\$(\d+),&\$\$\);$`)
	reFlags         = regexp.MustCompile(`^\$\$=\$(\d+)\|\$(\d+);$`)
	reNumber        = regexp.MustCompile(`^interror;\$\$=\((ulong|ulonglong|int|uint|longlong)\)my_strtoll10\(\$(\d+)\.str,nullptr,&error\);(if\(error!=0\)\{[^}]*\})?$`)
	reAppend3       = regexp.MustCompile(`^(if\(\$(\d+)==nullptr\|\|\$(\d+)->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;|if\(\$(\d+)->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;|\$(\d+)->push_(back|front)\(\$(\d+)\);)\$\$=\$(\d+);(\$\$->m_pos=@\$;)?$`)
	reAppendDflt    = regexp.MustCompile(`^if\(\$\$->push_(back|front)\(&?\$(\d+)(?:->\w+)?\)\)MYSQL_YYABORT;\$\$=\$(\d+);(\$\$->m_pos=@\$;)?$`)
	reAppendVal     = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$\.push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reAppendNull    = regexp.MustCompile(`^\$\$=\$(\d+);if\(\$\$==nullptr\|\|\$\$->push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;$`)
	reListInitVal   = regexp.MustCompile(`^\$\$\.init\(YYMEM_ROOT\);(if\(\$\$\.push_(back|front)\(\$(\d+)\)\)MYSQL_YYABORT;)?$`)
	reStruct        = regexp.MustCompile(`^(\$\$\.[A-Za-z_0-9]+=[^;]+;)+$`)
	reStructField   = regexp.MustCompile(`\$\$\.([A-Za-z_0-9]+)=([^;]+);`)
	reArgChild      = regexp.MustCompile(`^\$(\d+)$`)
	reArgField      = regexp.MustCompile(`^\$(\d+)((?:\.|->)[A-Za-z_0-9.]+?)(?:\.get_or_default\(\))?$`)
)

// normalize strips comments and whitespace so that the shapes can be matched textually;
// it also returns the error guards it removed.
func normalize(action string) (string, []string) {
	a := reCComment.ReplaceAllString(action, "")
	a = reLineCmt.ReplaceAllString(a, "")
	a = strings.TrimSpace(a)
	for strings.HasPrefix(a, "{") && strings.HasSuffix(a, "}") { // `{ { ... } }`
		a = strings.TrimSpace(a[1 : len(a)-1])
	}
	a = reSpace.ReplaceAllString(a, "")
	a = reNullCheck.ReplaceAllString(a, "")
	a = stripCalls(a, "push_warning(", "push_warning_printf(", "push_deprecated_warn(", "push_deprecated_warn_no_replacement(",
		"warn_on_deprecated_user_defined_collation(", "warn_about_deprecated_national(", "warn_about_deprecated_binary(", "DBUG_EXECUTE_IF(", "MYSQL_YYABORT_UNLESS(", "MAKE_CMD_DDL_DUMMY(")
	var guards []string
	for _, re := range []*regexp.Regexp{reSpGuard, reMsgGuard} {
		guards = append(guards, re.FindAllString(a, -1)...)
		a = re.ReplaceAllString(a, "")
	}
	a = reCastWrap.ReplaceAllString(a, "$1")
	var g2 []string
	a, g2 = stripGuards(a)
	guards = append(guards, g2...)
	a = reSetCall.ReplaceAllString(a, "$$$$.$1=$2;") // `$$.algo.set(x)` -> `$$.algo=x`
	a = reInitCall.ReplaceAllString(a, "")           // `$$.init()`, `$$.flags.init()`
	a = reDigest.ReplaceAllString(a, "")
	a = reFoundSemicolon.ReplaceAllString(a, "")
	return a, guards
}

// stripGuards removes error checks: `if (cond) MYSQL_YYABORT;` and `if (cond) { ...
// MYSQL_YYABORT; }` whose condition does not build anything (no push_back, no $$=).
func stripGuards(a string) (string, []string) {
	var removed []string
	for _, re := range []*regexp.Regexp{reGuardBlock, reGuardStmt} {
		a = re.ReplaceAllStringFunc(a, func(m string) string {
			cond := m[strings.Index(m, "(")+1:]
			if strings.Contains(cond, "push_") || strings.Contains(cond, "$$") {
				return m
			}
			removed = append(removed, m)
			return ""
		})
	}
	return a, removed
}

// stripCalls removes statements that are a call to one of the named functions (warnings
// and other bookkeeping the server does besides building the tree).
func stripCalls(a string, names ...string) string {
	for {
		changed := false
		for _, name := range names {
			i := strings.Index(a, name)
			if i < 0 || (i > 0 && a[i-1] != ';' && a[i-1] != '}') {
				continue
			}
			depth := 0
			j := i + len(name) - 1
			for ; j < len(a); j++ {
				if a[j] == '(' {
					depth++
				} else if a[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j+1 < len(a) && a[j+1] == ';' {
				a = a[:i] + a[j+2:]
				changed = true
			}
		}
		if !changed {
			return a
		}
	}
}

func classify(rule string, idx int, syms []string, action string) Alt {
	norm, guards := normalize(action)
	a := Alt{Rule: rule, Index: idx, Syms: syms, Action: norm, Guards: guards}
	n := a.Action
	switch {
	case action == "":
		a.Kind = ActDefault
	case n == "$$=nullptr;" || n == "$$=NULL;" || n == "$$={};":
		a.Kind = ActEmpty
	case !strings.Contains(n, "$"):
		// only side effects on the server's state (or nothing left after stripping):
		// the value is bison's default, the first child
		a.Kind = ActDefault
	case reStructExt.MatchString(n):
		m := reStructExt.FindStringSubmatch(n)
		a.Kind = ActStruct
		a.Fields = []Field{{Name: "$base", Arg: Arg{Child: atoi(m[1])}}}
		for _, f := range reStructField.FindAllStringSubmatch(m[2], -1) {
			a.Fields = append(a.Fields, Field{Name: f[1], Arg: parseArgs(f[2])[0]})
		}
	case reAllocNew.MatchString(n):
		m := reAllocNew.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = parseArgs(m[2])
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
	case reNullValue.MatchString(n):
		a.Kind = ActEmpty
	case reConvert.MatchString(n), reStrmake.MatchString(n):
		a.Kind = ActPass
		a.Args = []Arg{{Child: 1}}
	case reParseTree.MatchString(n):
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(reParseTree.FindStringSubmatch(n)[1])}}
	case reContextualize.MatchString(n):
		a.Kind = ActPass
		a.Args = []Arg{{Child: atoi(reContextualize.FindStringSubmatch(n)[1])}}
	case reDequeNew.MatchString(n):
		a.Kind = ActListNew
		for _, m := range regexp.MustCompile(`\$\$->push_back\(\$(\d+)\);`).FindAllStringSubmatch(n, -1) {
			a.Args = append(a.Args, Arg{Child: atoi(m[1])})
		}
	case reLexSet.MatchString(n):
		// legacy: the alternative sets fields of the statement's LEX; keep them as a struct
		a.Kind = ActStruct
		for _, f := range reLexSetField.FindAllStringSubmatch(n, -1) {
			a.Fields = append(a.Fields, Field{Name: f[1], Arg: parseArgs(f[2])[0]})
		}
	case reFlags.MatchString(n):
		m := reFlags.FindStringSubmatch(n)
		a.Kind = ActFlags
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[2])}}
	case reNumber.MatchString(n):
		a.Kind = ActNumber
		a.Args = []Arg{{Child: atoi(reNumber.FindStringSubmatch(n)[2])}}
	case reNumberHex.MatchString(n):
		a.Kind = ActNumber
		a.Const = "16"
		a.Args = []Arg{{Child: atoi(reNumberHex.FindStringSubmatch(n)[1])}}
	case reAtol.MatchString(n):
		a.Kind = ActNumber
		a.Args = []Arg{{Child: atoi(reAtol.FindStringSubmatch(n)[1])}}
	case reNullStruct.MatchString(n):
		a.Kind = ActEmpty
	case reAppendDflt.MatchString(n):
		m := reAppendDflt.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: atoi(m[3])}, {Child: atoi(m[2])}}
	case reAppendValPre.MatchString(n):
		m := reAppendValPre.FindStringSubmatch(n)
		if m[1] == m[3] {
			a.Kind = ActListAppend
			a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[2])}}
		}
	case reAppendPlain.MatchString(n):
		m := reAppendPlain.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: atoi(m[1])}, {Child: atoi(m[3])}}
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
	case reHelperNew.MatchString(n) && reHelperNew.FindStringSubmatch(n)[1] != "new":
		// a helper that builds the node (create_func_cast, make_index_engine_attribute ...)
		m := reHelperNew.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = parseArgs(m[2])
	case reBraceNew.MatchString(n):
		m := reBraceNew.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = parseArgs(m[2])
	case reBraceAnon.MatchString(n):
		// `$$= {$1, false}`: a by-value struct initialized positionally
		a.Kind = ActStruct
		for i, g := range parseArgs(reBraceAnon.FindStringSubmatch(n)[1]) {
			a.Fields = append(a.Fields, Field{Name: fmt.Sprintf("%d", i), Arg: g})
		}
	case reNewList.MatchString(n):
		a.Kind = ActListNew
		for _, m := range reNewListPB.FindAllStringSubmatch(n, -1) {
			g := Arg{Child: atoi(m[2]), Field: m[3]}
			if m[1] == "front" {
				a.Args = append([]Arg{g}, a.Args...)
			} else {
				a.Args = append(a.Args, g)
			}
		}
	case reNewListVal.MatchString(n):
		a.Kind = ActListNew
		if m := reNewListVal.FindStringSubmatch(n); m[1] != "" {
			a.Args = []Arg{{Child: atoi(m[1])}}
		}
	case reNewListPlain.MatchString(n):
		m := reNewListPlain.FindStringSubmatch(n)
		a.Kind = ActListNew
		a.Class = m[1]
		a.Args = []Arg{{Child: atoi(m[2]), Field: m[3]}}
	case reAppendOnly.MatchString(n):
		// `if ($$->push_back($3)) MYSQL_YYABORT;` on top of bison's default $$ = $1
		m := reAppendOnly.FindStringSubmatch(n)
		a.Kind = ActListAppend
		a.Args = []Arg{{Child: 1}, {Child: atoi(m[2])}}
	case reNewFieldList.MatchString(n):
		m := reNewFieldList.FindStringSubmatch(n)
		a.Kind = ActListNew
		a.Class = m[1]
		a.Args = []Arg{{Child: atoi(m[2]), Field: m[3]}}
	case reFlatten.MatchString(n):
		m := reFlatten.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = []Arg{{Child: atoi(m[2])}, {Child: atoi(m[3])}}
	case reNewPlain.MatchString(n):
		m := reNewPlain.FindStringSubmatch(n)
		a.Kind = ActNew
		a.Class = m[1]
		a.Args = parseArgs(m[2])
	}
	return a
}

// remap renumbers $N references from RHS positions (with mid-rule actions) to CST child
// indexes; a reference to a mid-rule action's own value makes the alternative unknown.
func remap(a Alt, posMap []int) Alt {
	fix := func(g *Arg) bool {
		if g.Child == 0 {
			return true
		}
		if g.Child >= len(posMap) || posMap[g.Child] == 0 {
			return false
		}
		g.Child = posMap[g.Child]
		return true
	}
	for i := range a.Args {
		if !fix(&a.Args[i]) {
			a.Kind = ActUnknown
			return a
		}
	}
	for i := range a.Fields {
		if !fix(&a.Fields[i].Arg) {
			a.Kind = ActUnknown
			return a
		}
	}
	return a
}

func parseArgs(s string) []Arg {
	var out []Arg
	for _, x := range splitTopLevel(s) {
		x = strings.TrimPrefix(x, "&") // passing a child by address
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
		switch s[i] { // '<' '>' are not brackets here: `->` is common, templates are not
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
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

// GuardReport lists the error guards dropped from the alternatives reachable from roots
// (all alternatives when roots is empty): the parse-time checks a later layer owes.
func GuardReport(alts []Alt, roots []string) string {
	reach := map[string]bool{}
	if len(roots) > 0 {
		reach = Reach(alts, roots)
	}
	var b strings.Builder
	n := 0
	for _, a := range alts {
		if len(roots) > 0 && !reach[a.Rule] {
			continue
		}
		for _, g := range a.Guards {
			n++
			if len(g) > 110 {
				g = g[:110]
			}
			fmt.Fprintf(&b, "%s/%d  %s\n", a.Rule, a.Index, g)
		}
	}
	return fmt.Sprintf("dropped guards: %d\n", n) + b.String()
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
