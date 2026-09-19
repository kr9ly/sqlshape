package mysqltest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

func start(t *testing.T, schema string) (context.Context, *mysqltest.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, schema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

// The server runs as the schema declares: sql_mode and lower_case_table_names reach mysqld.
func TestDeclaredSettings(t *testing.T) {
	ctx, db := start(t, `-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'
-- sqlshape: server lower_case_table_names = 1
CREATE TABLE Users (id INT PRIMARY KEY, "name" VARCHAR(10) NOT NULL);
INSERT INTO USERS VALUES (1, 'a' || 'b');
`)
	var mode string
	var lctn int
	if err := db.Conn().QueryRowContext(ctx, "SELECT @@SESSION.sql_mode, @@GLOBAL.lower_case_table_names").Scan(&mode, &lctn); err != nil {
		t.Fatal(err)
	}
	if mode != "REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ONLY_FULL_GROUP_BY,ANSI,STRICT_ALL_TABLES" || lctn != 1 {
		t.Errorf("sql_mode = %q, lower_case_table_names = %d", mode, lctn)
	}
	var name string
	if err := db.Conn().QueryRowContext(ctx, `SELECT "name" FROM users`).Scan(&name); err != nil || name != "ab" {
		t.Errorf("name = %q, %v", name, err)
	}
	var table string
	if err := db.Conn().QueryRowContext(ctx, "SHOW TABLES").Scan(&table); err != nil || table != "users" {
		t.Errorf("SHOW TABLES = %q, %v (lower_case_table_names=1 stores the name lower-cased)", table, err)
	}
}

// The defaults when the schema declares nothing.
func TestDefaultSettings(t *testing.T) {
	ctx, db := start(t, "CREATE TABLE t (a INT);")
	var mode string
	var lctn int
	if err := db.Conn().QueryRowContext(ctx, "SELECT @@SESSION.sql_mode, @@GLOBAL.lower_case_table_names").Scan(&mode, &lctn); err != nil {
		t.Fatal(err)
	}
	if mode != "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION" || lctn != 0 {
		t.Errorf("sql_mode = %q, lower_case_table_names = %d", mode, lctn)
	}
}

// A variable mysqld does not know keeps the server from starting.
func TestUnknownSetting(t *testing.T) {
	ctx := context.Background()
	db, err := mysqltest.Start(ctx, "-- sqlshape: server no_such_variable = 1\nCREATE TABLE t (a INT);")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH")
	}
	if err == nil {
		db.Close()
		t.Fatal("started")
	}
	if !strings.Contains(err.Error(), "did not come up: mysqld exited") || !strings.Contains(err.Error(), "unknown variable 'no-such-variable=1'") {
		t.Errorf("%v", err)
	}
}

// Under Main (this package's TestMain) consecutive Starts with the same settings share one
// mysqld: the second gets the same socket, with the first schema's database gone; other
// settings mean another server.
func TestSharedServer(t *testing.T) {
	ctx := context.Background()
	socket := func(db *mysqltest.DB) string {
		cfg, err := mysql.ParseDSN(db.DSN())
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Addr
	}
	first, err := mysqltest.Start(ctx, "CREATE TABLE only_here (a INT);")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	sock := socket(first)
	// while first is in use, a second Start with the same settings boots another server
	overlap, err := mysqltest.Start(ctx, "CREATE TABLE t (a INT);")
	if err != nil {
		t.Fatal(err)
	}
	if socket(overlap) == sock {
		t.Errorf("a Start while the server is in use reused it")
	}
	overlap.Close()
	first.Close()

	second, err := mysqltest.Start(ctx, "CREATE TABLE t (a INT);")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if socket(second) != sock {
		t.Errorf("second Start got socket %q, want the first's %q", socket(second), sock)
	}
	var n int
	if err := second.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'only_here'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the first schema's table survived into the second Start")
	}

	other, err := mysqltest.Start(ctx, "-- sqlshape: server sql_mode = 'ANSI'\nCREATE TABLE t (a INT);")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if socket(other) == sock {
		t.Errorf("a schema with other settings shared the server")
	}
}
