package mysql_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
)

func TestVerify(t *testing.T) {
	ctx, srv := start(t)
	db := srv.Conn()
	// the test schema declares nothing: the server's defaults
	if err := mysql.Verify(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	// a declaration the server does not run with
	err := mysql.Verify(ctx, db, "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI'\n"+schema)
	if err == nil || !strings.Contains(err.Error(), "runs with sql_mode") || !strings.Contains(err.Error(), "declares 'REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ONLY_FULL_GROUP_BY,ANSI'") {
		t.Errorf("ANSI declared: %v", err)
	}
	err = mysql.Verify(ctx, db, "-- sqlshape: server lower_case_table_names = 1\n"+schema)
	if err == nil || !strings.Contains(err.Error(), "lower_case_table_names = 0 but schema.sql declares 1") {
		t.Errorf("lctn declared: %v", err)
	}
	// a connection whose session was given another mode (what a DSN's sql_mode= does)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = 'STRICT_ALL_TABLES'"); err != nil {
		t.Fatal(err)
	}
	err = mysql.Verify(ctx, conn, schema)
	if err == nil || !strings.Contains(err.Error(), "runs with sql_mode 'STRICT_ALL_TABLES'") {
		t.Errorf("session mode: %v", err)
	}
	if err := mysql.Verify(ctx, conn, "-- sqlshape: server sql_mode = 'strict_all_tables'\n"+schema); err != nil {
		t.Errorf("session mode declared: %v", err)
	}
}
