package analyze

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ON CONFLICT DO NOTHING without a conflict target takes every unique constraint, unique
// index and exclusion constraint of the table as an arbiter: a conflict on any of them is
// absorbed. A DEFERRABLE one among them makes the statement fail whether or not a row
// conflicts (SQLSTATE 55000). Both measured on PostgreSQL 17 (the server run is the
// demonstration; the assertions on the analyzer are the verdict).
func TestOnConflictDoNothingWithoutTarget(t *testing.T) {
	const schemaSQL = `-- sqlshape: postgres 17
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE t (id int PRIMARY KEY, room int NOT NULL, during int4range NOT NULL,
  CONSTRAINT t_room_excl EXCLUDE USING gist (room WITH =, during WITH &&));
CREATE TABLE d (id int PRIMARY KEY, email text, CONSTRAINT d_email_key UNIQUE (email) DEFERRABLE INITIALLY IMMEDIATE);
CREATE TABLE p (id int PRIMARY KEY, email text, active bool NOT NULL DEFAULT true);
CREATE UNIQUE INDEX p_email_active ON p (email) WHERE active;
`
	// the keys and the exclusion are absorbed; the NOT NULL violations of the parameters stay
	keys := func(sql string) []string {
		var out []string
		for _, k := range advViolationKeys(t, schemaSQL, sql) {
			if !strings.HasPrefix(k, "23502 ") {
				out = append(out, k)
			}
		}
		return out
	}
	if got := keys("INSERT INTO t (id, room, during) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING"); len(got) != 0 {
		t.Errorf("exclusion absorbed: violations %v, want none", got)
	}
	if got := keys("INSERT INTO p (id, email) VALUES ($1, $2) ON CONFLICT DO NOTHING"); len(got) != 0 {
		t.Errorf("partial unique index absorbed: violations %v, want none", got)
	}
	if got := keys("INSERT INTO t (id, room, during) VALUES ($1, $2, $3)"); len(got) != 2 {
		t.Errorf("without ON CONFLICT: violations %v, want the key and the exclusion", got)
	}
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, "INSERT INTO d (id, email) VALUES ($1, $2) ON CONFLICT DO NOTHING")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range r.Notes {
		found = found || strings.Contains(n.Message, "SQLSTATE 55000") && strings.Contains(n.Message, "d_email_key is DEFERRABLE")
	}
	if !found {
		t.Errorf("deferrable arbiter: notes %+v, want the always-fails note", r.Notes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := advStartOracle(t, ctx, schemaSQL)
	defer o.Close()
	conn := o.Conn()
	for _, q := range []string{
		"INSERT INTO t (id, room, during) VALUES (1, 1, int4range(1,5))",
		"INSERT INTO d (id, email) VALUES (1, 'a')",
		"INSERT INTO p (id, email) VALUES (1, 'a')",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		sql  string
		rows int64
		code string
	}{
		{"INSERT INTO t (id, room, during) VALUES (2, 1, int4range(3,8)) ON CONFLICT DO NOTHING", 0, ""},
		{"INSERT INTO p (id, email) VALUES (2, 'a') ON CONFLICT DO NOTHING", 0, ""},
		{"INSERT INTO d (id, email) VALUES (2, 'a') ON CONFLICT DO NOTHING", 0, "55000"},
		{"INSERT INTO d (id, email) VALUES (3, 'b') ON CONFLICT DO NOTHING", 0, "55000"},
	} {
		tag, err := conn.Exec(ctx, c.sql)
		switch {
		case c.code == "" && (err != nil || tag.RowsAffected() != c.rows):
			t.Errorf("%s: %v rows, %v", c.sql, tag.RowsAffected(), err)
		case c.code != "" && (err == nil || !strings.Contains(err.Error(), "SQLSTATE "+c.code)):
			t.Errorf("%s: %v, want SQLSTATE %s", c.sql, err, c.code)
		}
	}
}
