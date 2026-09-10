package obligation

import (
	"regexp"
	"strings"
)

// Waivers reads the list of a `waive` directive: comma-separated (outside parentheses)
// entries of `<table> [<obligation>]`, where the obligation is the body as declared
// (`pinned(tenant_id)`, `via view`, a predicate) and its absence waives every obligation
// on the table ("*"). The grammar is the directive's, so every dialect reads it here.
func Waivers(list string) map[string][]string {
	out := map[string][]string{}
	depth, start := 0, 0
	items := []string{}
	for i := 0; i <= len(list); i++ {
		if i == len(list) || (list[i] == ',' && depth == 0) {
			items = append(items, strings.TrimSpace(list[start:i]))
			start = i + 1
			continue
		}
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	for _, it := range items {
		if it == "" {
			continue
		}
		table, spec, _ := strings.Cut(it, " ")
		spec = strings.Join(strings.Fields(spec), " ")
		if spec == "" {
			spec = "*"
		}
		out = AddWaiver(out, table, spec)
	}
	return out
}

// AddWaiver appends specs to the table's waivers.
func AddWaiver(m map[string][]string, table string, specs ...string) map[string][]string {
	if m == nil {
		m = map[string][]string{}
	}
	m[table] = append(m[table], specs...)
	return m
}

var sqlDirective = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*([a-z ]+?)[ \t]+(.+?)[ \t]*$`)

// StatementWaivers reads a statement's opt-outs from its `-- sqlshape:` lines:
// `unfiltered a, b` (the predicate obligations of a and b) and `waive a pinned(x), b` (one
// named obligation, or every obligation, of a table). Empty when it has none.
func StatementWaivers(sql string) map[string][]string {
	out := map[string][]string{}
	for _, m := range sqlDirective.FindAllStringSubmatch(sql, -1) {
		switch m[1] {
		case "unfiltered":
			for _, it := range strings.Split(m[2], ",") {
				if it = strings.TrimSpace(it); it != "" {
					out = AddWaiver(out, it, "unfiltered")
				}
			}
		case "waive":
			for table, spec := range Waivers(m[2]) {
				out = AddWaiver(out, table, spec...)
			}
		}
	}
	return out
}
