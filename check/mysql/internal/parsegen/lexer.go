package parsegen

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
)

// cc holds the hand-written C++: the THD shim, the CST builder, the driver,
// and the head/tail files the server code is spliced between.
//
//go:embed cc
var cc embed.FS

// Lexer is the server's lexer cut out of sql_lex.cc / sql_lex.h.
type Lexer struct {
	// LexerCC is lexer.cc: head + Lex_input_stream::init/reset + shims + the token functions.
	LexerCC string
	// SQLLexH replaces sql/sql_lex.h: enum_comment_state + class Lex_input_stream + the shim's Parser_state.
	SQLLexH string
}

// CutLexer extracts the lexer. Every cut is anchored on a signature; a missing
// anchor is an error, never an empty output.
func CutLexer(sqlLexCC, sqlLexH string) (*Lexer, error) {
	head, err := ccFile("cc/lexer_head.cc")
	if err != nil {
		return nil, err
	}
	shims, err := ccFile("cc/lexer_shims.cc")
	if err != nil {
		return nil, err
	}
	hHead, err := ccFile("cc/shim/sql/sql_lex_head.h")
	if err != nil {
		return nil, err
	}
	hTail, err := ccFile("cc/shim/sql/sql_lex_tail.h")
	if err != nil {
		return nil, err
	}

	initFn, err := cutFunction(sqlLexCC, "bool Lex_input_stream::init(THD *thd")
	if err != nil {
		return nil, err
	}
	resetFn, err := cutFunction(sqlLexCC, "void Lex_input_stream::reset(const char *buffer")
	if err != nil {
		return nil, err
	}
	body, err := cutBetweenLines(sqlLexCC, "static int find_keyword(Lex_input_stream *lip", "void trim_whitespace(")
	if err != nil {
		return nil, err
	}
	// The special-purpose parser states (partition, gcol, ...) are constructed in
	// the same stretch; they belong to the server's Parser_state hierarchy, not
	// to the lexer.
	ctors := reParserStateCtor.FindAllStringIndex(body, -1)
	if len(ctors) == 0 {
		return nil, fmt.Errorf("lexer: expected the *_parser_state constructors inside find_keyword..trim_whitespace; the server source moved")
	}
	body = reParserStateCtor.ReplaceAllString(body, "")

	commentState, err := cutBetweenLines(sqlLexH, "enum enum_comment_state {", "class Lex_input_stream {")
	if err != nil {
		return nil, err
	}
	class, err := cutClass(sqlLexH, "class Lex_input_stream {")
	if err != nil {
		return nil, err
	}

	var l strings.Builder
	l.WriteString(head)
	l.WriteString("// --- sql_lex.cc: Lex_input_stream::init / reset\n")
	l.WriteString(initFn)
	l.WriteString(resetFn)
	l.WriteString(shims)
	l.WriteString("// --- sql_lex.cc: find_keyword .. lex_one_token\n")
	l.WriteString(body)

	var h strings.Builder
	h.WriteString(hHead)
	h.WriteString(commentState)
	h.WriteString(class)
	h.WriteString(hTail)
	return &Lexer{LexerCC: l.String(), SQLLexH: h.String()}, nil
}

var reParserStateCtor = regexp.MustCompile(`(?m)^[A-Za-z_]*_parser_state::[A-Za-z_]*_parser_state\(\)\n    : Parser_state\(.*\) \{\}\n`)

func ccFile(name string) (string, error) {
	b, err := cc.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("embedded %s: %w", name, err)
	}
	return string(b), nil
}

// lineStart returns the offset of the first line beginning with prefix.
func lineStart(src, prefix string) (int, error) {
	if strings.HasPrefix(src, prefix) {
		return 0, nil
	}
	i := strings.Index(src, "\n"+prefix)
	if i < 0 {
		return 0, fmt.Errorf("anchor %q not found; the server source moved", prefix)
	}
	return i + 1, nil
}

// cutFunction returns the lines from the one starting with sig through the
// first line that is exactly "}".
func cutFunction(src, sig string) (string, error) {
	s, err := lineStart(src, sig)
	if err != nil {
		return "", err
	}
	e := strings.Index(src[s:], "\n}\n")
	if e < 0 {
		return "", fmt.Errorf("anchor %q: no closing brace line", sig)
	}
	return src[s : s+e+3], nil
}

// cutClass returns the lines from the one starting with sig through the first
// line that is exactly "};".
func cutClass(src, sig string) (string, error) {
	s, err := lineStart(src, sig)
	if err != nil {
		return "", err
	}
	e := strings.Index(src[s:], "\n};\n")
	if e < 0 {
		return "", fmt.Errorf("anchor %q: no closing brace line", sig)
	}
	return src[s : s+e+4], nil
}

// cutBetweenLines returns the lines from the one starting with from up to, and
// excluding, the one starting with to.
func cutBetweenLines(src, from, to string) (string, error) {
	s, err := lineStart(src, from)
	if err != nil {
		return "", err
	}
	e, err := lineStart(src[s:], to)
	if err != nil {
		return "", fmt.Errorf("after %q: %w", from, err)
	}
	return src[s : s+e], nil
}
