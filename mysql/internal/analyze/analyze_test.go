package analyze

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/internal/schema"
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
	{"SELECT LENGTH(name), UPPER(name), CONCAT(name, email), ABS(total), ROUND(total), ROUND(total, 1), FLOOR(total), CEIL(u.id), SQRT(u.id), NOW(), CURDATE(), CURTIME(), UNIX_TIMESTAMP(), YEAR(created_at), DATE(created_at), DATEDIFF(created_at, NOW()), DATE_FORMAT(created_at, '%Y'), JSON_EXTRACT('{}', '$'), MD5(name), UUID(), LAST_INSERT_ID(), HEX(u.id), CONV(u.id, 10, 16), ISNULL(email), CHAR_LENGTH(email) FROM users u JOIN orders o ON o.user_id = u.id", []string{"LENGTH(name) bigint", "UPPER(name) varchar null", "CONCAT(name, email) varchar null", "ABS(total) decimal", "ROUND(total) decimal(10,0)", "ROUND(total, 1) decimal", "FLOOR(total) bigint", "CEIL(u.id) bigint unsigned", "SQRT(u.id) double null", "NOW() datetime", "CURDATE() date", "CURTIME() time", "UNIX_TIMESTAMP() ? null", "YEAR(created_at) year null", "DATE(created_at) date null", "DATEDIFF(created_at, NOW()) bigint null", "DATE_FORMAT(created_at, '%Y') varchar null", "JSON_EXTRACT('{}', '$') json null", "MD5(name) varchar null", "UUID() varchar null", "LAST_INSERT_ID() bigint unsigned", "HEX(u.id) varchar null", "CONV(u.id, 10, 16) varchar null", "ISNULL(email) bigint(1)", "CHAR_LENGTH(email) bigint null"}, nil},
	// aggregates
	{"SELECT SUM(total), SUM(u.id), AVG(total), MIN(name), MAX(created_at), COUNT(DISTINCT email), GROUP_CONCAT(name), BIT_OR(u.id), STD(total), SUM(name) FROM users u JOIN orders o ON o.user_id = u.id", []string{"SUM(total) decimal null", "SUM(u.id) decimal null", "AVG(total) decimal null", "MIN(name) varchar(100) null", "MAX(created_at) datetime(6) null", "COUNT(DISTINCT email) bigint", "GROUP_CONCAT(name) text null", "BIT_OR(u.id) bigint unsigned", "STD(total) double null", "SUM(name) double null"}, nil},
	// casts
	{"SELECT CAST(u.id AS SIGNED), CAST(name AS UNSIGNED), CAST(total AS CHAR(5)), CAST(name AS DECIMAL(10,2)), CAST(name AS DECIMAL), CAST(name AS DATE), CAST(name AS DATETIME(3)), CAST(name AS JSON), CONVERT(name, BINARY), BINARY name, CAST(u.id AS DOUBLE) FROM users u JOIN orders o ON o.user_id = u.id", []string{"CAST(u.id AS SIGNED) bigint", "CAST(name AS UNSIGNED) bigint unsigned", "CAST(total AS CHAR(5)) varchar(5) null", "CAST(name AS DECIMAL(10,2)) decimal(10,2)", "CAST(name AS DECIMAL) decimal(10,0)", "CAST(name AS DATE) date null", "CAST(name AS DATETIME(3)) datetime(3) null", "CAST(name AS JSON) json null", "CONVERT(name, BINARY) varbinary(100) null", "BINARY name varbinary(100) null", "CAST(u.id AS DOUBLE) double"}, nil},
	// placeholders typed through function facts and branch aggregation
	{"SELECT id FROM users WHERE name = CONCAT($1, 'x') AND id > IF($2, 1, 2) AND created_at > DATE_ADD($3, INTERVAL 1 DAY) AND email = COALESCE($4, name) AND id IN ($5, id + $6) AND LENGTH($7) > 1 AND name LIKE CONCAT('%', $8, '%') LIMIT $9", []string{"id bigint unsigned"}, []string{"varchar", "bigint", "?", "varchar(100)", "bigint unsigned", "bigint unsigned", "varchar", "varchar", "bigint unsigned"}},
	{"SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1 + 1 AND total = ROUND($2, 2) AND u.id DIV $3 = 1 AND $4 = CASE WHEN u.id = 1 THEN 'a' ELSE 'b' END", []string{"id bigint unsigned"}, []string{"bigint", "decimal", "bigint", "varchar"}}, // $1 takes the other operand's type, the literal's
	// subqueries are not entered: their scope is their own
	{"SELECT id, EXISTS (SELECT 1 FROM orders WHERE total > 1), id IN (SELECT user_id FROM orders), (SELECT 1) FROM users", []string{"id bigint unsigned", "EXISTS (SELECT 1 FROM orders WHERE total > 1) bigint(1)", "id IN (SELECT user_id FROM orders) bigint(1) null", "(SELECT 1) ? null"}, nil},
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
		"SELECT id FROM v_users",
		"SELECT 1 UNION SELECT 2",
		"SELECT a FROM (SELECT 1 a) d",
		"INSERT INTO users (name) SELECT name FROM users",
		"SHOW TABLES",
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
