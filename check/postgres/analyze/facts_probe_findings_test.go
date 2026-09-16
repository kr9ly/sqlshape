package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Found by x/factsprobe (the facts oracle, first run on PostgreSQL): a subquery inside a
// join's ON clause referencing a column of either joined side recorded the reference as a
// Known term ("t1.s": a value fixed before the statement runs) instead of an Outer term (the
// enclosing row's own column). The subquery's scope sits under the passthrough scope the
// join is analyzed in, whose items (the two sides) were not yet the query level's while the
// join was being analyzed, so outerRef's resolution against the level's items failed and
// fell through to termFacts. In WHERE the same reference was Outer all along.
func TestFactsOuterRefInJoinOn(t *testing.T) {
	s, err := Load(`
CREATE TABLE t0 (id integer PRIMARY KEY, a integer, s text);
CREATE TABLE t1 (id integer PRIMARY KEY, a integer, s text, UNIQUE (a));
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SELECT t1.a FROM t0 JOIN t1 ON t1.a = t0.a AND EXISTS (SELECT 1 FROM t0 AS s0 WHERE s0.s = t1.s)",
		"SELECT t1.a FROM t0 JOIN t1 ON t1.a = t0.a AND EXISTS (SELECT 1 FROM t0 AS s0 WHERE s0.s = t0.s)",
		"SELECT t1.a FROM t0 LEFT JOIN t1 ON t1.a = t0.a AND t1.id IN (SELECT s0.id FROM t0 AS s0 WHERE s0.s = t1.s)",
		"SELECT t1.a FROM t0 JOIN t1 ON t1.a = t0.a WHERE EXISTS (SELECT 1 FROM t0 AS s0 WHERE s0.s = t1.s)",
	} {
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var exists *facts.Pred
		for i := range r.Facts.Top.Preds {
			if r.Facts.Top.Preds[i].Op == facts.Exists {
				exists = &r.Facts.Top.Preds[i]
			}
		}
		if exists == nil || exists.Sub == nil {
			t.Fatalf("%s: no EXISTS predicate in the top scope's facts", sql)
		}
		var outer int
		for _, p := range exists.Sub.Preds {
			if p.Op == facts.Eq && p.Col.Column == "s" {
				if p.Term.Kind != facts.Outer {
					t.Errorf("%s: s0.s = <outer column> recorded as a %v term %q, want Outer", sql, p.Term.Kind, p.Term.Text)
				}
				outer++
			}
		}
		if outer != 1 {
			t.Errorf("%s: %d equalities on s0.s in the EXISTS body, want 1", sql, outer)
		}
	}
}
