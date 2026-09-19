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

// The constant function arguments the server refuses by value (funcval.go / strtodate.go),
// pinned against mysqld per sql_mode group: INET_ATON / INET6_ATON / UNHEX / STR_TO_DATE
// warn, so only a strict write fails (1411, or the tail's 1292); UUID_TO_BIN / BIN_TO_UUID
// (1411) and PERIOD_ADD / PERIOD_DIFF (1210) fail every statement; NAME_CONST, ESCAPE,
// NTILE, NTH_VALUE and MATCH / AGAINST are refused at resolution (1210 / 1382), rows or
// none. Each case's message is the analyzer's too.
func TestFuncValServer(t *testing.T) {
	const ddl = "CREATE TABLE t (a INT, b VARBINARY(16), c VARCHAR(40));\n" +
		"CREATE TABLE ft (x INT, txt TEXT, txt2 TEXT, FULLTEXT (txt));\n" +
		"CREATE TABLE ft2 (y INT, doc TEXT, FULLTEXT (doc));\n"
	type tc struct {
		sql  string
		want int
		msg  string
	}
	groups := map[string][]tc{
		"": { // the default strict mode
			// inet_aton: Item_func_inet_aton::val_int's parse
			{"INSERT INTO t (a) VALUES (INET_ATON('122.256'))", 1411, "Incorrect string value: ''122.256'' for function inet_aton"},
			{"INSERT INTO t (a) VALUES (INET_ATON('122.226.'))", 1411, "Incorrect string value: ''122.226.'' for function inet_aton"},
			{"INSERT INTO t (a) VALUES (INET_ATON('1.2.3.4.5'))", 1411, "Incorrect string value: ''1.2.3.4.5'' for function inet_aton"},
			{"INSERT INTO t (a) VALUES (INET_ATON(' 1.2.3.4'))", 1411, "Incorrect string value: '' 1.2.3.4'' for function inet_aton"},
			{"INSERT INTO t (a) VALUES (INET_ATON('192.168.0x8.2'))", 1411, "Incorrect string value: ''192.168.0x8.2'' for function inet_aton"},
			{"INSERT INTO t (a) VALUES (INET_ATON('1.2.3.4'))", 0, ""},
			{"INSERT INTO t (a) VALUES (INET_ATON('1.2.3'))", 0, ""},
			{"INSERT INTO t (a) VALUES (INET_ATON('01.2.3.4'))", 0, ""},
			{"INSERT INTO t (a) VALUES (INET_ATON(NULL))", 0, ""},
			{"SELECT INET_ATON('122.256')", 0, ""},
			{"INSERT IGNORE INTO t (a) VALUES (INET_ATON('122.256'))", 0, ""},
			// inet6_aton: str_to_ipv4 / str_to_ipv6
			{"INSERT INTO t (b) VALUES (INET6_ATON('1.0002.3.4'))", 1411, "Incorrect string value: ''1.0002.3.4'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1.2.255'))", 1411, "Incorrect string value: ''1.2.255'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1::2::3'))", 1411, "Incorrect string value: ''1::2::3'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('12345::'))", 1411, "Incorrect string value: ''12345::'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('::ffff:1.2.3.400'))", 1411, "Incorrect string value: ''::ffff:1.2.3.400'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1:2:3:4:5:6:7'))", 1411, "Incorrect string value: ''1:2:3:4:5:6:7'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1:2:3:4:5:6:7:8:9'))", 1411, "Incorrect string value: ''1:2:3:4:5:6:7:8:9'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON(':1::2'))", 1411, "Incorrect string value: '':1::2'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1:2:'))", 1411, "Incorrect string value: ''1:2:'' for function inet6_aton"},
			{"INSERT INTO t (b) VALUES (INET6_ATON('::1'))", 0, ""},
			{"INSERT INTO t (b) VALUES (INET6_ATON('::'))", 0, ""},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1:2:3:4:5:6:7:8'))", 0, ""},
			{"INSERT INTO t (b) VALUES (INET6_ATON('::ffff:1.2.3.4'))", 0, ""},
			{"INSERT INTO t (b) VALUES (INET6_ATON('fe80::1'))", 0, ""},
			{"INSERT INTO t (b) VALUES (INET6_ATON('1.2.3.4'))", 0, ""},
			// unhex
			{"INSERT INTO t (b) VALUES (UNHEX('GG'))", 1411, "Incorrect string value: ''GG'' for function unhex"},
			{"INSERT INTO t (b) VALUES (UNHEX('0A F'))", 1411, "Incorrect string value: ''0A F'' for function unhex"},
			{"INSERT INTO t (b) VALUES (UNHEX('0AF'))", 0, ""},
			{"INSERT INTO t (b) VALUES (UNHEX(123))", 0, ""},
			{"SELECT UNHEX('GG')", 0, ""},
			// str_to_date
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('a', '%Y'))", 1411, "Incorrect datetime value: 'a' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-32', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024-01-32' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-02-30', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024-02-30' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-00-01', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024-00-01' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('0000-00-00', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '0000-00-00' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024/01/01', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024/01/01' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('15:61:00', '%H:%i:%s'))", 1411, "Incorrect datetime value: '15:61:00' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('13', '%h'))", 1411, "Incorrect datetime value: '13' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('Feb 29 2023', '%b %d %Y'))", 1411, "Incorrect datetime value: 'Feb 29 2023' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('13:30:45 PM', '%r'))", 1411, "Incorrect time value: '13:30:45 PM' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('24:30:45', '%T'))", 1411, "Incorrect datetime value: '24:30:45' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('', '%Y'))", 1411, "Incorrect datetime value: '' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01', ''))", 1411, "Incorrect datetime value: '2024-01-01' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024', '%q'))", 1411, "Incorrect datetime value: '2024' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('99999', '%Y'))", 1411, "Incorrect datetime value: '99999' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('60', '%y'))", 1411, "Incorrect datetime value: '60' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('0', '%w'))", 1411, "Incorrect datetime value: '0' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('31.10.0000 15.30', '%d.%m.%Y %H.%i'))", 1411, "Incorrect datetime value: '31.10.0000 15.30' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('58 Mon 2024', '%u %a %Y'))", 1411, "Incorrect datetime value: '58 Mon 2024' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('5 Monday 24', '%V %W %X'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01 extra', '%Y-%m-%d'))", 1292, "Truncated incorrect date value: '2024-01-01 extra'"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01x', '%Y-%m-%d'))", 1292, "Truncated incorrect date value: '2024-01-01x'"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('23:30:45x', '%H:%i:%s'))", 1292, "Truncated incorrect time value: '23:30:45x'"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01', '%Y-%m-%dx'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE(' 2024-01-01', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024- 01-01', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-1-1', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('Feb 29 2024', '%b %d %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('February 29 2024', '%M %d %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('5 Monday 2024', '%u %W %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('5 Monday 2024', '%V %W %x'))", 1411, "Incorrect datetime value: '5 Monday 2024' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('5 Monday 2024', '%V %W %X'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('366 2024', '%j %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('366 2023', '%j %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('11:30:45 PM', '%r'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('23:30:45', '%T'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('123', '%f'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('3rd 1 2024', '%D %m %Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('abc-01-2024', '%@-%m-%Y'))", 1411, "Incorrect datetime value: 'abc-01-2024' for function str_to_date"},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE(NULL, '%Y'))", 0, ""},
			// uuid_to_bin / bin_to_uuid: every statement, every mode
			{"SELECT UUID_TO_BIN('zz')", 1411, "Incorrect string value: 'zz' for function uuid_to_bin"},
			{"INSERT IGNORE INTO t (b) VALUES (UUID_TO_BIN('zz'))", 1411, "Incorrect string value: 'zz' for function uuid_to_bin"},
			{"SELECT UUID_TO_BIN('{6ccd780cbaba102695645b8c656024db}')", 1411, "Incorrect string value: '{6ccd780cbaba102695645b8c656024db}' for function uuid_to_bin"},
			{"SELECT UUID_TO_BIN('6ccd780cb-aba-1026-9564-5b8c656024db')", 1411, "Incorrect string value: '6ccd780cb-aba-1026-9564-5b8c656024db' for function uuid_to_bin"},
			{"SELECT UUID_TO_BIN(' 6ccd780cbaba102695645b8c656024db')", 1411, "Incorrect string value: ' 6ccd780cbaba102695645b8c656024db' for function uuid_to_bin"},
			{"SELECT UUID_TO_BIN(123)", 1411, "Incorrect string value: '123' for function uuid_to_bin"},
			{"SELECT UUID_TO_BIN('6ccd780c-baba-1026-9564-5b8c656024db')", 0, ""},
			{"SELECT UUID_TO_BIN('{6ccd780c-baba-1026-9564-5b8c656024db}')", 0, ""},
			{"SELECT UUID_TO_BIN('6CCD780CBABA102695645B8C656024DB')", 0, ""},
			{"SELECT UUID_TO_BIN('6ccd780c-baba-1026-9564-5b8c656024db', 2)", 0, ""},
			{"SELECT UUID_TO_BIN(NULL)", 0, ""},
			{"SELECT BIN_TO_UUID(0x11)", 1411, "Incorrect string value: '\\x11' for function bin_to_uuid"},
			{"SELECT BIN_TO_UUID(0x6ccd780cbaba102695645b8c656024db)", 0, ""},
			{"SELECT BIN_TO_UUID('0123456789abcdef')", 0, ""},
			{"SELECT BIN_TO_UUID(UNHEX('7f9d04ae61b34468ac798ffcc984ab668'))", 1411, `Incorrect string value: '\x07\xF9\xD0J\xE6\x1B4F\x8A\xC7\x98\xFF\xCC\x98J\xB6h' for function bin_to_uuid`},
			{"SELECT BIN_TO_UUID(UNHEX('7f9d04ae61b34468ac798ffcc984ab66'))", 0, ""},
			// period_add / period_diff: every statement, every mode
			{"SELECT PERIOD_ADD(200013, 1)", 1210, "Incorrect arguments to period_add"},
			{"SELECT PERIOD_ADD(0, 1)", 1210, "Incorrect arguments to period_add"},
			{"SELECT PERIOD_ADD(13, 1)", 1210, "Incorrect arguments to period_add"},
			{"SELECT PERIOD_ADD('x', 1)", 1210, "Incorrect arguments to period_add"},
			{"SELECT PERIOD_DIFF(200013, 200001)", 1210, "Incorrect arguments to period_diff"},
			{"SELECT PERIOD_DIFF(200001, 0)", 1210, "Incorrect arguments to period_diff"},
			{"INSERT INTO t (a) VALUES (PERIOD_ADD(200013, 1))", 1210, "Incorrect arguments to period_add"},
			{"SELECT PERIOD_ADD(200012, 1)", 0, ""},
			{"SELECT PERIOD_ADD(101, 1)", 0, ""},
			{"SELECT PERIOD_ADD(1000001, 1)", 0, ""},
			{"SELECT PERIOD_ADD(200001.5, 1)", 0, ""},
			{"SELECT PERIOD_ADD('200001', 1)", 0, ""},
			{"SELECT PERIOD_ADD(NULL, 1)", 0, ""},
			// name_const: at resolution
			{"SELECT NAME_CONST(a, 1) FROM t", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST('n', a) FROM t", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST('n', 1+1)", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST('n', -(-1))", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST('n', TRUE)", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST('n', DATE'2024-01-01')", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NAME_CONST(NULL, 1)", 1382, "The 'NAME_CONST' syntax is reserved for purposes internal to the MySQL server"},
			{"SELECT NAME_CONST('n', -1)", 0, ""},
			{"SELECT NAME_CONST('n', 1.5)", 0, ""},
			{"SELECT NAME_CONST('n', NULL)", 0, ""},
			{"SELECT NAME_CONST('n', 0x41)", 0, ""},
			{"SELECT NAME_CONST(1, 1)", 0, ""},
			{"SELECT NAME_CONST('n', 'a' COLLATE utf8mb4_bin)", 0, ""},
			// escape: at resolution
			{"SELECT 'a' LIKE 'b' ESCAPE 'xx'", 1210, "Incorrect arguments to ESCAPE"},
			{"SELECT 'a' LIKE 'b' ESCAPE 33", 1210, "Incorrect arguments to ESCAPE"},
			{"SELECT 'a' LIKE 'b' ESCAPE a FROM t", 1210, "Incorrect arguments to ESCAPE"},
			{"SELECT 'a' LIKE 'b' ESCAPE 'x'", 0, ""},
			{"SELECT 'a' LIKE 'b' ESCAPE ''", 0, ""},
			{"SELECT 'a' LIKE 'b' ESCAPE 3", 0, ""},
			{"SELECT 'a' LIKE 'b' ESCAPE NULL", 0, ""},
			{"SELECT 'a' LIKE 'b' ESCAPE (SELECT 'x')", 0, ""},
			{"SELECT 'a%' LIKE 'a!%' ESCAPE '' || '!'", 0, ""},
			// window arguments: at resolution, rows or none
			{"SELECT NTILE(0) OVER () FROM t", 1210, "Incorrect arguments to ntile"},
			{"SELECT NTILE(0) OVER () FROM t WHERE FALSE", 1210, "Incorrect arguments to ntile"},
			{"SELECT NTILE(2) OVER () FROM t", 0, ""},
			{"SELECT NTH_VALUE(a, 0) OVER () FROM t", 1210, "Incorrect arguments to nth_value"},
			{"SELECT NTH_VALUE(a, -1) OVER () FROM t", 1210, "Incorrect arguments to nth_value"},
			{"SELECT NTH_VALUE(a, 1.5) OVER () FROM t", 1210, "Incorrect arguments to nth_value"},
			{"SELECT NTH_VALUE(a, 'x') OVER () FROM t", 1210, "Incorrect arguments to nth_value"},
			{"SELECT NTH_VALUE(a, 2) OVER () FROM t", 0, ""},
			// match / against: at resolution
			{"SELECT * FROM ft WHERE MATCH (txt) AGAINST (ft.txt)", 1210, "Incorrect arguments to AGAINST"},
			{"SELECT * FROM ft WHERE MATCH (txt) AGAINST (x)", 1210, "Incorrect arguments to AGAINST"},
			{"SELECT * FROM ft, ft2 WHERE MATCH (txt, doc) AGAINST ('x')", 1210, "Incorrect arguments to MATCH"},
			{"SELECT GROUP_CONCAT(txt) AS st FROM ft HAVING MATCH(st) AGAINST('x')", 1210, "Incorrect arguments to MATCH"},
			{"SELECT * FROM ft WHERE MATCH (txt) AGAINST ('x')", 0, ""},
			{"SELECT * FROM ft WHERE MATCH (ft.txt) AGAINST ('x')", 0, ""},
			{"SELECT * FROM ft WHERE MATCH (txt) AGAINST (concat('a','b'))", 0, ""},
			{"SELECT * FROM (SELECT txt FROM ft) d WHERE MATCH (d.txt) AGAINST ('x')", 0, ""},
			{"SELECT MATCH (txt) AGAINST ('x') FROM ft", 0, ""},
			{"SELECT * FROM ft HAVING MATCH (txt) AGAINST ('x')", 0, ""},
			{"SELECT x FROM ft GROUP BY x, MATCH(txt) AGAINST ('x')", 0, ""},
			{"SELECT txt FROM ft ORDER BY MATCH(txt) AGAINST ('x') DESC", 0, ""},
			// placement: a term the optimizer never reaches
			{"SELECT INET_ATON('122.256')", 0, ""},
			{"INSERT INTO t (a) VALUES (IF(0, INET_ATON('122.256'), 1))", 0, ""},
			{"INSERT INTO t (a) SELECT INET_ATON('122.256') FROM t WHERE FALSE", 0, ""},
			{"SELECT UUID_TO_BIN('zz') FROM t WHERE FALSE", 0, ""},
			{"SELECT IF(0, UUID_TO_BIN('zz'), 1)", 0, ""},
			{"SELECT PERIOD_ADD(200013, 1) FROM t WHERE FALSE", 0, ""},
		},
		"-- sqlshape: server sql_mode = NO_ENGINE_SUBSTITUTION\n": { // non-strict
			{"INSERT INTO t (a) VALUES (INET_ATON('122.256'))", 0, ""},
			{"INSERT INTO t (a) VALUES (STR_TO_DATE('a', '%Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-01 extra', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (b) VALUES (UNHEX('GG'))", 0, ""},
			{"INSERT INTO t (b) VALUES (UUID_TO_BIN('zz'))", 1411, "Incorrect string value: 'zz' for function uuid_to_bin"},
			{"SELECT PERIOD_ADD(200013, 1)", 1210, "Incorrect arguments to period_add"},
			{"SELECT NAME_CONST('n', 1+1)", 1210, "Incorrect arguments to NAME_CONST"},
			{"SELECT NTILE(0) OVER () FROM t", 1210, "Incorrect arguments to ntile"},
		},
		"-- sqlshape: server sql_mode = STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION\n": { // strict without the zero-date modes
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-00-01', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('0000-00-00', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024', '%Y'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-02-30', '%Y-%m-%d'))", 1411, "Incorrect datetime value: '2024-02-30' for function str_to_date"},
			{"INSERT INTO t (a) VALUES (INET_ATON('122.256'))", 1411, "Incorrect string value: ''122.256'' for function inet_aton"},
		},
		"-- sqlshape: server sql_mode = STRICT_TRANS_TABLES,ALLOW_INVALID_DATES,NO_ENGINE_SUBSTITUTION\n": {
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-02-30', '%Y-%m-%d'))", 0, ""},
			{"INSERT INTO t (c) VALUES (STR_TO_DATE('2024-01-32', '%Y-%m-%d'))", 0, ""},
		},
	}
	for header, cases := range groups {
		hdr := "-- sqlshape: mysql 8.4\n" + header
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		db, err := mysqltest.Start(ctx, hdr+ddl)
		if errors.Is(err, mysqltest.ErrNoServer) {
			cancel()
			t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
		}
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		conn := db.Conn()
		s, err := schema.Load(hdr + ddl)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for _, c := range cases {
			_, serr := conn.ExecContext(ctx, c.sql)
			var me *driver.MySQLError
			scode, smsg := 0, ""
			if errors.As(serr, &me) {
				scode, smsg = int(me.Number), me.Message
			} else if serr != nil {
				t.Errorf("%s: %v", c.sql, serr)
				continue
			}
			if scode != c.want || smsg != c.msg {
				t.Errorf("[%s] %s: the server says %d %q, the checker %d %q", strings.TrimSpace(header), c.sql, scode, smsg, c.want, c.msg)
				continue
			}
			_, aerr := Analyze(s, c.sql)
			if c.want == 0 {
				if aerr != nil {
					t.Errorf("[%s] %s: analyzer %v, the server accepts", strings.TrimSpace(header), c.sql, aerr)
				}
				continue
			}
			var e *Error
			if !errors.As(aerr, &e) || e.Code != c.want || e.Message != c.msg {
				t.Errorf("[%s] %s: analyzer %v, want %d %q", strings.TrimSpace(header), c.sql, aerr, c.want, c.msg)
			}
		}
		db.Close()
		cancel()
	}
}

// The per-row placements: a constant the statement evaluates per row is the violation
// keyed by its own number, and the server fails only over a row (fold.go's placement,
// applied to funcval.go's functions).
func TestFuncValPerRowServer(t *testing.T) {
	const ddl = "CREATE TABLE t (a INT, b VARBINARY(16), c VARCHAR(40));\nINSERT INTO t (a) VALUES (1);\n"
	cases := []struct {
		sql string
		key string
	}{
		{"UPDATE t SET a = INET_ATON('122.256')", "1411"},
		{"SELECT UUID_TO_BIN('zz') FROM t", "1411"},
		{"UPDATE t SET c = STR_TO_DATE('a', '%Y')", "1411"},
		{"SELECT PERIOD_ADD(200013, 1) FROM t", "1210"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n"+ddl)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()
	s, err := schema.Load("-- sqlshape: mysql 8.4\n" + ddl)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		_, serr := conn.ExecContext(ctx, c.sql)
		var me *driver.MySQLError
		if !errors.As(serr, &me) || itoa(int(me.Number)) != c.key {
			t.Errorf("%s: the server says %v, want the error %s over a row", c.sql, serr, c.key)
			continue
		}
		r, aerr := Analyze(s, c.sql)
		if aerr != nil {
			t.Errorf("%s: analyzer %v, want the violation %s", c.sql, aerr, c.key)
			continue
		}
		found := false
		for _, v := range r.Violations {
			if v.Constraint == c.key {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: analyzer violations %v, want %s", c.sql, r.Violations, c.key)
		}
	}
}
