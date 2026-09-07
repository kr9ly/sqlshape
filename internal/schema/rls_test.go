package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

func TestPolicies(t *testing.T) {
	base := `
CREATE TABLE docs (id int PRIMARY KEY, tenant uuid NOT NULL, body text);
CREATE VIEW v AS SELECT * FROM docs;
`
	s := mustLoad(t, base+`
ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
ALTER TABLE docs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON docs USING (tenant = current_setting('app.tenant')::uuid);
CREATE POLICY writers ON docs AS RESTRICTIVE FOR INSERT TO editor, admin WITH CHECK (body IS NOT NULL);
CREATE POLICY gone ON docs FOR DELETE USING (false);
ALTER POLICY tenant_rows ON docs USING (tenant = current_setting('app.tenant', true)::uuid);
ALTER POLICY gone ON docs RENAME TO removable;
DROP POLICY removable ON docs;
ALTER TABLE docs NO FORCE ROW LEVEL SECURITY;
`)
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	rel := s.Relation("", "docs")
	if !rel.RowSecurity || rel.ForceRowSecurity {
		t.Errorf("flags: %v %v", rel.RowSecurity, rel.ForceRowSecurity)
	}
	if len(rel.Policies) != 2 {
		t.Fatalf("policies: %d", len(rel.Policies))
	}
	tr := rel.Policy("tenant_rows")
	if tr == nil || tr.Command != "all" || !tr.Permissive || tr.Roles != nil || !strings.Contains(schema.Deparse(tr.Using), "true") || tr.WithCheck != nil {
		t.Errorf("tenant_rows: %+v", tr)
	}
	if !strings.Contains(tr.Definition, "ALTER POLICY") {
		t.Errorf("definition should carry the ALTER: %s", tr.Definition)
	}
	w := rel.Policy("writers")
	if w == nil || w.Command != "insert" || w.Permissive || strings.Join(w.Roles, ",") != "editor,admin" || w.Using != nil || w.WithCheck == nil {
		t.Errorf("writers: %+v", w)
	}
	for _, c := range []struct{ sql, problem string }{
		{"CREATE POLICY p ON v USING (true);", "is not a table"},
		{"CREATE POLICY p ON nope USING (true);", "does not exist"},
		{"CREATE POLICY p ON docs USING (true); CREATE POLICY p ON docs USING (true);", "already exists"},
		{"DROP POLICY p ON docs;", "no such policy"},
		{"DROP POLICY IF EXISTS p ON docs;", ""},
	} {
		s := mustLoad(t, base+c.sql)
		var msgs []string
		for _, p := range s.Problems {
			msgs = append(msgs, p.Message)
		}
		got := strings.Join(msgs, "\n")
		if (c.problem == "" && got != "") || (c.problem != "" && !strings.Contains(got, c.problem)) {
			t.Errorf("%s: want %q, got %q", c.sql, c.problem, got)
		}
	}
}
