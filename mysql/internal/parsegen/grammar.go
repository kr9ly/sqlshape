// Package parsegen turns the MySQL server's own grammar and lexer into a
// concrete-syntax-tree parser: it strips the semantic actions from
// sql/sql_yacc.yy, cuts the token functions out of sql/sql_lex.cc behind a
// small THD shim, and drives bison, the server's table generators and the C++
// compiler (natively and through emcc).
//
// Everything copied from the server source is generated; only the shim and the
// generator are hand-written. Every cut is anchored on a signature and fails
// loudly when the server source moves.
package parsegen

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func strconvQuote(s string) string { return strconv.Quote(s) }

// Grammar is the action-stripped grammar and the counts the spike established.
type Grammar struct {
	Text           string
	Rules          int
	Alternatives   int
	MidRuleActions int
	Start          string
	// Kinds names every CST node kind: the rules in order of definition, then the
	// terminals in order of first use. Index = the kind id the generated actions
	// pass to mk/L and the id the Go side decodes.
	Kinds []string
	// ValueTokens are the tokens whose value the lexer sets as a string (identifiers,
	// literals: `%type <lexer.lex_str>`); their leaves carry that value besides the span.
	ValueTokens []string
}

// kindID returns the id of name, adding it to Kinds when new.
func (g *Grammar) kindID(name string, seen map[string]int) int {
	if id, ok := seen[name]; ok {
		return id
	}
	id := len(g.Kinds)
	g.Kinds = append(g.Kinds, name)
	seen[name] = id
	return id
}

// StripGrammar rewrites sql_yacc.yy so that every alternative builds a generic
// CST node: terminals become leaves carrying their source span (via the
// location), nonterminals pass their node up. Token numbering, precedence and
// %expect are kept, so bison verifies the grammar is untouched.
func StripGrammar(src string) (*Grammar, error) {
	sections := reSectionSep.FindAllStringIndex(src, -1)
	if len(sections) < 1 {
		return nil, fmt.Errorf("grammar: no %%%% section separator")
	}
	decl := src[:sections[0][0]]
	rulesEnd := len(src)
	if len(sections) > 1 {
		rulesEnd = sections[1][0]
	}
	rules := src[sections[0][1]:rulesEnd]

	tokens := map[string]bool{}
	for _, m := range reTokenDecl.FindAllStringSubmatch(decl, -1) {
		for _, name := range strings.Fields(m[1]) { // one %token line may declare several
			if isIdentStart(name[0]) {
				tokens[name] = true
			}
		}
	}
	// tokens the lexer gives a string value: the unquoted identifier, the unescaped literal
	valueTokens := map[string]bool{}
	for _, m := range reLexStrType.FindAllStringSubmatch(decl, -1) {
		for _, name := range strings.Fields(m[1]) {
			if tokens[name] {
				valueTokens[name] = true
			}
		}
	}
	charsetTokens := map[string]bool{}
	for _, m := range reCharsetType.FindAllStringSubmatch(decl, -1) {
		for _, name := range strings.Fields(m[1]) {
			if tokens[name] {
				charsetTokens[name] = true
			}
		}
	}
	startM := reStart.FindStringSubmatch(decl)
	if startM == nil {
		return nil, fmt.Errorf("grammar: no %%start")
	}
	start := startM[1]

	d := decl
	d = rePrologue.ReplaceAllString(d, "")
	d = reParseParam.ReplaceAllString(d, "")
	d = reLexParam.ReplaceAllString(d, "")
	d = reApiPure.ReplaceAllString(d, "")
	d = reTypeDecl.ReplaceAllString(d, "") // api.value.type is one union; %type lines are dropped
	d = reTaggedDecl.ReplaceAllString(d, "$1")

	toks, err := lexRules(rules)
	if err != nil {
		return nil, err
	}
	g := &Grammar{Start: start}
	for name := range valueTokens {
		g.ValueTokens = append(g.ValueTokens, name)
	}
	sort.Strings(g.ValueTokens)
	seen := map[string]int{}
	for i := 0; i+1 < len(toks); i++ { // rules first, so that their ids are dense and stable
		if toks[i].kind == tkID && toks[i+1].kind == tkColon && (i == 0 || toks[i-1].kind != tkPrec) {
			g.kindID(toks[i].text, seen)
		}
	}
	var o strings.Builder
	i := 0
	for i < len(toks) {
		lhs := toks[i]
		if lhs.kind != tkID || i+1 >= len(toks) || toks[i+1].kind != tkColon {
			return nil, fmt.Errorf("grammar: expected rule head near %q", lhs.text)
		}
		i += 2
		g.Rules++
		fmt.Fprintf(&o, "%s:\n", lhs.text)
		first := true
		alt := 0
		for {
			var syms []string
			prec := ""
			mids := 0
			for i < len(toks) {
				t := toks[i]
				if t.kind == tkBar || t.kind == tkSemi {
					break
				}
				if t.kind == tkID && i+1 < len(toks) && toks[i+1].kind == tkColon {
					break // next rule head; the previous rule had no ';'
				}
				i++
				switch t.kind {
				case tkID, tkLit:
					syms = append(syms, t.text)
				case tkEmpty:
				case tkPrec:
					if i >= len(toks) {
						return nil, fmt.Errorf("grammar: %%prec without a token in %s", lhs.text)
					}
					prec = " %prec " + toks[i].text
					i++
				case tkAction:
					// final if followed by '|' or ';' (or a rule head); otherwise a mid-rule action
					if i < len(toks) && toks[i].kind != tkBar && toks[i].kind != tkSemi &&
						!(toks[i].kind == tkID && i+1 < len(toks) && toks[i+1].kind == tkColon) {
						syms = append(syms, "{}")
						mids++
					}
				}
			}
			g.Alternatives++
			g.MidRuleActions += mids
			var real, args []string
			for k, x := range syms {
				if x == "{}" {
					continue
				}
				real = append(real, x)
				if strings.HasPrefix(x, "'") || strings.HasPrefix(x, `"`) || tokens[x] {
					switch {
					case valueTokens[x]:
						args = append(args, fmt.Sprintf("LS(YYTHD, %d, &@%d, &$%d.lexer.lex_str)", g.kindID(x, seen), k+1, k+1))
					case charsetTokens[x]:
						args = append(args, fmt.Sprintf("LC(YYTHD, %d, &@%d, $%d.lexer.charset)", g.kindID(x, seen), k+1, k+1))
					default:
						args = append(args, fmt.Sprintf("L(YYTHD, %d, &@%d)", g.kindID(x, seen), k+1))
					}
				} else {
					if _, ok := seen[x]; !ok {
						return nil, fmt.Errorf("grammar: %s uses %s, which is neither a token nor a rule", lhs.text, x)
					}
					args = append(args, fmt.Sprintf("$%d.node", k+1))
				}
			}
			body := "%empty"
			if len(real) > 0 {
				body = strings.Join(syms, " ")
			}
			sep := "| "
			if first {
				sep = "  "
			}
			call := fmt.Sprintf(`mk(YYTHD, %d /* %s */, %d, %d`, g.kindID(lhs.text, seen), lhs.text, alt, len(real))
			alt++
			if len(args) > 0 {
				call += ", " + strings.Join(args, ", ")
			}
			call += ")"
			out := ""
			if lhs.text == start {
				out = " *out = $$.node;"
			}
			fmt.Fprintf(&o, "%s%s%s { $$.node = %s;%s }\n", sep, body, prec, call, out)
			first = false
			if i < len(toks) && toks[i].kind == tkBar {
				i++
				continue
			}
			if i < len(toks) && toks[i].kind == tkSemi {
				i++
			}
			break
		}
		o.WriteString(";\n\n")
	}
	g.Text = grammarPrologue + d + "\n%%\n\n" + o.String() + "\n%%\n"
	return g, nil
}

// cQuote renders a name as a C string literal (only ' " \ need care).
func cQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// KindsH is the C header naming every kind, for the CST builder and the probe driver.
func (g *Grammar) KindsH() string {
	var b strings.Builder
	b.WriteString("// Generated by parsegen from sql_yacc.yy: one name per CST node kind.\n#pragma once\n")
	fmt.Fprintf(&b, "#define KIND_COUNT %d\n#define KIND_FIRST_TERMINAL %d\n", len(g.Kinds), g.Rules)
	b.WriteString("static const char *const kind_names[KIND_COUNT] = {\n")
	for _, k := range g.Kinds {
		fmt.Fprintf(&b, "  %s,\n", cQuote(k))
	}
	b.WriteString("};\n")
	return b.String()
}

// KindsGo is the Go source naming every kind, for package mysqlparse.
func (g *Grammar) KindsGo(pkg string, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by parsegen from MySQL %s sql_yacc.yy. DO NOT EDIT.\n\npackage %s\n\n", version, pkg)
	fmt.Fprintf(&b, "// firstTerminal is the first kind that is a token rather than a rule.\nconst firstTerminal Kind = %d\n\n", g.Rules)
	b.WriteString("var kindNames = [...]string{\n")
	for _, k := range g.Kinds {
		fmt.Fprintf(&b, "\t%s,\n", strconvQuote(k))
	}
	b.WriteString("}\n")
	return b.String()
}

// ShapesGo is the Go source of the action table: for every (rule, alternative), what the
// server's action did with the children, as ReadActions read it. Package mysqlparse builds
// the AST from it.
func ShapesGo(pkg string, version string, kinds []string, alts []Alt) string {
	byRule := map[string][]Alt{}
	for _, a := range alts {
		byRule[a.Rule] = append(byRule[a.Rule], a)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by parsegen from MySQL %s sql_yacc.yy. DO NOT EDIT.\n\npackage %s\n\n", version, pkg)
	b.WriteString("// shapes[rule kind][alternative] is what the server's semantic action built.\nvar shapes = [...][]Shape{\n")
	for id, name := range kinds {
		as, ok := byRule[name]
		if !ok {
			break // terminals follow the rules
		}
		fmt.Fprintf(&b, "\t%d: { // %s\n", id, name)
		for _, a := range as {
			fmt.Fprintf(&b, "\t\t{Kind: Act%s", exportName(a.Kind.String()))
			if a.Class != "" {
				fmt.Fprintf(&b, ", Class: %s", strconv.Quote(a.Class))
			}
			if a.Const != "" {
				fmt.Fprintf(&b, ", Const: %s", strconv.Quote(a.Const))
			}
			if len(a.Args) > 0 {
				b.WriteString(", Args: []Arg{")
				for i, g := range a.Args {
					if i > 0 {
						b.WriteString(", ")
					}
					b.WriteString(argGo(g))
				}
				b.WriteString("}")
			}
			if len(a.Fields) > 0 {
				b.WriteString(", Fields: []Field{")
				for i, f := range a.Fields {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "{Name: %s, Arg: Arg%s}", strconv.Quote(f.Name), argGo(f.Arg))
				}
				b.WriteString("}")
			}
			b.WriteString("},\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func argGo(g Arg) string {
	switch {
	case g.Text != "":
		return fmt.Sprintf("{Text: %s}", strconv.Quote(g.Text))
	case g.Field != "":
		return fmt.Sprintf("{Child: %d, Field: %s}", g.Child, strconv.Quote(g.Field))
	default:
		return fmt.Sprintf("{Child: %d}", g.Child)
	}
}

// exportName turns "list-append" into "ListAppend".
func exportName(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '-' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

const grammarPrologue = `%{
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "cst.h"
#define YYINITDEPTH 100
#define YYMAXDEPTH 3200
%}
%code requires {
#include "cst.h"
#include "sql/lexer_yystype.h"
#include "sql/parse_location.h"
union CST_STYPE { Node *node; Lexer_yystype lexer; };
}
%define api.pure full
%define api.value.type { CST_STYPE }
%define api.location.type { MY_SQL_PARSER_LTYPE }
%locations
%parse-param { class THD *YYTHD } { Node **out }
%lex-param { class THD *YYTHD }
%code provides {
int my_sql_parser_lex(CST_STYPE *v, MY_SQL_PARSER_LTYPE *l, class THD *thd);
void my_sql_parser_error(MY_SQL_PARSER_LTYPE *l, class THD *thd, Node **out, const char *msg);
}
`

var (
	reSectionSep  = regexp.MustCompile(`(?m)^%%[ \t]*$`)
	reTokenDecl   = regexp.MustCompile(`(?m)^%token\s*(?:<[^>]*>)?\s+([^\n/]*)`)
	reStart       = regexp.MustCompile(`(?m)^%start\s+(\S+)`)
	rePrologue    = regexp.MustCompile(`(?s)%\{.*?%\}`)
	reParseParam  = regexp.MustCompile(`(?m)^%parse-param.*$`)
	reLexParam    = regexp.MustCompile(`(?m)^%lex-param.*$`)
	reApiPure     = regexp.MustCompile(`(?m)^%define api\.pure.*$`)
	reTypeDecl    = regexp.MustCompile(`(?m)^%type\b[^\n]*(\n[ \t]+[^\n%][^\n]*)*`)
	reLexStrType  = regexp.MustCompile(`(?m)^%type\s*<lexer\.lex_str>((?:[^\n]*)(?:\n[ \t]+[^\n%][^\n]*)*)`)
	reCharsetType = regexp.MustCompile(`(?m)^%type\s*<lexer\.charset>((?:[^\n]*)(?:\n[ \t]+[^\n%][^\n]*)*)`)
	reTaggedDecl  = regexp.MustCompile(`(?m)^(%token|%left|%right|%nonassoc|%precedence)\s*<[^>]*>`)
)

type tokKind int

const (
	tkID tokKind = iota
	tkLit
	tkColon
	tkBar
	tkSemi
	tkAction
	tkPrec
	tkEmpty
)

type ruleTok struct {
	kind tokKind
	text string
}

// lexRules tokenizes the rules section: identifiers, character/string
// literals, ':' '|' ';', brace-balanced actions, %prec and %empty. Comments and
// whitespace are dropped.
func lexRules(s string) ([]ruleTok, error) {
	var out []ruleTok
	n := len(s)
	i := 0
	lineOf := func(pos int) int { return strings.Count(s[:pos], "\n") + 1 }
	for i < n {
		c := s[i]
		switch {
		case c == '/' && i+1 < n && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("grammar: unterminated comment at line %d", lineOf(i))
			}
			i += 2 + j + 2
		case c == '/' && i+1 < n && s[i+1] == '/':
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = n - i
			}
			i += j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '{':
			j, err := matchBrace(s, i)
			if err != nil {
				return nil, fmt.Errorf("grammar: %v at line %d", err, lineOf(i))
			}
			out = append(out, ruleTok{tkAction, s[i:j]})
			i = j
		case c == '\'' || c == '"':
			j := i + 1
			for j < n && s[j] != c {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("grammar: unterminated literal at line %d", lineOf(i))
			}
			out = append(out, ruleTok{tkLit, s[i : j+1]})
			i = j + 1
		case c == ':':
			out = append(out, ruleTok{tkColon, ":"})
			i++
		case c == '|':
			out = append(out, ruleTok{tkBar, "|"})
			i++
		case c == ';':
			out = append(out, ruleTok{tkSemi, ";"})
			i++
		case c == '%':
			switch {
			case strings.HasPrefix(s[i:], "%prec") && !isIdentByte(at(s, i+5)):
				out = append(out, ruleTok{tkPrec, "%prec"})
				i += 5
			case strings.HasPrefix(s[i:], "%empty") && !isIdentByte(at(s, i+6)):
				out = append(out, ruleTok{tkEmpty, "%empty"})
				i += 6
			default:
				return nil, fmt.Errorf("grammar: unknown %% directive in rules at line %d", lineOf(i))
			}
		case isIdentStart(c):
			j := i
			for j < n && isIdentByte(s[j]) {
				j++
			}
			out = append(out, ruleTok{tkID, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("grammar: unexpected %q at line %d", s[i:min(i+20, n)], lineOf(i))
		}
	}
	return out, nil
}

// matchBrace returns the index just past the '}' closing the '{' at i, skipping
// C strings, chars and comments inside the action.
func matchBrace(s string, i int) (int, error) {
	depth := 0
	n := len(s)
	for j := i; j < n; j++ {
		c := s[j]
		switch {
		case c == '/' && j+1 < n && s[j+1] == '*':
			k := strings.Index(s[j+2:], "*/")
			if k < 0 {
				return 0, fmt.Errorf("unterminated comment in action")
			}
			j += 2 + k + 1
		case c == '/' && j+1 < n && s[j+1] == '/':
			k := strings.IndexByte(s[j:], '\n')
			if k < 0 {
				return 0, fmt.Errorf("unterminated action")
			}
			j += k
		case c == '"' || c == '\'':
			k := j + 1
			for k < n && s[k] != c {
				if s[k] == '\\' {
					k++
				}
				k++
			}
			j = k
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return j + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced brace")
}

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }
