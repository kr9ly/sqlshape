package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
)

// The `-- sqlshape: postgres <N>` declaration picks the grammar every statement is parsed
// with; RETURNING WITH (OLD AS ...) is 18 syntax.
func TestPostgresVersionDeclaration(t *testing.T) {
	const table = "CREATE TABLE t (id int PRIMARY KEY, a int);\n"
	const stmt18 = "UPDATE t SET a = 1 WHERE id = $1 RETURNING WITH (OLD AS o) o.a"

	s := mustLoad(t, table)
	if s.Version != pgparse.Default {
		t.Errorf("undeclared: version %d, want Default %d", s.Version, pgparse.Default)
	}
	if _, err := analyze.Analyze(s, stmt18); err == nil {
		t.Error("17 accepts 18 syntax")
	}

	s = mustLoad(t, "-- sqlshape: postgres 18\n"+table)
	if s.Version != pgparse.PG18 {
		t.Errorf("declared 18: version %d", s.Version)
	}
	if p := problems(s); p != "" {
		t.Errorf("the declaration is reported as a stray directive:\n%s", p)
	}
	// the grammar accepts it; the analyzer's RETURNING OLD/NEW support is separate work, so
	// only a syntax error would be wrong here
	if _, err := analyze.Analyze(s, stmt18); err != nil && strings.Contains(err.Error(), "syntax error") {
		t.Errorf("18 rejects 18 syntax: %v", err)
	}
	// the schema's own DDL is parsed with the declared grammar too
	if _, err := analyze.Load("-- sqlshape: postgres 18\n" + table + "CREATE TABLE u (p int, r int4range, PRIMARY KEY (p, r WITHOUT OVERLAPS));\n"); err != nil {
		t.Errorf("18 schema DDL: %v", err)
	}
	// anywhere in the file, and repeated with the same value
	s = mustLoad(t, table+"-- sqlshape: postgres 18\nCREATE TABLE u (id int);\n-- sqlshape: postgres 18\n")
	if s.Version != pgparse.PG18 || problems(s) != "" {
		t.Errorf("version %d, problems %q", s.Version, problems(s))
	}

	for sql, want := range map[string]string{
		"-- sqlshape: postgres 16\n" + table:                                "supports PostgreSQL 17, 18",
		"-- sqlshape: postgres x\n" + table:                                 "want a major version number",
		"-- sqlshape: postgres 17\n" + table + "-- sqlshape: postgres 18\n": "declared twice",
	} {
		if _, err := analyze.Load(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", sql, err, want)
		}
	}
	if err := s.Apply("-- sqlshape: postgres 17\nCREATE TABLE w (id int);"); err == nil {
		t.Error("Apply accepts a version declaration")
	}
}
