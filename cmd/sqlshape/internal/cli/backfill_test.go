package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// A backfill that fixes the rows ahead of a CHECK, a UNIQUE and a FOREIGN KEY the table
// gains, its column definitions unchanged, is part of the step: diff emits each UPDATE
// before its constraint and apply succeeds on rows that violate all three. Once applied,
// the same declarations left in schema.sql are stale: diff reports them (exit 1) and leaves
// the UPDATEs out.
func TestBackfillAheadOfConstraints(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base := `CREATE TABLE b (id int PRIMARY KEY);
CREATE TABLE a (id int PRIMARY KEY, n int, u int, r int);`
	db, err := oracle.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := pgx.Connect(ctx, db.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO b VALUES (1); INSERT INTO a VALUES (1, -5, 7, 99), (2, 3, 7, 1);"); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	target := pg17Decl + `-- @migrate backfill a.n = 1 where n <= 0
-- @migrate backfill a.u = id where u = 7
-- @migrate backfill a.r = NULL where r NOT IN (SELECT id FROM b)
CREATE TABLE b (id int PRIMARY KEY);
CREATE TABLE a (id int PRIMARY KEY, n int CHECK (n > 0), u int UNIQUE, r int REFERENCES b);`
	if err := os.WriteFile(schemaPath, []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "diff", "-db", db.ConnString(), "-schema", schemaPath)
	if code != 0 {
		t.Fatalf("diff: code %d\n%s%s", code, out, errs)
	}
	for _, c := range []struct{ update, constraint string }{
		{`SET "n" = 1 WHERE n <= 0`, "CHECK (n > 0)"},
		{`SET "u" = id WHERE u = 7`, "UNIQUE (u)"},
		{`SET "r" = NULL WHERE r NOT IN (SELECT id FROM b)`, "FOREIGN KEY (r)"},
	} {
		u, k := strings.Index(out, c.update), strings.Index(out, c.constraint)
		if u < 0 || k < 0 || u > k {
			t.Errorf("want %q before %q:\n%s", c.update, c.constraint, out)
		}
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run(t, "apply", "-db", db.ConnString(), "-schema", schemaPath, ddlPath); code != 0 {
		t.Fatalf("apply: code %d\n%s%s", code, out, errs)
	}
	code, out, errs = run(t, "diff", "-db", db.ConnString(), "-schema", schemaPath)
	if code != 1 || strings.Contains(out, "UPDATE") || strings.Count(errs, "does not change its table") != 3 {
		t.Fatalf("diff after apply: code %d\n%s%s", code, out, errs)
	}
}

// The same on MySQL: a data fix ahead of a CHECK, a UNIQUE key and a FOREIGN KEY the table
// gains runs before them, on rows that violate all three, and is stale once applied.
func TestMySQLBackfillAheadOfConstraints(t *testing.T) {
	ctx := context.Background()
	base := `-- sqlshape: mysql 8.4
CREATE TABLE b (id INT PRIMARY KEY);
CREATE TABLE a (id INT PRIMARY KEY, n INT, u INT, r INT);`
	db, err := mysqltest.Start(ctx, base)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"INSERT INTO b VALUES (1)", "INSERT INTO a VALUES (1, -5, 7, 99), (2, 3, 7, 1)"} {
		if _, err := db.Conn().ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	target := `-- sqlshape: mysql 8.4
-- @migrate backfill a.n = 1 where n <= 0
-- @migrate backfill a.u = id where u = 7
-- @migrate backfill a.r = NULL where r NOT IN (SELECT id FROM b)
CREATE TABLE b (id INT PRIMARY KEY);
CREATE TABLE a (id INT PRIMARY KEY, n INT, u INT, r INT,
  CONSTRAINT a_n_check CHECK (n > 0), UNIQUE KEY a_u_key (u),
  CONSTRAINT a_r_fkey FOREIGN KEY (r) REFERENCES b (id));`
	if err := os.WriteFile(schemaPath, []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "diff", "-db", db.DSN(), "-schema", schemaPath)
	if code != 0 {
		t.Fatalf("diff: code %d\n%s%s", code, out, errs)
	}
	for _, c := range []struct{ update, constraint string }{
		{"SET `n` = 1 WHERE n <= 0", "a_n_check"},
		{"SET `u` = id WHERE u = 7", "a_u_key"},
		{"SET `r` = NULL WHERE r NOT IN (SELECT id FROM b)", "a_r_fkey"},
	} {
		u, k := strings.Index(out, c.update), strings.Index(out, c.constraint)
		if u < 0 || k < 0 || u > k {
			t.Errorf("want %q before %q:\n%s", c.update, c.constraint, out)
		}
	}
	ddlPath := filepath.Join(dir, "up.sql")
	if err := os.WriteFile(ddlPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run(t, "apply", "-db", db.DSN(), "-schema", schemaPath, ddlPath); code != 0 {
		t.Fatalf("apply: code %d\n%s%s", code, out, errs)
	}
	code, out, errs = run(t, "diff", "-db", db.DSN(), "-schema", schemaPath)
	if code != 1 || strings.Contains(out, "UPDATE") || strings.Count(errs, "does not change its table") != 3 {
		t.Fatalf("diff after apply: code %d\n%s%s", code, out, errs)
	}
}
