package mysqltest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
