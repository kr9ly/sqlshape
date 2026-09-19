package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

const checkOptionSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  tenant_id INT NOT NULL,
  v INT NOT NULL
);
CREATE VIEW v1 AS SELECT id, tenant_id, v FROM t WHERE tenant_id = 1;
CREATE VIEW v2 AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH CASCADED CHECK OPTION;
CREATE VIEW v2local AS SELECT id, tenant_id, v FROM v1 WHERE v > 0 WITH LOCAL CHECK OPTION;
CREATE VIEW v2plain AS SELECT id, tenant_id, v FROM v1 WHERE v > 0;
`

// TestCheckOptionViolations1369 predicts and then measures, against a real mysqld, the
// 1369 (ER_VIEW_CHECK_FAILED) an UPDATE through a WITH CHECK OPTION view may hit: present
// for CASCADED and LOCAL (each can fail its own WHERE, v > 0), absent for a plain view with
// no CHECK OPTION at all (v2plain lets the row go invisible silently -- MySQL's own
// documented behavior for a view without the clause).
func TestCheckOptionViolations1369(t *testing.T) {
	s, err := schema.Load(checkOptionSchema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql      string
		wantCode int
	}{
		{`UPDATE v2 SET v = $1 WHERE id = $2`, 1369},
		{`UPDATE v2local SET v = $1 WHERE id = $2`, 1369},
		{`UPDATE v2plain SET v = $1 WHERE id = $2`, 0},
	}
	for _, c := range cases {
		r, aerr := Analyze(s, c.sql)
		if aerr != nil {
			t.Fatalf("%s: %v", c.sql, aerr)
		}
		found := false
		for _, v := range r.Violations {
			if v.Code == 1369 {
				found = true
			}
		}
		if found != (c.wantCode == 1369) {
			t.Errorf("%s: predicted 1369 = %v, want %v (violations: %+v)", c.sql, found, c.wantCode == 1369, r.Violations)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, checkOptionSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, `INSERT INTO t (id, tenant_id, v) VALUES (1, 1, 5)`); err != nil {
		t.Fatal(err)
	}
	// v > 0 is the CHECK OPTION'd predicate on both v2 and v2local; setting v to -1 fails
	// it either way (a LOCAL check option still re-checks its own WHERE, only not an
	// underlying view's).
	for _, sql := range []string{
		`UPDATE v2 SET v = -1 WHERE id = 1`,
		`UPDATE v2local SET v = -1 WHERE id = 1`,
	} {
		_, execErr := conn.ExecContext(ctx, sql)
		var me *driver.MySQLError
		if !errors.As(execErr, &me) || me.Number != 1369 {
			t.Errorf("%s: want 1369 (ER_VIEW_CHECK_FAILED), got %v", sql, execErr)
		}
	}
	// v2plain has no CHECK OPTION: the server lets the row go invisible through it
	// (documented MySQL behavior), no error
	if _, err := conn.ExecContext(ctx, `UPDATE v2plain SET v = -1 WHERE id = 1`); err != nil {
		t.Errorf("UPDATE v2plain SET v = -1: want no error (no CHECK OPTION declared), got %v", err)
	}
}
