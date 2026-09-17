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

const storeSchema = `-- sqlshape: mysql 8.4
CREATE TABLE lit (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  ti TINYINT,
  tu TINYINT UNSIGNED,
  sm SMALLINT,
  mi MEDIUMINT UNSIGNED,
  i INT,
  iu INT UNSIGNED,
  bi BIGINT,
  bu BIGINT UNSIGNED,
  y YEAR,
  de DECIMAL(5,2),
  du DECIMAL(5,2) UNSIGNED,
  fl FLOAT,
  d DATE,
  dt DATETIME,
  dt3 DATETIME(3),
  ts TIMESTAMP NULL,
  tm TIME,
  tm2 TIME(2),
  c3 CHAR(3),
  v5 VARCHAR(5),
  b2 BINARY(2),
  vb3 VARBINARY(3),
  e ENUM('small', 'Large', 'x-large'),
  st SET('a', 'b', 'c'),
  g GEOMETRY,
  p POINT
);
CREATE TABLE lit_myisam (ti TINYINT, d DATE) ENGINE=MyISAM;
`

// storeCases are literal stores the checker judges, each pinned against mysqld: want is the
// error number the server raises in strict mode, 0 when it stores the value.
var storeCases = []struct {
	sql  string
	want int
}{
	// integers: range, strings, rounding
	{"INSERT INTO lit (ti) VALUES (127)", 0},
	{"INSERT INTO lit (ti) VALUES (128)", 1264},
	{"INSERT INTO lit (ti) VALUES (-128)", 0},
	{"INSERT INTO lit (ti) VALUES (-129)", 1264},
	{"INSERT INTO lit (tu) VALUES (255)", 0},
	{"INSERT INTO lit (tu) VALUES (256)", 1264},
	{"INSERT INTO lit (tu) VALUES (-1)", 1264},
	{"INSERT INTO lit (tu) VALUES (-1.1E-3)", 0},
	{"INSERT INTO lit (tu) VALUES ('-1.2E-3')", 0},
	{"INSERT INTO lit (tu) VALUES (-0.6)", 1264},
	{"INSERT INTO lit (ti) VALUES (127.4)", 0},
	{"INSERT INTO lit (ti) VALUES (127.5)", 1264},
	{"INSERT INTO lit (ti) VALUES (1.5E2)", 1264},
	{"INSERT INTO lit (sm) VALUES (32767), (32768)", 1264},
	{"INSERT INTO lit (mi) VALUES (16777215)", 0},
	{"INSERT INTO lit (mi) VALUES (16777216)", 1264},
	{"INSERT INTO lit (i) VALUES (2147483647)", 0},
	{"INSERT INTO lit (i) VALUES (2147483648)", 1264},
	{"INSERT INTO lit (i) VALUES (-2147483649.0)", 1264},
	{"INSERT INTO lit (iu) VALUES (4294967295)", 0},
	{"INSERT INTO lit (iu) VALUES (4294967296)", 1264},
	{"INSERT INTO lit (bi) VALUES (9223372036854775807)", 0},
	{"INSERT INTO lit (bi) VALUES (9223372036854775808)", 1264},
	{"INSERT INTO lit (bi) VALUES (-9223372036854775808)", 0},
	{"INSERT INTO lit (bi) VALUES (-9223372036854775809)", 1264},
	{"INSERT INTO lit (bu) VALUES (18446744073709551615)", 0},
	{"INSERT INTO lit (bu) VALUES (18446744073709551616)", 1264},
	{"INSERT INTO lit (ti) VALUES ('127')", 0},
	{"INSERT INTO lit (ti) VALUES ('128')", 1264},
	{"INSERT INTO lit (ti) VALUES (' 12 ')", 0},
	{"INSERT INTO lit (ti) VALUES ('12a')", 1265},
	{"INSERT INTO lit (ti) VALUES ('abc')", 1366},
	{"INSERT INTO lit (ti) VALUES ('')", 1366},
	{"INSERT INTO lit (ti) VALUES ('1e2')", 0},
	{"INSERT INTO lit (ti) VALUES ('1.5')", 0},
	{"INSERT INTO lit (ti) VALUES (TRUE)", 0},
	// YEAR
	{"INSERT INTO lit (y) VALUES (2155)", 0},
	{"INSERT INTO lit (y) VALUES (2156)", 1264},
	{"INSERT INTO lit (y) VALUES (1900)", 1264},
	{"INSERT INTO lit (y) VALUES (99)", 0},
	{"INSERT INTO lit (y) VALUES (0)", 0},
	{"INSERT INTO lit (y) VALUES (-1)", 1264},
	{"INSERT INTO lit (y) VALUES ('2155')", 0},
	{"INSERT INTO lit (y) VALUES ('2156')", 1264},
	// DECIMAL / FLOAT
	{"INSERT INTO lit (de) VALUES (999.99)", 0},
	{"INSERT INTO lit (de) VALUES (999.999)", 1264},
	{"INSERT INTO lit (de) VALUES (999.994)", 0},
	{"INSERT INTO lit (de) VALUES (1000)", 1264},
	{"INSERT INTO lit (de) VALUES (-999.99)", 0},
	{"INSERT INTO lit (du) VALUES (-1)", 1264},
	{"INSERT INTO lit (de) VALUES ('1000')", 1264},
	{"INSERT INTO lit (de) VALUES ('abc')", 1366},
	{"INSERT INTO lit (fl) VALUES (3.4e38)", 0},
	{"INSERT INTO lit (fl) VALUES (3.5e38)", 1264},
	// DATE / DATETIME / TIMESTAMP strings
	{"INSERT INTO lit (d) VALUES ('2004-02-29')", 0},
	{"INSERT INTO lit (d) VALUES ('2005-02-29')", 1292},
	{"INSERT INTO lit (d) VALUES ('2004-13-15')", 1292},
	{"INSERT INTO lit (d) VALUES ('2004-10-32')", 1292},
	{"INSERT INTO lit (d) VALUES ('2004-0-31')", 1292},
	{"INSERT INTO lit (d) VALUES ('0000-00-00')", 1292},
	{"INSERT INTO lit (d) VALUES ('2004-01-04 10:00:00')", 0},
	{"INSERT INTO lit (d) VALUES ('2004-01-04 25:00:00')", 1292},
	{"INSERT INTO lit (d) VALUES ('20040104')", 0},
	{"INSERT INTO lit (d) VALUES ('040104')", 0},
	{"INSERT INTO lit (d) VALUES ('2004/01/04')", 0},
	{"INSERT INTO lit (d) VALUES ('2004-01-04x')", 1292},
	{"INSERT INTO lit (d) VALUES ('abc')", 1292},
	{"INSERT INTO lit (d) VALUES ('')", 1292},
	{"INSERT INTO lit (d) VALUES (1)", 1292},
	{"INSERT INTO lit (d) VALUES (20040104)", 0},
	{"INSERT INTO lit (d) VALUES (0)", 1292},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:65:00')", 1292},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:60')", 1292},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:59')", 0},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29T15:59:59')", 0},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:59.5')", 0},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:59.1234567')", 0},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:59+01:00')", 0},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29 15:59:59+15:00')", 1292},
	{"INSERT INTO lit (dt) VALUES ('0000-00-00 00:00:00')", 1292},
	{"INSERT INTO lit (dt) VALUES ('2004-02-29')", 0},
	{"INSERT INTO lit (dt) VALUES (20040229155959)", 0},
	{"INSERT INTO lit (dt) VALUES (20040229156000)", 1292},
	{"INSERT INTO lit (dt) VALUES (-1)", 1292},
	{"INSERT INTO lit (dt3) VALUES ('2004-02-29 15:59:59.9999')", 0},
	{"INSERT INTO lit (ts) VALUES ('2038-01-19 03:14:08+00:00')", 1292},
	{"INSERT INTO lit (ts) VALUES ('2039-01-01 00:00:00')", 1292},
	{"INSERT INTO lit (ts) VALUES ('1969-12-31 00:00:00')", 1292},
	{"INSERT INTO lit (ts) VALUES ('2004-02-29 15:59:59')", 0},
	{"INSERT INTO lit (ts) VALUES ('2004-02-00 15:59:59')", 1292},
	// TIME
	{"INSERT INTO lit (tm) VALUES ('838:59:59')", 0},
	{"INSERT INTO lit (tm) VALUES ('839:00:00')", 1292},
	{"INSERT INTO lit (tm) VALUES ('-838:59:59.9999999')", 1292},
	{"INSERT INTO lit (tm) VALUES ('10:70:00')", 1292},
	{"INSERT INTO lit (tm) VALUES ('10:00:60')", 1292},
	{"INSERT INTO lit (tm) VALUES ('1 10:00:00')", 0},
	{"INSERT INTO lit (tm) VALUES ('100000')", 0},
	{"INSERT INTO lit (tm) VALUES ('12:34')", 0},
	{"INSERT INTO lit (tm) VALUES ('2004-02-29 15:59:59')", 0},
	{"INSERT INTO lit (tm) VALUES ('abc')", 1292},
	{"INSERT INTO lit (tm) VALUES ('10:00:00x')", 1292},
	{"INSERT INTO lit (tm) VALUES (8385959)", 0},
	{"INSERT INTO lit (tm) VALUES (8385960)", 1292},
	{"INSERT INTO lit (tm) VALUES (-8385959)", 0},
	{"INSERT INTO lit (tm) VALUES (20040229155959)", 0},
	{"INSERT INTO lit (tm2) VALUES ('10:00:00.999')", 0},
	// strings
	{"INSERT INTO lit (c3) VALUES ('abc')", 0},
	{"INSERT INTO lit (c3) VALUES ('abcd')", 1406},
	{"INSERT INTO lit (c3) VALUES ('abc   ')", 0},
	{"INSERT INTO lit (c3) VALUES ('日本語')", 0},
	{"INSERT INTO lit (v5) VALUES ('abcdef')", 1406},
	{"INSERT INTO lit (v5) VALUES ('日本語日本')", 0},
	{"INSERT INTO lit (b2) VALUES ('ab')", 0},
	{"INSERT INTO lit (b2) VALUES ('abc')", 1406},
	{"INSERT INTO lit (vb3) VALUES ('日')", 0},
	{"INSERT INTO lit (vb3) VALUES ('日本')", 1406},
	// ENUM / SET
	{"INSERT INTO lit (e) VALUES ('small')", 0},
	{"INSERT INTO lit (e) VALUES ('LARGE')", 0},
	{"INSERT INTO lit (e) VALUES ('medium')", 1265},
	{"INSERT INTO lit (e) VALUES ('small ')", 0},
	{"INSERT INTO lit (e) VALUES ('3')", 0},
	{"INSERT INTO lit (e) VALUES ('4')", 1265},
	{"INSERT INTO lit (e) VALUES (3)", 0},
	{"INSERT INTO lit (e) VALUES (4)", 1265},
	{"INSERT INTO lit (e) VALUES (0)", 1265},
	{"INSERT INTO lit (e) VALUES ('')", 1265},
	{"INSERT INTO lit (st) VALUES ('a,c')", 0},
	{"INSERT INTO lit (st) VALUES ('a,d')", 1265},
	{"INSERT INTO lit (st) VALUES ('')", 0},
	{"INSERT INTO lit (st) VALUES ('A')", 0},
	// geometry
	{"INSERT INTO lit (g) VALUES ('Garbage')", 1416},
	{"INSERT INTO lit (g) VALUES (1)", 1416},
	{"INSERT INTO lit (p) VALUES (1.11)", 1416},
	{"INSERT INTO lit (p) VALUES (ST_GeomFromText('POINT(1 1)'))", 0},
	// the statement shapes: UPDATE, ON DUPLICATE KEY UPDATE, REPLACE, the row number
	{"UPDATE lit SET ti = 128 WHERE id = 1", 1264},
	{"UPDATE lit SET d = '2004-13-15' WHERE id = 1", 1292},
	{"INSERT INTO lit (id, ti) VALUES (1, 1) ON DUPLICATE KEY UPDATE ti = 128", 1264},
	{"REPLACE INTO lit (ti) VALUES (128)", 1264},
	{"INSERT INTO lit (ti) VALUES (1), (128)", 1264},
	// STRICT_TRANS_TABLES alone: a nontransactional table is strict for the first row only
	{"INSERT INTO lit_myisam (ti) VALUES (128)", 1264},
	{"INSERT INTO lit_myisam (d) VALUES ('2004-13-15')", 1292},
	{"INSERT INTO lit_myisam (ti) VALUES (1), (128)", 0},
	{"INSERT INTO lit_myisam (d) VALUES ('2004-01-01'), ('2004-13-15')", 0},
	// what is not judged: IGNORE, a NULL (an expression such as 100 + 28 is not read either,
	// though the server folds it and fails the same way)
	{"INSERT IGNORE INTO lit (ti) VALUES (128)", 0},
	{"UPDATE IGNORE lit SET ti = 128 WHERE id = 1", 0},
	{"INSERT INTO lit (ti) VALUES (NULL)", 0},
}

// TestLiteralStore checks the checker's verdict on each store case, without a server.
func TestLiteralStore(t *testing.T) {
	s, err := schema.Load(storeSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range storeCases {
		_, err := Analyze(s, c.sql)
		if got := errCode(err); got != c.want {
			t.Errorf("%s: checker says %d (%v), want %d", c.sql, got, err, c.want)
		}
	}
}

// TestLiteralStoreServer runs each store case on mysqld (strict mode, the default) and
// requires the server's error number to be the checker's, or the value to be stored when
// the checker says nothing. Skipped without a mysqld on PATH (nix-shell -p mysql84).
func TestLiteralStoreServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, storeSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO lit (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range storeCases {
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
			msg := ""
			if err != nil {
				msg = strings.TrimSpace(err.Error())
			}
			t.Errorf("%s: the server says %d (%s), the checker %d", c.sql, got, msg, c.want)
		}
	}
	// outside strict mode the same values are stored adjusted, with a warning
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = ''"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"INSERT INTO lit (ti) VALUES (128)", "INSERT INTO lit (d) VALUES ('2004-13-15')", "INSERT INTO lit (c3) VALUES ('abcd')", "INSERT INTO lit (e) VALUES ('medium')"} {
		if _, err := conn.ExecContext(ctx, sql); err != nil {
			t.Errorf("%s without strict mode: %v", sql, err)
		}
	}
	lenient, err := schema.Load(strings.Replace(storeSchema, "-- sqlshape: mysql 8.4\n", "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = ''\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range storeCases {
		if c.want == 0 {
			continue
		}
		if _, err := Analyze(lenient, c.sql); err != nil {
			t.Errorf("%s: the checker still says %v without strict mode", c.sql, err)
		}
	}
}
