package vet

import (
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/expand"
)

// The failure contract of a statement is written in its template:
//
//	-- sqlshape: expect users_email_key, orders.total
//
// listing the constraints it is prepared to violate (a NOT NULL as table.column). The
// checker diffs that against what the analyzer says each expansion may violate: an
// undeclared possible violation means the caller has not thought about that failure,
// a declared impossible one means the schema no longer backs the handling. The runtime
// turns the PG error into a ConstraintError keyed by the same names.

var expectLine = directive("expect")

// directive matches `-- sqlshape: <verb> a, b, c` lines; group 1 is the list.
func directive(verb string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*` + verb + `[ \t]+(.+?)[ \t]*$`)
}

// expectations parses the expect lines of a template: key → offset of its mention,
// and the offset of the first expect line (0 when there is none).
func expectations(text string) (map[string]int, int) {
	return directiveItems(text, expectLine)
}

// directiveItems parses the comma-separated items of a directive's lines.
func directiveItems(text string, re *regexp.Regexp) (map[string]int, int) {
	out := map[string]int{}
	first := 0
	for i, m := range re.FindAllStringSubmatchIndex(text, -1) {
		if i == 0 {
			first = m[0]
		}
		list := text[m[2]:m[3]]
		off := m[2]
		for _, item := range strings.Split(list, ",") {
			trimmed := strings.TrimSpace(item)
			if trimmed != "" {
				if _, dup := out[trimmed]; !dup {
					out[trimmed] = off + strings.Index(item, trimmed)
				}
			}
			off += len(item) + 1
		}
	}
	return out, first
}

// possibleViolations filters an expansion's violations by what P can actually send:
// a NOT NULL violation carried by a parameter is dropped when its Go type cannot be NULL.
func (c *checker) possibleViolations(e *expand.Expansion, r *analyze.Result, pType types.Type) []analyze.Violation {
	var out []analyze.Violation
	for _, v := range r.Violations {
		if v.Param > 0 {
			nullable := true
			for _, p := range e.Params {
				if int32(p.N) == v.Param {
					if gt, err := c.resolvePath(pType, p.Path); err == nil {
						_, nullable = unwrapNullable(gt)
					}
				}
			}
			if !nullable {
				continue
			}
		}
		out = append(out, v)
	}
	return out
}

// checkExpectations reports the diff between the template's expect line and the
// violations possible in any expansion (possible: key → violation, with the branch it
// was first seen in).
func (c *checker) checkExpectations(lit literal, possible map[string]analyze.Violation, branch map[string]string, report func(token.Pos, string, ...any)) {
	expected, at := expectations(lit.text)
	var keys []string
	for k := range possible {
		if _, ok := expected[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := possible[k]
		report(lit.pos(at), "may violate %s (%s); add `-- sqlshape: expect %s` to the template or make it impossible%s", k, describeViolation(v), k, branch[k])
	}
	keys = keys[:0]
	for k := range expected {
		if _, ok := possible[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		report(lit.pos(expected[k]), "expects %s but no expansion can violate it", k)
	}
}

func describeViolation(v analyze.Violation) string {
	s := describeViolationAt(v)
	if v.Function != "" {
		s += ", through " + v.Function + "()"
	}
	return s
}

func describeViolationAt(v analyze.Violation) string {
	cols := strings.Join(v.Columns, ", ")
	switch v.Code {
	case "23505":
		return "UNIQUE (" + cols + ") on " + v.Table + ", SQLSTATE 23505"
	case "23503":
		return "FOREIGN KEY (" + cols + ") on " + v.Table + " REFERENCES " + v.RefTable + ", SQLSTATE 23503"
	case "23514":
		return "CHECK on " + v.Table + " (" + cols + "), SQLSTATE 23514"
	case "23502":
		return "NOT NULL on " + v.Table + "." + cols + ", SQLSTATE 23502"
	}
	if v.Trigger != "" {
		s := "raised by trigger " + v.Trigger + " on " + v.Table
		if v.Name != "" {
			s += " as " + v.Name
		}
		return s + ", SQLSTATE " + v.Code
	}
	if v.Name != "" {
		return "raised as " + v.Name + ", SQLSTATE " + v.Code
	}
	return "SQLSTATE " + v.Code
}

var notNullLine = directive("not null")

// notNullOverrides parses `-- sqlshape: not null col, col` lines: result columns the
// template author asserts are never NULL (the SQL-side twin of the `col:",notnull"` tag).
func notNullOverrides(text string) (map[string]int, int) {
	return directiveItems(text, notNullLine)
}
