package migrate

// What the migrate probe (probe_test.go) found and the planner now does, each pinned as the
// minimized pair the probe reported, replayed against a real mysqld the way `plan` does:
// the DDL applies cleanly and reads back as the target.

import (
	"strings"
	"testing"
)

// A target that declares its own key over a foreign key's columns replaces the index the
// server created for the constraint (the canonical form of the target has only the declared
// key): the planner's DROP INDEX of the server's one and its ADD of the declared one must be
// one ALTER TABLE, since the foreign key needs an index at every statement boundary (Error
// 1553 "Cannot drop index: needed in a foreign key constraint" otherwise, measured).
func TestProbeForeignKeyIndexReplacedByDeclaredKey(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT NOT NULL,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT NOT NULL,
  UNIQUE KEY children_parent_key (parent_id),
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id));
`)
	plan(t, ctx, "declared key over a foreign key", base, to)
}

// The primary key moving off a column another table's foreign key references: the DROP
// PRIMARY KEY needs the target's replacement key on that column in the same ALTER TABLE
// (the same Error 1553 on the referenced side, measured).
func TestProbePrimaryKeyMovesOffReferencedColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY, code BIGINT UNSIGNED);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE SET NULL);
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT NOT NULL, code BIGINT UNSIGNED NOT NULL, PRIMARY KEY (code), UNIQUE KEY parents_id_key (id));
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE SET NULL);
`)
	plan(t, ctx, "primary key off a referenced column", base, to)
}

// Renaming a column a generated column reads: MySQL refuses the RENAME COLUMN on its own
// (Error 3108 "has a generated column dependency", measured) and accepts it with the
// dependent generated columns rewritten in the same ALTER TABLE.
func TestProbeRenamedColumnReadByGeneratedColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, qty INT NOT NULL DEFAULT 0,
  qty_plus BIGINT GENERATED ALWAYS AS (qty + 1) STORED,
  qty_twice BIGINT GENERATED ALWAYS AS (qty * 2) VIRTUAL,
  KEY t_qty_plus (qty_plus));
CREATE VIEW t_qty AS SELECT id, qty FROM t;
`)
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate rename t.qty -> t.quantity
CREATE TABLE t (id INT PRIMARY KEY, quantity INT NOT NULL DEFAULT 0,
  qty_plus BIGINT GENERATED ALWAYS AS (quantity + 1) STORED,
  qty_twice BIGINT GENERATED ALWAYS AS (quantity * 2) VIRTUAL,
  KEY t_qty_plus (qty_plus));
CREATE VIEW t_qty AS SELECT id, quantity FROM t;
`
	to := mustCanonical(t, ctx, toSQL)
	ddl := plan(t, ctx, "renamed column read by generated columns", base, to)
	for _, s := range ddl {
		if len(s) > 12 && s[:12] == "ALTER TABLE " && strings.Contains(s, "RENAME COLUMN") && !strings.Contains(s, "MODIFY COLUMN `qty_plus`") {
			t.Errorf("the RENAME COLUMN must carry its dependent generated columns:\n%s", s)
		}
	}
}

// A view whose new definition reads a column the same plan adds: the CREATE OR REPLACE VIEW
// must come after the ADD COLUMN (Error 1054 "Unknown column" otherwise, measured).
func TestProbeReplacedViewReadsAddedColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT);
CREATE VIEW v AS SELECT id, a FROM t;
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT);
CREATE VIEW v AS SELECT id, a, b FROM t;
`)
	plan(t, ctx, "replaced view reads an added column", base, to)
}

// A brand new table whose foreign key references a column (and key) a table that stays is
// only now gaining: the CREATE TABLE must come after that ADD COLUMN / ADD KEY (Error 3734
// "Missing column ... in the referenced table" and 6125 "Missing unique key" otherwise,
// measured), while a staying table's new foreign key to a new table must come after the
// CREATE TABLE.
func TestProbeNewTableReferencesAddedColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY, name VARCHAR(50));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE regions (id INT PRIMARY KEY);
CREATE TABLE parents (id INT NOT NULL, code BIGINT UNSIGNED NOT NULL, name VARCHAR(50), region_id INT,
  PRIMARY KEY (code), UNIQUE KEY parents_id_key (id),
  CONSTRAINT fk_region FOREIGN KEY (region_id) REFERENCES regions (id));
CREATE TABLE children (id INT PRIMARY KEY, parent_code BIGINT UNSIGNED,
  CONSTRAINT fk_parent FOREIGN KEY (parent_code) REFERENCES parents (code));
`)
	plan(t, ctx, "new table references an added column", base, to)
}

// A renamed table whose foreign key is dropped ahead of a type change on either side
// (coordinateForeignKeys): the DROP FOREIGN KEY runs after the RENAME TABLE and must name
// the table as it is by then (Error 1146 "Table doesn't exist" otherwise, measured).
func TestProbeRenamedTableForeignKeyCoordinated(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id));
`)
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate rename children -> kids
CREATE TABLE parents (id BIGINT PRIMARY KEY);
CREATE TABLE kids (id INT PRIMARY KEY, parent_id BIGINT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id));
`
	to := mustCanonical(t, ctx, toSQL)
	plan(t, ctx, "renamed table with a coordinated foreign key", base, to)
}
