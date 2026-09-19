package oracle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const schema = `-- sqlshape: mysql 8.4
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(255),
  total DECIMAL(10,2) NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);
`

func start(t *testing.T) *Oracle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	o, err := Start(ctx, schema)
	if errors.Is(err, ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	return o
}

func TestDescribe(t *testing.T) {
	o := start(t)
	if !strings.HasPrefix(o.Version, "8.") {
		t.Errorf("version %q", o.Version)
	}
	ctx := context.Background()
	d, err := o.Describe(ctx, "SELECT id, name, email, total, created_at, id = $1, (SELECT COUNT(*) FROM users) FROM users WHERE id = $2")
	if err != nil {
		t.Fatal(err)
	}
	want := []Column{
		{Name: "id", Type: "UNSIGNED BIGINT"},
		{Name: "name", Type: "VARCHAR", Length: 400},
		{Name: "email", Type: "VARCHAR", Nullable: true, Length: 1020},
		{Name: "total", Type: "DECIMAL", Prec: 10, Scale: 2},
		{Name: "created_at", Type: "DATETIME", Scale: 6},
		{Name: "id = ?", Type: "BIGINT", Nullable: true},
		{Name: "(SELECT COUNT(*) FROM users)", Type: "BIGINT", Nullable: true}, // a scalar subquery is nullable
	}
	if len(d.Columns) != len(want) {
		t.Fatalf("columns %+v", d.Columns)
	}
	for i, c := range d.Columns {
		if c.Name != want[i].Name || c.Type != want[i].Type || c.Nullable != want[i].Nullable {
			t.Errorf("column %d: got %+v, want %+v", i, c, want[i])
		}
	}
	// a write is prepared, not run
	if _, err := o.Describe(ctx, "INSERT INTO users (name) VALUES ($1)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := o.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after a described INSERT: %d, %v", n, err)
	}
	// the server's own errors
	_, err = o.Describe(ctx, "SELECT nope FROM users")
	var oe *Error
	if !errors.As(err, &oe) || oe.Number != 1054 {
		t.Errorf("unknown column: %v", err)
	}
}
