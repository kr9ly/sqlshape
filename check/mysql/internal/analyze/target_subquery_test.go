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

// Found by x/factsprobe (the facts oracle, first run): the server refuses an UPDATE or
// DELETE whose subquery reads the table being written -- 1093 for the table itself, in
// WHERE, EXISTS or IN alike, 1443 for a view over it -- and the analyzer accepted both. A
// derived table over the target is materialized and accepted, INSERT ... SELECT from its own
// table is accepted, a SELECT is unconcerned (all measured, TestTargetInSubqueryServer).
const targetSubquerySchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT NOT NULL);
CREATE TABLE u (id INT PRIMARY KEY, a INT);
CREATE VIEW v AS SELECT id, a FROM t WHERE a = 1;
CREATE VIEW w AS SELECT id, a FROM v;
`

var targetSubqueryCases = []struct {
	sql  string
	code int
}{
	{"UPDATE t AS t0 SET a = 5 WHERE t0.id IN (SELECT s0.id FROM t AS s0 WHERE s0.b = 1)", 1093},
	{"UPDATE t AS t0 SET a = 5 WHERE EXISTS (SELECT 1 FROM t AS s0 WHERE s0.b = t0.a)", 1093},
	{"UPDATE t AS t0 SET a = 5 WHERE EXISTS (SELECT 1 FROM u AS s0 WHERE s0.a = t0.a)", 0},
	{"UPDATE t AS t0 SET a = 5 WHERE t0.id IN (SELECT x.id FROM (SELECT s0.id FROM t AS s0) AS x)", 0},
	{"UPDATE t AS t0 SET a = (SELECT MAX(s0.a) FROM u AS s0) WHERE t0.id = 1", 0},
	{"UPDATE t AS t0 SET a = 5 WHERE t0.id IN (SELECT s0.id FROM v AS s0)", 1443},
	{"UPDATE t AS t0 SET a = 5 WHERE t0.id IN (SELECT s0.id FROM w AS s0)", 1443},
	{"DELETE FROM t AS t0 WHERE t0.id IN (SELECT s0.id FROM t AS s0 WHERE s0.b = 9)", 1093},
	{"DELETE FROM t AS t0 WHERE EXISTS (SELECT 1 FROM t AS s0 WHERE s0.b = t0.a + 9)", 1093},
	{"DELETE FROM t AS t0 WHERE EXISTS (SELECT 1 FROM v AS s0 WHERE s0.a = t0.a + 9)", 1443},
	{"DELETE FROM t AS t0 WHERE t0.id IN (SELECT s0.id FROM u AS s0)", 0},
	{"INSERT INTO u (id, a) SELECT s0.id + 10, s0.a FROM u AS s0", 0},
	{"SELECT t0.id FROM t AS t0 WHERE EXISTS (SELECT 1 FROM t AS s0 WHERE s0.b = t0.a)", 0},
}

func TestTargetInSubquery(t *testing.T) {
	s, err := schema.Load(targetSubquerySchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range targetSubqueryCases {
		_, err := Analyze(s, c.sql)
		if errCode(err) != c.code {
			t.Errorf("%s: %v, want %d", c.sql, err, c.code)
		}
	}
	// the message names the target's alias, and 1443 the view's
	if _, err := Analyze(s, targetSubqueryCases[0].sql); err == nil || err.Error() != "You can't specify target table 't0' for update in FROM clause (MySQL error 1093)" {
		t.Errorf("1093 message: %v", err)
	}
	if _, err := Analyze(s, targetSubqueryCases[9].sql); err == nil || err.Error() != "The definition of table 's0' prevents operation DELETE on table 't0'. (MySQL error 1443)" {
		t.Errorf("1443 message: %v", err)
	}
}

// The measurements above, on mysqld 8.4.
func TestTargetInSubqueryServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, targetSubquerySchema+"INSERT INTO t VALUES (1, 1, 1), (2, 2, 2);\nINSERT INTO u VALUES (1, 1);\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range targetSubqueryCases {
		_, err := db.Conn().ExecContext(ctx, c.sql)
		var me *driver.MySQLError
		got := 0
		if errors.As(err, &me) {
			got = int(me.Number)
		}
		if got != c.code {
			t.Errorf("server: %s => %v, want %d", c.sql, err, c.code)
		}
	}
}
