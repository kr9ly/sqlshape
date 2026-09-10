// Package cardinality proves, from a statement's facts alone, that it touches at most one
// row: the One contract, written once for every dialect. A producer that can prove more
// (an aggregate without GROUP BY, LIMIT 1, a single VALUES row) sets Facts.AtMostOne itself;
// this package adds the proof from keys — every leaf of the top scope is fixed to one row by
// a unique key whose columns are all equal to values known before the statement runs, the
// equalities of the joins carrying knowledge from one leaf to the next.
package cardinality

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// AtMostOne reports whether f proves the statement touches at most one row; when it does
// not, why names the first leaf the proof failed on, the way the checker reports it.
func AtMostOne(f *facts.Facts) (bool, string) {
	if f == nil {
		return false, "the statement's shape is not analyzed for its cardinality"
	}
	if f.AtMostOne {
		return true, ""
	}
	if f.Top == nil {
		return false, "the statement's shape is not analyzed for its cardinality"
	}
	if len(f.Top.Leaves) == 0 {
		return f.Kind == facts.Select, "" // SELECT without FROM is one row
	}
	return ScopeSingle(f.Top)
}

// ScopeSingle is the key proof over one scope: true when every leaf is fixed to at most one
// row. A column is known when it is Fixed, when an Edge leads to it from a known column, or
// when it belongs to a leaf already fixed to one row (a single row determines all its
// columns); a table leaf is single when all the columns of one of its UniqueKeys are known;
// a derived leaf is single when its own scope is, on its own.
func ScopeSingle(sc *facts.Scope) (bool, string) {
	known := map[facts.ColRef]bool{}
	for _, c := range sc.Fixed {
		known[c] = true
	}
	single := make([]bool, len(sc.Leaves))
	for changed := true; changed; {
		changed = false
		for i, l := range sc.Leaves {
			if single[i] {
				continue
			}
			if leafSingle(l, i, known) {
				single[i] = true
				changed = true
			}
		}
		for _, e := range sc.Edges {
			if known[e.To] {
				continue
			}
			if known[e.From] || (e.From.Leaf >= 0 && e.From.Leaf < len(single) && single[e.From.Leaf]) {
				known[e.To] = true
				changed = true
			}
		}
	}
	for i, l := range sc.Leaves {
		if !single[i] {
			return false, describe(l, known, i)
		}
	}
	return true, ""
}

func leafSingle(l facts.Leaf, i int, known map[facts.ColRef]bool) bool {
	for _, key := range l.UniqueKeys {
		if len(key) == 0 {
			continue
		}
		all := true
		for _, col := range key {
			if !known[facts.ColRef{Leaf: i, Column: col}] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	if l.Kind != facts.Table && l.View != nil {
		ok, _ := ScopeSingle(l.View)
		return ok
	}
	return false
}

func describe(l facts.Leaf, known map[facts.ColRef]bool, i int) string {
	name := l.Alias
	if name == "" {
		name = l.Table
	}
	if l.Kind != facts.Table {
		return name + ": a derived table or view is not proved to return one row"
	}
	if len(l.UniqueKeys) == 0 {
		return name + ": no unique key"
	}
	keys := make([]string, len(l.UniqueKeys))
	for k, key := range l.UniqueKeys {
		keys[k] = "(" + strings.Join(key, ", ") + ")"
	}
	var have []string
	for c := range known {
		if c.Leaf == i {
			have = append(have, c.Column)
		}
	}
	if len(have) > 0 {
		return fmt.Sprintf("%s: no unique key is fixed by equality (keys: %s; fixed: %s)", name, strings.Join(keys, ", "), strings.Join(have, ", "))
	}
	return fmt.Sprintf("%s: no unique key is fixed by equality (keys: %s)", name, strings.Join(keys, ", "))
}
