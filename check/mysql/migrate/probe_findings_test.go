package migrate

// What the migrate probe (probe_test.go) found and the planner now does, each pinned as the
// minimized pair the probe reported, replayed against a real mysqld the way `plan` does:
// the DDL applies cleanly and reads back as the target.

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
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

// With rows in the table: a column turning NOT NULL runs its declared backfill before the
// MODIFY (Error 1138 "Invalid use of NULL value" otherwise, measured); a new NOT NULL
// foreign key column runs its backfill before the constraint goes on (Error 1452 on the
// implicit-default rows otherwise, measured); a new AUTO_INCREMENT primary key column gets
// its key in the same statement (Error 1075 otherwise, measured).
func TestProbeBackfillsUnderRows(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY);
CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT);
`)
	rows := "INSERT INTO parents VALUES (1), (2);\nINSERT INTO t VALUES (1, 1, 1), (2, NULL, 2);\n"
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate backfill t.a = 9 where a is null
-- @migrate backfill t.parent_id = 1
-- @migrate backfill t.seq = id
CREATE TABLE parents (id INT PRIMARY KEY);
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, b INT, parent_id INT NOT NULL, seq INT NOT NULL,
  n BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  PRIMARY KEY (n), UNIQUE KEY t_id_key (id), UNIQUE KEY t_seq_key (seq),
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents (id));
`
	to := mustCanonical(t, ctx, toSQL)
	ddl, err := Plan(base.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	changes, err := Verify(ctx, dump.Local{}, base.text+"\n"+rows, joined, to.s)
	if err != nil {
		t.Fatalf("verify: %v\nDDL:\n%s", err, joined)
	}
	if len(changes) > 0 {
		t.Errorf("changes: %v\nDDL:\n%s", changes, joined)
	}
}

// A column SHOW CREATE TABLE writes with a versioned comment (`/*!80023 INVISIBLE */`)
// keeps the comment's closing in its definition text, so MODIFY COLUMN from that text
// parses (a syntax error near ” otherwise, measured); and every table's foreign keys are
// dropped before any table's keys, since a child's foreign key may rest on the parent's
// key that goes (Error 1553 otherwise, measured).
func TestProbeInvisibleColumnAndForeignKeyBeforeKeyDrops(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY, a INT NOT NULL, b INT NOT NULL, hidden INT INVISIBLE,
  UNIQUE KEY parents_ab (a, b));
CREATE TABLE children (id INT PRIMARY KEY, ra INT, rb INT,
  CONSTRAINT fk_ab FOREIGN KEY (ra, rb) REFERENCES parents (a, b));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
-- @migrate drop parents.b
CREATE TABLE parents (id INT PRIMARY KEY, a INT NOT NULL, hidden INT NOT NULL DEFAULT 0 INVISIBLE);
CREATE TABLE children (id INT PRIMARY KEY, ra INT, rb INT);
`)
	plan(t, ctx, "invisible column modified, composite key dropped under a child's foreign key", base, to)
}
