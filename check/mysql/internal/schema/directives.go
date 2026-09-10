package schema

import (
	"regexp"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// Directives: the `-- sqlshape: <text>` comment lines written above a CREATE TABLE / CREATE
// VIEW annotate it. A table's are the obligations x/obligation parses (`require ...`,
// `aggregate ...`, `context ...`, `sensitive ...`, `transitions ...`, `visible where ...`);
// a view's are those plus its own opt-outs (`unfiltered t1, t2`, `waive t pinned(x)`),
// which the analyzer carries on the leaves of the view's body.

var directiveLine = regexp.MustCompile(`^[ \t]*--[ \t]*sqlshape:[ \t]*(.+?)[ \t]*$`)

// leadingDirectives reads the directives among the comment lines in front of a statement
// (the splitter keeps them with the statement); the file's version declaration is not one.
func leadingDirectives(sql string) []string {
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "--"), strings.HasPrefix(trimmed, "#"):
			if m := directiveLine.FindStringSubmatch(line); m != nil && !versionLine.MatchString(line) {
				out = append(out, strings.Join(strings.Fields(m[1]), " "))
			}
			continue
		}
		return out // the statement proper
	}
	return out
}

// obligationDirective reports whether a normalized directive is one x/obligation reads.
func obligationDirective(norm string) bool {
	lower := strings.ToLower(norm)
	for _, p := range []string{"require ", "aggregate ", "context ", "sensitive ", "transitions ", "visible where "} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// tableDirectives applies the directives written above a CREATE TABLE.
func (s *Schema) tableDirectives(t *Table, sql string, pos int) {
	for _, d := range leadingDirectives(sql) {
		if !obligationDirective(d) {
			s.problem(pos, "table %s: unknown directive %q", t.Name, d)
			continue
		}
		t.Directives = append(t.Directives, d)
	}
}

// viewDirectives applies the directives written above a CREATE VIEW.
func (s *Schema) viewDirectives(v *View, sql string, pos int) {
	for _, d := range leadingDirectives(sql) {
		lower := strings.ToLower(d)
		switch {
		case strings.HasPrefix(lower, "require "), strings.HasPrefix(lower, "context "), strings.HasPrefix(lower, "sensitive "):
			v.Directives = append(v.Directives, d)
		case strings.HasPrefix(lower, "unfiltered "):
			for _, t := range strings.Split(d[len("unfiltered "):], ",") {
				if t = strings.TrimSpace(t); t != "" {
					if v.Unfiltered == nil {
						v.Unfiltered = map[string]bool{}
					}
					v.Unfiltered[t] = true
					v.Waived = obligation.AddWaiver(v.Waived, t, "unfiltered")
				}
			}
		case strings.HasPrefix(lower, "waive "):
			for table, spec := range obligation.Waivers(d[len("waive "):]) {
				v.Waived = obligation.AddWaiver(v.Waived, table, spec...)
			}
		default:
			s.problem(pos, "view %s: unknown directive %q", v.Name, d)
		}
	}
}

// CheckName is the CHECK constraint's name as the server reports it: the declared one, or
// the generated `<table>_chk_<n>` numbering the unnamed constraints in order.
func (t *Table) CheckName(c *Check) string {
	if c.Name != "" {
		return c.Name
	}
	n := 0
	for _, other := range t.Checks {
		if other.Name == "" {
			n++
		}
		if other == c {
			break
		}
	}
	return t.Name + "_chk_" + itoa(n)
}

// ForeignKeyName is the REFERENCES constraint's name as the server reports it: the
// declared one, or the generated `<table>_ibfk_<n>`.
func (t *Table) ForeignKeyName(fk *ForeignKey) string {
	if fk.Name != "" {
		return fk.Name
	}
	n := 0
	for _, other := range t.ForeignKeys {
		if other.Name == "" {
			n++
		}
		if other == fk {
			break
		}
	}
	return t.Name + "_ibfk_" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
