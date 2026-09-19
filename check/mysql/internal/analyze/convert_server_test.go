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

// The schema the conversion cases run against, on the server and in the checker alike.
const convertSchema = `-- sqlshape: mysql 8.4
CREATE TABLE tt (
  i INT NOT NULL,
  d DATE,
  dt DATETIME,
  tm TIME,
  ts TIMESTAMP NULL,
  ts6 TIMESTAMP(6) NULL,
  y YEAR,
  c3 CHAR(3),
  vc VARCHAR(10),
  dbl DOUBLE
);
CREATE TABLE ttk (
  i INT NOT NULL PRIMARY KEY,
  ts TIMESTAMP NULL,
  KEY (ts)
);
`

// convertCases are pinned against mysqld over empty tables under the default (strict)
// mode: want is the server's error number, 0 when the statement runs. For a 1292 or a 1525
// the checker must raise the same code and message.
var convertCases = []struct {
	sql  string
	want int
}{
	// a constant CAST to DATE / DATETIME the value does not survive fails a strict write
	// and runs anywhere else
	{"INSERT INTO tt (d) VALUES (CAST('2004-10-0' AS DATE))", 1292},
	{"INSERT INTO tt (d) VALUES (CONVERT('2004-10-0', DATE))", 1292},
	{"INSERT INTO tt (vc) VALUES (CAST('2004-10-0' AS DATE))", 1292},
	{"INSERT INTO tt (i, d) VALUES (1, CAST('2004-10-01' AS DATE))", 0},
	{"INSERT INTO tt (i, dt) VALUES (1, CAST('2004-10-01 15:30' AS DATETIME))", 0},
	{"INSERT INTO tt (d) VALUES (CAST('0000-00-00' AS DATE))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, CAST('0000-10-31 15:30' AS DATETIME))", 0},
	{"INSERT INTO tt (dt) VALUES (CAST('2004-0-10 15:30' AS DATETIME))", 1292},
	{"INSERT INTO tt (i, d) VALUES (1, CAST('2020-00-01' AS DATE))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, CAST('0000-01-01' AS DATETIME))", 0},
	{"INSERT INTO tt (dt) VALUES (CAST(65 AS DATETIME))", 1292},
	{"INSERT INTO tt (d) VALUES (CAST(20041000 AS DATE))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('2004-10-0' AS DATE) IS NULL)", 1292},
	{"INSERT INTO tt (i) VALUES (IF(1, 1, CAST('2004-10-0' AS DATE)))", 0},
	{"INSERT INTO tt (i) VALUES (IF(0, 1, CAST('2004-10-0' AS DATE)))", 1292},
	{"SELECT CAST('2004-10-0' AS DATE)", 0},
	{"SELECT 1 FROM tt WHERE CAST('2004-10-0' AS DATE)", 0},
	{"DELETE FROM tt WHERE CAST('2004-10-0' AS DATE)", 1292},
	// TIME and YEAR
	{"INSERT INTO tt (tm) VALUES (CAST('abc' AS TIME))", 1292},
	{"INSERT INTO tt (tm) VALUES (CAST('839:00:00' AS TIME))", 1292},
	{"INSERT INTO tt (tm) VALUES (CAST(8385960 AS TIME))", 1292},
	{"INSERT INTO tt (i, tm) VALUES (1, CAST('10:00:00' AS TIME))", 0},
	{"SELECT CAST('abc' AS TIME)", 0},
	{"INSERT INTO tt (y) VALUES (CAST('2020extra' AS YEAR))", 1292},
	{"INSERT INTO tt (y) VALUES (CAST(20201 AS YEAR))", 1292},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(2155 AS YEAR))", 0},
	{"INSERT INTO tt (y) VALUES (CAST(2156 AS YEAR))", 1292},
	{"INSERT INTO tt (y) VALUES (CAST(-1 AS YEAR))", 1292},
	{"INSERT INTO tt (y) VALUES (CAST(100 AS YEAR))", 1292},
	{"INSERT INTO tt (y) VALUES (CAST(1900 AS YEAR))", 1292},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(1901 AS YEAR))", 0},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(0 AS YEAR))", 0},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(99 AS YEAR))", 0},
	{"INSERT INTO tt (i, y) VALUES (1, CAST('abc' AS YEAR))", 0},
	{"INSERT INTO tt (i, y) VALUES (1, CAST('' AS YEAR))", 0},
	{"INSERT INTO tt (y) VALUES (CAST('2020.5' AS YEAR))", 1292},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(2020.6 AS YEAR))", 0},
	{"INSERT INTO tt (i, y) VALUES (1, CAST(' 2020' AS YEAR))", 0},
	// CHAR(n)
	{"INSERT INTO tt (c3) VALUES (CAST(1000 AS CHAR(3)))", 1292},
	{"INSERT INTO tt (vc) VALUES (CAST(1000 AS CHAR(3)))", 1292},
	{"INSERT INTO tt (c3) VALUES (CAST(1000.0 AS CHAR(3)))", 1292},
	{"INSERT INTO tt (c3) VALUES (CAST(1000E+0 AS CHAR(3)))", 1292},
	{"INSERT INTO tt (c3) VALUES (CAST('1000' AS CHAR(3)))", 1292},
	{"INSERT INTO tt (i, c3) VALUES (1, CAST('abcd' AS CHAR(3)))", 1292},
	{"INSERT INTO tt (i, c3) VALUES (1, CAST(100 AS CHAR(3)))", 0},
	{"INSERT INTO tt (i, c3) VALUES (1, CAST(NULL AS CHAR(3)))", 0},
	{"SELECT CAST(1000 AS CHAR(3))", 0},
	// SIGNED / UNSIGNED / DOUBLE / DECIMAL from a string
	{"INSERT INTO tt (i) VALUES (CAST('abc' AS SIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('10a' AS UNSIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('' AS SIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST(' 10' AS SIGNED))", 0},
	{"INSERT INTO tt (i) VALUES (CAST('10 ' AS SIGNED))", 0},
	{"INSERT INTO tt (i) VALUES (CAST('1.5' AS SIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('1e2' AS SIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('99999999999999999999' AS SIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST('18446744073709551616' AS UNSIGNED))", 1292},
	{"INSERT INTO tt (i) VALUES (CAST(99999999999999999999.0 AS SIGNED))", 1292},
	{"SELECT CAST('abc' AS SIGNED)", 0},
	{"INSERT INTO tt (dbl) VALUES (CAST('abc' AS DOUBLE))", 1292},
	{"INSERT INTO tt (dbl) VALUES (CAST('1e999' AS DOUBLE))", 1292},
	{"INSERT INTO tt (dbl) VALUES (CAST('abc' AS DECIMAL(5,2)))", 1292},
	// a string operand of an arithmetic or bit operator
	{"INSERT INTO tt (dbl) VALUES (10E+0 + 'a')", 1292},
	{"INSERT INTO tt (dbl) VALUES (10 + '1a')", 1292},
	{"INSERT INTO tt (i, dbl) VALUES (1, 10 + '1')", 0},
	{"INSERT INTO tt (i, dbl) VALUES (1, 10 + ' 1')", 0},
	{"INSERT INTO tt (i, dbl) VALUES (1, 10 + '1 ')", 0},
	{"SELECT 10E+0 + 'a'", 0},
	{"INSERT INTO tt (i) VALUES (1 >> '')", 1292},
	{"INSERT INTO tt (i) VALUES (1 >> 'a')", 1292},
	{"UPDATE tt SET i = 1 WHERE (i IS NULL) >> ('')", 1292},
	{"UPDATE tt SET i = 1 WHERE (i IS NULL) >> ('' COLLATE utf8mb4_0900_ai_ci)", 1292},
	// a numeric column compared with a constant string holding no number
	{"UPDATE tt SET i = 1 WHERE i = '1invalid'", 1292},
	{"UPDATE tt SET i = 1 WHERE i = 'abc'", 1292},
	{"UPDATE tt SET i = 1 WHERE dbl > '1invalid'", 1292},
	{"UPDATE tt SET i = 1 WHERE y = 'abc'", 1292},
	{"DELETE FROM tt WHERE i = '1invalid'", 1292},
	{"INSERT INTO ttk (i) SELECT i FROM tt WHERE i = '1invalid'", 1292},
	{"UPDATE tt SET i = 1 WHERE i = ''", 0},
	{"UPDATE tt SET i = 1 WHERE i = ' 1'", 0},
	{"UPDATE tt SET i = 1 WHERE i = '1e1'", 0},
	{"UPDATE tt SET i = 1 WHERE i = '1.5'", 0},
	{"UPDATE tt SET i = 1 WHERE vc = '1invalid'", 0},
	{"UPDATE tt SET i = 1 WHERE i IN ('abc', '1')", 1292},
	{"SELECT 1 FROM tt WHERE i = '1invalid'", 0},
	// a DATE / DATETIME / TIMESTAMP value compared with a constant string that is no
	// datetime: the statement's error wherever the comparison sits, whatever the statement
	{"SELECT 1 FROM tt WHERE dt = 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE d = '2020-00-01'", 1525},
	{"SELECT 1 FROM tt WHERE d = '0000-00-00'", 1525},
	{"SELECT 1 FROM tt WHERE dt > 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE dt BETWEEN 'abc' AND '2020-01-01'", 0},
	{"SELECT 1 FROM tt WHERE dt IN ('abc', '2020-01-01')", 0},
	{"SELECT 1 FROM tt WHERE tm = 'abc'", 0},
	{"SELECT 1 FROM tt WHERE ts = '2020-01-01 00:00:00+99:00'", 1525},
	{"SELECT 1 FROM tt WHERE dt = '2020-01-32'", 1525},
	{"SELECT 1 FROM tt WHERE dt = 65", 0},
	{"SELECT 1 FROM tt WHERE 1 = 0 AND dt = 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE 'abc' = dt", 1525},
	{"SELECT 1 FROM tt WHERE dt <=> 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE dt != 'abc'", 1525},
	{"SELECT dt = 'abc' FROM tt", 1525},
	{"SELECT 1 FROM tt HAVING MAX(dt) = 'abc'", 1525},
	{"SELECT 1 FROM tt a JOIN ttk b ON a.dt = 'abc'", 1525},
	{"SELECT 1 FROM (SELECT 1 AS x FROM tt WHERE dt = 'abc') q", 1525},
	{"WITH q AS (SELECT 1 AS x FROM tt WHERE dt = 'abc') SELECT 1 FROM q", 1525},
	{"SELECT 1 FROM tt WHERE NOT (dt = 'abc')", 1525},
	{"SELECT 1 FROM tt WHERE dt = 'abc' OR 1 = 1", 1525},
	{"SELECT 1 FROM tt WHERE CASE WHEN 1 = 0 THEN dt = 'abc' ELSE 0 END", 1525},
	{"SELECT 1 FROM tt ORDER BY dt = 'abc'", 1525},
	{"SELECT 1 FROM tt GROUP BY dt = 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE EXISTS (SELECT 1 FROM ttk WHERE tt.dt = 'abc')", 1525},
	{"SELECT 1 FROM tt WHERE d >= 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE ts = 'abc'", 1525},
	{"SELECT 1 FROM tt WHERE ts = '2038-01-19 03:14:08+00:00'", 0},
	// in a strict write the same failure wears the store's message
	{"UPDATE tt SET i = 1 WHERE dt = 'abc'", 1292},
	{"UPDATE tt SET i = 1 WHERE d = '2020-00-01'", 1292},
	{"UPDATE tt SET i = 1 WHERE d = 'abc'", 1292},
	{"UPDATE tt SET i = 1 WHERE i = 1 AND dt = 'abc'", 1292},
	{"INSERT INTO ttk (i) SELECT i FROM tt WHERE dt = 'abc'", 1292},
	// a year 0 over a real month and day is out of the TIMESTAMP range
	{"INSERT INTO tt (i, ts) VALUES (1, '0000-10-31 15:30:00')", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, '0000-10-31 15:30:00')", 0},
	// TIMESTAMP() of a constant that is no datetime
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('0000-00-00 10:00:00'))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('2019-01-01 00:00:71'))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('abc'))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP(65))", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('2020-01-01'))", 0},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('2020-01-01', '25:00:00'))", 0},
	{"SELECT TIMESTAMP('18:16:35.025453')", 0},
	{"DELETE FROM tt WHERE TIMESTAMP('18:16:35.025453') IS NULL", 1292},
}

// convertCodeOnly are pinned by code alone: a TIMESTAMP literal out of the column's UTC
// range, whose server message spells the value converted to the session's time zone, which
// the checker does not know.
var convertCodeOnly = []struct {
	sql  string
	want int
}{
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'2038-01-19 03:14:08+00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'2038-01-19 03:14:07+00:00')", 0},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'1970-01-01 00:00:00+00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'1970-01-01 00:00:01+00:00')", 0},
	{"INSERT INTO tt (i, ts6) VALUES (1, TIMESTAMP'2038-01-19 03:14:07.999999+00:00')", 0},
	// the fraction rounds to the column's precision before the range check
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'1970-01-01 00:00:00.999999+00:00')", 0},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'1970-01-01 00:00:00.000001+00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'2038-01-19 03:14:07.999999+00:00')", 1292},
	{"INSERT INTO tt (i, ts6) VALUES (1, TIMESTAMP'1970-01-01 00:00:00.999999+00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'9999-12-31 23:59:59.999999+00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'2038-01-19 12:14:07+09:00')", 0},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'2040-01-01 00:00:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, DATE'2020-01-01')", 0},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP'9999-12-31 23:59:59+00:00')", 0},
	{"INSERT INTO tt (i, ts) VALUES (1, TIMESTAMP'0000-10-31 15:30:00')", 1292},
}

// convertInvalidDatesCases run under STRICT_ALL_TABLES,ALLOW_INVALID_DATES: a DATETIME
// takes an invalid calendar date, a TIMESTAMP still refuses it (a real moment), and a
// year 0 over a real month and day is out of the TIMESTAMP range.
var convertInvalidDatesCases = []struct {
	sql  string
	want int
}{
	{"INSERT INTO tt (i, ts) VALUES (1, '2004-02-30 15:30:04')", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, '2004-02-30 15:30:04')", 0},
	{"INSERT INTO tt (i, d) VALUES (1, '2004-02-30')", 0},
	{"INSERT INTO tt (i, ts) VALUES (1, '2004-0-31 15:30:00')", 1292},
	{"INSERT INTO tt (i, ts) VALUES (1, '0000-10-31 15:30:00')", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, '0000-10-31 15:30:00')", 0},
}

const invalidDatesHeader = "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'STRICT_ALL_TABLES,ALLOW_INVALID_DATES'\n"

// convertPerRow run their failing expression per row: the server (over empty tables)
// accepts them, the checker lists the violation 1292.
var convertPerRow = []string{
	"UPDATE tt SET d = CAST('2004-10-0' AS DATE)",
	"UPDATE tt SET vc = CAST('2004-10-0' AS DATE)",
	"UPDATE tt SET c3 = CAST(1000 AS CHAR(3))",
	"UPDATE tt SET i = CAST('abc' AS SIGNED)",
	"DELETE FROM tt WHERE d = CAST('2004-10-0' AS DATE)",
	"UPDATE tt SET i = 1 WHERE d = CAST('2004-10-0' AS DATE)",
	"INSERT INTO tt (i) SELECT CAST('abc' AS SIGNED) FROM tt",
	"INSERT INTO ttk (i) VALUES (1) ON DUPLICATE KEY UPDATE ts = CAST('2004-10-0' AS DATE)",
	"UPDATE tt SET ts = TIMESTAMP'2038-01-19 03:14:08+00:00'",
	"UPDATE tt SET i = i >> ''",
	"UPDATE tt SET i = 1 WHERE i BETWEEN 'abc' AND '9'",
}

// convertUnclaimed fail on the server but the checker stays silent: documented ceilings
// (a scalar subquery's constant, a TIME column's per-row conversion misses the eager
// escalation this file models).
var convertUnclaimed = []struct {
	sql  string
	want int
}{
	{"SELECT 1 FROM tt WHERE dt = (SELECT 'abc')", 1525},
}

// convertNonStrictCases run under sql_mode = 'NO_ENGINE_SUBSTITUTION' (no strict mode):
// the write escalations vanish, the comparison conversion stays.
var convertNonStrictCases = []struct {
	sql  string
	want int
}{
	{"INSERT INTO tt (d) VALUES (CAST('2004-10-0' AS DATE))", 0},
	{"INSERT INTO tt (c3) VALUES (CAST(1000 AS CHAR(3)))", 0},
	{"INSERT INTO tt (i) VALUES (CAST('abc' AS SIGNED))", 0},
	{"INSERT INTO tt (dbl) VALUES (10E+0 + 'a')", 0},
	{"UPDATE tt SET i = 1 WHERE i = '1invalid'", 0},
	{"INSERT INTO tt (ts) VALUES (TIMESTAMP'2038-01-19 03:14:08+00:00')", 0},
	{"INSERT INTO tt (y) VALUES (CAST(20201 AS YEAR))", 0},
	{"SELECT 1 FROM tt WHERE dt = 'abc'", 1525},
	{"UPDATE tt SET i = 1 WHERE dt = 'abc'", 1525},
	{"SELECT TIMESTAMP '2020-00-01 08:00:00.123456+00:00'", 1292},
	{"SELECT 1 FROM tt WHERE ts = '2020-00-01 00:00:00.123456+00:00'", 1292},
	{"SELECT DATE '2020-00-01'", 0},
	{"SELECT TIMESTAMP('0000-00-00 10:00:00')", 0},
	{"INSERT INTO tt (i, dt) VALUES (1, TIMESTAMP('0000-00-00 10:00:00'))", 0},
}

// convertZeroInDateCases run under the default mode minus NO_ZERO_IN_DATE (strict, zero
// month / day allowed): the displacement conversion still refuses a zero month or day.
var convertZeroInDateCases = []struct {
	sql  string
	want int
}{
	{"SELECT TIMESTAMP '2020-00-01 08:00:00.123456+00:00'", 1292},
	{"SELECT TIMESTAMP '2020-01-00 08:00:00.123456+00:00'", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, '2020-00-01 00:00:00.123456+00:00')", 1292},
	{"INSERT INTO tt (i, dt) VALUES (1, '2020-01-00 00:00:00.123456+00:00')", 1292},
	{"SELECT 1 FROM tt WHERE dt = '2020-00-01 00:00:00.123456+00:00'", 1292},
	{"SELECT 1 FROM tt WHERE dt = '2020-01-00 00:00:00.123456+00:00'", 1292},
	{"INSERT INTO tt (i, d) VALUES (1, CAST('2020-00-01' AS DATE))", 0},
	{"SELECT DATE '2020-00-01'", 0},
}

const nonStrictHeader = "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'NO_ENGINE_SUBSTITUTION'\n"
const zeroInDateHeader = "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'\n"

func TestConvertServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, convertSchema)
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
		conn.ExecContext(ctx, "DELETE FROM tt")
		conn.ExecContext(ctx, "DELETE FROM ttk")
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
	s := loadConvert(t, convertSchema)
	for _, c := range convertCases {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("%s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		if c.want != 1292 && c.want != 1525 {
			continue
		}
		_, err := Analyze(s, c.sql)
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want {
			t.Errorf("%s: analyzer %v, want %d", c.sql, err, c.want)
			continue
		}
		if e.Message != msg {
			t.Errorf("%s: analyzer says %q, the server %q", c.sql, e.Message, msg)
		}
	}
	for _, c := range convertCodeOnly {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("%s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		_, err := Analyze(s, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("%s: analyzer %v, want none", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want {
			t.Errorf("%s: analyzer %v, want %d", c.sql, err, c.want)
		}
	}
	for _, sql := range convertPerRow {
		if got, msg := run(sql); got != 0 && got >= 0 {
			t.Errorf("%s over no rows: the server says %d %s, want 0", sql, got, msg)
		}
	}
	for _, c := range convertUnclaimed {
		if got, msg := run(c.sql); got != c.want && got >= 0 {
			t.Errorf("%s: the server says %d %s, the note claims %d", c.sql, got, msg, c.want)
		}
	}
	// with a row, the per-row constants fail as the violations say
	if _, err := conn.ExecContext(ctx, "INSERT INTO tt (i, d, dt, tm, ts, y, c3, vc, dbl) VALUES (1, '2020-01-01', '2020-01-01 00:00:00', '10:00:00', '2020-01-01 00:00:00', 2020, 'ab', 'x', 1.5)"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"UPDATE tt SET d = CAST('2004-10-0' AS DATE)",
		"UPDATE tt SET c3 = CAST(1000 AS CHAR(3))",
		"UPDATE tt SET i = CAST('abc' AS SIGNED)",
		"UPDATE tt SET ts = TIMESTAMP'2038-01-19 03:14:08+00:00'",
		"UPDATE tt SET i = i >> ''",
		"UPDATE tt SET i = 1 WHERE i BETWEEN 'abc' AND '9'",
	} {
		if _, err := conn.ExecContext(ctx, sql); err == nil {
			t.Errorf("%s over a row: the server accepts, want 1292", sql)
		} else {
			var me *driver.MySQLError
			if errors.As(err, &me) && me.Number != 1292 {
				t.Errorf("%s over a row: the server says %d, want 1292", sql, me.Number)
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM tt"); err != nil {
		t.Fatal(err)
	}
	// the non-strict mode drops the write escalations
	if _, err := conn.ExecContext(ctx, "SET sql_mode = 'NO_ENGINE_SUBSTITUTION'"); err != nil {
		t.Fatal(err)
	}
	ns := loadConvert(t, strings.Replace(convertSchema, "-- sqlshape: mysql 8.4\n", nonStrictHeader, 1))
	for _, c := range convertNonStrictCases {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("non-strict %s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		_, err := Analyze(ns, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("non-strict %s: analyzer %v, want none", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want {
			t.Errorf("non-strict %s: analyzer %v, want %d", c.sql, err, c.want)
			continue
		}
		if e.Message != msg {
			t.Errorf("non-strict %s: analyzer says %q, the server %q", c.sql, e.Message, msg)
		}
	}
	// strict with ALLOW_INVALID_DATES: a TIMESTAMP ignores it
	if _, err := conn.ExecContext(ctx, "SET sql_mode = 'STRICT_ALL_TABLES,ALLOW_INVALID_DATES'"); err != nil {
		t.Fatal(err)
	}
	aid := loadConvert(t, strings.Replace(convertSchema, "-- sqlshape: mysql 8.4\n", invalidDatesHeader, 1))
	for _, c := range convertInvalidDatesCases {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("invalid-dates %s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		_, err := Analyze(aid, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("invalid-dates %s: analyzer %v, want none", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want {
			t.Errorf("invalid-dates %s: analyzer %v, want %d", c.sql, err, c.want)
			continue
		}
		if e.Message != msg {
			t.Errorf("invalid-dates %s: analyzer says %q, the server %q", c.sql, e.Message, msg)
		}
	}
	// strict with NO_ZERO_IN_DATE off: the displacement conversion still refuses zero parts
	if _, err := conn.ExecContext(ctx, "SET sql_mode = 'ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"); err != nil {
		t.Fatal(err)
	}
	zid := loadConvert(t, strings.Replace(convertSchema, "-- sqlshape: mysql 8.4\n", zeroInDateHeader, 1))
	for _, c := range convertZeroInDateCases {
		got, msg := run(c.sql)
		if got != c.want && got >= 0 {
			t.Errorf("zero-in-date %s: the server says %d %s, the checker %d", c.sql, got, msg, c.want)
		}
		_, err := Analyze(zid, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("zero-in-date %s: analyzer %v, want none", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want {
			t.Errorf("zero-in-date %s: analyzer %v, want %d", c.sql, err, c.want)
			continue
		}
		if e.Message != msg {
			t.Errorf("zero-in-date %s: analyzer says %q, the server %q", c.sql, e.Message, msg)
		}
	}
}

// TestConvert is the checker's side of the conversion cases without a server.
func TestConvert(t *testing.T) {
	s := loadConvert(t, convertSchema)
	for _, c := range convertCases {
		_, err := Analyze(s, c.sql)
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
		}
	}
	for _, sql := range convertPerRow {
		res, err := Analyze(s, sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if !hasKeyedViolation(res.Violations, code1292) {
			t.Errorf("%s: no violation 1292 listed", sql)
		}
	}
	for _, c := range convertUnclaimed {
		if _, err := Analyze(s, c.sql); err != nil {
			t.Errorf("%s: analyzer %v, want silence (a documented ceiling)", c.sql, err)
		}
	}
}

func loadConvert(t *testing.T, text string) *schema.Schema {
	t.Helper()
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func hasKeyedViolation(vs []Violation, code int) bool {
	for _, v := range vs {
		if v.Code == code && v.Key() == itoa(code) {
			return true
		}
	}
	return false
}
