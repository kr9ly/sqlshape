package analyze

// jsonpath input: a port of PG's jsonpath_scan.l / jsonpath_gram.y acceptance rules and
// the checks flattenJsonPathParseItem and makeItemLikeRegex add. Only the verdict
// matters: accepted, or the SQLSTATE PG raises (42601 syntax, 22P02 empty input /
// surrogate misuse, 22P05 a NUL escape, 0A000 the "x" regex flag, 2201B a bad regex).

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

type jpToken struct {
	kind string // IDENT STRING VARIABLE NUMERIC INT, a keyword, or an operator / special char
	text string
}

type jpError struct {
	code, msg string
}

func jpErr(code, msg string) *jpError { return &jpError{code, msg} }

var jpKeywords = map[string]bool{
	"is": true, "to": true, "abs": true, "lax": true, "date": true, "flag": true, "last": true, "null": true,
	"size": true, "time": true, "true": true, "type": true, "with": true, "false": true, "floor": true,
	"bigint": true, "double": true, "exists": true, "number": true, "starts": true, "strict": true,
	"string": true, "boolean": true, "ceiling": true, "decimal": true, "integer": true, "time_tz": true,
	"unknown": true, "datetime": true, "keyvalue": true, "timestamp": true, "like_regex": true, "timestamp_tz": true,
}

// keywords that must be spelled in lower case to be recognized
var jpLowerOnly = map[string]bool{"null": true, "true": true, "false": true}

var jpMethods = map[string]bool{
	"abs": true, "size": true, "type": true, "floor": true, "double": true, "ceiling": true, "keyvalue": true,
	"bigint": true, "boolean": true, "date": true, "integer": true, "number": true, "string": true,
}

const jpSpecial = "?%$.[]{}()|&!=<>@#,*:-+/"

func jpIsSpecial(c byte) bool { return strings.IndexByte(jpSpecial, c) >= 0 }
func jpIsBlank(c byte) bool   { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
func jpIsOther(c byte) bool   { return !jpIsSpecial(c) && !jpIsBlank(c) && c != '\\' && c != '"' }

var (
	jpDecInt  = regexp.MustCompile(`^(0|[1-9](_?[0-9])*)`)
	jpHexInt  = regexp.MustCompile(`^0[xX][0-9A-Fa-f](_?[0-9A-Fa-f])*`)
	jpOctInt  = regexp.MustCompile(`^0[oO][0-7](_?[0-7])*`)
	jpBinInt  = regexp.MustCompile(`^0[bB][01](_?[01])*`)
	jpDecimal = regexp.MustCompile(`^((0|[1-9](_?[0-9])*)\.([0-9](_?[0-9])*)?|\.[0-9](_?[0-9])*)`)
	jpReal    = regexp.MustCompile(`^((0|[1-9](_?[0-9])*)\.([0-9](_?[0-9])*)?|\.[0-9](_?[0-9])*|0|[1-9](_?[0-9])*)[Ee][-+]?[0-9](_?[0-9])*`)
)

type jpLexer struct {
	s string
	p int
}

// lex tokenizes the whole input; a scanner error is returned as PG raises it.
func (l *jpLexer) lex() ([]jpToken, *jpError) {
	var toks []jpToken
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		if t.kind == "EOF" {
			return toks, nil
		}
		toks = append(toks, t)
	}
}

func (l *jpLexer) next() (jpToken, *jpError) {
	s := l.s
	for {
		if l.p >= len(s) {
			return jpToken{kind: "EOF"}, nil
		}
		c := s[l.p]
		if jpIsBlank(c) {
			l.p++
			continue
		}
		if c == '/' && l.p+1 < len(s) && s[l.p+1] == '*' {
			end := strings.Index(s[l.p+2:], "*/")
			if end < 0 {
				return jpToken{}, jpErr("42601", "unexpected end of comment")
			}
			l.p += 2 + end + 2
			continue
		}
		break
	}
	c := s[l.p]
	rest := s[l.p:]
	// multi-character operators, longest first
	for _, op := range []string{"&&", "||", "**", "<=", "==", "<>", "!=", ">="} {
		if strings.HasPrefix(rest, op) {
			l.p += 2
			return jpToken{kind: op}, nil
		}
	}
	switch {
	case c == '$':
		l.p++
		if l.p < len(s) && s[l.p] == '"' {
			l.p++
			str, err := l.quoted()
			if err != nil {
				return jpToken{}, err
			}
			return jpToken{kind: "VARIABLE", text: str}, nil
		}
		st := l.p
		for l.p < len(s) && jpIsOther(s[l.p]) {
			l.p++
		}
		if l.p > st {
			return jpToken{kind: "VARIABLE", text: s[st:l.p]}, nil
		}
		return jpToken{kind: "$"}, nil
	case c == '"':
		l.p++
		str, err := l.quoted()
		if err != nil {
			return jpToken{}, err
		}
		return jpToken{kind: "STRING", text: str}, nil
	case cIsDigit(c) || (c == '.' && l.p+1 < len(s) && cIsDigit(s[l.p+1])):
		return l.number()
	case jpIsSpecial(c):
		l.p++
		return jpToken{kind: string(c)}, nil
	}
	// identifier (xnq): other+ and escapes, ended by blank / special / quote / EOF
	var buf []byte
	for l.p < len(s) {
		c := s[l.p]
		switch {
		case jpIsOther(c):
			buf = append(buf, c)
			l.p++
		case c == '\\':
			b, err := l.escape(&buf)
			if err != nil {
				return jpToken{}, err
			}
			buf = b
		case c == '/' && l.p+1 < len(s) && s[l.p+1] == '*':
			return l.ident(buf), nil
		default:
			return l.ident(buf), nil
		}
	}
	return l.ident(buf), nil
}

func (l *jpLexer) ident(buf []byte) jpToken {
	word := string(buf)
	low := strings.ToLower(word)
	if jpKeywords[low] && (!jpLowerOnly[low] || low == word) {
		return jpToken{kind: low, text: word}
	}
	return jpToken{kind: "IDENT", text: word}
}

// number matches the longest numeric pattern; a following "other" character is the
// trailing-junk error (which also covers "1e" and the realfail forms).
func (l *jpLexer) number() (jpToken, *jpError) {
	rest := l.s[l.p:]
	best, kind := "", ""
	for _, r := range []struct {
		re   *regexp.Regexp
		kind string
	}{{jpReal, "NUMERIC"}, {jpDecimal, "NUMERIC"}, {jpHexInt, "INT"}, {jpOctInt, "INT"}, {jpBinInt, "INT"}, {jpDecInt, "INT"}} {
		if m := r.re.FindString(rest); len(m) > len(best) {
			best, kind = m, r.kind
		}
	}
	if best == "" {
		l.p++
		return jpToken{kind: string(rest[0])}, nil
	}
	l.p += len(best)
	if l.p < len(l.s) && jpIsOther(l.s[l.p]) {
		return jpToken{}, jpErr("42601", "trailing junk after numeric literal")
	}
	return jpToken{kind: kind, text: best}, nil
}

// quoted reads a string body after the opening quote.
func (l *jpLexer) quoted() (string, *jpError) {
	var buf []byte
	for {
		if l.p >= len(l.s) {
			return "", jpErr("42601", "unterminated quoted string")
		}
		c := l.s[l.p]
		switch c {
		case '"':
			l.p++
			return string(buf), nil
		case '\\':
			b, err := l.escape(&buf)
			if err != nil {
				return "", err
			}
			buf = b
		default:
			buf = append(buf, c)
			l.p++
		}
	}
}

// escape handles a backslash sequence at l.p (which points at the backslash).
func (l *jpLexer) escape(buf *[]byte) ([]byte, *jpError) {
	s := l.s
	if l.p+1 >= len(s) {
		return nil, jpErr("42601", "unexpected end after backslash")
	}
	switch s[l.p+1] {
	case 'b':
		*buf = append(*buf, '\b')
	case 'f':
		*buf = append(*buf, '\f')
	case 'n':
		*buf = append(*buf, '\n')
	case 'r':
		*buf = append(*buf, '\r')
	case 't':
		*buf = append(*buf, '\t')
	case 'v':
		*buf = append(*buf, '\v')
	case 'x':
		// \xHH
		if l.p+3 >= len(s) || !isHexDigit(s[l.p+2]) || !isHexDigit(s[l.p+3]) {
			return nil, jpErr("42601", "invalid hexadecimal character sequence")
		}
		ch := hexVal(s[l.p+2])<<4 | hexVal(s[l.p+3])
		if ch == 0 {
			return nil, jpErr("22P05", "unsupported Unicode escape sequence")
		}
		*buf = utf8.AppendRune(*buf, rune(ch))
		l.p += 4
		return *buf, nil
	case 'u':
		// a run of \u escapes is decoded together so surrogate pairs can join
		hi := -1
		for l.p+1 < len(s) && s[l.p] == '\\' && s[l.p+1] == 'u' {
			i := l.p + 2
			ch := 0
			if i < len(s) && s[i] == '{' {
				i++
				n := 0
				for i < len(s) && s[i] != '}' && isHexDigit(s[i]) && n < 6 {
					ch = ch<<4 | hexVal(s[i])
					i++
					n++
				}
				if n == 0 || i >= len(s) || s[i] != '}' {
					return nil, jpErr("42601", "invalid Unicode escape sequence")
				}
				i++
			} else {
				for n := 0; n < 4; n++ {
					if i >= len(s) || !isHexDigit(s[i]) {
						return nil, jpErr("42601", "invalid Unicode escape sequence")
					}
					ch = ch<<4 | hexVal(s[i])
					i++
				}
			}
			l.p = i
			switch {
			case ch >= 0xD800 && ch <= 0xDBFF:
				if hi != -1 {
					return nil, jpErr("22P02", "invalid input syntax for type jsonpath")
				}
				hi = ch
				continue
			case ch >= 0xDC00 && ch <= 0xDFFF:
				if hi == -1 {
					return nil, jpErr("22P02", "invalid input syntax for type jsonpath")
				}
				ch = ((hi & 0x3FF) << 10) + (ch & 0x3FF) + 0x10000
				hi = -1
			default:
				if hi != -1 {
					return nil, jpErr("22P02", "invalid input syntax for type jsonpath")
				}
			}
			if ch == 0 {
				return nil, jpErr("22P05", "unsupported Unicode escape sequence")
			}
			*buf = utf8.AppendRune(*buf, rune(ch))
		}
		if hi != -1 {
			return nil, jpErr("22P02", "invalid input syntax for type jsonpath")
		}
		return *buf, nil
	default:
		*buf = append(*buf, s[l.p+1])
	}
	l.p += 2
	return *buf, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// --- parser ---------------------------------------------------------------------------

type jpNode struct {
	pred  bool // a predicate (comparison, &&, ||, !, exists, is unknown, starts with, like_regex)
	paren bool // '(' predicate ')' — usable as a delimited predicate
}

type jpParser struct {
	toks    []jpToken
	i       int
	nesting int  // filter depth, for "@"
	inSubsc bool // inside [ ], for "last"
	err     *jpError
}

func (p *jpParser) peek() jpToken {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return jpToken{kind: "EOF"}
}

func (p *jpParser) is(kind string) bool { return p.peek().kind == kind }

func (p *jpParser) advance() jpToken {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

func (p *jpParser) syntax() *jpError {
	if p.err == nil {
		if p.is("EOF") {
			p.err = jpErr("42601", "syntax error at end of jsonpath input")
		} else {
			p.err = jpErr("42601", "syntax error at or near "+p.peek().kind+" of jsonpath input")
		}
	}
	return p.err
}

func (p *jpParser) fail(code, msg string) *jpError {
	if p.err == nil {
		p.err = jpErr(code, msg)
	}
	return p.err
}

func (p *jpParser) expect(kind string) bool {
	if !p.is(kind) {
		p.syntax()
		return false
	}
	p.advance()
	return true
}

// validateJsonpathLiteral is jsonpath_in.
func validateJsonpathLiteral(s string, loc int32) *Error {
	toks, lerr := (&jpLexer{s: s}).lex()
	if lerr != nil {
		return errAt(lerr.code, loc, "%s", lerr.msg)
	}
	if len(toks) == 0 {
		return errAt("22P02", loc, "invalid input syntax for type jsonpath: %q", s)
	}
	p := &jpParser{toks: toks}
	if p.is("strict") || p.is("lax") {
		p.advance()
	}
	p.parseOr()
	if p.err == nil && !p.is("EOF") {
		p.syntax()
	}
	if p.err != nil {
		return errAt(p.err.code, loc, "%s", p.err.msg)
	}
	return nil
}

// parseOr: predicate OR_P predicate
func (p *jpParser) parseOr() jpNode {
	left := p.parseAnd()
	for p.err == nil && p.is("||") {
		p.advance()
		right := p.parseAnd()
		if p.err != nil {
			return left
		}
		if !left.pred || !right.pred {
			p.syntax()
			return left
		}
		left = jpNode{pred: true}
	}
	return left
}

func (p *jpParser) parseAnd() jpNode {
	left := p.parseNot()
	for p.err == nil && p.is("&&") {
		p.advance()
		right := p.parseNot()
		if p.err != nil {
			return left
		}
		if !left.pred || !right.pred {
			p.syntax()
			return left
		}
		left = jpNode{pred: true}
	}
	return left
}

// parseNot: NOT_P delimited_predicate, else a comparison level
func (p *jpParser) parseNot() jpNode {
	if p.is("!") {
		p.advance()
		return p.parseDelimitedPredicate()
	}
	if p.is("exists") {
		return p.parseDelimitedPredicate()
	}
	return p.parseComparison()
}

// parseDelimitedPredicate: '(' predicate ')' | EXISTS_P '(' expr ')'
func (p *jpParser) parseDelimitedPredicate() jpNode {
	switch {
	case p.is("exists"):
		p.advance()
		if !p.expect("(") {
			return jpNode{}
		}
		n := p.parseOr()
		if p.err != nil {
			return n
		}
		if n.pred {
			// EXISTS takes an expr; '(' predicate ')' is only an expr when an accessor follows
			p.syntax()
			return n
		}
		if !p.expect(")") {
			return n
		}
		return jpNode{pred: true}
	case p.is("("):
		p.advance()
		n := p.parseOr()
		if p.err != nil {
			return n
		}
		if !n.pred {
			p.syntax()
			return n
		}
		if !p.expect(")") {
			return n
		}
		return jpNode{pred: true, paren: true}
	}
	p.syntax()
	return jpNode{}
}

func (p *jpParser) parseComparison() jpNode {
	left := p.parseAdditive()
	if p.err != nil {
		return left
	}
	switch k := p.peek().kind; k {
	case "==", "<>", "!=", "<", "<=", ">", ">=":
		p.advance()
		if left.pred {
			p.syntax()
			return left
		}
		right := p.parseAdditive()
		if p.err != nil {
			return right
		}
		if right.pred {
			p.syntax()
			return right
		}
		return jpNode{pred: true}
	case "starts":
		p.advance()
		if left.pred || !p.expect("with") {
			p.syntax()
			return left
		}
		if !p.is("STRING") && !p.is("VARIABLE") {
			p.syntax()
			return left
		}
		p.advance()
		return jpNode{pred: true}
	case "like_regex":
		p.advance()
		if left.pred || !p.is("STRING") {
			p.syntax()
			return left
		}
		pattern := p.advance().text
		var flags string
		if p.is("flag") {
			p.advance()
			if !p.is("STRING") {
				p.syntax()
				return left
			}
			flags = p.advance().text
		}
		if e := jpCheckLikeRegex(pattern, flags); e != nil {
			p.fail(e.code, e.msg)
		}
		return jpNode{pred: true}
	case "is":
		p.advance()
		if !left.paren || !p.expect("unknown") {
			p.syntax()
			return left
		}
		return jpNode{pred: true}
	}
	return left
}

func (p *jpParser) parseAdditive() jpNode {
	left := p.parseMul()
	for p.err == nil && (p.is("+") || p.is("-")) {
		p.advance()
		right := p.parseMul()
		if p.err != nil {
			return right
		}
		if left.pred || right.pred {
			p.syntax()
			return left
		}
		left = jpNode{}
	}
	return left
}

func (p *jpParser) parseMul() jpNode {
	left := p.parseUnary()
	for p.err == nil && (p.is("*") || p.is("/") || p.is("%")) {
		p.advance()
		right := p.parseUnary()
		if p.err != nil {
			return right
		}
		if left.pred || right.pred {
			p.syntax()
			return left
		}
		left = jpNode{}
	}
	return left
}

func (p *jpParser) parseUnary() jpNode {
	if p.is("+") || p.is("-") {
		p.advance()
		n := p.parseUnary()
		if p.err == nil && n.pred {
			p.syntax()
		}
		return jpNode{}
	}
	return p.parseAccessorExpr()
}

// parseAccessorExpr: path_primary accessor_op* | '(' expr-or-predicate ')' accessor_op*
func (p *jpParser) parseAccessorExpr() jpNode {
	var n jpNode
	switch t := p.peek(); t.kind {
	case "(":
		p.advance()
		inner := p.parseOr()
		if p.err != nil {
			return inner
		}
		if !p.expect(")") {
			return inner
		}
		if inner.pred {
			// '(' predicate ')': a delimited predicate, or an expr when an accessor follows
			if p.startsAccessor() {
				n = jpNode{}
			} else {
				return jpNode{pred: true, paren: true}
			}
		} else {
			n = jpNode{}
		}
	case "STRING", "null", "true", "false", "NUMERIC", "INT", "VARIABLE", "$":
		p.advance()
	case "@":
		p.advance()
		if p.nesting <= 0 {
			p.fail("42601", "@ is not allowed in root expressions")
		}
	case "last":
		p.advance()
		if !p.inSubsc {
			p.fail("42601", "LAST is allowed only in array subscripts")
		}
	default:
		p.syntax()
		return jpNode{}
	}
	for p.err == nil && p.startsAccessor() {
		p.parseAccessorOp()
	}
	return n
}

func (p *jpParser) startsAccessor() bool {
	return p.is(".") || p.is("[") || p.is("?")
}

func (p *jpParser) parseAccessorOp() {
	switch p.advance().kind {
	case ".":
		switch t := p.peek(); {
		case t.kind == "*":
			p.advance()
		case t.kind == "**":
			p.advance()
			if p.is("{") {
				p.advance()
				if !p.anyLevel() {
					return
				}
				if p.is("to") {
					p.advance()
					if !p.anyLevel() {
						return
					}
				}
				p.expect("}")
			}
		case t.kind == "IDENT" || t.kind == "STRING" || jpKeywords[t.kind]:
			p.advance()
			if !p.is("(") {
				return // '.' key
			}
			if t.kind == "IDENT" || t.kind == "STRING" {
				p.syntax()
				return
			}
			p.advance()
			switch {
			case jpMethods[t.kind]:
				p.expect(")")
			case t.kind == "decimal":
				n := 0
				for p.err == nil && !p.is(")") {
					if p.is("+") || p.is("-") {
						p.advance()
					}
					if !p.expect("INT") {
						return
					}
					n++
					if p.is(",") {
						p.advance()
						if p.is(")") {
							p.syntax()
							return
						}
					} else if !p.is(")") {
						p.syntax()
						return
					}
				}
				if n > 2 {
					p.fail("42601", "invalid input syntax for type jsonpath")
					return
				}
				p.expect(")")
			case t.kind == "datetime":
				if p.is("STRING") {
					p.advance()
				}
				p.expect(")")
			case t.kind == "time" || t.kind == "time_tz" || t.kind == "timestamp" || t.kind == "timestamp_tz":
				if p.is("INT") {
					p.advance()
				}
				p.expect(")")
			default:
				p.syntax()
			}
		default:
			p.syntax()
		}
	case "[":
		if p.is("*") {
			p.advance()
			p.expect("]")
			return
		}
		saved := p.inSubsc
		p.inSubsc = true
		for p.err == nil {
			n := p.parseOr()
			if p.err != nil {
				break
			}
			if n.pred {
				p.syntax()
				break
			}
			if p.is("to") {
				p.advance()
				n = p.parseOr()
				if p.err != nil {
					break
				}
				if n.pred {
					p.syntax()
					break
				}
			}
			if p.is(",") {
				p.advance()
				continue
			}
			break
		}
		p.inSubsc = saved
		if p.err == nil {
			p.expect("]")
		}
	case "?":
		if !p.expect("(") {
			return
		}
		p.nesting++
		n := p.parseOr()
		p.nesting--
		if p.err != nil {
			return
		}
		if !n.pred {
			p.syntax()
			return
		}
		p.expect(")")
	}
}

func (p *jpParser) anyLevel() bool {
	if p.is("INT") || p.is("last") {
		p.advance()
		return true
	}
	p.syntax()
	return false
}

// jpCheckLikeRegex is makeItemLikeRegex: flag letters, the unimplemented "x" flag, and a
// pattern the regex engine rejects (approximated with Go's engine on structural errors).
func jpCheckLikeRegex(pattern, flags string) *jpError {
	quote := false
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'i', 's', 'm':
		case 'x':
			if !strings.ContainsRune(flags, 'q') {
				return jpErr("0A000", `XQuery "x" flag (expanded regular expressions) is not implemented`)
			}
		case 'q':
			quote = true
		default:
			return jpErr("42601", "invalid input syntax for type jsonpath")
		}
	}
	if quote {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		msg := err.Error()
		for _, structural := range []string{"missing closing )", "unexpected )", "missing closing ]", "missing argument to repetition operator", "invalid nested repetition operator", "trailing backslash"} {
			if strings.Contains(msg, structural) {
				return jpErr("2201B", "invalid regular expression")
			}
		}
	}
	return nil
}
