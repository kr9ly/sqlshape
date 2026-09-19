package analyze

import (
	"sort"
	"strings"
	"testing"
)

const pg18Schema = `-- sqlshape: postgres 18
CREATE TABLE p (id int4range, valid_at daterange, name text,
  CONSTRAINT p_pk PRIMARY KEY (id, valid_at WITHOUT OVERLAPS));
CREATE TABLE m (id int, valid_at datemultirange, name text,
  CONSTRAINT m_uq UNIQUE (id, valid_at WITHOUT OVERLAPS));
CREATE TABLE c (id int PRIMARY KEY, pid int4range, at daterange, day date,
  CONSTRAINT c_fk FOREIGN KEY (pid, PERIOD at) REFERENCES p (id, PERIOD valid_at));
CREATE TABLE g (a int PRIMARY KEY, b int GENERATED ALWAYS AS (a * 2) VIRTUAL, c int GENERATED ALWAYS AS (a * 2) STORED);
`

// A temporal key (WITHOUT OVERLAPS) proves a single row when its scalar columns are fixed
// by equality and its range column either equals a known value or contains a known point:
// no two rows with the same scalar values have overlapping ranges.
func TestTemporalKeyOne(t *testing.T) {
	s, err := Load(pg18Schema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql string
		one bool
	}{
		{"SELECT name FROM p WHERE id = $1 AND valid_at = $2", true},
		{"SELECT name FROM p WHERE id = $1 AND valid_at @> $2::date", true},
		{"SELECT name FROM p WHERE id = $1 AND $2::date <@ valid_at", true},
		{"SELECT name FROM m WHERE id = $1 AND valid_at @> $2::date", true},
		{"SELECT p.name FROM p JOIN c ON c.pid = p.id AND p.valid_at @> c.day WHERE c.id = $1", true},
		// not enough: the scalar part alone, an overlap, a range on the known side (it could be empty), an untyped point
		{"SELECT name FROM p WHERE id = $1", false},
		{"SELECT name FROM p WHERE id = $1 AND valid_at && $2::daterange", false},
		{"SELECT name FROM p WHERE id = $1 AND valid_at @> $2::daterange", false},
		{"SELECT name FROM p WHERE valid_at @> $1::date", false},
		{"SELECT p.name FROM p JOIN c ON c.pid = p.id AND p.valid_at @> c.at WHERE c.id = $1", false},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if r.AtMostOne != c.one {
			t.Errorf("%s: at most one = %v (%s), want %v", c.sql, r.AtMostOne, r.ManyRowsWhy, c.one)
		}
	}
}

// PostgreSQL 18 columns and constraints in DML: a PERIOD foreign key raises 23503 like any
// other, a VIRTUAL generated column is read like a column and never assigned.
func TestPG18DML(t *testing.T) {
	s, err := Load(pg18Schema)
	if err != nil {
		t.Fatal(err)
	}
	codes := func(sql string) string {
		r, err := Analyze(s, sql)
		if err != nil {
			return "error: " + err.Error()
		}
		var got []string
		for _, v := range r.Violations {
			got = append(got, v.Code+" "+v.Key())
		}
		sort.Strings(got)
		return strings.Join(got, ", ")
	}
	for _, c := range []struct{ sql, want string }{
		{"INSERT INTO c (id, pid, at) VALUES ($1, $2, $3)", "23502 c.id, 23503 c_fk, 23505 c_pkey"},
		{"UPDATE c SET at = $2 WHERE id = $1", "23503 c_fk"},
		{"DELETE FROM p WHERE id = $1 AND valid_at = $2", "23503 c_fk"},
		{"INSERT INTO g (a) VALUES ($1)", "23502 g.a, 23505 g_pkey"},
		{"INSERT INTO g (a, b) VALUES ($1, $2)", `error: 428C9: cannot insert a non-DEFAULT value into column "b"`},
		{"UPDATE g SET b = 1 WHERE a = $1", `error: 428C9: column "b" can only be updated to DEFAULT`},
		{"UPDATE g SET b = DEFAULT WHERE a = $1", ""},
	} {
		if got := codes(c.sql); got != c.want && !(strings.HasPrefix(c.want, "error:") && strings.HasPrefix(got, c.want)) {
			t.Errorf("%s\n got  %s\n want %s", c.sql, got, c.want)
		}
	}
	r, err := Analyze(s, "SELECT b, c FROM g WHERE a = $1")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 2 || r.Columns[0].Source == nil || r.Columns[0].Source.Column != "b" || !r.AtMostOne {
		t.Errorf("reading a virtual column: %+v", r.Columns)
	}
}
