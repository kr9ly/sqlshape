package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// TestAdvParamSourceThroughUpdatableView covers a placeholder assigned through an
// updatable, merged view (v_users is `SELECT id, name FROM users`, plain column
// references only, so the server accepts writes through it and applies them to the base
// table). update() resolves the assigned column to the base schema.Column (col.col, via
// relation.column's `ref.col = c.base`) but passes col.rel.table -- nil for a view
// relation, which only carries .cols -- to assign()/noteParamSource() instead of the
// column's own baseTable (col.c.baseTable, which does hold *users*). The placeholder's
// Type still comes out right (it only needs col.col.Type), but its Source is silently
// dropped: ParamSource is nil where it should name users.name, NotNull=true,
// Assigned=true, exactly as it does for the very same statement rewritten to update the
// base table directly, and exactly as it still does for a real base-table column assigned
// in the same UPDATE's other placeholder.
//
// Confirmed against a real mysqld: a real server accepts `UPDATE v_users SET name = ?
// WHERE id = ?` and applies it to users.name (see TestAdvParamSourceThroughViewServer),
// so users.name is the placeholder's genuine Source, not a case the analyzer is right to
// decline.
func TestAdvParamSourceThroughUpdatableView(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want string // "-" for none, else "table.column notnull=%v assigned=%v" per placeholder
	}{
		{"UPDATE v_users SET name = $1 WHERE id = $2", "users.name notnull=true assigned=true"},
		{"UPDATE v_users SET name = $1", "users.name notnull=true assigned=true"},
		{
			"UPDATE v_users JOIN orders o ON o.user_id = v_users.id SET v_users.name = $1, o.note = $2 WHERE v_users.id = $3",
			"users.name notnull=true assigned=true",
		},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(r.Params) == 0 {
			t.Fatalf("%s: no placeholders found", c.sql)
		}
		p := r.Params[0]
		if p.Source == nil {
			t.Errorf("%s: $1's Source is nil, want %s (the view is updatable and the write reaches users.name on a real server)", c.sql, c.want)
			continue
		}
		got := formatParamSource(p.Source)
		if got != c.want {
			t.Errorf("%s: $1's Source is %q, want %q", c.sql, got, c.want)
		}
	}
}

func formatParamSource(s *ParamSource) string {
	if s == nil {
		return "-"
	}
	return s.Table + "." + s.Column + " notnull=" + boolStr(s.NotNull) + " assigned=" + boolStr(s.Assigned)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestAdvParamSourceThroughViewServer is the ground truth TestAdvParamSourceThroughUpdatableView
// relies on: a real mysqld accepts `UPDATE v_users SET name = ? WHERE id = ?` (v_users is
// a plain, mergeable view over users) and the write lands on users.name, proving the view
// update is a genuine, server-sanctioned write to that base column -- not a case the
// analyzer is entitled to treat as sourceless.
func TestAdvParamSourceThroughViewServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, testSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO users (id, name) VALUES (1, 'orig')"); err != nil {
		t.Fatal(err)
	}
	res, err := conn.ExecContext(ctx, "UPDATE v_users SET name = ? WHERE id = ?", "changed", 1)
	if err != nil {
		t.Fatalf("the server rejects an update through v_users: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("update through v_users affected %d rows, want 1", n)
	}
	var name string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM users WHERE id = 1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "changed" {
		t.Fatalf("the write through v_users did not reach users.name: got %q", name)
	}
}
