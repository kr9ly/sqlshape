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

// rowidSchema declares the key shapes `_rowid` resolves against (relation.rowid): the
// first key after the server's sort must be unique over one NOT NULL integer column.
const rowidSchema = `-- sqlshape: mysql 8.4
CREATE TABLE r1 (a INT, b VARCHAR(10), UNIQUE (b));
CREATE TABLE r2 (a INT, b INT, UNIQUE (a, b));
CREATE TABLE r3 (a INT NULL, UNIQUE (a));
CREATE TABLE r4 (a INT, b BIGINT NOT NULL, c VARCHAR(5) NOT NULL, UNIQUE (c), UNIQUE (b));
CREATE TABLE r5 (a INT, b DECIMAL(5) NOT NULL, UNIQUE (b));
CREATE TABLE r6 (a INT, b YEAR NOT NULL, UNIQUE (b));
CREATE TABLE r7 (a INT, b BIGINT UNSIGNED NOT NULL, c INT NOT NULL, PRIMARY KEY (c), UNIQUE (b));
CREATE TABLE r8 (a INT, b VARCHAR(5) NOT NULL, c INT NOT NULL, PRIMARY KEY (b), UNIQUE (c));
CREATE TABLE r9 (a INT, b INT NOT NULL, c INT NOT NULL, KEY (b), UNIQUE (c));
CREATE TABLE r10 (a INT, b INT, c INT NOT NULL, UNIQUE (b), UNIQUE (c));
CREATE TABLE r11 (a INT, b BIT(8) NOT NULL, UNIQUE (b));
CREATE TABLE r12 (a INT, b INT NOT NULL, UNIQUE (b), UNIQUE (a, b));
CREATE TABLE r14 (a VARCHAR(5) NOT NULL, b INT NOT NULL, PRIMARY KEY (a, b), UNIQUE (b));
CREATE TABLE r15 (a INT, b INT NOT NULL, c INT, UNIQUE (c), UNIQUE (b));
CREATE TABLE r17 (a INT, b TINYINT NOT NULL, UNIQUE (b));
CREATE TABLE r18 (a INT, b BOOLEAN NOT NULL, UNIQUE (b));
CREATE TABLE r19 (a INT, b MEDIUMINT NOT NULL, UNIQUE (b));
CREATE TABLE r20 (a INT, b DOUBLE NOT NULL, UNIQUE (b));
CREATE TABLE r21 (a INT, b DATE NOT NULL, UNIQUE (b));
CREATE TABLE r22 (a INT, b ENUM('x') NOT NULL, UNIQUE (b));
CREATE TABLE r24 (a INT, b INT NOT NULL, UNIQUE ((b + 1)));
CREATE TABLE r25 (a INT, b INT NOT NULL, c INT NOT NULL, UNIQUE (b), UNIQUE (c), PRIMARY KEY (a));
CREATE TABLE r26 (a INT, b INT GENERATED ALWAYS AS (a + 1) STORED NOT NULL, UNIQUE (b));
CREATE TABLE r27 (a INT, b INT NOT NULL INVISIBLE, UNIQUE (b));
CREATE TABLE r29 (a INT, b VARCHAR(5) NOT NULL, KEY (b), c INT NOT NULL, UNIQUE (c));
CREATE TABLE r30 (a INT, b VARCHAR(5), UNIQUE (b), c INT NOT NULL, UNIQUE (c));
CREATE TABLE users (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, name VARCHAR(100) NOT NULL);
CREATE TABLE orders (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, user_id BIGINT UNSIGNED NOT NULL, note TEXT, total DECIMAL(10,2) NOT NULL);
CREATE VIEW v_users AS SELECT id, name FROM users;
`

// resolveCases are name resolutions pinned against mysqld: want is the server's error
// number, 0 when the statement runs. They cover the select list's aliases seen through a
// nested query (lookup / outerItem), the same-level GROUP BY / ORDER BY / HAVING
// expressions, HAVING's own tables (havingWalk), `_rowid`, and what ON DUPLICATE KEY
// UPDATE sees.
var resolveCases = []struct {
	sql  string
	want int
}{
	// aliases in the block's own ORDER BY / GROUP BY / HAVING expressions
	{"SELECT COUNT(*) AS c FROM users ORDER BY c+1", 0},
	{"SELECT name COLLATE utf8mb4_bin AS n2 FROM users ORDER BY n2, hex(n2)", 0},
	{"SELECT 'a' AS f1 FROM users WHERE id='8' GROUP BY f1 ORDER BY CONCAT(f1)", 0},
	{"SELECT id AS name FROM users ORDER BY name + 1", 0},
	{"SELECT id AS name FROM users HAVING name + 1", 0},
	{"SELECT COUNT(*) AS name FROM users GROUP BY name + 1", 0},
	{"SELECT id + 1 AS c FROM users GROUP BY c", 0},
	{"SELECT id AS c FROM users WHERE c+1", 1054},
	{"SELECT name c, c FROM users", 1054},
	{"SELECT name AS c, name AS c FROM users ORDER BY c + 1", 0},
	{"SELECT name AS c, name AS c FROM users ORDER BY c", 0},
	{"SELECT name c, name c FROM users HAVING c", 0},
	{"SELECT name AS c, id AS c FROM users HAVING c + 1", 1052},
	{"SELECT name c, id c FROM users ORDER BY c + 1", 1052},
	{"SELECT name c, id c FROM users ORDER BY c", 1052},
	{"SELECT name c, id c FROM users GROUP BY c + 1", 1052},
	{"SELECT name c, id c FROM users GROUP BY c", 1052},
	// an aggregate cannot be grouped on
	{"SELECT 1 FROM users GROUP BY SUM(id)", 1111},
	{"SELECT COUNT(*) FROM users GROUP BY COUNT(*)", 1056},
	{"SELECT SUM(id) s FROM users GROUP BY s+1", 1056},
	{"SELECT SUM(id) s FROM users GROUP BY s", 1056},
	{"SELECT name FROM users GROUP BY (SELECT 1 FROM orders GROUP BY COUNT(*) LIMIT 1)", 1111},
	// VALUES() outside ON DUPLICATE KEY UPDATE is NULL
	{"SELECT VALUES(id) FROM users", 0},
	// aliases seen from a nested query: by the enclosing clause it sits in
	{"SELECT name c, (SELECT id FROM orders WHERE note = c) FROM users", 0},
	{"SELECT name c FROM users WHERE (SELECT id FROM orders WHERE note = c)", 1054},
	{"SELECT name c FROM users HAVING (SELECT id FROM orders WHERE note = c)", 0},
	{"SELECT name c FROM users ORDER BY (SELECT id FROM orders WHERE note = c)", 0},
	{"SELECT name c FROM users JOIN orders ON (SELECT id FROM orders WHERE note = c)", 1054},
	{"SELECT name c FROM users WHERE id IN (SELECT id FROM orders HAVING c)", 1054},
	{"SELECT name c FROM users WHERE id IN (SELECT id FROM orders ORDER BY c)", 1054},
	{"SELECT name c FROM users JOIN (SELECT c) d", 1054},
	{"SELECT name c FROM users GROUP BY c HAVING (SELECT 1 HAVING c)", 0},
	{"SELECT name c, (SELECT 1 FROM orders JOIN users ON note = c) FROM users", 0},
	{"SELECT name c, (SELECT (SELECT 1 FROM orders WHERE note = c)) FROM users", 0},
	{"SELECT name c, (SELECT 1 FROM (SELECT c) d) FROM users", 0},
	{"SELECT 1 AS a, (SELECT a UNION SELECT a)", 0},
	{"SELECT 1 AS c1, (SELECT COUNT(*) FROM orders HAVING c1 > 0) FROM DUAL", 0},
	{"SELECT 1 AS c, (SELECT c)", 0},
	{"SELECT id AS c, (SELECT 1 FROM orders GROUP BY c) FROM users", 0},
	{"SELECT id AS c FROM users ORDER BY (SELECT 1 FROM orders ORDER BY c)", 0},
	// a later alias is a forward reference
	{"SELECT (SELECT 1 FROM orders WHERE note = c), name c FROM users", 1247},
	{"SELECT (SELECT c), name c FROM users", 1247},
	{"SELECT (SELECT c), 1 AS c", 1247},
	{"SELECT (SELECT 1 FROM orders GROUP BY c), id AS c FROM users", 1247},
	// an aggregate's alias: only from the nested query's HAVING, never from a GROUP BY
	{"SELECT COUNT(*) AS c, (SELECT 1 FROM orders HAVING c) FROM users", 0},
	{"SELECT SUM(id) AS s, (SELECT 1 HAVING s) FROM users", 0},
	{"SELECT COUNT(*) AS c FROM users HAVING (SELECT 1 FROM orders HAVING c)", 0},
	{"SELECT COUNT(*) AS c FROM users ORDER BY (SELECT 1 FROM orders HAVING c)", 0},
	{"SELECT COUNT(*) AS c FROM users ORDER BY (SELECT 1 HAVING c)", 0},
	{"SELECT COUNT(*) AS c, (SELECT c) FROM users", 1247},
	{"SELECT COUNT(*) AS c, (SELECT 1 FROM orders WHERE c) FROM users", 1247},
	{"SELECT COUNT(*) AS c, (SELECT 1 FROM orders ORDER BY c) FROM users", 1247},
	{"SELECT COUNT(*) AS c FROM users GROUP BY (SELECT 1 FROM orders HAVING c)", 1247},
	{"SELECT COUNT(*) AS c FROM users GROUP BY (SELECT c)", 1247},
	{"SELECT COUNT(*) AS c FROM users HAVING (SELECT c)", 1247},
	{"SELECT COUNT(*) AS c FROM users ORDER BY (SELECT 1 FROM orders GROUP BY c)", 1247},
	{"SELECT SUM(id) + 1 AS s FROM users GROUP BY (SELECT 1 HAVING s)", 1247},
	{"SELECT (SELECT c), COUNT(*) AS c FROM users", 1247},
	{"SELECT COUNT(*) AS c FROM users WHERE (SELECT c)", 1054},
	// HAVING never consults the block's own tables, but an enclosing block's
	{"SELECT name FROM users HAVING id > 0", 1054},
	{"SELECT COUNT(*) FROM users HAVING name = 'x'", 1054},
	{"SELECT COUNT(*) FROM users GROUP BY name HAVING name = 'x'", 0},
	{"SELECT COUNT(*) FROM users HAVING MAX(id) > 1", 0},
	{"SELECT 1 FROM users WHERE EXISTS (SELECT 1 FROM orders HAVING id)", 0},
	{"SELECT 1 FROM users WHERE EXISTS (SELECT 1 FROM orders HAVING note)", 1054},
	{"SELECT 1 FROM users u WHERE EXISTS (SELECT 1 FROM orders HAVING u.id)", 0},
	{"SELECT 1 FROM users u WHERE EXISTS (SELECT 1 FROM orders o HAVING o.note)", 1054},
	{"SELECT 1 FROM users u WHERE EXISTS (SELECT 1 FROM orders o GROUP BY note HAVING o.note)", 0},
	{"SELECT 1 FROM users WHERE EXISTS (SELECT 1 FROM orders HAVING (id / -7777777777) IN ('a'))", 0},
	{"SELECT (SELECT 1 FROM orders HAVING name) FROM users", 0},
	// _rowid
	{"SELECT _rowid FROM r1", 1054},
	{"SELECT _rowid FROM r2", 1054},
	{"SELECT _rowid FROM r3", 1054},
	{"SELECT _rowid FROM r4", 1054},
	{"SELECT _rowid FROM r5", 1054},
	{"SELECT _rowid FROM r6", 0},
	{"SELECT _rowid FROM r7", 0},
	{"SELECT _rowid FROM r8", 1054},
	{"SELECT _rowid FROM r9", 0},
	{"SELECT _rowid FROM r10", 0},
	{"SELECT _rowid FROM r11", 0},
	{"SELECT _rowid FROM r12", 0},
	{"SELECT _rowid FROM r14", 1054},
	{"SELECT _rowid FROM r15", 0},
	{"SELECT _rowid FROM r17", 0},
	{"SELECT _rowid FROM r18", 0},
	{"SELECT _rowid FROM r19", 0},
	{"SELECT _rowid FROM r20", 1054},
	{"SELECT _rowid FROM r21", 1054},
	{"SELECT _rowid FROM r22", 1054},
	{"SELECT _rowid FROM r24", 1054},
	{"SELECT _rowid FROM r25", 0},
	{"SELECT _rowid FROM r26", 0},
	{"SELECT _rowid FROM r27", 0},
	{"SELECT _rowid FROM r29", 0},
	{"SELECT _rowid FROM r30", 0},
	{"SELECT _ROWID FROM users", 0},
	{"SELECT _rowid, users._rowid FROM users", 0},
	{"SELECT u._rowid FROM users u", 0},
	{"SELECT _rowid FROM users, orders", 1054},
	{"SELECT _rowid FROM users JOIN orders USING (id)", 1054},
	{"SELECT u._rowid, o._rowid FROM users u LEFT JOIN orders o ON 1 = 0", 0},
	{"SELECT _rowid FROM (SELECT id FROM users) d", 1054},
	{"SELECT _rowid FROM v_users", 1054},
	{"SELECT * FROM users ORDER BY _rowid", 0},
	{"SELECT name FROM users WHERE _rowid = 1", 0},
	{"SELECT name FROM users GROUP BY _rowid", 0},
	{"SELECT (SELECT _rowid) FROM users", 0},
	{"SELECT (SELECT _rowid FROM orders, users LIMIT 1) FROM users", 0},
	{"UPDATE users SET _rowid = 1", 0},
	{"UPDATE users SET name = 'y' WHERE _rowid = 7", 0},
	{"DELETE FROM users WHERE _rowid = 7", 0},
	{"INSERT INTO users (_rowid, name) VALUES (5, 'x')", 0},
	// ON DUPLICATE KEY UPDATE sees the SELECT's tables
	{"INSERT INTO users (id, name) SELECT id, name FROM users u ON DUPLICATE KEY UPDATE name = u.name", 0},
	{"INSERT INTO users (id, name) SELECT id, name FROM users u ON DUPLICATE KEY UPDATE users.name = u.name", 0},
	{"INSERT INTO users (id, name) SELECT id, name FROM users u ON DUPLICATE KEY UPDATE name = nosuch.name", 1054},
	{"INSERT INTO users (id, name) SELECT o.id, 'x' FROM orders o ON DUPLICATE KEY UPDATE name = note", 0},
	{"INSERT INTO users (id, name) SELECT o.id, 'x' FROM orders o ON DUPLICATE KEY UPDATE name = o.total", 0},
	{"INSERT INTO users (id, name) SELECT o.id, 'x' FROM (SELECT id FROM orders) o ON DUPLICATE KEY UPDATE name = o.id", 0},
	{"INSERT INTO users (id, name) SELECT o.id, 'x' FROM orders o ON DUPLICATE KEY UPDATE name = (SELECT total FROM orders WHERE id = o.id)", 0},
	{"INSERT INTO users (id, name) SELECT u.id, u.name FROM users u ON DUPLICATE KEY UPDATE name = id", 1052},
	{"INSERT INTO users (id, name) SELECT o.id, 'x' AS nm FROM orders o ON DUPLICATE KEY UPDATE name = nm", 1054},
}

func TestNameResolutionServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, rowidSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	for _, c := range resolveCases {
		_, err := conn.ExecContext(ctx, c.sql)
		got := 0
		if err != nil {
			var me *driver.MySQLError
			if !errors.As(err, &me) {
				t.Errorf("%s: %v", c.sql, err)
				continue
			}
			got = int(me.Number)
		}
		if got != c.want {
			t.Errorf("%s: the server says %d (%v), the checker %d", c.sql, got, err, c.want)
		}
	}
}

// TestNameResolution is the checker's side of resolveCases.
func TestNameResolution(t *testing.T) {
	s, err := schema.Load(rowidSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resolveCases {
		res, err := Analyze(s, c.sql)
		got := 0
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				t.Errorf("%s: %v", c.sql, err)
				continue
			}
			got = e.Code
		}
		if got != c.want {
			t.Errorf("%s: got %d (%v), want %d", c.sql, got, err, c.want)
		}
		if err == nil && c.sql == "SELECT VALUES(id) FROM users" && (len(res.Columns) != 1 || res.Columns[0].Type.Name != "null") {
			t.Errorf("VALUES(id) outside INSERT: %+v, want the null type", res.Columns)
		}
		if err == nil && c.sql == "SELECT _rowid, users._rowid FROM users" {
			if len(res.Columns) != 2 || res.Columns[0].Name != "_rowid" || res.Columns[0].Type.Name != "bigint" || !res.Columns[0].Type.Unsigned || res.Columns[0].Nullable {
				t.Errorf("_rowid columns: %+v", res.Columns)
			}
		}
	}
}
