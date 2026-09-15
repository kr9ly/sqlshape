package dialect

import (
	"testing"

	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

const checkOptionContractSchema = `-- sqlshape: mysql 8.4
-- sqlshape: require pinned(tenant_id)
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  tenant_id INT NOT NULL,
  v INT NOT NULL
);
CREATE VIEW v1 AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1;
CREATE VIEW v2 AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH CASCADED CHECK OPTION;
CREATE VIEW v2local AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH LOCAL CHECK OPTION;
CREATE VIEW v2plain AS SELECT id, tenant_id, v FROM v1 WHERE v > 0;
`

// TestCheckOptionPinsWriteThroughView: an UPDATE through v2 (WITH CASCADED CHECK OPTION,
// built on v1's own WHERE tenant_id = 1) never touches tenant_id itself, yet
// `require pinned(tenant_id)` is discharged (ByView) -- the server refuses any row the
// chain's WHERE would not accept (TestCheckOptionViolations1369, internal/analyze,
// measures the 1369 that guarantees it). v2local's own WITH LOCAL CHECK OPTION does not
// reach v1's WHERE (measured there too: LOCAL lets a write move tenant_id away from 1), so
// the same statement through it must not be discharged; through v2plain (no CHECK OPTION
// at all) it must not be discharged either.
func TestCheckOptionPinsWriteThroughView(t *testing.T) {
	m := mustLoad(t, checkOptionContractSchema)
	decls, problems := obligation.Declarations(m.Contract())
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	cases := []struct {
		view string
		want string
	}{
		{"v2", "view"},
		{"v2local", "FAIL"},
		{"v2plain", "FAIL"},
	}
	for _, c := range cases {
		sql := `UPDATE ` + c.view + ` SET v = $1 WHERE id = $2`
		r, err := m.Analyze(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		got := "FAIL"
		for _, d := range obligation.Check(m.Contract(), decls, r.Facts, m) {
			if d.Obligation.Body.Pinned == "tenant_id" {
				got = pathName(d.Path)
			}
		}
		if got != c.want {
			t.Errorf("%s: pinned(tenant_id) discharge = %q, want %q", sql, got, c.want)
		}
	}
}

func mustLoad(t *testing.T, schemaSQL string) *mysql {
	t.Helper()
	an, err := load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	m := an.(*mysql)
	if p := m.Problems(); len(p) > 0 {
		t.Fatalf("schema problems: %v", p)
	}
	return m
}
