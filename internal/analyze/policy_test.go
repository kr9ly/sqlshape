package analyze

import (
	"os"
	"strings"
	"testing"
)

// TestVisibilityPolicy: `-- sqlshape: visible where deleted_at IS NULL` on memos.
func TestVisibilityPolicy(t *testing.T) {
	base, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema: %s", p)
	}
	cases := []struct {
		sql  string
		want string // substring of the policy note, "" when none expected
	}{
		{"SELECT id FROM memos WHERE deleted_at IS NULL", ""},
		{"SELECT m.id FROM memos m WHERE m.deleted_at IS NULL AND m.user_id = $1", ""},
		{"SELECT m.id FROM users u JOIN memos m ON m.user_id = u.id AND m.deleted_at IS NULL", ""},
		{"SELECT m.id FROM users u LEFT JOIN memos m ON m.user_id = u.id AND m.deleted_at IS NULL WHERE u.id = $1", ""},
		{"SELECT id FROM live_memos WHERE user_id = $1", ""},
		{"UPDATE memos SET body = $1 WHERE id = $2 AND deleted_at IS NULL", ""},
		{"SELECT id FROM memos", "rows of memos are visible where deleted_at IS NULL: add that predicate for memos"},
		{"SELECT m.id FROM memos m WHERE m.user_id = $1", "add that predicate for memos m"},
		{"SELECT m.id FROM users u LEFT JOIN memos m ON m.user_id = u.id WHERE m.deleted_at IS NULL", ""},
		{"SELECT u.id FROM users u WHERE EXISTS (SELECT 1 FROM memos m WHERE m.user_id = u.id)", "add that predicate for memos m"},
		{"SELECT id FROM memos WHERE deleted_at IS NOT NULL", "add that predicate"},
		{"UPDATE memos SET body = $1 WHERE id = $2", "add that predicate"},
		{"DELETE FROM memos WHERE id = $1", "add that predicate"},
		{"-- sqlshape: unfiltered memos\nSELECT id FROM memos WHERE deleted_at IS NOT NULL", ""},
		{"INSERT INTO memos (user_id, body) VALUES ($1, $2)", ""},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []string
		for _, n := range r.Notes {
			if n.Code == notePolicy {
				got = append(got, n.Message)
			}
		}
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected policy note %q", c.sql, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("%s: want %q, got %q", c.sql, c.want, got)
		}
	}

	// a view that forgets the policy is a schema problem, found by AnalyzeView
	bad, err := Load(string(base) + "\nCREATE VIEW all_memos AS SELECT id FROM memos;")
	if err != nil {
		t.Fatal(err)
	}
	r, err := AnalyzeView(bad, bad.Relation("", "all_memos"))
	if err != nil || len(r.Notes) != 1 || r.Notes[0].Code != notePolicy {
		t.Errorf("AnalyzeView: %v %v", err, r.Notes)
	}
	// readers of a view do not inherit the view body's findings
	r, err = Analyze(bad, "SELECT id FROM all_memos")
	if err != nil || len(r.Notes) != 0 {
		t.Errorf("reader of bad view: %v %v", err, r.Notes)
	}
}
