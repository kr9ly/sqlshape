package analyze

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/cardinality"
)

const testSchema = `-- sqlshape: mysql 8.4
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(255),
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  secret VARCHAR(10) INVISIBLE
);
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note TEXT,
  FOREIGN KEY (user_id) REFERENCES users (id)
);
CREATE VIEW v_users AS SELECT id, name FROM users;
CREATE VIEW v_stats AS SELECT user_id, COUNT(*) AS n FROM orders GROUP BY user_id;
`

func load(t *testing.T) *schema.Schema {
	s, err := schema.Load(testSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

// col spells a result column as "name type[?]" (? = nullable), "name ?" when untyped.
func col(c Column) string {
	s := c.Name + " "
	if c.Known {
		s += c.Type.String()
	} else {
		s += "?"
	}
	if c.Nullable {
		s += " null"
	}
	return s
}

func param(p Param) string {
	if !p.Known {
		return "?"
	}
	return p.Type.String()
}

type analyzeCase struct {
	sql     string
	columns []string
	params  []string
}

// analyzeCases are the statements the analyzer types; TestOracle checks the same
// statements against a real server.
var analyzeCases = []analyzeCase{
	{"SELECT id, name, email FROM users", []string{"id bigint unsigned", "name varchar(100)", "email varchar(255) null"}, nil},
	{"SELECT * FROM users", []string{"id bigint unsigned", "name varchar(100)", "email varchar(255) null", "created_at datetime(6)"}, nil},
	{"SELECT u.id AS uid, o.total, o.note FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1", []string{"uid bigint unsigned", "total decimal(10,2)", "note text null"}, []string{"bigint unsigned"}},
	{"SELECT u.id, o.total FROM users u LEFT JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned", "total decimal(10,2) null"}, nil},
	{"SELECT u.id, o.total FROM users u RIGHT JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned null", "total decimal(10,2)"}, nil},
	{"SELECT o.* FROM users u JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned", "user_id bigint unsigned", "total decimal(10,2)", "note text null"}, nil},
	{"SELECT count(*), count(email) c, 1, 'x', 1.5, NULL FROM users", []string{"count(*) bigint", "c bigint", "1 bigint(1)", "x varchar(1)", "1.5 decimal(2,1)", "NULL null null"}, nil},
	{"SELECT id = 1, id > 1 AND name = 'x', NOT id, id IS NULL, email IS NOT NULL, TRUE FROM users", []string{"id = 1 bigint(1)", "id > 1 AND name = 'x' bigint(1)", "NOT id bigint(1)", "id IS NULL bigint(1)", "email IS NOT NULL bigint(1)", "TRUE bigint(1)"}, nil},
	{"SELECT id FROM users WHERE name = $1 AND email = $2 LIMIT $3 OFFSET $4", []string{"id bigint unsigned"}, []string{"varchar(100)", "varchar(255)", "bigint unsigned", "bigint unsigned"}},
	{"SELECT id FROM users WHERE $1 = name", []string{"id bigint unsigned"}, []string{"varchar(100)"}},
	{"SELECT id FROM users WHERE id IN ($1, $2) AND name LIKE $3 AND created_at BETWEEN $4 AND $5", []string{"id bigint unsigned"}, []string{"bigint unsigned", "bigint unsigned", "varchar(100)", "datetime(6)", "datetime(6)"}},
	{"SELECT id FROM users WHERE id = $1 AND name = $1", []string{"id bigint unsigned"}, []string{"bigint unsigned"}},
	{"SELECT COALESCE(email, name) FROM users WHERE id = $1", []string{"COALESCE(email, name) varchar"}, []string{"bigint unsigned"}},
	// arithmetic: Item_num_op promotion, unsigned propagation, division
	{"SELECT u.id + 1, u.id - 1, total * 2, total + 1.5, u.id / 2, u.id DIV 2, u.id % 3, -u.id, total / 0.5, name + 1, created_at + 0 FROM users u JOIN orders o ON o.user_id = u.id", []string{"u.id + 1 bigint unsigned", "u.id - 1 bigint unsigned", "total * 2 decimal", "total + 1.5 decimal", "u.id / 2 decimal null", "u.id DIV 2 bigint unsigned null", "u.id % 3 bigint unsigned null", "-u.id bigint", "total / 0.5 decimal null", "name + 1 double", "created_at + 0 decimal"}, nil},
	// branches: aggregate_type and its nullability rules
	{"SELECT IF(u.id > 1, name, email), IF(u.id > 1, u.id, total), IF(u.id > 1, name, NULL), IFNULL(email, 'x'), IFNULL(email, NULL) FROM users u JOIN orders o ON o.user_id = u.id", []string{"IF(u.id > 1, name, email) varchar null", "IF(u.id > 1, u.id, total) decimal", "IF(u.id > 1, name, NULL) varchar null", "IFNULL(email, 'x') varchar", "IFNULL(email, NULL) varchar null"}, nil},
	{"SELECT CASE WHEN u.id = 1 THEN 'a' WHEN u.id = 2 THEN 'b' END, CASE u.id WHEN 1 THEN 1 ELSE 2.5 END, CASE WHEN u.id = 1 THEN u.id ELSE -1 END, GREATEST(u.id, 2), LEAST(total, 1), NULLIF(name, 'x') FROM users u JOIN orders o ON o.user_id = u.id", []string{"CASE WHEN u.id = 1 THEN 'a' WHEN u.id = 2 THEN 'b' END varchar null", "CASE u.id WHEN 1 THEN 1 ELSE 2.5 END decimal", "CASE WHEN u.id = 1 THEN u.id ELSE -1 END decimal", "GREATEST(u.id, 2) decimal", "LEAST(total, 1) decimal", "NULLIF(name, 'x') varchar(100) null"}, nil},
	// registry functions by family and facts
	{"SELECT LENGTH(name), UPPER(name), CONCAT(name, email), ABS(total), ROUND(total), ROUND(total, 1), FLOOR(total), CEIL(u.id), SQRT(u.id), NOW(), CURDATE(), CURTIME(), UNIX_TIMESTAMP(), YEAR(created_at), DATE(created_at), DATEDIFF(created_at, NOW()), DATE_FORMAT(created_at, '%Y'), JSON_EXTRACT('{}', '$'), MD5(name), UUID(), LAST_INSERT_ID(), HEX(u.id), CONV(u.id, 10, 16), ISNULL(email), CHAR_LENGTH(email) FROM users u JOIN orders o ON o.user_id = u.id", []string{"LENGTH(name) bigint", "UPPER(name) varchar null", "CONCAT(name, email) varchar null", "ABS(total) decimal", "ROUND(total) decimal(10,0)", "ROUND(total, 1) decimal", "FLOOR(total) bigint", "CEIL(u.id) bigint unsigned", "SQRT(u.id) double null", "NOW() datetime", "CURDATE() date", "CURTIME() time", "UNIX_TIMESTAMP() bigint", "YEAR(created_at) year null", "DATE(created_at) date null", "DATEDIFF(created_at, NOW()) bigint null", "DATE_FORMAT(created_at, '%Y') varchar null", "JSON_EXTRACT('{}', '$') json null", "MD5(name) varchar null", "UUID() varchar null", "LAST_INSERT_ID() bigint unsigned", "HEX(u.id) varchar null", "CONV(u.id, 10, 16) varchar null", "ISNULL(email) bigint(1)", "CHAR_LENGTH(email) bigint null"}, nil},
	// aggregates
	{"SELECT SUM(total), SUM(u.id), AVG(total), MIN(name), MAX(created_at), COUNT(DISTINCT email), GROUP_CONCAT(name), BIT_OR(u.id), STD(total), SUM(name) FROM users u JOIN orders o ON o.user_id = u.id", []string{"SUM(total) decimal null", "SUM(u.id) decimal null", "AVG(total) decimal null", "MIN(name) varchar(100) null", "MAX(created_at) datetime(6) null", "COUNT(DISTINCT email) bigint", "GROUP_CONCAT(name) text null", "BIT_OR(u.id) bigint unsigned", "STD(total) double null", "SUM(name) double null"}, nil},
	// casts
	{"SELECT CAST(u.id AS SIGNED), CAST(name AS UNSIGNED), CAST(total AS CHAR(5)), CAST(name AS DECIMAL(10,2)), CAST(name AS DECIMAL), CAST(name AS DATE), CAST(name AS DATETIME(3)), CAST(name AS JSON), CONVERT(name, BINARY), BINARY name, CAST(u.id AS DOUBLE) FROM users u JOIN orders o ON o.user_id = u.id", []string{"CAST(u.id AS SIGNED) bigint", "CAST(name AS UNSIGNED) bigint unsigned", "CAST(total AS CHAR(5)) varchar(5) null", "CAST(name AS DECIMAL(10,2)) decimal(10,2)", "CAST(name AS DECIMAL) decimal(10,0)", "CAST(name AS DATE) date null", "CAST(name AS DATETIME(3)) datetime(3) null", "CAST(name AS JSON) json null", "CONVERT(name, BINARY) varbinary(100) null", "BINARY name varbinary(100) null", "CAST(u.id AS DOUBLE) double"}, nil},
	// placeholders typed through function facts and branch aggregation
	{"SELECT id FROM users WHERE name = CONCAT($1, 'x') AND id > IF($2, 1, 2) AND created_at > DATE_ADD($3, INTERVAL 1 DAY) AND email = COALESCE($4, name) AND id IN ($5, id + $6) AND LENGTH($7) > 1 AND name LIKE CONCAT('%', $8, '%') LIMIT $9", []string{"id bigint unsigned"}, []string{"varchar", "bigint", "?", "varchar(100)", "bigint unsigned", "bigint unsigned", "varchar", "varchar", "bigint unsigned"}},
	{"SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1 + 1 AND total = ROUND($2, 2) AND u.id DIV $3 = 1 AND $4 = CASE WHEN u.id = 1 THEN 'a' ELSE 'b' END", []string{"id bigint unsigned"}, []string{"bigint", "decimal", "bigint", "varchar"}}, // $1 takes the other operand's type, the literal's
	// subqueries: entered, with the enclosing query's names in reach
	{"SELECT id, EXISTS (SELECT 1 FROM orders WHERE total > 1), id IN (SELECT user_id FROM orders), (SELECT 1) FROM users", []string{"id bigint unsigned", "EXISTS (SELECT 1 FROM orders WHERE total > 1) bigint(1)", "id IN (SELECT user_id FROM orders) bigint(1) null", "(SELECT 1) bigint(1)"}, nil},
	{"SELECT (SELECT COUNT(*) FROM orders o WHERE o.user_id = u.id) AS c, (SELECT MAX(total) FROM orders o WHERE o.user_id = u.id) AS m FROM users u", []string{"c bigint null", "m decimal(10,2) null"}, nil},
	{"SELECT u.id FROM users u WHERE u.id IN (SELECT user_id FROM orders WHERE total > $1) AND EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id AND o.note = $2)", []string{"id bigint unsigned"}, []string{"decimal(10,2)", "text"}},
	{"SELECT id FROM users WHERE $1 IN (SELECT user_id FROM orders) AND id = ANY (SELECT user_id FROM orders) AND id > ALL (SELECT user_id FROM orders WHERE note IS NULL)", []string{"id bigint unsigned"}, []string{"bigint unsigned"}},
	{"SELECT id, email IN (SELECT note FROM orders), (1, 2) IN (SELECT id, user_id FROM orders) FROM users", []string{"id bigint unsigned", "email IN (SELECT note FROM orders) bigint(1) null", "(1, 2) IN (SELECT id, user_id FROM orders) bigint(1) null"}, nil},
	// derived tables, views, common table expressions
	{"SELECT d.id, d.n, d.c FROM (SELECT id, name AS n, COUNT(*) c FROM users GROUP BY id, name) AS d WHERE d.id = $1", []string{"id bigint unsigned", "n varchar(100)", "c bigint"}, []string{"bigint unsigned"}},
	{"SELECT d.* FROM (SELECT u.id, o.total FROM users u LEFT JOIN orders o ON o.user_id = u.id) d", []string{"id bigint unsigned", "total decimal(10,2) null"}, nil},
	{"SELECT x, y FROM (SELECT 1, 'a') AS d(x, y)", []string{"x int(1)", "y varchar(1)"}, nil}, // materialized: a small integer literal becomes an int
	{"SELECT d.b, d.c, d.i FROM (SELECT id = 1 AS b, COUNT(*) AS c, id AS i FROM users GROUP BY id) d", []string{"b int(1)", "c bigint", "i bigint unsigned"}, nil},
	{"SELECT d.b FROM (SELECT id = 1 AS b FROM users) d", []string{"b bigint(1)"}, nil}, // merged: the item's own type
	{"SELECT u.id, l.total FROM users u JOIN LATERAL (SELECT o.total FROM orders o WHERE o.user_id = u.id LIMIT 1) l ON TRUE", []string{"id bigint unsigned", "total decimal(10,2)"}, nil},
	{"SELECT id, name FROM v_users WHERE id = $1", []string{"id bigint unsigned", "name varchar(100)"}, []string{"bigint unsigned"}},
	{"SELECT v.*, o.total FROM v_users v LEFT JOIN orders o ON o.user_id = v.id", []string{"id bigint unsigned", "name varchar(100)", "total decimal(10,2) null"}, nil},
	{"WITH t AS (SELECT id, total FROM orders WHERE total > $1) SELECT t.id, t.total FROM t", []string{"id bigint unsigned", "total decimal(10,2)"}, []string{"decimal(10,2)"}},
	{"WITH a AS (SELECT id FROM users), b AS (SELECT a.id, o.total FROM a JOIN orders o ON o.user_id = a.id) SELECT * FROM b", []string{"id bigint unsigned", "total decimal(10,2)"}, nil},
	{"WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 5) SELECT n FROM t", []string{"n bigint(1) null"}, nil}, // a UNION's types merge through Item_type_holder, and a recursive CTE is nullable throughout
	{"SELECT (SELECT 1), (SELECT id FROM users LIMIT 1), (SELECT 1 UNION SELECT id FROM users), (SELECT 1 WHERE (SELECT COUNT(*) FROM orders) > 0) FROM users", []string{"(SELECT 1) bigint(1)", "(SELECT id FROM users LIMIT 1) bigint unsigned null", "(SELECT 1 UNION SELECT id FROM users) decimal", "(SELECT 1 WHERE (SELECT COUNT(*) FROM orders) > 0) bigint(1) null"}, nil},
	{"UPDATE v_users SET name = $1 WHERE id = $2", nil, []string{"varchar(100)", "bigint unsigned"}},
	{"WITH t AS (SELECT id FROM users) SELECT id FROM users WHERE id IN (SELECT id FROM t)", []string{"id bigint unsigned"}, nil},
	// ORDER BY / GROUP BY / HAVING: positions, select-list aliases, table columns
	{"SELECT id AS n, COUNT(*) c FROM users GROUP BY n HAVING c > $1 AND n > 0 ORDER BY 2 DESC, n, users.id", []string{"n bigint unsigned", "c bigint"}, []string{"bigint"}},
	{"SELECT id, name FROM users u GROUP BY id, name HAVING name = $1 ORDER BY u.name, id DESC LIMIT $2", []string{"id bigint unsigned", "name varchar(100)"}, []string{"varchar(100)", "bigint unsigned"}},
	{"SELECT id AS name, COUNT(*) c FROM users GROUP BY name, id", []string{"name bigint unsigned", "c bigint"}, nil}, // GROUP BY name is the table column, not the alias
	{"(SELECT id FROM users) UNION (SELECT user_id FROM orders) ORDER BY 1 LIMIT 3", []string{"id bigint unsigned"}, nil},
	{"SELECT u.id, o.total FROM users u JOIN orders o ON o.user_id = u.id ORDER BY o.total DESC, u.created_at", []string{"id bigint unsigned", "total decimal(10,2)"}, nil},
	// set operations: the first operand names the columns, the types merge
	{"SELECT id, name FROM users UNION ALL SELECT id, note FROM orders", []string{"id bigint unsigned", "name text null"}, nil},
	{"SELECT id FROM users UNION SELECT total FROM orders", []string{"id decimal"}, nil},
	{"(SELECT id FROM users) UNION (SELECT user_id FROM orders) ORDER BY id LIMIT $1", []string{"id bigint unsigned"}, []string{"bigint unsigned"}},
	{"SELECT id FROM users EXCEPT SELECT user_id FROM orders", []string{"id bigint unsigned"}, nil},
	{"SELECT id FROM users INTERSECT SELECT user_id FROM orders WHERE total > $1", []string{"id bigint unsigned"}, []string{"decimal(10,2)"}},
	{"SELECT 1 AS a UNION SELECT 'x' UNION SELECT NULL", []string{"a varchar null"}, nil},
	// INSERT ... SELECT: the query feeds the targets in order
	{"INSERT INTO orders (id, user_id, total) SELECT id, id, $1 FROM users WHERE name = $2", nil, []string{"decimal(10,2)", "varchar(100)"}},
	{"INSERT INTO users (name, email) SELECT name, email FROM users WHERE id = $1 ON DUPLICATE KEY UPDATE email = $2", nil, []string{"bigint unsigned", "varchar(255)"}},
	{"UPDATE users SET email = (SELECT note FROM orders o WHERE o.user_id = users.id LIMIT 1) WHERE id = $1", nil, []string{"bigint unsigned"}},
	{"DELETE FROM users WHERE id IN (SELECT user_id FROM orders WHERE total > $1)", nil, []string{"decimal(10,2)"}},
	{"WITH big AS (SELECT user_id FROM orders WHERE total > $1) DELETE FROM users WHERE id IN (SELECT user_id FROM big)", nil, []string{"decimal(10,2)"}},
	{"SELECT id FROM users WHERE created_at > NOW() - INTERVAL $1 DAY", []string{"id bigint unsigned"}, []string{"?"}},
	{"INSERT INTO users (name, email) VALUES ($1, $2)", nil, []string{"varchar(100)", "varchar(255)"}},
	{"INSERT INTO orders VALUES ($1, $2, $3, $4)", nil, []string{"bigint unsigned", "bigint unsigned", "decimal(10,2)", "text"}},
	{"INSERT INTO users (name) VALUES ($1) ON DUPLICATE KEY UPDATE email = $2", nil, []string{"varchar(100)", "varchar(255)"}},
	{"INSERT INTO users SET name = $1, email = $2", nil, []string{"varchar(100)", "varchar(255)"}},
	{"UPDATE users SET name = $1, email = $2 WHERE id = $3", nil, []string{"varchar(100)", "varchar(255)", "bigint unsigned"}},
	{"UPDATE users u JOIN orders o ON o.user_id = u.id SET o.note = $1 WHERE u.name = $2", nil, []string{"text", "varchar(100)"}},
	{"DELETE FROM users WHERE id = $1 LIMIT $2", nil, []string{"bigint unsigned", "bigint unsigned"}},
	{"SELECT id FROM users u WHERE u.name = 'a$1' AND u.email = $1", []string{"id bigint unsigned"}, []string{"varchar(255)"}},
}

func TestAnalyze(t *testing.T) {
	s := load(t)
	for _, c := range analyzeCases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var cols, params []string
		for _, col0 := range r.Columns {
			cols = append(cols, col(col0))
		}
		for _, p := range r.Params {
			params = append(params, param(p))
		}
		if strings.Join(cols, "; ") != strings.Join(c.columns, "; ") {
			t.Errorf("%s:\n columns %q\n want    %q", c.sql, cols, c.columns)
		}
		if strings.Join(params, "; ") != strings.Join(c.params, "; ") {
			t.Errorf("%s:\n params %q\n want   %q", c.sql, params, c.params)
		}
	}
}

type errorCase struct {
	sql  string
	code int
	msg  string
	pos  int
}

// errorCases are the statements MySQL rejects, with its error number and message.
var errorCases = []errorCase{
	{"SELECT id FROM nobody", 1146, "Table 'nobody' doesn't exist", 15},
	{"SELECT idd FROM users", 1054, "Unknown column 'idd' in 'field list'", 7},
	{"SELECT u.idd FROM users u", 1054, "Unknown column 'u.idd' in 'field list'", 7},
	{"SELECT x.id FROM users u", 1054, "Unknown column 'x.id' in 'field list'", 7},
	{"SELECT id FROM users u JOIN orders o ON o.user_id = u.id", 1052, "Column 'id' in field list is ambiguous", 7},
	{"SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE total = $1 AND nope = $2", 1054, "Unknown column 'nope' in 'where clause'", 80},
	{"SELECT u.id FROM users u JOIN orders o ON o.user_idd = u.id", 1054, "Unknown column 'o.user_idd' in 'on clause'", 42},
	{"SELECT x.* FROM users u", 1051, "Unknown table 'x'", 7},
	{"SELECT id FROM users u, orders u", 1066, "Not unique table/alias: 'u'", 24},
	{"SELECT id FROM users WHERE", 1064, "", 26},
	{"INSERT INTO users (name) VALUES ($1, $2)", 1136, "Column count doesn't match value count at row 1", -1},
	{"SELECT id FROM users WHERE name = $1 AND $2 = $3 AND nope = 1", 1054, "Unknown column 'nope' in 'where clause'", 53},
	{"SELECT NOPE(id) FROM users", 1305, "FUNCTION NOPE does not exist", 7},
	{"SELECT LENGTH(id, name) FROM users", 1582, "Incorrect parameter count in the call to native function 'LENGTH'", 7},
	{"SELECT (SELECT id, name FROM users) FROM users", 1241, "Operand should contain 1 column(s)", 7},
	{"SELECT id FROM users WHERE id IN (SELECT id, user_id FROM orders)", 1241, "Operand should contain 1 column(s)", 27},
	{"SELECT id FROM users u WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.idd)", 1054, "Unknown column 'u.idd' in 'where clause'", 78},
	{"SELECT d.id FROM (SELECT name FROM users) d", 1054, "Unknown column 'd.id' in 'field list'", 7},
	{"SELECT id FROM (SELECT id FROM users)", 1248, "Every derived table must have its own alias", 15},
	{"SELECT id FROM users UNION SELECT id, user_id FROM orders", 1222, "The used SELECT statements have a different number of columns", 0},
	{"WITH t AS (SELECT id FROM users) SELECT name FROM t", 1054, "Unknown column 'name' in 'field list'", 40},
	{"SELECT x FROM (SELECT 1, 2) AS d(x)", 1353, "View's SELECT and view's field list have different column counts", 14},
	{"UPDATE v_stats SET n = 1", 1288, "The target table v_stats of the UPDATE is not updatable", 19},
	{"SELECT id FROM users ORDER BY nope", 1054, "Unknown column 'nope' in 'order clause'", 30},
	{"SELECT id FROM users ORDER BY 2", 1054, "Unknown column '2' in 'order clause'", 30},
	{"SELECT id FROM users GROUP BY nope", 1054, "Unknown column 'nope' in 'group statement'", 30},
	{"SELECT id FROM users HAVING nope > 1", 1054, "Unknown column 'nope' in 'having clause'", 28},
	{"SELECT id AS x, name AS x FROM users ORDER BY x", 1052, "Column 'x' in order clause is ambiguous", 46},
	{"SELECT id FROM users UNION SELECT user_id FROM orders ORDER BY name", 1054, "Unknown column 'name' in 'order clause'", 63},
	{"UPDATE users SET name = 'x' ORDER BY nope LIMIT 1", 1054, "Unknown column 'nope' in 'order clause'", 37},
}

// oneCases are the statements the One proof judges: proven means every expansion touches
// at most one row, by the facts the analyzer records (x/cardinality does the proving).
var oneCases = []struct {
	sql    string
	proven bool
}{
	{"SELECT id, name FROM users WHERE id = $1", true},
	{"SELECT id, name FROM users WHERE name = $1", false},
	{"SELECT id FROM users WHERE id = 1 AND name = 'x'", true},
	{"SELECT id FROM users WHERE id = $1 OR name = $2", false},
	{"SELECT id FROM users u WHERE u.id = $1 + 1", true},
	{"SELECT COUNT(*) FROM users", true},
	{"SELECT COUNT(*), MAX(id) FROM users WHERE name = 'x'", true},
	{"SELECT COUNT(*) FROM users GROUP BY name", false},
	{"SELECT id FROM users LIMIT 1", true},
	{"SELECT id FROM users LIMIT 2", false},
	{"SELECT 1", true},
	{"SELECT u.id, o.total FROM users u JOIN orders o ON o.user_id = u.id WHERE o.id = $1", true},
	{"SELECT u.id, o.total FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1", false},
	{"SELECT u.id, o.total FROM users u JOIN orders o ON o.id = u.id WHERE u.id = $1", true},
	{"SELECT u.id, o.total FROM users u LEFT JOIN orders o ON o.id = u.id WHERE u.id = $1", true},
	{"SELECT u.id, o.total FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.id = $1", false},
	{"SELECT u.id FROM users u JOIN orders o USING (id) WHERE o.id = $1", true},
	{"SELECT id FROM users WHERE id IN ($1, $2)", false},
	{"SELECT id FROM users WHERE id = (SELECT MAX(user_id) FROM orders)", false},
	{"SELECT d.id FROM (SELECT id FROM users WHERE id = 1) d", true},
	{"SELECT d.id FROM (SELECT id FROM users) d WHERE d.id = 1", true}, // the derived table's output is users.id: the proof looks into the body
	{"SELECT id FROM v_users WHERE id = $1", true},
	{"SELECT id FROM users UNION SELECT user_id FROM orders WHERE id = 1", false},
	{"INSERT INTO users (name) VALUES ($1)", true},
	{"INSERT INTO users (name) VALUES ($1), ($2)", false},
	{"INSERT INTO users (name) SELECT name FROM users", false},
	{"UPDATE users SET name = $1 WHERE id = $2", true},
	{"UPDATE users SET name = $1 WHERE name = $2", false},
	{"UPDATE users SET name = $1 WHERE name = $2 LIMIT 1", true},
	{"UPDATE users u JOIN orders o ON o.user_id = u.id SET o.note = $1 WHERE o.id = $2", true},
	{"DELETE FROM users WHERE id = $1", true},
	{"DELETE FROM users WHERE email = $1", false},
	{"DELETE FROM users WHERE name = $1 LIMIT 1", true},
}

func TestOne(t *testing.T) {
	s := load(t)
	for _, c := range oneCases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		got, why := cardinality.AtMostOne(r.Facts)
		if got != c.proven {
			t.Errorf("%s: proven=%v (%s), want %v", c.sql, got, why, c.proven)
		}
	}
}

func TestErrors(t *testing.T) {
	s := load(t)
	for _, c := range errorCases {
		_, err := Analyze(s, c.sql)
		e, ok := err.(*Error)
		if !ok {
			t.Errorf("%s: got %v, want *Error %d", c.sql, err, c.code)
			continue
		}
		if e.Code != c.code || (c.msg != "" && e.Message != c.msg) || e.Position != c.pos {
			t.Errorf("%s:\n got  %d %q at %d\n want %d %q at %d", c.sql, e.Code, e.Message, e.Position, c.code, c.msg, c.pos)
		}
	}
}

func TestUnsupported(t *testing.T) {
	s := load(t)
	for _, sql := range []string{
		"SHOW TABLES",
		"INSERT INTO v_users (name) VALUES ('x')",
	} {
		_, err := Analyze(s, sql)
		if err == nil {
			t.Errorf("%s: analyzed, want an unsupported error", sql)
			continue
		}
		if _, isDB := err.(*Error); isDB {
			t.Errorf("%s: %v is a database error, want the analyzer's own limit", sql, err)
		}
	}
}

// TestParamSources covers the table column a placeholder stands for: the column it is
// compared with (=, IN, BETWEEN, a subquery's output) or stored into (assigned).
func TestParamSources(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want string // one entry per placeholder: table.column, "=" when assigned, "-" for none
	}{
		{"SELECT id FROM users WHERE id = $1", "users.id"},
		{"SELECT id FROM users WHERE $1 = id AND name IN ($2, $3)", "users.id users.name users.name"},
		{"SELECT id FROM users WHERE created_at BETWEEN $1 AND $2", "users.created_at users.created_at"},
		{"SELECT id FROM users WHERE id = $1 + 1", "-"},
		{"SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE $1 IN (SELECT user_id FROM orders)", "orders.user_id"},
		{"SELECT id FROM users WHERE name = $1 AND email = $1", "users.name"},
		{"INSERT INTO users (name, email) VALUES ($1, $2)", "users.name= users.email="},
		{"UPDATE users SET name = $1 WHERE id = $2", "users.name= users.id"},
		{"INSERT INTO orders (id, user_id, total) SELECT $1, id, 0 FROM users WHERE id = $2", "orders.id= users.id"},
		{"SELECT id FROM users LIMIT $1", "-"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var parts []string
		for _, p := range r.Params {
			if p.Source == nil {
				parts = append(parts, "-")
				continue
			}
			e := p.Source.Table + "." + p.Source.Column
			if p.Source.Assigned {
				e += "="
			}
			parts = append(parts, e)
		}
		if got := strings.Join(parts, " "); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.sql, got, c.want)
		}
	}
}
