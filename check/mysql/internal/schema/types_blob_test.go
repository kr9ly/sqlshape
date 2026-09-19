package schema

import (
	"strings"
	"testing"
)

// The BLOB family and the TEXT family share PT_blob_type, told apart by the charset the
// grammar's action passes. Every spelling below is pinned to what SHOW CREATE TABLE spells
// back on 8.4 (measured), the CHARACTER SET binary degradation to the blob included.
func TestBlobTextTypes(t *testing.T) {
	s, err := Load(`CREATE TABLE b (
  a TINYTEXT, b MEDIUMTEXT, c LONGTEXT,
  d TINYBLOB, e BLOB, f MEDIUMBLOB, g LONGBLOB,
  h TEXT, i LONG VARCHAR, j LONG, k LONG VARBINARY,
  l TINYTEXT CHARACTER SET binary, m TINYTEXT BINARY, n MEDIUMTEXT CHARACTER SET utf8mb4
);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	want := map[string]string{
		"a": "tinytext", "b": "mediumtext", "c": "longtext",
		"d": "tinyblob", "e": "blob", "f": "mediumblob", "g": "longblob",
		"h": "text", "i": "mediumtext", "j": "mediumtext", "k": "mediumblob",
		"l": "tinyblob", "m": "tinytext", "n": "mediumtext",
	}
	tb := s.Table("b")
	for name, typ := range want {
		c := tb.Column(name)
		if c.Type.Name != typ {
			t.Errorf("%s: %q, want %q", name, c.Type.Name, typ)
		}
	}
	if c := tb.Column("m"); !c.Type.Binary {
		t.Error("TINYTEXT BINARY lost the binary collation flag")
	}
	if c := tb.Column("n"); c.Type.Charset != "utf8mb4" {
		t.Errorf("n charset %q", c.Type.Charset)
	}
	if c := tb.Column("l"); c.Type.Charset != "" {
		t.Errorf("l charset %q (the binary charset is the type, not a charset)", c.Type.Charset)
	}
}

// A foreign key whose reference list is missing or does not match its column count is the
// server's own CREATE-time refusal (1239, measured on 8.4); the loader never falls back
// to the parent's primary key.
func TestForeignKeyReferenceMismatch(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE p (id INT NOT NULL PRIMARY KEY);\nCREATE TABLE c (pid INT NOT NULL, FOREIGN KEY (pid) REFERENCES p);",
		"CREATE TABLE p (a INT NOT NULL, b INT NOT NULL, PRIMARY KEY (a, b));\nCREATE TABLE c (x INT NOT NULL, y INT NOT NULL, FOREIGN KEY (x, y) REFERENCES p (a));",
		"CREATE TABLE p (a INT NOT NULL, b INT NOT NULL, PRIMARY KEY (a, b));\nCREATE TABLE c (x INT NOT NULL, FOREIGN KEY (x) REFERENCES p (a, b));",
	} {
		s, err := Load(sql)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range s.Problems {
			if strings.Contains(p.Message, "Key reference and table reference don't match (1239)") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no 1239 problem; problems: %v", sql, s.Problems)
		}
		if len(s.Table("c").ForeignKeys) != 0 {
			t.Errorf("%s: the refused foreign key was kept", sql)
		}
	}
}
