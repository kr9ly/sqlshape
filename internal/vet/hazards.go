package vet

import (
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/internal/expand"
	"github.com/kr9ly/sqlshape/internal/pgparse"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Template hazards: places where a template action does not do what it looks like.
//
// An action inside a string literal ('%{{.Q}}%') or a comment is text: the expander
// still numbers it, but PG sees a literal and no parameter, so the value never reaches
// the query and the checker has nothing to bind — write '%' || {{.Q}} || '%'. A bare
// parameter as an ORDER BY / GROUP BY item (ORDER BY {{.Sort}}) orders by a constant:
// the column name in the value is never looked at — branch on it instead.
//
// Both checks below run per expansion, on the expansion's rendered SQL (Expansion.SQL),
// not on the raw template text. Every {{...}} action — value or control — becomes exactly
// one of: literal text (an {{if}}/{{range}} branch not taken contributes nothing; a taken
// branch's body is spliced in directly), or a "$n" placeholder (a value action, see
// expand.Expand's state.param). So one expansion's SQL is a single, real, straight-line
// SQL string with no alternative ever concatenated into it — exactly the string PostgreSQL
// would see for that branch combination. Scanning it, once per expansion, is what keeps
// quote/comment state from crossing an if/else (or any other) branch that at runtime only
// ever takes one side of, and lets an ORDER BY / GROUP BY / PARTITION BY item be found
// anywhere in the list rather than only in the leading position.

// paramRef matches a "$n" placeholder text as expand.Expand writes it (state.param).
var paramRef = regexp.MustCompile(`^\$[0-9]+`)

// checkActionPlacement reports a parameter placeholder that lands inside a string literal
// or a comment of one expansion's rendered SQL.
func checkActionPlacement(e *expand.Expansion, lit literal, report func(token.Pos, string, ...any), where string) {
	const (
		code     = iota
		quote    // '...'
		equote   // E'...' with backslash escapes
		dollar   // $tag$ ... $tag$
		lineCmt  // -- ...
		blockCmt // /* ... */ (nesting)
	)
	byN := make(map[string]*expand.Param, len(e.Params))
	for i := range e.Params {
		byN["$"+strconv.Itoa(e.Params[i].N)] = &e.Params[i]
	}
	text := e.SQL
	state, depth := code, 0
	tag := ""
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '$' {
			// unlike the dollar-quote tag below, a placeholder is not required to start at
			// a word boundary: expand splices "$n" directly after whatever literal text
			// precedes the action (e.g. 'bar$1' from 'bar{{.Q}}), which is exactly the
			// string-literal hazard this scan exists to catch.
			if m := paramRef.FindString(text[i:]); m != "" {
				if p, ok := byN[m]; ok {
					switch state {
					case quote, equote, dollar:
						report(lit.pos(e.TemplatePos(i)), "{{%s}} is inside a string literal: it becomes text, not a parameter (write '%%' || {{%s}} || '%%' to concatenate)%s", p.Path, p.Path, where)
					case lineCmt, blockCmt:
						report(lit.pos(e.TemplatePos(i)), "{{%s}} is inside a comment and has no effect%s", p.Path, where)
					}
					i += len(m) - 1
					continue
				}
			}
		}
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

// checkBareOrderBy reports a parameter standing alone as an ORDER BY / GROUP BY / window
// PARTITION BY item: PostgreSQL sorts/groups by the constant value it received, never
// looking at what column name (if any) the value names, so branching on the value's
// content is the only way to select a column dynamically. This walks the parsed AST of
// the expansion's rendered SQL (the same parse analyze.Analyze itself would produce) so a
// bare parameter is caught in any position of the list, decorated with ASC/DESC/NULLS
// FIRST/LAST or not, inside a window definition's PARTITION BY or ORDER BY too — not only
// when it is the leading item of a plain ORDER BY / GROUP BY.
func checkBareOrderBy(e *expand.Expansion, lit literal, report func(token.Pos, string, ...any), where string) {
	tree, err := pgparse.Parse(e.SQL)
	if err != nil {
		return // analyze.Analyze reports the parse failure itself
	}
	byN := make(map[int32]*expand.Param, len(e.Params))
	for i := range e.Params {
		byN[int32(e.Params[i].N)] = &e.Params[i]
	}
	flag := func(kw string, target *pgparse.Node) {
		ref := target.GetParamRef()
		if ref == nil {
			return
		}
		p, ok := byN[ref.Number]
		if !ok {
			return
		}
		report(lit.pos(e.TemplatePos(int(ref.Location))), "%s BY {{%s}} sorts by a constant, not by the column the value names: branch on it instead ({{if eq %s \"total\"}} total {{else}} id {{end}})%s", kw, p.Path, p.Path, where)
	}
	schema.WalkNodes(tree, func(n *pgparse.Node) {
		switch v := n.Node.(type) {
		case *pgparse.Node_SortBy:
			flag("ORDER", v.SortBy.Node)
		case *pgparse.Node_SelectStmt:
			for _, g := range v.SelectStmt.GroupClause {
				flag("GROUP", g)
			}
		case *pgparse.Node_WindowDef:
			for _, p := range v.WindowDef.PartitionClause {
				flag("PARTITION", p)
			}
		}
	})
}
