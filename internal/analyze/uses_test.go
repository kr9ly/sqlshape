package analyze

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Result.Uses: the relation columns a statement depends on, each once at its first
// position, views as themselves, nothing from inside view definitions.
func TestUses(t *testing.T) {
	base, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(base))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ sql, want string }{
		{"SELECT m.id, body FROM memos m WHERE m.user_id = $1", "memos.id@8 memos.body@14 memos.user_id@38"},
		{"SELECT * FROM memos", "memos.id@8 memos.user_id@8 memos.body@8 memos.deleted_at@8"},
		{"SELECT m FROM memos m", "memos.id@8 memos.user_id@8 memos.body@8 memos.deleted_at@8"},
		{"SELECT id FROM live_memos WHERE user_id = $1", "live_memos.id@8 live_memos.user_id@33"},
		{"INSERT INTO memos (user_id, body) VALUES ($1, $2)", "memos.user_id@20 memos.body@29"},
		{"INSERT INTO memos VALUES (DEFAULT, $1, $2, NULL)", "memos.id@13 memos.user_id@13 memos.body@13 memos.deleted_at@13"},
		{"UPDATE memos SET body = $1 WHERE id = $2", "memos.body@18 memos.id@34"},
		{"WITH m AS (SELECT id FROM memos WHERE deleted_at IS NULL) SELECT id FROM m", "memos.id@19 memos.deleted_at@39"},
		{"SELECT u.id FROM users u JOIN memos m ON m.user_id = u.id", "users.id@8 memos.user_id@42"},
		{"DELETE FROM memos WHERE id = $1", "memos.id@25"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var got []string
		for _, u := range r.Uses {
			got = append(got, fmt.Sprintf("%s.%s@%d", u.Table, u.Column, u.Position))
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.sql, g, c.want)
		}
	}
}
