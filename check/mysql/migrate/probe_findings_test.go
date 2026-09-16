package migrate

// What the migrate probe (probe_test.go) found and the planner now does, each pinned as the
// minimized pair the probe reported, replayed against a real mysqld the way `plan` does:
// the DDL applies cleanly and reads back as the target.

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
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

// A renamed column that also participates in a foreign key (here, the parent side a child's
// constraint references) and is read by a generated column: the RENAME COLUMN + MODIFY COLUMN
// combination Error 3108 forces is itself refused by the server's default algorithm (Error
// 1846 "ALGORITHM=COPY is not supported ... Columns participating in a foreign key are
// renamed. Try ALGORITHM=INPLACE", measured); the same ALTER TABLE with ALGORITHM=INPLACE
// succeeds.
func TestProbeRenamedForeignKeyColumnReadByGeneratedColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, g BIGINT GENERATED ALWAYS AS (id + 1) STORED);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES t (id));
`)
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate rename t.id -> t.id_new
CREATE TABLE t (id_new INT PRIMARY KEY, g BIGINT GENERATED ALWAYS AS (id_new + 1) STORED);
CREATE TABLE children (id INT PRIMARY KEY, parent_id INT,
  CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES t (id_new));
`
	to := mustCanonical(t, ctx, toSQL)
	plan(t, ctx, "renamed foreign-key column read by a generated column", base, to)
}

// A column newly gaining ON UPDATE CURRENT_TIMESTAMP fires on any later UPDATE against the
// row, even one naming only an unrelated column (a fresh column's backfill): every row the
// backfill's UPDATE touches gets the same instant, which a UNIQUE key over the auto-updating
// column then refuses as a duplicate (Error 1062 "Duplicate entry", measured) if the MODIFY
// that adds it runs ahead of the backfill in the same plan. The MODIFY must wait until every
// row-touching statement in the plan is done (deferredMods).
func TestProbeOnUpdateModifyBeforeBackfill(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, ts DATETIME(6), UNIQUE KEY t_ts (ts));
`)
	rows := "INSERT INTO t (id, ts) VALUES (1, '2024-01-01 00:00:00'), (2, '2024-01-02 00:00:00'), (3, NULL);\n"
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate backfill t.a = id
CREATE TABLE t (id INT PRIMARY KEY, ts DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  a INT NOT NULL DEFAULT 0, UNIQUE KEY t_ts (ts));
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

// The primary key going, with a surviving invisible UNIQUE key: InnoDB promotes a UNIQUE key
// over only NOT NULL columns to substitute clustering key the moment no PRIMARY KEY is left,
// and an invisible index cannot serve as one (Error 3522 "A primary key index cannot be
// invisible", measured) -- not only on the DROP PRIMARY KEY itself, but at a later MODIFY in
// the same window that completes such a key's column list (b here turns NOT NULL, under its
// declared backfill, after the DROP and before the new PRIMARY KEY goes back on). The
// planner forces every invisible UNIQUE key VISIBLE for that whole window and restores
// INVISIBLE, once the target's own PRIMARY KEY exists, at the very end of the plan
// (invisibleUniqueKeysOf, deferredMods).
func TestProbeInvisibleUniqueKeyBeforePrimaryKeyDrop(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a TEXT NOT NULL, b SMALLINT, PRIMARY KEY (id),
  UNIQUE KEY t_ab (a(10), b) INVISIBLE);
`)
	rows := "INSERT INTO t (id, a, b) VALUES (1, 'x', 1), (2, 'y', 2), (3, 'z', NULL);\n"
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate backfill t.b = 9 where b is null
CREATE TABLE t (id INT, a TEXT NOT NULL, b SMALLINT NOT NULL, PRIMARY KEY (b),
  UNIQUE KEY t_ab (a(10), b) INVISIBLE);
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

// A key whose only change is going invisible (or back): DROP INDEX + ADD, the recreate path
// every other key change takes, silently keeps the old visibility on this server (measured:
// neither statement errors, but the result reads back the same as before either way) rather
// than applying the new one -- only a plain ALTER INDEX ... [NOT] VISIBLE actually flips it.
func TestProbeKeyVisibilityOnlyChangeUsesAlterIndex(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY, a INT NOT NULL, b INT NOT NULL, UNIQUE KEY parents_ab (a, b));
CREATE TABLE children (id INT PRIMARY KEY, ra INT, rb INT,
  CONSTRAINT fk_ab FOREIGN KEY (ra, rb) REFERENCES parents (a, b));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE parents (id INT PRIMARY KEY, a INT NOT NULL, b INT NOT NULL, UNIQUE KEY parents_ab (a, b) INVISIBLE);
CREATE TABLE children (id INT PRIMARY KEY, ra INT, rb INT,
  CONSTRAINT fk_ab FOREIGN KEY (ra, rb) REFERENCES parents (a, b));
`)
	plan(t, ctx, "a key turning invisible, needed by another table's foreign key", base, to)
}

// A key the plan itself is turning invisible (keyVisAlters), not merely one already
// invisible and surviving: the same Error 3522 hits if that key leads with only NOT NULL
// columns and no PRIMARY KEY exists at the moment it goes invisible -- here the key is over
// the very column the PRIMARY KEY is moving onto, so it must wait until the target's own
// PRIMARY KEY exists (the very end of the plan), not run right after alters().
func TestProbeKeyTurningInvisibleWaitsForPrimaryKey(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a TINYINT(1) NOT NULL, PRIMARY KEY (id), UNIQUE KEY t_a (a));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT, a TINYINT(1) NOT NULL, PRIMARY KEY (a), UNIQUE KEY t_a (a) INVISIBLE);
`)
	plan(t, ctx, "a key over the primary key's replacement turning invisible", base, to)
}

// The primary key moving off an AUTO_INCREMENT column: dropKeysOf's own fold (coverEarly)
// adds the AUTO_INCREMENT column's replacement key in the same statement as DROP PRIMARY
// KEY (Error 1075 otherwise); when the target declares that replacement key itself
// INVISIBLE, adding it already invisible hits the same Error 3522 an already-invisible
// surviving key does (invisibleUniqueKeysOf) -- coverEarly must add it VISIBLE and let
// keyVisAlters turn it invisible once the target's own PRIMARY KEY exists.
func TestProbeCoverEarlyKeyInvisibleFromCreation(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL AUTO_INCREMENT, a TINYINT(1) NOT NULL, PRIMARY KEY (id));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL AUTO_INCREMENT, a TINYINT(1) NOT NULL, PRIMARY KEY (a),
  UNIQUE KEY t_id (id) INVISIBLE);
`)
	plan(t, ctx, "the primary key moving off an auto_increment column onto an invisible replacement", base, to)
}

// Canonicalizing must be idempotent: an ENUM column under a table with its own DEFAULT
// CHARSET / COLLATE, freshly created from raw declarative SQL, omits an explicit CHARACTER
// SET on the column (COLLATE alone, matching the table default); reading the same column
// back from an existing table (SHOW CREATE TABLE, canonicalizing a second time) always
// spells both (measured against mysqld 8.4). Without stripRedundantCharset (diff.go) and
// clearing Type.Charset once Column.Collation is set (schema.go's column()), a plan
// comparing the two spellings of the same column would see one as changed forever --
// pinned two ways: canonicalizing the same schema twice must compare equal (diff.Compare
// empty both ways), and a plan between them must be empty.
func TestProbeCanonicalizeIdempotentUnderTableCharset(t *testing.T) {
	ctx := start(t)
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, c ENUM('a','b') NOT NULL DEFAULT 'a')
  ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
`
	first := mustCanonical(t, ctx, sql)
	second := mustCanonical(t, ctx, first.text)
	if changes := diff.Compare(first.s, second.s); len(changes) > 0 {
		var d []string
		for _, ch := range changes {
			d = append(d, ch.String())
		}
		t.Fatalf("canonicalizing twice does not compare equal:\n%s\nfirst:\n%s\nsecond:\n%s",
			strings.Join(d, "\n"), first.text, second.text)
	}
	if changes := diff.Compare(second.s, first.s); len(changes) > 0 {
		var d []string
		for _, ch := range changes {
			d = append(d, ch.String())
		}
		t.Fatalf("canonicalizing twice does not compare equal (reversed):\n%s", strings.Join(d, "\n"))
	}
	ddl, err := Plan(first.s, second.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(ddl) > 0 {
		t.Errorf("plan between two canonicalizations of the same schema is not empty:\n%s", strings.Join(ddl, "\n"))
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

// A table's primary key moving onto a new column that the same target also partitions by:
// alterTable used to write the ALTER TABLE ... PARTITION BY in the same early pass as the
// other table options, ahead of the ADD COLUMN and the backfill that give the row its
// value (Error 1054 "Unknown column 'c' in 'partition function'", measured) -- partitionAlters
// now holds every table's partitioning change for its own late phase, after adds() and
// backfills() have run.
func TestProbePartitionByNewPrimaryKeyColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT NOT NULL);
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
-- @migrate backfill t.c = id
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, c BIGINT UNSIGNED NOT NULL, PRIMARY KEY (c))
PARTITION BY HASH (c) PARTITIONS 2;
`)
	plan(t, ctx, "primary key onto a new column the target also partitions by", base, to)
}

// A table's DEFAULT CHARSET / COLLATE changing while it still holds a plain (no explicit
// per-column COLLATE) varchar column: the column's real encoding is the table's own default
// at the moment it was last touched (ALTER TABLE ... DEFAULT CHARSET= alone never
// retroactively converts an existing column, measured), so alterTable's MODIFY COLUMN over
// it (implicitCharsetChange) is what makes the column actually converge on the new default,
// not only the table option itself -- without it a second Plan from the applied result is
// not empty (SHOW CREATE TABLE starts spelling the column's now-mismatched charset out
// explicitly, which the target's own canonical, authored fresh under the new default, never
// does). A column that already spells its own COLLATE (b) is untouched either way.
func TestProbeTableCharsetConvergesImplicitColumn(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a VARCHAR(20) NOT NULL, b VARCHAR(20) COLLATE utf8mb4_bin NOT NULL)
  ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a VARCHAR(20) NOT NULL, b VARCHAR(20) COLLATE utf8mb4_bin NOT NULL)
  ENGINE=InnoDB DEFAULT CHARSET=latin1 COLLATE=latin1_swedish_ci;
`)
	plan(t, ctx, "table charset over a plain column", base, to)
}

// LIST partitioning, structured the same way RANGE already is (schema.Partitioning.Kind
// "LIST", Partition.Bound its own VALUES IN list): adding a partition, dropping one declared
// (`-- @migrate drop partition`), and a value moving from one named partition to another
// (both partitions kept, migrate.go's alterListPartitioning reorganizing them together in
// one REORGANIZE PARTITION so the row in between never has nowhere to go).
func TestProbeListPartitionAddDropMove(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY LIST (id)
(PARTITION p0 VALUES IN (1,2), PARTITION p1 VALUES IN (3));
`)
	rows := "\nINSERT INTO t (id, a) VALUES (1,1),(2,2),(3,3);\n"

	// add: a partition covering values no row holds yet
	toAdd := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY LIST (id)
(PARTITION p0 VALUES IN (1,2), PARTITION p1 VALUES IN (3), PARTITION p2 VALUES IN (4,5));
`)
	plan(t, ctx, "list partition: add", canonical{s: base.s, text: base.text + rows, intents: base.intents}, toAdd)

	// drop: the added partition again, declared (no row of the source above ever lands there)
	toDrop := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
-- @migrate drop partition t.p2
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY LIST (id)
(PARTITION p0 VALUES IN (1,2), PARTITION p1 VALUES IN (3));
`)
	addedRows := "\nINSERT INTO t (id, a) VALUES (1,1),(2,2),(3,3);\n"
	plan(t, ctx, "list partition: drop", canonical{s: toAdd.s, text: toAdd.text + addedRows, intents: toAdd.intents}, toDrop)

	// move: id 2's own value moves from p0 to p1, both partitions kept, one REORGANIZE
	toMove := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY LIST (id)
(PARTITION p0 VALUES IN (1), PARTITION p1 VALUES IN (2,3));
`)
	plan(t, ctx, "list partition: value moves between two named partitions", canonical{s: base.s, text: base.text + rows, intents: base.intents}, toMove)
}

// KEY partitioning, structured (schema.Partitioning.Kind "KEY", Cols the column list, no
// Expr): PARTITIONS n changes the same ADD PARTITION PARTITIONS n / COALESCE PARTITION n as
// HASH's own (migrate.go's alterPartitioning treats the two kinds alike), and LINEAR or
// ALGORITHM changing is a whole rewrite (partitioningKeyChanged), not an incremental one.
func TestProbeKeyPartitionCountAndAlgorithm(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY KEY (id) PARTITIONS 2;
`)
	rows := "\nINSERT INTO t (id, a) VALUES (1,1),(2,2),(3,3);\n"
	base = canonical{s: base.s, text: base.text + rows, intents: base.intents}

	// count: ADD PARTITION PARTITIONS n, the server redistributing every row
	toMore := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY KEY (id) PARTITIONS 4;
`)
	plan(t, ctx, "key partition: count grows", base, toMore)

	// LINEAR flips on: the clause's own whole rewrite, not an ADD PARTITION
	toLinear := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY LINEAR KEY (id) PARTITIONS 4;
`)
	plan(t, ctx, "key partition: LINEAR", base, toLinear)

	// ALGORITHM changes: also a whole rewrite (KEY's own hashing function itself changes)
	toAlgo := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY KEY ALGORITHM=1 (id) PARTITIONS 4;
`)
	plan(t, ctx, "key partition: ALGORITHM", base, toAlgo)
}

// RANGE COLUMNS partitioning (schema.Partitioning.Columns true, Cols the column list):
// spells VALUES LESS THAN a single-column bound the same way a plain RANGE does (measured),
// so alterRangePartitioning's own ADD / DROP / REORGANIZE reach it unchanged; this pins that
// the COLUMNS header itself round-trips and that an ADD PARTITION plans correctly over it.
func TestProbeRangeColumnsPartitionAdd(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE COLUMNS (id)
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100));
`)
	to := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE COLUMNS (id)
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100), PARTITION p2 VALUES LESS THAN MAXVALUE);
`)
	plan(t, ctx, "range columns partition: add", base, to)
}

// A RANGE Partitioning's own SUBPARTITION BY HASH: ADD PARTITION / REORGANIZE PARTITION
// take the same DDL this package already writes for a plain RANGE clause (the server splits
// the new partition into the default subpartition count on its own, measured), so this pins
// that alterRangePartitioning reaches a subpartitioned table unchanged, and that the
// SUBPARTITION BY clause's own count changing, or going away entirely, is a whole rewrite
// (subPartitioningChanged).
func TestProbeSubpartitionedRangeAddAndRewrite(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE (id)
SUBPARTITION BY HASH (id)
SUBPARTITIONS 2
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100));
`)
	rows := "\nINSERT INTO t (id, a) VALUES (1,1),(2,2),(3,3);\n"
	base = canonical{s: base.s, text: base.text + rows, intents: base.intents}

	// add: the new partition splits into 2 subpartitions by default, no explicit SUBPARTITION list
	toAdd := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE (id)
SUBPARTITION BY HASH (id)
SUBPARTITIONS 2
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100), PARTITION p2 VALUES LESS THAN MAXVALUE);
`)
	plan(t, ctx, "subpartitioned range: add partition", base, toAdd)

	// the subpartition count itself changing: a whole rewrite, not an incremental one
	toCount := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE (id)
SUBPARTITION BY HASH (id)
SUBPARTITIONS 4
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100));
`)
	plan(t, ctx, "subpartitioned range: subpartition count changes", base, toCount)

	// SUBPARTITION BY dropped entirely: also a whole rewrite
	toNone := mustCanonical(t, ctx, `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL, a INT NOT NULL, PRIMARY KEY (id))
PARTITION BY RANGE (id)
(PARTITION p0 VALUES LESS THAN (2), PARTITION p1 VALUES LESS THAN (100));
`)
	plan(t, ctx, "subpartitioned range: SUBPARTITION BY removed", base, toNone)
}
