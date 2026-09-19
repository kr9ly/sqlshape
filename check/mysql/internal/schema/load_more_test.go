package schema

import (
	"strings"
	"testing"
)

// loadOK loads schemaSQL and fails the test on any problem.
func loadOK(t *testing.T, schemaSQL string) *Schema {
	t.Helper()
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	return s
}

// loadProblem loads schemaSQL and returns the schema, requiring at least one problem whose
// message contains want.
func loadProblem(t *testing.T, schemaSQL, want string) *Schema {
	t.Helper()
	s, err := Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		if strings.Contains(p.Message, want) {
			return s
		}
	}
	t.Errorf("no problem containing %q; problems: %v", want, s.Problems)
	return s
}

func TestCreateTableLike(t *testing.T) {
	s := loadOK(t, `
CREATE TABLE src (
  id INT NOT NULL PRIMARY KEY,
  name VARCHAR(10),
  parent_id INT,
  UNIQUE KEY uq_name (name),
  CONSTRAINT src_ok CHECK (id > 0),
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES src (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin ROW_FORMAT=DYNAMIC
  PARTITION BY HASH (id) PARTITIONS 2;
CREATE TABLE dup LIKE src;
`)
	d := s.Table("dup")
	if d == nil {
		t.Fatal("no dup")
	}
	if len(d.Columns) != 3 || len(d.Keys) != 2 || len(d.Checks) != 1 {
		t.Errorf("copied %d columns, %d keys, %d checks", len(d.Columns), len(d.Keys), len(d.Checks))
	}
	if d.Engine != "InnoDB" || d.Charset != "utf8mb4" || d.Collation != "utf8mb4_bin" || d.RowFormat != "DYNAMIC" {
		t.Errorf("options not copied: %q %q %q %q", d.Engine, d.Charset, d.Collation, d.RowFormat)
	}
	// LIKE copies no foreign keys and no partitioning (measured against mysqld 8.4)
	if len(d.ForeignKeys) != 0 {
		t.Errorf("LIKE copied %d foreign keys", len(d.ForeignKeys))
	}
	if d.Partitioning != nil {
		t.Error("LIKE copied the partitioning")
	}
	// the copies are the new table's own: renaming a copied column must not touch src
	if c := d.Columns[1]; c.Name != "name" {
		t.Fatalf("column %q", c.Name)
	}
	d.Columns[1].Name = "renamed"
	if s.Table("src").Columns[1].Name != "name" {
		t.Error("the copy shares src's column")
	}

	loadProblem(t, "CREATE TABLE t2 LIKE nope;", "no such table")
	loadProblem(t, "CREATE TABLE t (a INT);\nCREATE TABLE t (b INT);", "already exists")
	loadOK(t, "CREATE TABLE t (a INT);\nCREATE TABLE IF NOT EXISTS t (b INT);")
	loadProblem(t, "CREATE TABLE t AS SELECT 1 AS a;", "columns come from the query")
}

func TestRenameTableStatement(t *testing.T) {
	s := loadOK(t, `
CREATE TABLE a (id INT PRIMARY KEY);
CREATE TABLE c (id INT PRIMARY KEY);
CREATE VIEW v AS SELECT id FROM a;
CREATE TRIGGER trg BEFORE INSERT ON a FOR EACH ROW SET NEW.id = 1;
RENAME TABLE a TO b, v TO w;
`)
	if s.Table("b") == nil || s.Table("a") != nil {
		t.Error("a not renamed to b")
	}
	if s.View("w") == nil || s.View("v") != nil {
		t.Error("v not renamed to w")
	}
	if len(s.Triggers) != 1 || s.Triggers[0].Table != "b" {
		t.Errorf("trigger follows the rename: %+v", s.Triggers)
	}
	loadProblem(t, "RENAME TABLE nope TO x;", "no such table")
}

func TestRowFormats(t *testing.T) {
	s := loadOK(t, `
CREATE TABLE f (a INT) ROW_FORMAT=FIXED;
CREATE TABLE z (a INT) ROW_FORMAT=COMPRESSED;
CREATE TABLE r (a INT) ROW_FORMAT=REDUNDANT;
CREATE TABLE c (a INT) ROW_FORMAT=COMPACT;
`)
	for name, want := range map[string]string{"f": "FIXED", "z": "COMPRESSED", "r": "REDUNDANT", "c": "COMPACT"} {
		if got := s.Table(name).RowFormat; got != want {
			t.Errorf("%s: ROW_FORMAT %q, want %q", name, got, want)
		}
	}
}

func TestPartitioningForms(t *testing.T) {
	s := loadOK(t, `
CREATE TABLE lc (a INT NOT NULL, b INT NOT NULL) PARTITION BY LIST COLUMNS (a, b) (
  PARTITION p0 VALUES IN ((1, 2), (3, 4)),
  PARTITION p1 VALUES IN ((5, 6)) COMMENT = 'rest'
);
CREATE TABLE rc (a INT NOT NULL, b INT NOT NULL) PARTITION BY RANGE COLUMNS (a, b) (
  PARTITION p0 VALUES LESS THAN (10, 20),
  PARTITION p1 VALUES LESS THAN (MAXVALUE, MAXVALUE)
);
CREATE TABLE rmax (a INT NOT NULL) PARTITION BY RANGE (a) (
  PARTITION p0 VALUES LESS THAN (10),
  PARTITION p1 VALUES LESS THAN MAXVALUE
);
CREATE TABLE sk (a INT NOT NULL, b INT NOT NULL, PRIMARY KEY (a, b))
  PARTITION BY RANGE (a)
  SUBPARTITION BY LINEAR KEY ALGORITHM=2 (b) SUBPARTITIONS 2 (
  PARTITION p0 VALUES LESS THAN (10)
);
`)
	lc := s.Table("lc").Partitioning
	if lc == nil || lc.Kind != "LIST" || !lc.Columns || len(lc.Cols) != 2 {
		t.Fatalf("lc partitioning: %+v", lc)
	}
	if lc.Parts[0].Bound != "(1, 2), (3, 4)" || lc.Parts[1].Bound != "(5, 6)" {
		t.Errorf("lc bounds: %q, %q", lc.Parts[0].Bound, lc.Parts[1].Bound)
	}
	if lc.Parts[1].Comment != "rest" {
		t.Errorf("lc comment: %q", lc.Parts[1].Comment)
	}
	rc := s.Table("rc").Partitioning
	if rc.Parts[0].Bound != "10, 20" || rc.Parts[1].Bound != "MAXVALUE, MAXVALUE" {
		t.Errorf("rc bounds: %q, %q", rc.Parts[0].Bound, rc.Parts[1].Bound)
	}
	// a COLUMNS partition's per-column MAXVALUE folds into the bound's text, never MaxValue
	if rc.Parts[1].MaxValue {
		t.Error("rc p1 marked MaxValue")
	}
	rmax := s.Table("rmax").Partitioning
	if rmax.Parts[0].MaxValue || rmax.Parts[0].Bound != "10" || !rmax.Parts[1].MaxValue {
		t.Errorf("rmax parts: %+v", rmax.Parts)
	}
	sub := s.Table("sk").Partitioning.Sub
	if sub == nil || sub.Kind != "KEY" || !sub.Linear || sub.Algorithm != 2 || len(sub.Cols) != 1 || sub.Num != 2 {
		t.Errorf("sk subpartitioning: %+v", sub)
	}

	// an explicit partition list under HASH, and a per-partition option this package does
	// not model, are problems rather than differences the model cannot see
	s = loadProblem(t, "CREATE TABLE h (a INT) PARTITION BY HASH (a) (PARTITION p0, PARTITION p1);",
		"an explicit partition list is not supported")
	if s.Table("h").Partitioning != nil {
		t.Error("h kept a partitioning")
	}
	loadProblem(t, "CREATE TABLE r2 (a INT) PARTITION BY RANGE (a) (PARTITION p0 VALUES LESS THAN (10) MAX_ROWS = 100);",
		"definition not understood")
}

func TestAlterTableForms(t *testing.T) {
	s := loadOK(t, `
CREATE TABLE t (
  a INT NOT NULL PRIMARY KEY,
  b VARCHAR(10),
  KEY ix_b (b),
  CONSTRAINT c_pos CHECK (a > 0)
);
ALTER TABLE t ALTER COLUMN b SET DEFAULT 'x';
ALTER TABLE t ALTER COLUMN b SET INVISIBLE;
ALTER TABLE t ALTER INDEX ix_b INVISIBLE;
ALTER TABLE t ALTER CHECK c_pos NOT ENFORCED;
ALTER TABLE t RENAME INDEX ix_b TO ix_b2;
ALTER TABLE t CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
ALTER TABLE t RENAME TO t2;
`)
	t2 := s.Table("t2")
	if t2 == nil {
		t.Fatal("no t2")
	}
	b := t2.Column("b")
	if b.Default == nil || !b.Invisible {
		t.Errorf("b: default %v, invisible %v", b.Default, b.Invisible)
	}
	if k := t2.key("ix_b2"); k == nil || !k.Invisible {
		t.Errorf("ix_b2: %+v", k)
	}
	if t2.Checks[0].Enforced {
		t.Error("c_pos still enforced")
	}
	if t2.Charset != "utf8mb4" || t2.Collation != "utf8mb4_bin" {
		t.Errorf("charset %q collation %q", t2.Charset, t2.Collation)
	}

	base := "CREATE TABLE t (a INT NOT NULL PRIMARY KEY, b VARCHAR(10), KEY ix_b (b));\n"
	loadProblem(t, base+"ALTER TABLE t ADD COLUMN a INT;", "duplicate column")
	loadProblem(t, base+"ALTER TABLE t DROP COLUMN nope;", "no such column")
	loadProblem(t, base+"ALTER TABLE t MODIFY nope INT;", "no such column")
	loadProblem(t, base+"ALTER TABLE t RENAME COLUMN nope TO x;", "no such column")
	loadProblem(t, base+"ALTER TABLE t RENAME INDEX nope TO x;", "no such index")
	loadProblem(t, base+"ALTER TABLE t DROP INDEX nope;", "no such index")
	loadProblem(t, base+"ALTER TABLE t DROP CONSTRAINT nope;", "no such constraint")
	loadProblem(t, base+"ALTER TABLE t ALTER COLUMN nope SET DEFAULT 1;", "no such column")

	// HASH partition arithmetic: ADD PARTITION PARTITIONS / COALESCE PARTITION move Num,
	// DROP PARTITION and REORGANIZE PARTITION rewrite a RANGE's part list
	s = loadOK(t, `
CREATE TABLE h (a INT NOT NULL PRIMARY KEY) PARTITION BY HASH (a) PARTITIONS 4;
ALTER TABLE h ADD PARTITION PARTITIONS 2;
ALTER TABLE h COALESCE PARTITION 3;
CREATE TABLE r (a INT NOT NULL PRIMARY KEY) PARTITION BY RANGE (a) (
  PARTITION p0 VALUES LESS THAN (10),
  PARTITION p1 VALUES LESS THAN (20),
  PARTITION pmax VALUES LESS THAN MAXVALUE
);
ALTER TABLE r DROP PARTITION p0;
ALTER TABLE r REORGANIZE PARTITION p1 INTO (
  PARTITION p1a VALUES LESS THAN (15),
  PARTITION p1b VALUES LESS THAN (20)
);
`)
	if n := s.Table("h").Partitioning.Num; n != 3 {
		t.Errorf("h partitions: %d", n)
	}
	var names []string
	for _, p := range s.Table("r").Partitioning.Parts {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "p1a,p1b,pmax" {
		t.Errorf("r parts: %v", names)
	}
	loadProblem(t, `
CREATE TABLE r (a INT NOT NULL PRIMARY KEY) PARTITION BY RANGE (a) (PARTITION p0 VALUES LESS THAN (10));
ALTER TABLE r REORGANIZE PARTITION p0 INTO (PARTITION p0a VALUES LESS THAN (5) MAX_ROWS = 10);`,
		"REORGANIZE PARTITION: definition not understood")
}

func TestCreateViewDDL(t *testing.T) {
	base := "CREATE TABLE t (a INT NOT NULL PRIMARY KEY);\nCREATE VIEW v AS SELECT a FROM t;\n"
	loadProblem(t, base+"CREATE VIEW v AS SELECT a FROM t;", "view already exists")
	loadProblem(t, base+"CREATE VIEW t AS SELECT a FROM t;", "a table of that name exists")
	s := loadOK(t, base+"CREATE OR REPLACE VIEW v (b) AS SELECT a + 1 FROM t;")
	if v := s.View("v"); len(v.Columns) != 1 || v.Columns[0] != "b" {
		t.Errorf("v not replaced: %+v", v)
	}
}

func TestLoadProblemStatements(t *testing.T) {
	loadProblem(t, "DROP INDEX nope ON missing;", "no such table")
	loadProblem(t, "CREATE TABLE t (a INT);\nDROP INDEX nope ON t;", "no such index")
	loadProblem(t, "DROP VIEW nope;", "no such view")
	loadProblem(t, "-- sqlshape: server max_sp_recursion_depth = 300\nCREATE TABLE t (a INT);", "want 0-255")
	loadProblem(t, "KILL 1;", "not applied to the schema")
}
