package parsegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Statement is one SQL statement cut out of a mysqltest .test file.
type Statement struct {
	File   string
	Line   int
	Expect string // the --error name/number that preceded it, or ""
	SQL    string
}

// Header is the comment the spike driver prints back: "/* file:line [expect=X] */".
func (s Statement) Header() string {
	if s.Expect != "" {
		return fmt.Sprintf("/* %s:%d expect=%s */", s.File, s.Line, s.Expect)
	}
	return fmt.Sprintf("/* %s:%d */", s.File, s.Line)
}

// SplitTestDir reads every *.test under dir the way mysqltest does: a command
// runs to the current delimiter found outside quotes and comments, whatever
// follows the delimiter on the same line starts the next command, and
// `delimiter X` changes the terminator. Only commands that start like SQL are
// kept; mysqltest's own commands (let, eval, connect, ...) and statements
// containing $variables are dropped. Files are read as latin-1 so that tests
// in other client charsets survive as bytes.
func SplitTestDir(dir string) ([]Statement, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".test") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Statement
	total := 0
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, 0, err
		}
		stmts, n := splitTest(name, string(b))
		out = append(out, stmts...)
		total += n
	}
	return out, total, nil
}

var (
	reSQLFirst  = regexp.MustCompile(`(?i)^(select|insert|update|delete|replace|create|drop|alter|set|show|explain|describe|desc|with|grant|revoke|flush|reset|start|begin|commit|rollback|savepoint|release|lock|unlock|analyze|optimize|repair|check|checksum|truncate|rename|load|call|do|handler|help|use|prepare|execute|deallocate|xa|install|uninstall|binlog|cache|kill|purge|change|stop|resume|import|clone|restart|shutdown|get|signal|resignal|table|values|declare|open|fetch|close|return|iterate|leave|loop|repeat|while|if|case)\b`)
	reCtlFlow   = regexp.MustCompile(`(?i)^(if|while)\s*\(`)
	reErrorCmd  = regexp.MustCompile(`(?i)^(?:--\s*)?error\s+(.+?)\s*;?\s*$`)
	reDelimCmd  = regexp.MustCompile(`(?i)^(?:--\s*)?delimiter\s+(.*)$`)
	reHeredoc   = regexp.MustCompile(`(?i)^(?:--\s*)?(perl|write_file|append_file)\b`)
	reHeredocTo = regexp.MustCompile(`(?i)\b(perl|write_file|append_file)\b(?:\s+\S+)?\s+(\w+)\s*;?\s*$`)
	reVar       = regexp.MustCompile(`\$\w+`)
	reBlockOnly = regexp.MustCompile(`^\{?\s*$`)
)

type quoteState struct {
	q            byte
	blockComment bool
}

// findDelim returns the index of delim in line outside quotes and comments, or -1.
func findDelim(line, delim string, st *quoteState) int {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if st.blockComment {
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				st.blockComment = false
				i++
			}
			continue
		}
		if st.q != 0 {
			if c == '\\' && st.q != '`' {
				i++
				continue
			}
			if c == st.q {
				st.q = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			st.q = c
		case c == '/' && i+1 < len(line) && line[i+1] == '*':
			st.blockComment = true
			i++
		case c == '#':
			return -1
		case c == '-' && i+1 < len(line) && line[i+1] == '-' && (i == 0 || isSpace(line[i-1])) && (i+2 >= len(line) || isSpace(line[i+2])):
			return -1
		case strings.HasPrefix(line[i:], delim):
			return i
		}
	}
	return -1
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func splitTest(name, text string) ([]Statement, int) {
	lines := strings.Split(text, "\n")
	delim := ";"
	expect := ""
	heredoc := ""
	inHeredoc := false
	var buf []string
	bufLine := 0
	isSQL := false
	st := quoteState{}
	var out []Statement
	total := 0
	i := 0
	var pending *string
	pendingLine := 0
	for i < len(lines) || pending != nil {
		var raw string
		var lineNo int
		if pending != nil {
			raw = *pending
			lineNo = pendingLine
			pending = nil
		} else {
			raw = lines[i]
			lineNo = i + 1
			i++
		}
		if inHeredoc {
			if strings.TrimSpace(raw) == heredoc {
				inHeredoc = false
			}
			continue
		}
		if len(buf) == 0 {
			t := strings.TrimSpace(raw)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if m := reErrorCmd.FindStringSubmatch(t); m != nil {
				expect = m[1]
				continue
			}
			if m := reDelimCmd.FindStringSubmatch(t); m != nil {
				d := strings.TrimSpace(m[1])
				if strings.HasSuffix(d, delim) && len(d) > len(delim) {
					d = d[:len(d)-len(delim)]
				}
				if strings.HasSuffix(d, ";") && len(d) > 1 && delim == ";" {
					d = d[:len(d)-1]
				}
				if d == "" {
					d = ";"
				}
				delim = d
				continue
			}
			if reHeredoc.MatchString(t) {
				heredoc = "EOF"
				if m := reHeredocTo.FindStringSubmatch(t); m != nil {
					heredoc = m[2]
				}
				inHeredoc = true
				continue
			}
			if strings.HasPrefix(t, "--") {
				continue // other one-line commands; --error still applies to the next statement
			}
			isSQL = reSQLFirst.MatchString(t) && !reCtlFlow.MatchString(t)
			bufLine = lineNo
			st = quoteState{}
		}
		at := findDelim(raw, delim, &st)
		if at < 0 {
			buf = append(buf, raw)
			continue
		}
		buf = append(buf, raw[:at])
		if rest := raw[at+len(delim):]; strings.TrimSpace(rest) != "" {
			r := rest
			pending = &r
			pendingLine = lineNo
		}
		stmt := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = nil
		if isSQL {
			total++
			if !reVar.MatchString(stmt) {
				out = append(out, Statement{File: name, Line: bufLine, Expect: expect, SQL: stmt})
			}
			expect = ""
		} else if !reBlockOnly.MatchString(stmt) {
			expect = "" // a non-SQL command consumed the --error; `{` / `}` do not
		}
	}
	return out, total
}

// JoinForDriver renders statements in the spike driver's input format:
// header line, statement, and "\n;;\n" between statements.
func JoinForDriver(stmts []Statement) string {
	var b strings.Builder
	for i, s := range stmts {
		if i > 0 {
			b.WriteString("\n;;\n")
		}
		b.WriteString(s.Header())
		b.WriteByte('\n')
		b.WriteString(s.SQL)
	}
	b.WriteString("\n;;\n")
	return b.String()
}
