package mysqlparse

import "strings"

// Statement is one statement of a script: its text without the terminating ';' and the
// byte offset of its first character in the script.
type Statement struct {
	SQL    string
	Offset int
}

// Split cuts a script into statements at the ';' outside quotes and comments, the way the
// mysql client does with the default delimiter. Comments and blank text between statements
// are dropped; a trailing statement without ';' is kept.
func Split(script string) []Statement {
	var out []Statement
	start := 0
	i := 0
	n := len(script)
	flush := func(end int) {
		text := script[start:end]
		trimmed := strings.TrimLeft(text, " \t\r\n")
		if strings.TrimSpace(stripLeadingComments(trimmed)) != "" {
			out = append(out, Statement{SQL: strings.TrimRight(trimmed, " \t\r\n"), Offset: start + len(text) - len(trimmed)})
		}
	}
	for i < n {
		c := script[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < n && script[i] != q {
				if script[i] == '\\' && q != '`' {
					i++
				}
				i++
			}
			i++
		case c == '#' || (c == '-' && i+1 < n && script[i+1] == '-' && (i+2 >= n || script[i+2] == ' ' || script[i+2] == '\t' || script[i+2] == '\n')):
			for i < n && script[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && script[i+1] == '*':
			j := strings.Index(script[i+2:], "*/")
			if j < 0 {
				i = n
			} else {
				i += 2 + j + 2
			}
		case c == ';':
			flush(i)
			i++
			start = i
		default:
			i++
		}
	}
	flush(n)
	return out
}

// stripLeadingComments removes comments (and the whitespace around them) from the front of
// a statement, so that a statement consisting only of comments is recognized as empty. The
// statement text itself keeps its comments: `-- sqlshape:` directives live there.
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "#"), strings.HasPrefix(s, "-- "), strings.HasPrefix(s, "--\n"), s == "--":
			if j := strings.IndexByte(s, '\n'); j >= 0 {
				s = s[j+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*") && !strings.HasPrefix(s, "/*!") && !strings.HasPrefix(s, "/*+"):
			j := strings.Index(s, "*/")
			if j < 0 {
				return ""
			}
			s = s[j+2:]
		default:
			return s
		}
	}
}
