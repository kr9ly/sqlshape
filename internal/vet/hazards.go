package vet

import (
	"go/token"
	"regexp"
	"strings"

	"github.com/kr9ly/sqlshape/internal/expand"
)

// Template hazards: places where a template action does not do what it looks like.
//
// An action inside a string literal ('%{{.Q}}%') or a comment is text: the expander
// still numbers it, but PG sees a literal and no parameter, so the value never reaches
// the query and the checker has nothing to bind — write '%' || {{.Q}} || '%'. A bare
// parameter as an ORDER BY / GROUP BY item (ORDER BY {{.Sort}}) orders by a constant:
// the column name in the value is never looked at — branch on it instead.

// checkActionPlacement reports actions inside string literals and comments of the template.
func checkActionPlacement(text string, lit literal, report func(token.Pos, string, ...any)) {
	const (
		code     = iota
		quote    // '...'
		equote   // E'...' with backslash escapes
		dollar   // $tag$ ... $tag$
		lineCmt  // -- ...
		blockCmt // /* ... */ (nesting)
	)
	state, depth := code, 0
	tag := ""
	for i := 0; i < len(text); i++ {
		if strings.HasPrefix(text[i:], "{{") {
			end := strings.Index(text[i:], "}}")
			if end < 0 {
				return
			}
			action := text[i : i+end+2]
			switch state {
			case quote, equote, dollar:
				report(lit.pos(i), "%s is inside a string literal: it becomes text, not a parameter (write '%%' || %s || '%%' to concatenate)", action, action)
			case lineCmt, blockCmt:
				report(lit.pos(i), "%s is inside a comment and has no effect", action)
			}
			i += end + 1
			continue
		}
		c := text[i]
		switch state {
		case code:
			switch {
			case c == '\'':
				state = quote
			case (c == 'E' || c == 'e') && i+1 < len(text) && text[i+1] == '\'' && (i == 0 || !isIdentByte(text[i-1])):
				state, i = equote, i+1
			case c == '$' && (i == 0 || !isIdentByte(text[i-1])):
				if m := dollarTag.FindString(text[i:]); m != "" {
					state, tag = dollar, m
					i += len(m) - 1
				}
			case strings.HasPrefix(text[i:], "--"):
				state = lineCmt
			case strings.HasPrefix(text[i:], "/*"):
				state, depth = blockCmt, 1
				i++
			}
		case quote:
			if c == '\'' {
				if i+1 < len(text) && text[i+1] == '\'' {
					i++
				} else {
					state = code
				}
			}
		case equote:
			switch c {
			case '\\':
				i++
			case '\'':
				if i+1 < len(text) && text[i+1] == '\'' {
					i++
				} else {
					state = code
				}
			}
		case dollar:
			if strings.HasPrefix(text[i:], tag) {
				i += len(tag) - 1
				state = code
			}
		case lineCmt:
			if c == '\n' {
				state = code
			}
		case blockCmt:
			switch {
			case strings.HasPrefix(text[i:], "/*"):
				depth++
				i++
			case strings.HasPrefix(text[i:], "*/"):
				depth--
				i++
				if depth == 0 {
					state = code
				}
			}
		}
	}
}

var dollarTag = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z_0-9]*\$|^\$\$`)

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

var bareOrderBy = regexp.MustCompile(`(?i)\b(ORDER|GROUP|PARTITION)\s+BY\s+((?:\$\d+\s*,\s*)*)\$(\d+)`)

// checkBareOrderBy reports a parameter standing alone as an ORDER BY / GROUP BY item.
func checkBareOrderBy(e *expand.Expansion, lit literal, report func(token.Pos, string, ...any), where string) {
	for _, m := range bareOrderBy.FindAllStringSubmatchIndex(e.SQL, -1) {
		n := e.SQL[m[6]:m[7]]
		var p *expand.Param
		for i := range e.Params {
			if strconvItoa(e.Params[i].N) == n {
				p = &e.Params[i]
				break
			}
		}
		if p == nil {
			continue
		}
		kw := strings.ToUpper(e.SQL[m[2]:m[3]])
		report(lit.pos(p.Pos), "%s BY {{%s}} sorts by a constant, not by the column the value names: branch on it instead ({{if eq %s \"total\"}} total {{else}} id {{end}})%s", kw, p.Path, p.Path, where)
	}
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
