package analyze

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// TestViolations covers the failure-mode enumeration (violation.go) against testdata/schema.sql.
func TestViolations(t *testing.T) {
	schemaSQL, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql   string
		want  string // sorted "code key[@param]" entries joined by ", "
		notes string
	}{
		{sql: "INSERT INTO users (email, name) VALUES ($1, $2)",
			want: "23502 users.email@1, 23505 users_email_key, 23514 email_check"},
		{sql: "INSERT INTO users (email, name) VALUES ('a@x', $1)",
			want: "23505 users_email_key, 23514 email_check"},
		{sql: "INSERT INTO users (email, balance) VALUES ($1, $2)",
			want: "23502 users.balance@2, 23502 users.email@1, 23505 users_email_key, 23514 email_check, 23514 yen_check"},
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, $2)",
			want: "23502 orders.total@2, 23502 orders.user_id@1, 23503 orders_user_id_fkey, 23505 orders_pkey, 23505 orders_user_note_key, 23514 orders_total_check, P0401 P0401"},
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, 10) ON CONFLICT (user_id, note) DO NOTHING",
			want: "23502 orders.user_id@1, 23503 orders_user_id_fkey, 23505 orders_pkey, 23514 orders_total_check, P0401 P0401"},
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, 10) ON CONFLICT ON CONSTRAINT orders_pkey DO UPDATE SET total = 5, status = 'paid'",
			want: "23502 orders.user_id@1, 23503 orders_user_id_fkey, 23505 orders_user_note_key, 23514 orders_total_check, P0401 P0401"},
		{sql: "INSERT INTO orders (user_id, total) VALUES ($1, 10) ON CONFLICT DO NOTHING",
			want: "23502 orders.user_id@1, 23503 orders_user_id_fkey, 23514 orders_total_check, P0401 P0401"},
		{sql: "INSERT INTO orders (user_id, total) SELECT id, 1 FROM users",
			want: "23503 orders_user_id_fkey, 23505 orders_pkey, 23505 orders_user_note_key, 23514 orders_total_check, P0401 P0401"},
		{sql: "INSERT INTO orders (user_id) VALUES ($1)",
			want:  "23502 orders.user_id@1, 23503 orders_user_id_fkey, 23505 orders_pkey, 23505 orders_user_note_key, P0401 P0401",
			notes: "INSERT omits orders.total, which is NOT NULL without a default: every execution fails"},
		{sql: "INSERT INTO users DEFAULT VALUES", want: "23505 users_email_key"},
		// UPDATE: only what the SET columns touch, plus FKs pointing at changed keys
		{sql: "UPDATE orders SET status = 'paid' WHERE id = $1", want: "P0401 P0401"},
		{sql: "UPDATE orders SET note = $1 WHERE id = $2", want: "23505 orders_user_note_key, P0401 P0401"},
		{sql: "UPDATE orders SET total = total - $1 WHERE id = $2", want: "23502 orders.total, 23514 orders_total_check, P0401 P0401"},
		{sql: "UPDATE orders SET total = 0 WHERE id = $1", want: "23514 orders_total_check, P0401 P0401"},
		{sql: "UPDATE orders SET user_id = $1 WHERE id = $2", want: "23502 orders.user_id@1, 23503 orders_user_id_fkey, 23505 orders_user_note_key, P0401 P0401"},
		{sql: "UPDATE orders SET id = $1 WHERE id = $2", want: "23502 orders.id@1, 23503 order_items_order_fk, 23505 orders_pkey, P0401 P0401"},
		{sql: "UPDATE orders SET id = 5 WHERE id = $1", want: "23503 order_items_order_fk, 23505 orders_pkey, P0401 P0401"},
		// users.id is GENERATED ALWAYS AS IDENTITY: PG rejects the assignment outright (428C9), see TestIdentityUpdate
		{sql: "UPDATE users SET balance = $1 WHERE id = $2", want: "23502 users.balance@1, 23514 yen_check"},
		{sql: "UPDATE users SET name = $1 WHERE id = $2", want: ""},
		// DELETE: referencing FKs with NO ACTION / RESTRICT
		{sql: "DELETE FROM users WHERE id = $1", want: "23503 memos_user_id_fkey, 23503 orders_user_id_fkey"},
		{sql: "DELETE FROM orders WHERE id = $1", want: "23503 order_items_order_fk"},
		{sql: "DELETE FROM order_items WHERE order_id = $1", want: ""},
		{sql: "SELECT id FROM users", want: ""},
		// data-modifying CTEs contribute their failure modes
		{sql: "WITH d AS (DELETE FROM orders WHERE id = $1 RETURNING id) SELECT id FROM d", want: "23503 order_items_order_fk"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			r, err := Analyze(s, c.sql)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			var got []string
			for _, v := range r.Violations {
				k := v.Code + " " + v.Key()
				if v.Param > 0 {
					k += "@" + itoa(v.Param)
				}
				got = append(got, k)
			}
			sort.Strings(got)
			if g := strings.Join(got, ", "); g != c.want {
				t.Errorf("violations:\n want %s\n got  %s", c.want, g)
			}
			var notes []string
			for _, n := range r.Notes {
				notes = append(notes, n.Message)
			}
			if g := strings.Join(notes, "; "); g != c.notes {
				t.Errorf("notes: want %q, got %q", c.notes, g)
			}
		})
	}
}

// TestMergeViolations: MERGE reports the union of what its INSERT / UPDATE / DELETE
// actions may violate.
func TestMergeViolations(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, `MERGE INTO orders o USING users u ON o.user_id = u.id
WHEN MATCHED AND u.role = 'x' THEN DELETE
WHEN MATCHED THEN UPDATE SET total = -1
WHEN NOT MATCHED THEN INSERT (user_id, total, note) VALUES (u.id, 0, u.name)`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range r.Violations {
		got = append(got, v.Constraint)
	}
	for _, want := range []string{"orders_total_check", "orders_user_id_fkey", "orders_user_note_key", "order_items_order_fk"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// TestIdentityUpdate: a GENERATED ALWAYS identity column takes no explicit value
// (428C9), on INSERT unless OVERRIDING SYSTEM VALUE, and on UPDATE except to DEFAULT.
func TestIdentityUpdate(t *testing.T) {
	schemaSQL, _ := os.ReadFile("testdata/schema.sql")
	s, err := Load(string(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	for sql, want := range map[string]string{
		"UPDATE users SET id = 5 WHERE id = $1":                                   "428C9",
		"UPDATE users SET id = DEFAULT WHERE id = $1":                             "",
		"INSERT INTO users (id, email) VALUES (5, 'a@x')":                         "428C9",
		"INSERT INTO users (id, email) OVERRIDING SYSTEM VALUE VALUES (5, 'a@x')": "",
		"INSERT INTO users (id, email) SELECT 5, 'a@x'":                           "428C9",
		"INSERT INTO users (id, email) VALUES (DEFAULT, 'a@x')":                   "",
	} {
		_, err := Analyze(s, sql)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", sql, err)
		case want != "" && (err == nil || !strings.HasPrefix(err.Error(), want)):
			t.Errorf("%s: want %s, got %v", sql, want, err)
		}
	}
}

// TestNotEnforcedViolations: a NOT ENFORCED constraint (PostgreSQL 18) never raises, so
// it is not a failure mode of the statements that touch its columns; an enforced one
// beside it still is, and ALTER CONSTRAINT ... ENFORCED brings it back.
func TestNotEnforcedViolations(t *testing.T) {
	const ddl = `-- sqlshape: postgres 18
CREATE TABLE p (id int PRIMARY KEY);
CREATE TABLE t (
  id int PRIMARY KEY,
  pid int CONSTRAINT t_fk REFERENCES p (id) NOT ENFORCED,
  n int CONSTRAINT t_ck CHECK (n > 0) NOT ENFORCED,
  m int CONSTRAINT t_ck2 CHECK (m > 0)
);
`
	codes := func(t *testing.T, s *schema.Schema, sql string) string {
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range r.Violations {
			got = append(got, v.Code+" "+v.Key())
		}
		sort.Strings(got)
		return strings.Join(got, ", ")
	}
	s, err := Load(ddl)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := codes(t, s, "INSERT INTO t (id, pid, n, m) VALUES ($1, $2, $3, $4)"), "23502 t.id, 23505 t_pkey, 23514 t_ck2"; got != want {
		t.Errorf("insert: got %q, want %q", got, want)
	}
	if got, want := codes(t, s, "DELETE FROM p WHERE id = $1"), ""; got != want {
		t.Errorf("delete of the referenced row: got %q, want %q", got, want)
	}
	s, err = Load(ddl + "ALTER TABLE t ALTER CONSTRAINT t_fk ENFORCED, ALTER CONSTRAINT t_ck ENFORCED;\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := codes(t, s, "INSERT INTO t (id, pid, n, m) VALUES ($1, $2, $3, $4)"), "23502 t.id, 23503 t_fk, 23505 t_pkey, 23514 t_ck, 23514 t_ck2"; got != want {
		t.Errorf("enforced again: got %q, want %q", got, want)
	}
}
