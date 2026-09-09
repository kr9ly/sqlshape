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

func TestAnalyze(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql     string
		columns []string
		params  []string
	}{
		{"SELECT id, name, email FROM users", []string{"id bigint unsigned", "name varchar(100)", "email varchar(255) null"}, nil},
		{"SELECT * FROM users", []string{"id bigint unsigned", "name varchar(100)", "email varchar(255) null", "created_at datetime(6)"}, nil},
		{"SELECT u.id AS uid, o.total, o.note FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1", []string{"uid bigint unsigned", "total decimal(10,2)", "note text null"}, []string{"bigint unsigned"}},
		{"SELECT u.id, o.total FROM users u LEFT JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned", "total decimal(10,2) null"}, nil},
		{"SELECT u.id, o.total FROM users u RIGHT JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned null", "total decimal(10,2)"}, nil},
		{"SELECT o.* FROM users u JOIN orders o ON o.user_id = u.id", []string{"id bigint unsigned", "user_id bigint unsigned", "total decimal(10,2)", "note text null"}, nil},
		{"SELECT count(*), count(email) c, 1, 'x', 1.5, NULL, id = 1 FROM users", []string{"count(*) bigint", "c bigint", "1 bigint", "'x' varchar", "1.5 decimal", "NULL null null", "id = 1 bigint"}, nil},
		{"SELECT id FROM users WHERE name = $1 AND email = $2 LIMIT $3 OFFSET $4", []string{"id bigint unsigned"}, []string{"varchar(100)", "varchar(255)", "bigint unsigned", "bigint unsigned"}},
		{"SELECT id FROM users WHERE $1 = name", []string{"id bigint unsigned"}, []string{"varchar(100)"}},
		{"SELECT id FROM users WHERE id IN ($1, $2) AND name LIKE $3 AND created_at BETWEEN $4 AND $5", []string{"id bigint unsigned"}, []string{"bigint unsigned", "bigint unsigned", "varchar(100)", "datetime(6)", "datetime(6)"}},
		{"SELECT id FROM users WHERE id = $1 AND name = $1", []string{"id bigint unsigned"}, []string{"bigint unsigned"}},
		{"SELECT COALESCE(email, name) FROM users WHERE id = $1", []string{"COALESCE(email, name) ? null"}, []string{"bigint unsigned"}},
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
	for _, c := range cases {
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

func TestErrors(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		code int
		msg  string
		pos  int
	}{
		{"SELECT id FROM nobody", 1146, "Table 'nobody' doesn't exist", 15},
		{"SELECT idd FROM users", 1054, "Unknown column 'idd' in 'field list'", 7},
		{"SELECT u.idd FROM users u", 1054, "Unknown column 'u.idd' in 'field list'", 7},
		{"SELECT x.id FROM users u", 1054, "Unknown column 'x.id' in 'field list'", 7},
		{"SELECT id FROM users u JOIN orders o ON o.user_id = u.id", 1052, "Column 'id' in field list is ambiguous", 7},
		{"SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE total = $1 AND nope = $2", 1054, "Unknown column 'nope' in 'where clause'", 80},
		{"SELECT u.id FROM users u JOIN orders o ON o.user_idd = u.id", 1054, "Unknown column 'o.user_idd' in 'on clause'", 42},
		{"SELECT x.* FROM users u", 1109, "Unknown table 'x'", 7},
		{"SELECT id FROM users u, orders u", 1066, "Not unique table/alias: 'u'", 24},
		{"SELECT id FROM users WHERE", 1064, "", 26},
		{"INSERT INTO users (name) VALUES ($1, $2)", 1136, "Column count doesn't match value count at row 1", -1},
		{"SELECT id FROM users WHERE name = $1 AND $2 = $3 AND nope = 1", 1054, "Unknown column 'nope' in 'where clause'", 53},
	}
	for _, c := range cases {
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

func TestPlaceholders(t *testing.T) {
	text, pm := placeholders("SELECT a$1, 'x$2', `c$3`, \"d$4\" FROM t WHERE x = $1 AND y = $12 AND z=$2")
	want := "SELECT a$1, 'x$2', `c$3`, \"d$4\" FROM t WHERE x = ? AND y = ? AND z=?"
	if text != want {
		t.Fatalf("got  %q\nwant %q", text, want)
	}
	if pm.count() != 12 || len(pm.marks) != 3 {
		t.Fatalf("count %d marks %d", pm.count(), len(pm.marks))
	}
	// the `?` for $12 is at index 56 in text; the original `$12` starts at 57 (one `$1` before it grew by 1)
	if n := pm.number(strings.Index(text, "y = ?") + 4); n != 12 {
		t.Errorf("number = %d, want 12", n)
	}
	off := strings.Index(text, "z=?") + 2
	if back := pm.back(off); back != off+1+2 {
		t.Errorf("back(%d) = %d, want %d", off, back, off+3)
	}
}
