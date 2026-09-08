package obligation_test

import (
	"os"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/obligation"
)

// TestVisibleWhere: `-- sqlshape: visible where deleted_at IS NULL` on memos, the first
// obligation, over the analyzer's shared schema. Moved here from analyze when the
// judgment left the analyzer.
func TestVisibleWhere(t *testing.T) {
	base, err := os.ReadFile("../analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := analyze.Load(string(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Fatalf("schema: %s", p)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("declarations: %+v", problems)
	}
	cases := []struct {
		sql  string
		want string // substring of the failure, "" when none expected
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
		{"INSERT INTO memos (user_id, body) VALUES ($1, $2) RETURNING id", ""},
		{"UPDATE memos SET body = $1 WHERE id = $2 AND deleted_at IS NULL RETURNING id", ""},
		// MERGE's ON is a level of its own now (the note-based check never looked at MERGE)
		{"MERGE INTO memos m USING users u ON m.user_id = u.id AND m.deleted_at IS NULL WHEN MATCHED THEN UPDATE SET body = 'x'", ""},
		{"MERGE INTO memos m USING users u ON m.user_id = u.id WHEN MATCHED THEN UPDATE SET body = 'x'", "add that predicate for memos m"},
	}
	for _, c := range cases {
		r, err := analyze.Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			if d.Failed() {
				got = append(got, d.Message)
			}
		}
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected failure %q", c.sql, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("%s: want %q, got %q", c.sql, c.want, got)
		}
	}

	// a view that forgets the policy is judged on its own body; its readers are not
	bad, err := analyze.Load(string(base) + "\nCREATE VIEW all_memos AS SELECT id FROM memos;")
	if err != nil {
		t.Fatal(err)
	}
	r, err := analyze.AnalyzeView(bad, bad.Relation("", "all_memos"))
	if err != nil {
		t.Fatal(err)
	}
	if ds := obligation.Check(bad, decls, r.Facts, lowerer{bad}); len(ds) != 1 || !ds[0].Failed() {
		t.Errorf("AnalyzeView: %+v", ds)
	}
	r, err = analyze.Analyze(bad, "SELECT id FROM all_memos")
	if err != nil {
		t.Fatal(err)
	}
	if ds := obligation.Check(bad, decls, r.Facts, lowerer{bad}); len(ds) != 0 {
		t.Errorf("reader of bad view: %+v", ds)
	}
}
