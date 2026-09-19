package analyze

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// The name of an unaliased select item, pinned against the server (itemName's rules): a
// column, a string / numeric / NULL literal are named by their own token even inside
// parentheses; everything else is named by the item's source text, parentheses and inner
// spacing included.
func TestItemNameServer(t *testing.T) {
	const ddl = "CREATE TABLE t (a INT, b INT);\n"
	queries := []string{
		"SELECT a > b FROM t",
		"SELECT (a > b) FROM t",
		"SELECT (a) FROM t",
		"SELECT ((a)) FROM t",
		"SELECT ( a + 1 ) FROM t",
		"SELECT ('x')",
		"SELECT (3)",
		"SELECT (NULL)",
		"SELECT (TRUE)",
		"SELECT (1.5)",
		"SELECT (1e0)",
		"SELECT (0x1F)",
		"SELECT (DATE'2024-01-01')",
		"SELECT (-1)",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n"+ddl)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	db, err := sql.Open("mysql", srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load("-- sqlshape: mysql 8.4\n" + ddl)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		want, _ := rows.Columns()
		rows.Close()
		r, err := Analyze(s, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if len(r.Columns) != len(want) {
			t.Errorf("%s: %d columns, server %d", q, len(r.Columns), len(want))
			continue
		}
		for i, c := range r.Columns {
			if c.Name != want[i] {
				t.Errorf("%s: column %d named %q, server %q", q, i+1, c.Name, want[i])
			}
		}
	}
}
