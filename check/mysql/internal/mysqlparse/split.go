package mysqlparse

import (
	"regexp"
	"strings"
)

// Statement is one statement of a script: its text without the terminating ';' and the
// byte offset of its first character in the script.
type Statement struct {
	SQL    string
	Offset int
}

// Split cuts a script into statements at the ';' outside quotes and comments, the way the
// mysql client does with the default delimiter. Comments and blank text between statements
// are dropped; a trailing statement without ';' is kept.
//
// Split is Split's mode-0 case: see SplitMode for a version that also cuts compound
// statement bodies (CREATE TRIGGER / PROCEDURE / FUNCTION / EVENT) as a single statement
// and understands the mysql client's `DELIMITER` command.
func Split(script string) []Statement {
	return SplitMode(script, 0)
}

// compoundCreateRe matches the start of a CREATE statement (after any leading comments are
// stripped) whose body is a compound statement that may contain ';' internally: CREATE
// [DEFINER = user] TRIGGER|PROCEDURE|FUNCTION|EVENT ...
var compoundCreateRe = regexp.MustCompile(`(?is)^create\s+(definer\s*=\s*\S+\s+)?(trigger|procedure|function|event)\b`)

// SplitMode cuts a script into statements the way the mysql client does: at the default
// delimiter (';') outside quotes and comments, except that the body of a CREATE TRIGGER /
// PROCEDURE / FUNCTION / EVENT is a single statement even though it may contain ';'
// internally (BEGIN ... END and the like). A `DELIMITER <token>` line, as the mysql client
// reads it, is consumed (not emitted as a statement); until the next `DELIMITER` line
// statements are cut at <token> instead of ';', and compound-statement detection is
// skipped (the caller is assumed to have picked <token> so the body needs no help).
//
// mode is the sql_mode used to decide whether a candidate cut is a complete statement; it
// is only consulted while probing candidate cuts of a CREATE TRIGGER / PROCEDURE / FUNCTION
// / EVENT body.
func SplitMode(script string, mode Mode) []Statement {
	var out []Statement
	appendStmt := func(start, end int) {
		text := script[start:end]
		trimmed := strings.TrimLeft(text, " \t\r\n")
		if strings.TrimSpace(stripLeadingComments(trimmed)) != "" {
			out = append(out, Statement{SQL: strings.TrimRight(trimmed, " \t\r\n"), Offset: start + len(text) - len(trimmed)})
		}
	}

	delim := ";"
	start := 0
	n := len(script)
	for start < n {
		if newDelim, next, ok := matchDelimiterLine(script, start); ok {
			delim = newDelim
			start = next
			continue
		}
		end, next, found := findDelimiter(script, start, delim)
		if delim == ";" && isCompoundCreate(script[start:end]) {
			for !found || parses(script[start:end], mode) != nil {
				if !found {
					// ran off the end of the script: keep the whole remainder as one
					// statement (a syntax error in the body degrades this way)
					end, next = n, n
					break
				}
				end2, next2, found2 := findDelimiter(script, next, delim)
				if !found2 {
					end, next, found = n, n, false
					continue
				}
				end, next, found = end2, next2, found2
			}
		}
		appendStmt(start, end)
		if !found {
			break
		}
		start = next
	}
	return out
}

// parses reports the error, if any, from parsing text as a single statement. It is used
// only to decide whether a candidate cut of a compound-statement body is complete; parse
// errors inside a genuinely broken statement are reported later, when the statement is
// parsed for real.
func parses(text string, mode Mode) error {
	_, err := Parse(strings.TrimSpace(text), mode)
	return err
}

// isCompoundCreate reports whether text, once its leading comments are stripped, begins a
// CREATE TRIGGER / PROCEDURE / FUNCTION / EVENT statement -- one whose body may be a
// compound statement containing ';' internally.
func isCompoundCreate(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	return compoundCreateRe.MatchString(stripLeadingComments(trimmed))
}

// findDelimiter finds the next occurrence of delim in script at or after pos, outside
// quotes and comments. It returns the index of the occurrence (end) and the index just
// past it (next); found is false if delim does not occur again, in which case end and next
// are both len(script).
func findDelimiter(script string, pos int, delim string) (end, next int, found bool) {
	i := pos
	n := len(script)
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
		case delim != "" && strings.HasPrefix(script[i:], delim):
			return i, i + len(delim), true
		default:
			i++
		}
	}
	return n, n, false
}

// matchDelimiterLine recognizes a `DELIMITER <token>` line, as the mysql client reads it,
// starting at or after pos (leading blank space is skipped). It returns the new delimiter
// token and the index just past the line (including its line break) if pos begins such a
// line.
func matchDelimiterLine(script string, pos int) (token string, next int, ok bool) {
	n := len(script)
	i := pos
	for i < n && (script[i] == ' ' || script[i] == '\t' || script[i] == '\r' || script[i] == '\n') {
		i++
	}
	const kw = "delimiter"
	if i+len(kw) > n || !strings.EqualFold(script[i:i+len(kw)], kw) {
		return "", 0, false
	}
	j := i + len(kw)
	if j >= n || (script[j] != ' ' && script[j] != '\t') {
		return "", 0, false
	}
	for j < n && (script[j] == ' ' || script[j] == '\t') {
		j++
	}
	tokStart := j
	for j < n && script[j] != '\n' && script[j] != '\r' {
		j++
	}
	token = strings.TrimRight(script[tokStart:j], " \t\r")
	if token == "" {
		return "", 0, false
	}
	k := j
	for k < n && (script[k] == '\r' || script[k] == '\n') {
		k++
	}
	return token, k, true
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
