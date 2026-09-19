package analyze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

const viewWriteSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (a INT PRIMARY KEY, b INT NOT NULL DEFAULT 5, c INT NOT NULL, d INT);
CREATE TABLE u (x INT PRIMARY KEY, y INT);
CREATE VIEW v_all AS SELECT a, b, c, d FROM t;
CREATE VIEW v_missing AS SELECT a, b, d FROM t;
CREATE VIEW v_expr AS SELECT a, a+1 AS a1, c, d FROM t;
CREATE VIEW v_ren AS SELECT a AS aa, c AS cc, d AS dd FROM t;
CREATE VIEW v_where AS SELECT a, b, c, d FROM t WHERE a > 10 WITH CHECK OPTION;
CREATE VIEW v_join AS SELECT t.a ta, t.c tc, t.d td, u.x ux, u.y uy FROM t JOIN u ON u.x = t.a;
CREATE VIEW v_agg AS SELECT a, COUNT(*) n FROM t GROUP BY a;
CREATE ALGORITHM=TEMPTABLE VIEW v_temp AS SELECT a, b, c, d FROM t;
CREATE VIEW v_nest AS SELECT a, b, c, d FROM v_all;
CREATE VIEW v_dist AS SELECT DISTINCT a, b, c, d FROM t;
CREATE VIEW v_lim AS SELECT a, b, c, d FROM t LIMIT 10;
CREATE VIEW v_dual AS SELECT 1 AS k;
CREATE VIEW v_joind AS SELECT t.a ta, t.c tc, u.x ux, t.a+1 e FROM t JOIN u ON u.x = t.a;
CREATE TABLE s (b INT, c INT NOT NULL, name VARCHAR(10) NOT NULL DEFAULT 'x');
CREATE VIEW v_collate AS SELECT b, c, name COLLATE utf8mb4_bin AS name FROM s;
CREATE VIEW v_dup AS SELECT a, a AS a2, c FROM t;
CREATE VIEW v_expronly AS SELECT a+1 AS a1 FROM t;
CREATE VIEW v_overtemp AS SELECT a, b, c, d FROM v_temp;
CREATE ALGORITHM=TEMPTABLE VIEW v_temp2 AS SELECT a AS a2, c AS b2 FROM t;
CREATE VIEW v_matjoin AS SELECT t.a ta, t.c tc, dt.a2, dt.b2 FROM t JOIN v_temp2 AS dt ON t.a = dt.a2;
CREATE VIEW v_outer AS SELECT t.a ta, t.c tc, u.y uy FROM t LEFT JOIN u ON u.x = t.a;
CREATE VIEW v_chain AS SELECT a, b, c, d FROM v_where;
CREATE VIEW v_selfread AS SELECT a, c FROM t WHERE a < (SELECT MAX(x) FROM u);
CREATE VIEW v_rereads AS SELECT a, c FROM t WHERE a < (SELECT MAX(d) FROM t);
CREATE VIEW v_overjoin AS SELECT ta, tc FROM v_join;
`

// TestViewWriteServer pins how a write through a view is judged, against mysqld: the
// statement's own errors (1471 not insertable, 1393 / 1394 / 1395 join view, 1348 a
// derived column, 1288 not updatable, 1054 / 1136 resolved against the view), the
// violations a running one may hit (the base table's own 1062, the view's 1369 and 1423),
// and the shapes that must go through clean.
func TestViewWriteServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, viewWriteSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(viewWriteSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO t (a, c) VALUES (100, 1), (11, 1)",
		"INSERT INTO u (x, y) VALUES (100, 2)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	cases := []struct {
		sql string
		// stmt: the statement's own error number, raised by the checker and the server
		// alike; 0 when the statement analyzes
		stmt int
		// want: "<number> <key>" the server raises and the checker must have predicted;
		// "" when the server must accept the write
		want string
	}{
		// a single-table view: the write lands on the base table
		{"INSERT INTO v_all (a, c) VALUES (200, 1)", 0, ""},
		{"INSERT INTO v_all VALUES (201, 2, 3, 4)", 0, ""},
		{"INSERT INTO v_all (a, c) VALUES (100, 9)", 0, "1062 PRIMARY"},
		{"INSERT INTO v_ren (aa, cc) VALUES (204, 1)", 0, ""},
		{"INSERT INTO v_nest (a, c) VALUES (205, 1)", 0, ""},
		{"INSERT INTO v_all (a, c) VALUES (100, 1) ON DUPLICATE KEY UPDATE d = 7", 0, ""},
		{"REPLACE INTO v_all (a, c) VALUES (100, 8)", 0, ""},
		// a base column with no default the write leaves out: the view's 1423, exposed or not
		{"INSERT INTO v_missing (a) VALUES (202)", 0, "1423 v_missing"},
		{"INSERT INTO v_missing (a) SELECT 203", 0, "1423 v_missing"},
		{"INSERT INTO v_missing (d) VALUES (1)", 0, "1423 v_missing"},
		{"INSERT INTO v_where (c) VALUES (1)", 0, "1423 v_where"},
		{"INSERT IGNORE INTO v_missing (a) VALUES (221)", 0, ""},
		// WITH CHECK OPTION: the view's 1369, INSERT and REPLACE alike
		{"INSERT INTO v_where (a, c) VALUES (1, 1)", 0, "1369 v_where"},
		{"INSERT INTO v_where (a, c) VALUES (215, 1)", 0, ""},
		{"REPLACE INTO v_where (a, c) VALUES (2, 1)", 0, "1369 v_where"},
		// a derived column anywhere makes the view non-insertable; a listed one is its own 1348
		{"INSERT INTO v_expr (a, c) VALUES (205, 1)", 1471, ""},
		{"INSERT INTO v_expr (a, c) SELECT 206, 1", 1471, ""},
		{"INSERT INTO v_expr (a1) VALUES (1)", 1348, ""},
		{"INSERT INTO v_joind (ta, tc) VALUES (260, 1)", 1471, ""},
		{"REPLACE INTO v_expr (a, c) VALUES (240, 1)", 1471, ""},
		// not merged: never insertable
		{"INSERT INTO v_agg (a) VALUES (210)", 1471, ""},
		{"INSERT INTO v_temp (a, c) VALUES (211, 1)", 1471, ""},
		{"INSERT INTO v_dist (a, c) VALUES (213, 1)", 1471, ""},
		{"INSERT INTO v_lim (a, c) VALUES (214, 1)", 1471, ""},
		{"INSERT INTO v_dual (k) VALUES (1)", 1471, ""},
		// a join view: one base table's columns, under an explicit list
		{"INSERT INTO v_join (ta, tc) VALUES (207, 1)", 0, ""},
		{"INSERT INTO v_join (ta, ux) VALUES (208, 1)", 1393, ""},
		{"INSERT INTO v_join VALUES (209, 1, 2, 3, 4)", 1394, ""},
		{"REPLACE INTO v_join (ta, tc) VALUES (220, 1)", 1395, ""},
		{"INSERT INTO v_join (ta, tc) VALUES (241, 1) ON DUPLICATE KEY UPDATE td = 1", 0, ""},
		{"INSERT INTO v_join (ta, tc) VALUES (242, 1) ON DUPLICATE KEY UPDATE uy = 1", 1393, ""},
		// the list resolves against the view
		{"INSERT INTO v_all (nope) VALUES (1)", 1054, ""},
		{"INSERT INTO v_missing (a, b) VALUES (231, 2, 3)", 1136, ""},
		// DELETE: a merged single-table view deletes from its base (derived columns do not
		// block it); a join view is 1395, anything not merged 1288
		{"DELETE FROM v_all WHERE a = 11", 0, ""},
		{"DELETE FROM v_where WHERE a = 215", 0, ""},
		{"DELETE FROM v_expr WHERE a = 0", 0, ""},
		{"DELETE FROM v_nest WHERE a = 201", 0, ""},
		{"DELETE FROM v_join WHERE ta = 1", 1395, ""},
		{"DELETE v_join FROM v_join JOIN u ON u.x = v_join.ta", 1395, ""},
		{"DELETE FROM v_agg WHERE a = 1", 1288, ""},
		{"DELETE FROM v_temp WHERE a = 1", 1288, ""},
		{"DELETE FROM v_dist WHERE a = 1", 1288, ""},
		{"DELETE FROM v_lim WHERE a = 1", 1288, ""},
		{"DELETE v_all FROM v_all JOIN u ON u.x = v_all.a", 0, ""},
		{"DELETE FROM v_all, u USING v_all JOIN u ON u.x = v_all.a", 0, ""},
		{"WITH cx AS (SELECT a FROM t) DELETE FROM cx", 1288, ""},
		{"WITH cx AS (SELECT a FROM t) UPDATE cx SET a = 1", 1288, ""},
		// UPDATE: a derived column is its own 1348, two base tables through a join view 1393
		{"UPDATE v_expr SET a1 = 1", 1348, ""},
		{"UPDATE v_expr SET a = 300 WHERE a = 0", 0, ""},
		{"UPDATE v_join SET td = 1, uy = 2 WHERE ta = 1", 1393, ""},
		{"UPDATE v_join SET td = 1 WHERE ta = 1", 0, ""},
		{"UPDATE v_agg SET a = 1", 1288, ""},
		// a COLLATE wrapper is transparent for view updating (field_for_view_update)
		{"INSERT INTO v_collate (c, name) VALUES (1, 'a')", 0, ""},
		{"UPDATE v_collate SET name = 'b' WHERE c = 0", 0, ""},
		// the same base column behind two view columns: not insertable, still updatable
		{"INSERT INTO v_dup (a, c) VALUES (300, 1)", 1471, ""},
		{"UPDATE v_dup SET a = 301 WHERE a = 0", 0, ""},
		// a view of only expressions still deletes and updates through (its leaf is
		// updatable), and refuses INSERT
		{"DELETE FROM v_expronly WHERE a1 = 0", 0, ""},
		{"INSERT INTO v_expronly (a1) VALUES (1)", 1348, ""},
		{"INSERT INTO v_expronly VALUES (1)", 1348, ""},
		// a view over a TEMPTABLE view: nothing under it is updatable
		{"INSERT INTO v_overtemp (a, c) VALUES (310, 1)", 1471, ""},
		{"UPDATE v_overtemp SET a = 1", 1288, ""},
		{"DELETE FROM v_overtemp WHERE a = 1", 1288, ""},
		// a join with a materialized side: never insertable; assigning the materialized
		// side's column is the view's 1288, the base side's fine
		{"INSERT INTO v_matjoin (ta, tc) VALUES (320, 1)", 1471, ""},
		{"UPDATE v_matjoin SET a2 = 1", 1288, ""},
		{"UPDATE v_matjoin SET td_no = 1", 1054, ""},
		{"UPDATE v_matjoin SET tc = 9 WHERE ta = 0", 0, ""},
		{"DELETE FROM v_matjoin WHERE ta = 1", 1395, ""},
		// an outer join: neither updatable nor insertable
		{"INSERT INTO v_outer (ta, tc) VALUES (330, 1)", 1471, ""},
		{"UPDATE v_outer SET tc = 1", 1288, ""},
		{"DELETE FROM v_outer WHERE ta = 1", 1288, ""},
		// a plain view over a WITH CHECK OPTION one: the chain still checks, naming the
		// view written through
		{"INSERT INTO v_chain (a, c) VALUES (3, 1)", 0, "1369 v_chain"},
		{"UPDATE v_chain SET a = 4 WHERE a = 215", 0, ""},
		// a view whose own body reads the target base table again in a subquery is not
		// insertable-into; reading another table that way is fine
		{"INSERT INTO v_selfread (a, c) VALUES (400, 1)", 0, ""},
		{"INSERT INTO v_rereads (a, c) VALUES (401, 1)", 1471, ""},
		// a single-leaf view over a join view flattens into its leaves
		{"DELETE FROM v_overjoin WHERE ta = 1", 1395, ""},
		{"INSERT INTO v_overjoin (ta, tc) VALUES (410, 1)", 0, ""},
		// a subquery reading the very view the statement writes: the target itself (1093)
		{"DELETE FROM v_all WHERE a = (SELECT MAX(a) FROM v_all)", 1093, ""},
		{"UPDATE v_all SET d = 1 WHERE a = (SELECT MAX(a) FROM v_all)", 1093, ""},
	}
	for _, c := range cases {
		r, aerr := Analyze(s, c.sql)
		if c.stmt != 0 {
			var e *Error
			if !errors.As(aerr, &e) || e.Code != c.stmt {
				t.Errorf("%s:\n checker %v\n want its own error %d", c.sql, aerr, c.stmt)
			}
		} else if aerr != nil {
			t.Errorf("%s: checker: %v", c.sql, aerr)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, serr := tx.ExecContext(ctx, c.sql)
		tx.Rollback()
		var me *driver.MySQLError
		switch {
		case c.stmt != 0:
			if !errors.As(serr, &me) || int(me.Number) != c.stmt {
				t.Errorf("%s:\n server %v\n want %d", c.sql, serr, c.stmt)
			}
		case c.want != "":
			if !errors.As(serr, &me) {
				t.Errorf("%s: server accepted, want %s", c.sql, c.want)
				continue
			}
			got := fmt.Sprintf("%d %s", me.Number, keyOfMessage(int(me.Number), me.Message))
			if got != c.want {
				t.Errorf("%s:\n server %s\n want   %s", c.sql, got, c.want)
			}
			if r == nil {
				continue
			}
			predicted := false
			for _, v := range r.Violations {
				predicted = predicted || fmt.Sprintf("%d %s", v.Code, v.Key()) == c.want
			}
			if !predicted {
				var keys []string
				for _, v := range r.Violations {
					keys = append(keys, fmt.Sprintf("%d %s", v.Code, v.Key()))
				}
				t.Errorf("%s:\n predicted %v\n want      %s", c.sql, keys, c.want)
			}
		default:
			if serr != nil {
				t.Errorf("%s: server: %v", c.sql, serr)
			}
		}
	}
}

// keyOfMessage reads the constraint key out of the server's message the way
// mysql.ConstraintError does, for the codes this test meets.
func keyOfMessage(number int, msg string) string {
	switch number {
	case 1062:
		if m := reDuplicate.FindStringSubmatch(msg); m != nil {
			return m[1]
		}
	case 1369, 1423:
		if i := strings.Index(msg, "'"); i >= 0 {
			name := strings.Trim(msg[i:], "'")
			if j := strings.Index(name, "'"); j >= 0 {
				name = name[:j]
			}
			if k := strings.Index(name, "."); k >= 0 {
				name = name[k+1:]
			}
			return name
		}
	}
	return ""
}
