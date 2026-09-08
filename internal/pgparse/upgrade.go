package pgparse

import (
	"encoding/json"
	"fmt"
	"strings"
)

// upgrade rewrites the JSON parse tree of an older version into the shape of the newest
// version's pg_query.proto, so protojson can read it into the newest node types. Between
// majors libpg_query renames or drops a handful of fields; each older version lists its
// rewrites here, and a field a rewrite does not cover fails the decode loudly (protojson
// rejects unknown fields) rather than vanishing.
func upgrade(v Version, js string) (string, error) {
	rules := upgrades[v]
	if len(rules.keys) == 0 || !containsAny(js, rules.keys) {
		return js, nil
	}
	var tree any
	if err := json.Unmarshal([]byte(js), &tree); err != nil {
		return "", fmt.Errorf("pgparse: reading parse tree: %w", err)
	}
	rules.walk(tree)
	out, err := json.Marshal(tree)
	if err != nil {
		return "", fmt.Errorf("pgparse: rewriting parse tree: %w", err)
	}
	return string(out), nil
}

// rewrite is one version's set of field rewrites; keys lists every JSON key it touches so
// a tree without any of them skips the decode/encode round trip.
type rewrite struct {
	keys  []string
	apply func(obj map[string]any)
}

func (r rewrite) walk(node any) {
	switch n := node.(type) {
	case map[string]any:
		r.apply(n)
		for _, v := range n {
			r.walk(v)
		}
	case []any:
		for _, v := range n {
			r.walk(v)
		}
	}
}

var upgrades = map[Version]rewrite{
	// 17 -> 18
	PG17: {
		keys: []string{`"returningList"`, `"inhcount"`, `"rctype"`, `"SinglePartitionSpec"`},
		apply: func(obj map[string]any) {
			// RETURNING grew options (RETURNING OLD/NEW): the list became a clause
			if list, ok := obj["returningList"]; ok {
				delete(obj, "returningList")
				obj["returningClause"] = map[string]any{"exprs": list}
			}
			// Constraint.inhcount and RowCompareExpr.rctype were dropped; both are catalog
			// or executor state, never set in a raw parse tree
			delete(obj, "inhcount")
			delete(obj, "rctype")
			// SinglePartitionSpec (ALTER TABLE ... SPLIT/MERGE PARTITION) was reverted before
			// 17.0 shipped; the node cannot appear for SQL 17 accepts
			delete(obj, "SinglePartitionSpec")
		},
	},
}

func containsAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
