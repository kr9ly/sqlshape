package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/cardinality"
)

// TestAdvGroupByVolatileFunctionIsNotSingle: GROUP BY groups the whole result into one row
// only when every grouping expression is pinned to one value before any row is examined
// (x/facts' Term doc: a Known term is "a value fixed before the row is examined... a stable
// function of the session"). RAND() is not stable across rows -- the server evaluates it
// per row (MySQL: functions without a fixed seed are not treated as constant for a single
// execution), so GROUP BY RAND() yields one group per (distinct) random value, i.e. one row
// per input row in practice, not one row overall.
//
// The MySQL producer's termFacts (facts.go) classifies any expression that reads no column
// of the block as a Known term, with no check for determinism; cardinality's proof then
// treats a Known term in Groups as pinning the group (facts.go doc: "a Column term whose
// column is known, or a Param / Const / Known / Outer term" -- all treated as fixed), so
// `SELECT COUNT(*) FROM t GROUP BY RAND()` is wrongly proved AtMostOne.
func TestAdvGroupByVolatileFunctionIsNotSingle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	const schemaSQL = `-- sqlshape: mysql 8.4
CREATE TABLE t (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, v INT NOT NULL);
`
	db, err := mysqltest.Start(ctx, schemaSQL)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	for i := 1; i <= 5; i++ {
		if _, err := conn.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (?, ?)", i, i*10); err != nil {
			t.Fatal(err)
		}
	}

	const sql = `SELECT COUNT(*) AS c FROM t GROUP BY RAND()`
	rows, err := conn.QueryContext(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n <= 1 {
		t.Skipf("RAND() happened to collide across rows this run (%d groups); cannot demonstrate the divergence", n)
	}

	s, err := schema.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyzer rejects what the server accepts: %v", err)
	}
	ok, why := cardinality.AtMostOne(r.Facts)
	if ok {
		t.Errorf("AtMostOne(%q) = true, but the server returned %d rows for 5 seeded rows: GROUP BY RAND() is not pinned by a Known term", sql, n)
	} else {
		t.Logf("correctly not single: %s", why)
	}
}
