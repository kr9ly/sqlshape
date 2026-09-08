package obligation

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// Parse reads one schema.sql directive (whitespace-normalized, without the `-- sqlshape:`
// prefix) as an obligation on subject. Two grammars are accepted:
//
//	require <body> [on <kinds>]
//	visible where <expr>            (= require <expr> on read; the original spelling)
//
// where <body> is `pinned(col)`, `immutable(col)`, `via view`, or an SQL boolean
// expression, and <kinds> is a comma-separated list of select / insert / update / delete
// / read / write / all. ok is false for a directive that is not an obligation at all
// (another package's), as opposed to a malformed one, which is an error.
func Parse(subject, directive string) (o Obligation, ok bool, err error) {
	norm := strings.Join(strings.Fields(directive), " ")
	lower := strings.ToLower(norm)
	o = Obligation{Subject: subject, Source: norm}
	switch {
	case strings.HasPrefix(lower, "visible where "):
		o.Body.Predicate = strings.TrimSpace(norm[len("visible where "):])
		o.Kinds = OnRead
		return o, true, nil
	case strings.HasPrefix(lower, "require "):
	default:
		return Obligation{}, false, nil
	}
	body := strings.TrimSpace(norm[len("require "):])
	// the `on <kinds>` suffix: the last ` on ` outside parentheses
	if i := lastTopLevel(body, " on "); i >= 0 {
		kinds, err := parseKinds(body[i+len(" on "):])
		if err != nil {
			return Obligation{}, true, fmt.Errorf("%s: %v", norm, err)
		}
		o.Kinds = kinds
		body = strings.TrimSpace(body[:i])
	}
	lb := strings.ToLower(body)
	switch {
	case body == "":
		return Obligation{}, true, fmt.Errorf("%s: nothing required", norm)
	case strings.HasPrefix(lb, "pinned(") && strings.HasSuffix(lb, ")"):
		o.Body.Pinned = strings.TrimSpace(body[len("pinned(") : len(body)-1])
		if o.Body.Pinned == "" {
			return Obligation{}, true, fmt.Errorf("%s: pinned needs a column", norm)
		}
		if o.Kinds == 0 {
			o.Kinds = OnAll
		}
	case strings.HasPrefix(lb, "immutable(") && strings.HasSuffix(lb, ")"):
		o.Body.Immutable = strings.TrimSpace(body[len("immutable(") : len(body)-1])
		if o.Body.Immutable == "" {
			return Obligation{}, true, fmt.Errorf("%s: immutable needs a column", norm)
		}
		if o.Kinds == 0 {
			o.Kinds = OnUpdate
		}
	case lb == "via view":
		o.Body.ViaView = true
		if o.Kinds == 0 {
			o.Kinds = OnRead
		}
		o.Body.IncludeWrites = o.Kinds&OnWrite != 0
	default:
		o.Body.Predicate = body
		if o.Kinds == 0 {
			o.Kinds = OnRead
		}
	}
	return o, true, nil
}

// lastTopLevel finds the last occurrence of sep outside parentheses and quotes.
func lastTopLevel(s, sep string) int {
	depth, quote := 0, byte(0)
	last := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(strings.ToLower(s[i:]), sep):
			last = i
		}
	}
	return last
}

func parseKinds(list string) (Kinds, error) {
	var k Kinds
	for _, w := range strings.Split(list, ",") {
		switch strings.ToLower(strings.TrimSpace(w)) {
		case "select":
			k |= OnSelect
		case "insert":
			k |= OnInsert
		case "update":
			k |= OnUpdate
		case "delete":
			k |= OnDelete
		case "read":
			k |= OnRead
		case "write":
			k |= OnWrite
		case "all":
			k |= OnAll
		case "":
			return 0, fmt.Errorf("empty statement kind in %q", list)
		default:
			return 0, fmt.Errorf("unknown statement kind %q (select, insert, update, delete, read, write, all)", strings.TrimSpace(w))
		}
	}
	return k, nil
}

// Declarations collects the obligations written in schema.sql: every relation's
// `require ...` and `visible where ...` directives. Malformed ones come back as Problems.
func Declarations(s *schema.Schema) ([]Obligation, []Problem) {
	var out []Obligation
	var problems []Problem
	for _, rel := range s.Relations {
		for _, d := range rel.Directives {
			o, ok, err := Parse(rel.FullName(), d)
			if !ok {
				continue
			}
			if err != nil {
				problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: err.Error()})
				continue
			}
			if col := o.Body.Pinned + o.Body.Immutable; col != "" && rel.Column(col) == nil {
				problems = append(problems, Problem{Subject: rel.FullName(), Source: d, Message: fmt.Sprintf("%s has no column %s", rel.Name, col)})
				continue
			}
			out = append(out, o)
		}
	}
	return out, problems
}

// FromFlags expands vet's table-wide flags into per-table obligations, the sugar they
// always were: -require-columns=a,b is `require pinned(a)` on every table that has the
// column, -no-tables is `require via view on all`, -no-table-reads is `require via view`.
func FromFlags(s *schema.Schema, requireColumns string, noTables, noTableReads bool) []Obligation {
	var out []Obligation
	var cols []string
	for _, c := range strings.Split(requireColumns, ",") {
		if c = strings.TrimSpace(c); c != "" {
			cols = append(cols, c)
		}
	}
	for _, rel := range s.Relations {
		if rel.Kind != schema.Table {
			continue
		}
		switch {
		case noTables:
			out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnAll, Body: Body{ViaView: true, IncludeWrites: true}, Source: "-no-tables"})
		case noTableReads:
			out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnRead, Body: Body{ViaView: true}, Source: "-no-table-reads"})
		}
		for _, c := range cols {
			if rel.Column(c) != nil {
				out = append(out, Obligation{Subject: rel.FullName(), Kinds: OnAll, Body: Body{Pinned: c}, Source: "-require-columns=" + requireColumns})
			}
		}
	}
	return out
}
