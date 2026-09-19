package parsegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The names of things. The actions pass children positionally into constructors
// (`NEW_PTN PT_query_specification(@$, $1, $2, ...)`) and into by-value structs
// (`$$= {$1, false}`); the server's headers say what each position is called. This file
// reads those names so that the AST can be addressed by them.

// TypeInfo is one member of the parser's value union (parser_yystype.h): the C++ type
// and, for struct-valued members, the field names in declaration order.
type TypeInfo struct {
	Type   string
	Fields []string
}

// ReadTypeTags returns nonterminal -> %type tag from the grammar's declarations.
func ReadTypeTags(decl string) map[string]string {
	out := map[string]string{}
	for _, m := range reTypeTagged.FindAllStringSubmatch(decl, -1) {
		for name := range strings.FieldsSeq(m[2]) {
			if isIdentStart(name[0]) {
				out[name] = m[1]
			}
		}
	}
	return out
}

var reTypeTagged = regexp.MustCompile(`(?m)^%type\s*<([^>]+)>((?:[^\n]*)(?:\n[ \t]+[^\n%][^\n]*)*)`)

// ReadYYStype reads the value union: tag -> type and fields.
func ReadYYStype(src string) (map[string]TypeInfo, error) {
	src = stripComments(src)
	structs := map[string][]string{} // named struct -> fields, from this header
	for _, m := range reStructDef.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		body, _ := balanced(src, m[4]-1, '{', '}')
		structs[name] = fieldNames(body)
	}
	i := strings.Index(src, "union MY_SQL_PARSER_STYPE")
	if i < 0 {
		return nil, fmt.Errorf("parser_yystype.h: no union MY_SQL_PARSER_STYPE")
	}
	body, err := balanced(src, strings.Index(src[i:], "{")+i, '{', '}')
	if err != nil {
		return nil, err
	}
	out := map[string]TypeInfo{}
	for _, decl := range splitDecls(body) {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		if strings.HasPrefix(decl, "struct") && strings.Contains(decl, "{") {
			// anonymous struct member: `struct { a; b; } name`
			inner, _ := balanced(decl, strings.Index(decl, "{"), '{', '}')
			name := strings.TrimSpace(decl[strings.LastIndex(decl, "}")+1:])
			out[name] = TypeInfo{Type: "struct", Fields: fieldNames(inner)}
			continue
		}
		// `Type a, b;` or `Type name;`
		typ, names := splitTypeNames(decl)
		for _, n := range names {
			out[n] = TypeInfo{Type: typ, Fields: structs[typ]}
		}
	}
	return out, nil
}

var reStructDef = regexp.MustCompile(`(?m)^struct\s+([A-Za-z_][A-Za-z_0-9]*)\s*(\{)`)

// fieldNames lists the declared names in a struct body, in order, skipping methods,
// nested types and access specifiers.
func fieldNames(body string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '(':
			depth++
		case '}', ')':
			depth--
		case ';':
			if depth == 0 {
				decl := strings.TrimSpace(body[start:i])
				start = i + 1
				if decl == "" || strings.Contains(decl, "(") || strings.HasPrefix(decl, "using ") ||
					strings.HasPrefix(decl, "static ") || strings.HasPrefix(decl, "typedef ") || strings.HasPrefix(decl, "return ") {
					continue
				}
				if j := strings.LastIndex(decl, "}"); j >= 0 { // nested `struct {..} name`
					decl = strings.TrimSpace(decl[j+1:])
					out = append(out, decl)
					continue
				}
				_, names := splitTypeNames(decl)
				out = append(out, names...)
			}
		}
	}
	return out
}

// splitTypeNames splits `const T *a, *b = x` into the type and the declared names.
func splitTypeNames(decl string) (string, []string) {
	decl = strings.TrimSpace(strings.TrimSuffix(decl, ";"))
	// drop initializers
	parts := splitTopLevel(decl)
	var names []string
	typ := ""
	for k, p := range parts {
		p = strings.TrimSpace(p)
		if eq := strings.Index(p, "="); eq >= 0 {
			p = strings.TrimSpace(p[:eq])
		}
		if br := strings.Index(p, "{"); br >= 0 {
			p = strings.TrimSpace(p[:br])
		}
		if k == 0 {
			// type is everything up to the last identifier
			m := reLastIdent.FindStringSubmatchIndex(p)
			if m == nil {
				return decl, nil
			}
			typ = strings.TrimSpace(strings.TrimRight(p[:m[2]], " *&"))
			typ = strings.TrimPrefix(typ, "const ")
			typ = strings.TrimPrefix(typ, "enum ")
			typ = strings.TrimPrefix(typ, "struct ")
			names = append(names, p[m[2]:m[3]])
		} else {
			m := reLastIdent.FindStringSubmatch(p)
			if m != nil {
				names = append(names, m[1])
			}
		}
	}
	return typ, names
}

var reLastIdent = regexp.MustCompile(`([A-Za-z_][A-Za-z_0-9]*)\s*(?:\[[^\]]*\])?\s*$`)

// splitDecls splits a struct/union body into member declarations at top-level ';'.
func splitDecls(body string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '(':
			depth++
		case '}', ')':
			depth--
		case ';':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	return out
}

// balanced returns the text between the bracket at open (inclusive of neither) and its match.
func balanced(s string, open int, lb, rb byte) (string, error) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case lb:
			depth++
		case rb:
			depth--
			if depth == 0 {
				return s[open+1 : i], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced %c at %d", lb, open)
}

var (
	reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reLineComment  = regexp.MustCompile(`//[^\n]*`)
)

func stripComments(s string) string {
	return reLineComment.ReplaceAllString(reBlockComment.ReplaceAllString(s, ""), "")
}

// Ctor is one constructor (or builder function) signature: the parameter names in order,
// how many have no default, and whether the first is the parse position.
type Ctor struct {
	Params   []string
	Required int
	HasPos   bool
}

// ReadCtors scans header files for the constructors of the named classes (and the
// declarations of builder functions of the same names) and returns their parameter names.
func ReadCtors(files []string, names map[string]bool) (map[string][]Ctor, error) {
	out := map[string][]Ctor{}
	aliases := map[string]string{} // typedef / using alias -> the template or class it names
	structs := map[string][]string{}
	var sources []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		src := stripComments(string(b))
		sources = append(sources, src)
		for _, m := range reTypedef.FindAllStringSubmatch(src, -1) {
			if names[m[2]] {
				aliases[m[2]] = m[1]
			}
		}
		for _, m := range reTypedefPlain.FindAllStringSubmatch(src, -1) {
			if names[m[2]] {
				aliases[m[2]] = m[1]
			}
		}
		for _, m := range reUsingAlias.FindAllStringSubmatch(src, -1) {
			if names[m[1]] {
				aliases[m[1]] = m[2]
			}
		}
		for _, m := range reStructDef.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[2]:m[3]]
			if names[name] {
				if body, err := balanced(src, m[4]-1, '{', '}'); err == nil {
					structs[name] = fieldNames(body)
				}
			}
		}
	}
	// aliases of templates: read the template's constructors under the alias name
	want := map[string]bool{}
	for n := range names {
		want[n] = true
	}
	for _, target := range aliases {
		want[target] = true
	}
	for _, src := range sources {
		for name := range want {
			if !strings.Contains(src, name+"(") && !strings.Contains(src, name+" (") {
				continue
			}
			re := ctorRegexp(name)
			for _, m := range re.FindAllStringIndex(src, -1) {
				open := m[1] - 1
				params, err := balanced(src, open, '(', ')')
				if err != nil {
					continue
				}
				// a declaration, not a call: what follows is `;`, `:` (init list), `{`, `const`, `override`, `= default`
				rest := strings.TrimSpace(src[open+len(params)+2:])
				if rest == "" || !(rest[0] == ';' || rest[0] == ':' || rest[0] == '{' || strings.HasPrefix(rest, "const") || strings.HasPrefix(rest, "override") || strings.HasPrefix(rest, "=")) {
					continue
				}
				c := Ctor{}
				if strings.TrimSpace(params) != "" {
					for k, p := range splitTopLevel(params) {
						p = strings.TrimSpace(p)
						hasDefault := false
						if eq := strings.Index(p, "="); eq >= 0 {
							p = strings.TrimSpace(p[:eq])
							hasDefault = true
						}
						if k == 0 && strings.Contains(p, "POS") {
							c.HasPos = true
						}
						if !hasDefault {
							c.Required = k + 1
						}
						if pm := reLastIdent.FindStringSubmatch(p); pm != nil && !isTypeOnly(p, pm[1]) {
							c.Params = append(c.Params, pm[1])
						} else {
							c.Params = append(c.Params, "")
						}
					}
				}
				out[name] = append(out[name], c)
			}
		}
	}
	for alias, target := range aliases {
		if len(out[alias]) == 0 {
			out[alias] = out[target]
		}
	}
	// a plain struct built with braces takes its fields in order
	for name, fields := range structs {
		if len(out[name]) == 0 && len(fields) > 0 {
			out[name] = []Ctor{{Params: fields}} // any prefix may be brace-initialized
		}
	}
	return out, nil
}

var (
	reTypedef      = regexp.MustCompile(`(?s)typedef\s+([A-Za-z_][A-Za-z_0-9]*)\s*<[^;]*?>\s*([A-Za-z_][A-Za-z_0-9]*)\s*;`)
	reTypedefPlain = regexp.MustCompile(`typedef\s+(?:struct\s+)?([A-Za-z_][A-Za-z_0-9]*)\s+([A-Za-z_][A-Za-z_0-9]*)\s*;`)
	reUsingAlias   = regexp.MustCompile(`using\s+([A-Za-z_][A-Za-z_0-9]*)\s*=\s*([A-Za-z_][A-Za-z_0-9]*)\s*<`)
)

// isTypeOnly reports a parameter declared without a name (`Item *`, `const POS &`).
func isTypeOnly(p, last string) bool {
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), last))
	if t == "" {
		return true // just a type name
	}
	return strings.HasSuffix(t, "::") || strings.HasSuffix(t, "<") || strings.HasSuffix(t, ",")
}

// ctorRegexp matches a declaration of Name( at the start of a declaration: after a newline
// with optional specifiers, or after a return type for builder functions.
func ctorRegexp(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*(?:(?:explicit|inline|constexpr|static|virtual|friend)\s+)*(?:[A-Za-z_][A-Za-z_0-9:<>*&, ]*\s+[*&]*)?\b` + regexp.QuoteMeta(name) + `\s*\(`)
}

// HeaderFiles lists the server headers worth scanning for constructors.
func HeaderFiles(src string) ([]string, error) {
	var out []string
	for _, pat := range []string{"sql/*.h", "sql/auth/*.h", "include/*.h", "sql-common/*.h"} {
		m, err := filepath.Glob(filepath.Join(src, pat))
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	sort.Strings(out)
	return out, nil
}

// Names resolves, for every ActNew / ActListNew alternative, the constructor parameter
// names matching its argument count, and for every positional struct the field names of
// the rule's type. It reports what it could not resolve.
type Names struct {
	Ctor    map[string][]Ctor   // class -> constructors
	Types   map[string]TypeInfo // %type tag -> type
	Tags    map[string]string   // rule -> tag
	Missing map[string]int      // class -> alternatives with no matching constructor
}

// ParamNames returns the parameter names for class with n arguments, or nil. MySQL's
// constructors name a parameter after the member it initializes with an `_arg` suffix;
// the suffix is dropped.
func (nm *Names) ParamNames(class string, n int, hasPos bool) []string {
	var best []string
	for _, c := range nm.Ctor[class] {
		if c.HasPos != hasPos {
			continue
		}
		if len(c.Params) == n {
			best = c.Params
			break
		}
		// a constructor with defaulted trailing parameters also fits
		if len(c.Params) > n && (n >= c.Required || c.Required == 0) && best == nil {
			best = c.Params[:n]
		}
	}
	if best == nil {
		return nil
	}
	out := make([]string, len(best))
	for i, p := range best {
		out[i] = strings.TrimSuffix(p, "_arg")
	}
	return out
}

// ReadNames gathers everything: type tags from the grammar declarations, the value union,
// and the constructors of every class the actions build.
func ReadNames(src string, yacc string, alts []Alt) (*Names, error) {
	sections := reSectionSep.FindAllStringIndex(yacc, -1)
	if len(sections) < 1 {
		return nil, fmt.Errorf("grammar: no %%%% section separator")
	}
	nm := &Names{Tags: ReadTypeTags(yacc[:sections[0][0]]), Missing: map[string]int{}}
	yy, err := os.ReadFile(filepath.Join(src, "sql", "parser_yystype.h"))
	if err != nil {
		return nil, err
	}
	nm.Types, err = ReadYYStype(string(yy))
	if err != nil {
		return nil, err
	}
	classes := map[string]bool{}
	for _, a := range alts {
		if (a.Kind == ActNew || a.Kind == ActListNew) && a.Class != "" {
			classes[a.Class] = true
		}
	}
	files, err := HeaderFiles(src)
	if err != nil {
		return nil, err
	}
	nm.Ctor, err = ReadCtors(files, classes)
	if err != nil {
		return nil, err
	}
	for _, a := range alts {
		if a.Kind == ActNew && a.Class != "" && nm.ParamNames(a.Class, len(a.Args), HasPosArg(a)) == nil {
			nm.Missing[a.Class]++
		}
	}
	return nm, nil
}

// HasPosArg reports whether the action passes the parse position first.
func HasPosArg(a Alt) bool {
	return len(a.Args) > 0 && a.Args[0].Child == 0 && (a.Args[0].Text == "@$" || strings.HasPrefix(a.Args[0].Text, "@"))
}

// Report lists the classes whose constructor could not be matched.
func (nm *Names) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "constructors: %d classes read, %d unresolved\n", len(nm.Ctor), len(nm.Missing))
	for _, kv := range top(nm.Missing, 60) {
		fmt.Fprintf(&b, "  %3d  %s\n", kv.n, kv.k)
	}
	return b.String()
}

// StructFields returns the field names of rule's value type when it is a struct.
func (nm *Names) StructFields(rule string) []string {
	tag, ok := nm.Tags[rule]
	if !ok {
		return nil
	}
	return nm.Types[tag].Fields
}
