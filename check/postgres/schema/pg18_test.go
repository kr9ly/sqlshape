package schema_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// PostgreSQL 18 DDL: named NOT NULL constraints, NOT ENFORCED, WITHOUT OVERLAPS / PERIOD,
// VIRTUAL generated columns.
func TestPG18DDL(t *testing.T) {
	s := mustLoad(t, `-- sqlshape: postgres 18
CREATE TABLE p (id int4range, valid_at daterange, CONSTRAINT p_pk PRIMARY KEY (id, valid_at WITHOUT OVERLAPS));
CREATE TABLE t (
  id int,
  CONSTRAINT t_nn NOT NULL id,
  c int CHECK (c > 0) NOT ENFORCED,
  d int CHECK (d > 0) ENFORCED,
  CONSTRAINT t_ck CHECK (c < 10) NOT ENFORCED,
  r int4range, v daterange,
  CONSTRAINT t_fk FOREIGN KEY (r, PERIOD v) REFERENCES p (id, PERIOD valid_at) NOT ENFORCED,
  CONSTRAINT t_uq UNIQUE (r, v WITHOUT OVERLAPS),
  g int GENERATED ALWAYS AS (c * 2) VIRTUAL,
  h int GENERATED ALWAYS AS (c * 2) STORED,
  i int GENERATED ALWAYS AS (c * 2)
);
ALTER TABLE t ADD CONSTRAINT t_nn2 NOT NULL c;
ALTER TABLE t ALTER CONSTRAINT t_ck ENFORCED;
`)
	if p := problems(s); p != "" {
		t.Fatalf("problems:\n%s", p)
	}
	tbl := s.Relation("", "t")
	con := func(name string) *schema.Constraint {
		for _, c := range tbl.Constraints {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no constraint %s", name)
		return nil
	}
	if !tbl.Column("id").NotNull || !tbl.Column("c").NotNull {
		t.Error("named NOT NULL constraints did not set NOT NULL")
	}
	if !con("t_fk").NotEnforced || !con("t_fk").WithPeriod || con("t_fk").Columns[1] != "v" {
		t.Errorf("t_fk: %+v", con("t_fk"))
	}
	if con("t_ck").NotEnforced {
		t.Error("ALTER CONSTRAINT ... ENFORCED did not take")
	}
	if !con("t_uq").WithoutOverlaps || !s.Relation("", "p").Constraints[0].WithoutOverlaps {
		t.Error("WITHOUT OVERLAPS not recorded")
	}
	colCheck := map[string]bool{}
	for _, c := range tbl.Constraints {
		if c.Kind == schema.Check && len(c.Columns) == 1 && c.Name != "t_ck" {
			colCheck[c.Columns[0]] = c.NotEnforced
		}
	}
	if len(colCheck) != 2 || !colCheck["c"] || colCheck["d"] {
		t.Errorf("column CHECK enforcement (column → not enforced): %v", colCheck)
	}
	if !tbl.Column("g").GeneratedVirtual || tbl.Column("h").GeneratedVirtual || !tbl.Column("i").GeneratedVirtual {
		t.Error("generated kind: VIRTUAL is 18's default")
	}

	// DROP CONSTRAINT lifts a named NOT NULL
	if err := s.Apply("ALTER TABLE t DROP CONSTRAINT t_nn"); err != nil {
		t.Fatal(err)
	}
	if tbl.Column("id").NotNull {
		t.Error("DROP CONSTRAINT t_nn left the column NOT NULL")
	}
	wantProblem(t, "-- sqlshape: postgres 18\nCREATE TABLE u (a int, CONSTRAINT nn NOT NULL b);\n", `NOT NULL column "b" does not exist`)

	// a 17 schema: generated columns are stored, constraints enforced
	s17 := mustLoad(t, "CREATE TABLE t (a int CHECK (a > 0), b int GENERATED ALWAYS AS (a * 2) STORED)")
	if tbl := s17.Relation("", "t"); tbl.Column("b").GeneratedVirtual || tbl.Constraints[0].NotEnforced {
		t.Error("17: virtual or not enforced")
	}
	if _, err := schema.Load("CREATE TABLE t (a int, b int GENERATED ALWAYS AS (a * 2) VIRTUAL)"); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("17 accepts VIRTUAL: %v", err)
	}
}
