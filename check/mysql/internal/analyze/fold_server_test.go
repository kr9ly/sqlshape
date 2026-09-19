package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// foldCases are constant expressions fold.go judges, each pinned against mysqld: want is
// the error number the server raises before reading a row, 0 when the statement runs. A
// constant that runs per row (a select item over a FROM, an UPDATE's SET, a comparison
// against an unindexed column) does not fail over an empty table: those are
// foldPerRowCases, run first while `users` is empty, where the checker lists the violation
// 1690 instead.
var foldCases = []struct {
	sql  string
	want int
}{
	// integer operators: the exact result must fit BIGINT, or BIGINT UNSIGNED when an
	// operand is unsigned
	{"SELECT 9223372036854775807 * 2", 1690},
	{"SELECT 9223372036854775807 + 1", 1690},
	{"SELECT 9223372036854775807 + 1.0", 0},
	{"SELECT 9223372036854775807 + 1e0", 0},
	{"SELECT 9223372036854775807 + TRUE", 1690},
	{"SELECT 9223372036854775807 + NULL", 0},
	{"SELECT '9223372036854775807' + 1", 0},
	{"SELECT '1e308' + '1e308'", 1690},
	{"SELECT 'abc' + 9223372036854775807", 0},
	{"SELECT -9223372036854775808 - 1", 1690},
	{"SELECT 9223372036854775807 - -1", 1690},
	{"SELECT 4294967296 * 4294967296", 1690},
	{"SELECT -9223372036854775808 * -1", 1690},
	{"SELECT 9223372036854775809 * -1", 1690},
	{"SELECT 18446744073709551615 + 1", 1690},
	{"SELECT 1 + 18446744073709551615", 1690},
	{"SELECT 9223372036854775807 + CAST(1 AS UNSIGNED)", 0},
	{"SELECT CAST(1 AS UNSIGNED) - 2", 1690},
	{"SELECT CAST(1 AS UNSIGNED) + -1", 0},
	{"SELECT CAST(1 AS UNSIGNED) + -2", 1690},
	{"SELECT -1 + CAST(1 AS UNSIGNED)", 0},
	{"SELECT -1 - CAST(1 AS UNSIGNED)", 1690},
	{"SELECT 1 - CAST(2 AS UNSIGNED)", 1690},
	{"SELECT 2 - 3 + CAST(1 AS UNSIGNED)", 0},
	{"SELECT -1 * CAST(1 AS UNSIGNED)", 1690},
	{"SELECT CAST(0 AS UNSIGNED) * -1", 0},
	{"SELECT 18446744073709551615 - (-1)", 1690},
	{"SELECT 9223372036854775807 * 1 + 1", 1690},
	{"SELECT (9223372036854775807 + 1) * 0", 1690},
	{"SELECT 1 + 1 + 9223372036854775807", 1690},
	{"SELECT 9223372036854775807 % 2 + 1", 0},
	{"SELECT 9223372036854775807 MOD 0", 0},
	{"SELECT -(-9223372036854775808)", 0},
	{"SELECT -9223372036854775809", 0},
	{"SELECT -(CAST(9223372036854775809 AS UNSIGNED))", 0},
	// DIV: the truncated quotient must fit; magnitudes are divided, so 2^63 never does
	{"SELECT -9223372036854775808 DIV -1", 1690},
	{"SELECT -9223372036854775808 DIV 1", 1690},
	{"SELECT 9223372036854775807 DIV -1", 0},
	{"SELECT 18446744073709551615 DIV -1", 1690},
	{"SELECT 18446744073709551615 DIV 1", 0},
	{"SELECT 18446744073709551615 DIV 2", 0},
	{"SELECT 18446744073709551615 DIV 1.0", 0},
	{"SELECT 18446744073709551615 DIV -0.5", 1690},
	{"SELECT -1 DIV CAST(1 AS UNSIGNED)", 1690},
	{"SELECT 10 DIV 0", 0},
	{"SELECT 1 DIV 0.5", 0},
	{"SELECT 1.5 DIV 1", 0},
	{"SELECT 9223372036854775807 DIV 0.5", 1690},
	{"SELECT 9223372036854775807 DIV 1e-1", 1690},
	{"SELECT -9223372036854775808 DIV -1.0", 1690},
	{"SELECT 123456789012345678901234567890 DIV 1", 1690},
	{"SELECT '18446744073709551616' DIV 1", 1690},
	{"SELECT 10 / 0", 0},
	{"SELECT 1e300 / 1e-300", 1690},
	// DOUBLE: an infinite result
	{"SELECT 1e308 + 1e308", 1690},
	{"SELECT -1e308 - 1e308", 1690},
	{"SELECT 1e300 * 1e300", 1690},
	{"SELECT 1e308 * 10", 1690},
	{"SELECT 1e308 * 1e0", 0},
	{"SELECT 3.4e38 * 10", 0},
	{"SELECT EXP(709)", 0},
	{"SELECT EXP(710)", 1690},
	{"SELECT EXP('710')", 1690},
	{"SELECT POW(2, 1023)", 0},
	{"SELECT POW(2, 1024)", 1690},
	{"SELECT POWER(2, 1024)", 1690},
	{"SELECT POW(0, -1)", 1690},
	{"SELECT POW(-1, 0.5)", 1690},
	{"SELECT COT(0)", 1690},
	{"SELECT COT(0.0)", 1690},
	{"SELECT COT('v')", 1690},
	{"SELECT COT(3.141592653589793)", 0},
	{"SELECT DEGREES(1e306)", 0},
	{"SELECT DEGREES(1e307)", 1690},
	{"SELECT RADIANS(1e308)", 0},
	{"SELECT LN(0)", 0},
	{"SELECT SQRT(-1)", 0},
	{"SELECT TAN(1e308)", 0},
	// ABS / ROUND over integers
	{"SELECT ABS(-9223372036854775808)", 1690},
	{"SELECT ABS(-9223372036854775808 + 0)", 1690},
	{"SELECT ABS(CAST(-1 AS SIGNED))", 0},
	{"SELECT ABS(-(1e19))", 0},
	{"SELECT ROUND(9223372036854775807, -1)", 1690},
	{"SELECT ROUND(9223372036854775805, -1)", 1690},
	{"SELECT ROUND(9223372036854775804, -1)", 0},
	{"SELECT ROUND(9223372036854775807, -18)", 0},
	{"SELECT ROUND(9223372036854775807, -19)", 1690},
	{"SELECT ROUND(9223372036854775807, -1.5)", 0},
	{"SELECT ROUND(9223372036854775807, '-1')", 1690},
	{"SELECT ROUND(9223372036854775807.0, -1)", 0},
	{"SELECT ROUND(-9223372036854775808, -1)", 1690},
	{"SELECT ROUND(-9223372036854775808, -4)", 1690},
	{"SELECT ROUND(18446744073709551615, -1)", 1690},
	{"SELECT ROUND(18446744073709551615, 0)", 0},
	{"SELECT ROUND(15, -1)", 0},
	{"SELECT TRUNCATE(9223372036854775807, -1)", 0},
	{"SELECT FLOOR(1e308)", 0},
	// CAST
	{"SELECT CAST(POW(2,63) AS SIGNED)", 1690},
	{"SELECT CAST(POW(2, 63) AS UNSIGNED)", 1690},
	{"SELECT CAST(POW(-2, 63) AS UNSIGNED)", 0},
	{"SELECT CAST(POW(2,64) AS UNSIGNED)", 1690},
	{"SELECT 1-CAST(POW(2,100) AS SIGNED)", 1690},
	{"SELECT CAST(EXP(50) AS SIGNED)", 1690},
	{"SELECT CAST(1e18 * 10 AS SIGNED)", 1690},
	{"SELECT CAST('1e19' + 0 AS SIGNED)", 1690},
	{"SELECT CAST(1e19 + 0 AS UNSIGNED)", 1690},
	{"SELECT CAST(-(1e19) AS SIGNED)", 1690},
	{"SELECT CAST(-(1e19) AS UNSIGNED)", 1690},
	{"SELECT CAST(-(5e18) AS UNSIGNED)", 0},
	{"SELECT CAST(-9.3e18 AS SIGNED)", 1690},
	{"SELECT CAST(-9.2e18 AS SIGNED)", 0},
	{"SELECT CAST(9.3e18 AS SIGNED)", 0},
	{"SELECT CAST(1.9e19 AS UNSIGNED)", 0},
	{"SELECT CAST(-1 AS UNSIGNED)", 0},
	{"SELECT CAST(-1.5 AS UNSIGNED)", 0},
	{"SELECT CAST(9223372036854775808 AS SIGNED)", 0},
	{"SELECT CAST(99999999999999999999 AS SIGNED)", 0},
	{"SELECT CAST('99999999999999999999' AS SIGNED)", 0},
	{"SELECT CAST(9223372036854775807 + 1.0 AS SIGNED)", 0},
	{"SELECT CAST(CAST(1e19 AS DOUBLE) AS SIGNED)", 1690},
	{"SELECT CAST(CAST(1e19 AS DECIMAL(30)) AS SIGNED)", 0},
	{"SELECT CAST(\"3.14e100\" AS FLOAT)", 1690},
	{"SELECT CAST(3.14e100 AS FLOAT)", 1690},
	{"SELECT CAST(1e40 AS FLOAT)", 1690},
	{"SELECT CAST(-3.5e38 AS FLOAT)", 1690},
	{"SELECT CAST(3.5e38 AS DOUBLE)", 0},
	{"SELECT CAST(1e308 * 10 AS DOUBLE)", 1690},
	{"SELECT CAST(1e39 AS DECIMAL(65))", 0},
	// RANDOM_BYTES
	{"SELECT RANDOM_BYTES(1)", 0},
	{"SELECT RANDOM_BYTES(1024)", 0},
	{"SELECT RANDOM_BYTES(0)", 1690},
	{"SELECT RANDOM_BYTES(1025)", 1690},
	{"SELECT RANDOM_BYTES(-1)", 1690},
	{"SELECT RANDOM_BYTES(1000000000000)", 1690},
	{"SELECT RANDOM_BYTES(0.9)", 0},
	{"SELECT RANDOM_BYTES(1024.9)", 1690},
	{"SELECT RANDOM_BYTES(1e3)", 0},
	{"SELECT RANDOM_BYTES(TRUE)", 0},
	{"SELECT RANDOM_BYTES(FALSE)", 1690},
	{"SELECT RANDOM_BYTES('5')", 0},
	{"SELECT RANDOM_BYTES('abc')", 1690},
	{"SELECT RANDOM_BYTES(NULL)", 0},
	{"SELECT LENGTH(RANDOM_BYTES(0))", 1690},
	// the constant is evaluated wherever it sits, unless something never reaches it
	{"SELECT (9223372036854775807 + 1) = 1", 1690},
	{"SELECT CONCAT(9223372036854775807 + 1)", 1690},
	{"SELECT CONCAT('a', POW(1000,1000))", 1690},
	{"SELECT ROW(POW(1000, 1000), 1) = ROW(1, 1)", 1690},
	{"SELECT GREATEST(1, POW(1000,1000))", 1690},
	{"SELECT NULL + POW(1000,1000)", 1690},
	{"SELECT 1 BETWEEN 0 AND POW(1000,1000)", 1690},
	{"SELECT NULLIF(1, POW(1000,1000))", 1690},
	{"SELECT NOT POW(1000,1000)", 1690},
	{"SELECT 1 XOR POW(1000,1000)", 1690},
	{"SELECT (9223372036854775807 + 1) IS NOT NULL", 1690},
	{"SELECT (9223372036854775807 + 1) IS NULL", 0},
	{"SELECT ISNULL(9223372036854775807 + 1)", 0},
	{"SELECT 1 AND POW(1000,1000)", 1690},
	{"SELECT 1 AND 1 AND POW(1000,1000)", 1690},
	{"SELECT 0 AND POW(1000,1000)", 0},
	{"SELECT 1 AND 0 AND POW(1000,1000)", 0},
	{"SELECT NULL AND POW(1000,1000)", 1690},
	{"SELECT POW(1000,1000) AND 0", 1690},
	{"SELECT 0 OR POW(1000,1000)", 1690},
	{"SELECT 1 OR POW(1000,1000)", 0},
	{"SELECT NULL OR POW(1000,1000)", 1690},
	{"SELECT IF(1, POW(1000,1000), 1)", 1690},
	{"SELECT IF(0, POW(1000,1000), 1)", 0},
	{"SELECT IF(NULL, POW(1000,1000), 1)", 0},
	{"SELECT IF('a', POW(1000,1000), 1)", 0},
	{"SELECT IF(1 = 1, 1, POW(1000,1000))", 0},
	{"SELECT CASE WHEN 0 THEN POW(1000,1000) END", 0},
	{"SELECT CASE WHEN 1 THEN 1 ELSE POW(1000,1000) END", 0},
	{"SELECT CASE WHEN 1 THEN 1 WHEN POW(1000,1000) THEN 2 END", 0},
	{"SELECT CASE WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE POW(1000,1000) END", 0},
	{"SELECT CASE 1 WHEN 1 THEN 1 ELSE POW(1000,1000) END", 0},
	{"SELECT CASE 1 WHEN 2 THEN POW(1000,1000) END", 0},
	{"SELECT CASE 1 WHEN POW(1000,1000) THEN 1 END", 1690},
	{"SELECT COALESCE(1, POW(1000,1000))", 0},
	{"SELECT COALESCE(NULL, 1, POW(1000,1000))", 0},
	{"SELECT COALESCE(NULL, POW(1000,1000))", 1690},
	{"SELECT IFNULL(1, POW(1000,1000))", 0},
	{"SELECT IFNULL(NULL, POW(1000,1000))", 1690},
	{"SELECT 1 IN (1, POW(1000,1000))", 0},
	{"SELECT 2 IN (1, POW(1000,1000))", 1690},
	// a select list without a FROM is evaluated once; a false WHERE or LIMIT 0 stops that
	{"SELECT 9223372036854775807 + 1 FROM DUAL", 1690},
	{"SELECT 9223372036854775807 + 1 FROM DUAL WHERE 1 = 0", 0},
	{"SELECT 9223372036854775807 + 1 LIMIT 0", 0},
	{"SELECT 9223372036854775807 + 1 UNION SELECT 1", 1690},
	{"SELECT 1 UNION SELECT 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users WHERE 1 = 0 UNION SELECT 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM (SELECT 9223372036854775807 + 1 AS x) d", 1690},
	{"SELECT 1 FROM (SELECT 1 UNION SELECT 9223372036854775807 + 1) d", 1690},
	{"WITH d AS (SELECT 9223372036854775807 + 1 AS x) SELECT 1 FROM d", 1690},
	{"SELECT EXISTS (SELECT 9223372036854775807 + 1)", 0},
	{"SELECT 1 FROM users u WHERE EXISTS (SELECT 9223372036854775807 + 1)", 0},
	{"SELECT 1 FROM users u WHERE (SELECT 9223372036854775807 + 1)", 1690},
	{"SELECT 1 FROM users WHERE id = (SELECT 9223372036854775807 + 1)", 1690},
	{"SELECT 1 FROM users WHERE id IN (SELECT 9223372036854775807 + 1)", 1690},
	{"SELECT 1 FROM users WHERE id > ALL (SELECT 9223372036854775807 + 1)", 1690},
	// a condition is folded before any row is read
	{"SELECT 1 FROM users WHERE 9223372036854775807 * 2", 1690},
	{"SELECT 1 FROM users WHERE ABS(-9223372036854775808)", 1690},
	{"SELECT 1 FROM users WHERE CAST(POW(2,63) AS SIGNED)", 1690},
	{"SELECT 1 FROM users WHERE id = 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users WHERE id > 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users WHERE id IN (1, 9223372036854775807 + 1)", 1690},
	{"SELECT 1 FROM users WHERE id IN (POW(1000,1000))", 1690},
	{"SELECT 1 FROM users WHERE id BETWEEN 1 AND POW(1000,1000)", 1690},
	{"SELECT 1 FROM users WHERE name LIKE CONCAT('%', POW(1000,1000))", 1690},
	{"SELECT 1 FROM users WHERE CAST(9223372036854775807 + 1 AS CHAR)", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users WHERE id = 1 OR 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND 9223372036854775807 * 2 = 0", 1690},
	{"SELECT 1 FROM users WHERE 1 = 0 AND 9223372036854775807 + 1", 0},
	{"SELECT 1 FROM users WHERE NULL AND POW(1000,1000)", 0},
	{"SELECT 1 FROM users WHERE POW(1000,1000) AND 1 = 0", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND 1 = 0 AND POW(1000,1000)", 0},
	{"SELECT 1 FROM users WHERE 1 = 0 AND id = 1 AND 9223372036854775807 + 1", 0},
	{"SELECT 1 FROM users WHERE id = 1 AND 9223372036854775807 + 1 > 0 AND 1 = 0", 1690},
	{"SELECT 1 FROM users WHERE 1 = 0 OR POW(1000,1000)", 1690},
	{"SELECT 1 FROM users WHERE 1 = 1 OR POW(1000,1000)", 0},
	{"SELECT 1 FROM users WHERE (1 = 0) AND (POW(1000,1000) OR id)", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND (0 OR POW(1000,1000))", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND (1 OR POW(1000,1000))", 0},
	{"SELECT 1 FROM users WHERE id = 1 AND NOT (1 OR POW(1000,1000))", 0},
	{"SELECT 1 FROM users WHERE 1 = 0 AND EXISTS (SELECT 1 FROM orders WHERE 9223372036854775807 + 1)", 1690},
	{"SELECT 1 FROM users WHERE id = 1 AND IF(1 = 1, 1, POW(1000,1000))", 0},
	{"SELECT 1 FROM users WHERE id = 1 AND CASE WHEN 1 THEN 1 ELSE POW(1000,1000) END", 0},
	{"SELECT 1 FROM users WHERE id = 1 AND COALESCE(1, POW(1000,1000))", 0},
	{"SELECT 1 FROM users WHERE IF(0, 1, POW(1000,1000))", 1690},
	{"SELECT 1 FROM users u JOIN orders o ON 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users u JOIN orders o ON 1 = 0 AND 9223372036854775807 + 1", 0},
	{"SELECT 1 FROM users u LEFT JOIN orders o ON o.user_id = u.id AND 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users HAVING 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users GROUP BY id HAVING 9223372036854775807 + 1", 1690},
	{"SELECT COUNT(*) FROM users HAVING 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users GROUP BY id WITH ROLLUP HAVING 9223372036854775807 + 1", 1690},
	{"SELECT 1 FROM users u WHERE u.id = (SELECT MAX(id) FROM orders HAVING POW(1000,1000))", 1690},
	{"SELECT 1 FROM users u WHERE u.id = (SELECT MAX(id) FROM orders HAVING 1 = 0 AND POW(1000,1000))", 0},
	{"SELECT 1 FROM users u WHERE u.id = (SELECT MAX(id) FROM orders WHERE 1 = 0 AND POW(1000,1000))", 0},
	{"DELETE FROM users WHERE 9223372036854775807 + 1", 1690},
	{"DELETE FROM users WHERE 1 = 0 AND POW(1000,1000)", 0},
	{"UPDATE users SET name = 'x' WHERE 9223372036854775807 + 1", 1690},
	{"UPDATE users SET name = 'a' WHERE 1 = 0 OR POW(1000,1000)", 1690},
	// GROUP BY and ORDER BY drop a constant unevaluated (a window's ORDER BY does not: it
	// runs per row, which the checker does not read yet)
	{"SELECT 1 FROM users GROUP BY 9223372036854775807 + 1", 0},
	{"SELECT 1 FROM users ORDER BY 9223372036854775807 + 1", 0},
	{"SELECT id FROM users GROUP BY id, 9223372036854775807 + 1", 0},
	// INSERT ... VALUES is evaluated; INSERT ... SELECT follows the SELECT's rule
	{"INSERT INTO users (id, name) VALUES (1, 9223372036854775807 + 1)", 1690},
	{"INSERT INTO users (id, name) VALUES (6, IF(1, 'a', POW(1000,1000)))", 0},
	{"INSERT INTO users (id, name) SELECT 5, 9223372036854775807 + 1", 1690},
	{"INSERT INTO users (id, name) SELECT 5, 'a' WHERE 1 = 0 AND POW(1000,1000)", 0},
	{"INSERT INTO metrics (id, small, tiny, y) VALUES (9223372036854775807 + 1, 1, 1, 2000)", 1690},
}

// foldPerRowCases run their constant per row: the server (with no row) accepts them, the
// checker lists the violation 1690.
var foldPerRowCases = []string{
	"SELECT 9223372036854775807 + 1 FROM users",
	"SELECT (SELECT 9223372036854775807 + 1) FROM users",
	"SELECT 1 FROM users WHERE RANDOM_BYTES(0)",
	"SELECT 1 FROM users WHERE name = 1e308 * 10",
	"SELECT 1 FROM users WHERE name = POW(1000,1000) AND id = 1",
	"SELECT POW(1000,1000) FROM users WHERE 1 = 0",
	"SELECT ABS(-9223372036854775808) FROM users",
	"SELECT SUM(9223372036854775807 + 1) FROM users",
	"SELECT id + (9223372036854775807 + 1) FROM users",
	"SELECT 1 FROM (SELECT 9223372036854775807 + 1 AS x FROM users) d",
	"SELECT 1 FROM users WHERE IF(id, 1, 9223372036854775807 + 1)",
	"SELECT MAX(id) FROM users HAVING MAX(id) > 9223372036854775807 + 1",
	"UPDATE users SET name = 9223372036854775807 + 1",
	"UPDATE users SET name = 9223372036854775807 + 1 WHERE id = 99",
	"INSERT INTO users (id, name) SELECT 5, 9223372036854775807 + 1 FROM users WHERE 1 = 0",
	"INSERT INTO users (id, name) VALUES (5, 'z') ON DUPLICATE KEY UPDATE name = 9223372036854775807 + 1",
}

func TestConstantFoldServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, testSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	run := func(sql string) (int, string) {
		_, err := conn.ExecContext(ctx, sql)
		if err == nil {
			return 0, ""
		}
		var me *driver.MySQLError
		if !errors.As(err, &me) {
			t.Errorf("%s: %v", sql, err)
			return -1, ""
		}
		return int(me.Number), me.Message
	}
	s := load(t)
	for _, sql := range foldPerRowCases {
		if got, msg := run(sql); got != 0 && got >= 0 {
			t.Errorf("%s over no rows: the server says %d %s, want 0", sql, got, msg)
		}
	}
	for _, c := range foldCases {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("%s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		if c.want != 1690 {
			continue
		}
		// the checker's message spells the expression as the server does
		_, err := Analyze(s, c.sql)
		var e *Error
		if !errors.As(err, &e) || e.Code != 1690 {
			t.Errorf("%s: analyzer %v, want 1690", c.sql, err)
			continue
		}
		if e.Message != msg {
			t.Errorf("%s: analyzer says %q, the server %q", c.sql, e.Message, msg)
		}
	}
	// with a row (the ON DUPLICATE KEY UPDATE case above inserted one), the per-row constant
	// fails as the checker's violation says
	for _, sql := range []string{"SELECT 9223372036854775807 + 1 FROM users", "SELECT (SELECT 9223372036854775807 + 1) FROM users", "UPDATE users SET name = 9223372036854775807 + 1", "SELECT 1 FROM users WHERE RANDOM_BYTES(0)", "SELECT 1 FROM users WHERE name = 1e308 * 10"} {
		if got, _ := run(sql); got != 1690 {
			t.Errorf("%s over a row: the server says %d, want 1690", sql, got)
		}
	}
}

// TestConstantFold is the checker's side of foldCases: the statement's error, or none, and
// the violation 1690 for a constant that runs per row.
func TestConstantFold(t *testing.T) {
	s := load(t)
	for _, c := range foldCases {
		res, err := Analyze(s, c.sql)
		code := 0
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				t.Errorf("%s: %v", c.sql, err)
				continue
			}
			code = e.Code
		}
		if code != c.want {
			t.Errorf("%s: got %d, want %d", c.sql, code, c.want)
			continue
		}
		if res != nil && has1690(res.Violations) {
			t.Errorf("%s: lists the violation 1690, wanted none", c.sql)
		}
	}
	for _, sql := range foldPerRowCases {
		res, err := Analyze(s, sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if !has1690(res.Violations) {
			t.Errorf("%s: no violation 1690 listed", sql)
		}
	}
	// NO_UNSIGNED_SUBTRACTION makes a subtraction's result signed
	nus, err := schema.Load(strings.Replace(testSchema, "-- sqlshape: mysql 8.4", "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'NO_UNSIGNED_SUBTRACTION'", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(nus, "SELECT CAST(1 AS UNSIGNED) - 2"); err != nil {
		t.Errorf("CAST(1 AS UNSIGNED) - 2 under NO_UNSIGNED_SUBTRACTION: %v", err)
	}
}

func has1690(vs []Violation) bool {
	for _, v := range vs {
		if v.Code == 1690 && v.Key() == "1690" {
			return true
		}
	}
	return false
}
