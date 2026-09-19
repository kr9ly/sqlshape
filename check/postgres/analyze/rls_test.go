package analyze

import (
	"strings"
	"testing"
)

func TestAnalyzePolicy(t *testing.T) {
	base := `
CREATE DOMAIN yen AS bigint;
CREATE TABLE docs (id int PRIMARY KEY, tenant uuid NOT NULL, price yen, body text);
ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
`
	cases := []struct{ policy, want string }{
		{"CREATE POLICY p ON docs USING (tenant = current_setting('app.tenant')::uuid);", ""},
		{"CREATE POLICY p ON docs FOR UPDATE USING (body IS NOT NULL) WITH CHECK (length(body) < 100);", ""},
		{"CREATE POLICY p ON docs USING (body);", "argument of USING must be type boolean, not type text"},
		{"CREATE POLICY p ON docs WITH CHECK (id + 1);", "argument of WITH CHECK must be type boolean, not type integer"},
		{"CREATE POLICY p ON docs USING (nope = 1);", "42703"},
		{"CREATE POLICY p ON docs USING (count(*) > 0);", "aggregate functions are not allowed"},
		{"CREATE POLICY p ON docs USING (price > 10 AND price = id);", "domain mismatch"},
	}
	for _, c := range cases {
		s, err := Load(base + c.policy)
		if err != nil {
			t.Fatal(err)
		}
		rel := s.Relation("", "docs")
		if len(rel.Policies) != 1 {
			t.Fatalf("%s: %v", c.policy, s.Problems)
		}
		notes, err := AnalyzePolicy(s, rel, rel.Policies[0])
		got := ""
		if err != nil {
			got = err.Error()
		}
		for _, n := range notes {
			got += " " + n.Message
		}
		if (c.want == "" && got != "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%s:\n  want %q\n  got  %q", c.policy, c.want, got)
		}
	}
	s, _ := Load(base + "CREATE POLICY p ON docs USING (tenant = current_setting('app.tenant', true)::uuid AND body = current_setting('x'));")
	if reads := SettingReads(s.Relation("", "docs").Policies[0]); len(reads) != 1 || reads[0] != "app.tenant" {
		t.Errorf("setting reads: %v", reads)
	}
}
