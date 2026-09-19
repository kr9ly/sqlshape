package migrate

// Adversarial round 2 for lane "migrate": dump / diff / migrate round trips, checked against a
// real mysqld 8.4 (nix-shell -p mysql84). See
// /tmp/claude-1000/-home-kr9ly-projects-sqlshape/3fb800a8-0c4b-48f3-83cd-e5fb3ffcc642/scratchpad/adv2/report-migrate.md
// for the write-up. Every test states the behavior a real server measurably has; the current
// planner disagrees with it (a DDL statement the server refuses, or Verify/verify-schema
// reporting "matches" when it silently does not), which is exactly why each test fails today.
//
// Common shape: mustCanonical(sql) builds a canonical schema+text (applies sql to a fresh
// mysqld and reads it back via SHOW CREATE ...); Plan(from, to, intents) is the planner under
// test; Verify replays the plan's DDL on a fresh server holding `from` and diffs the result
// against `to`. Every test here asserts what Verify *should* report for a schema edit that a
// real mysqld accepts perfectly well written any other way (as one CREATE TABLE, in the other
// statement order, combined into one ALTER, ...): no error and no leftover changes. Each
// assertion fails today because Verify's own Canonical call surfaces the real mysqld error the
// plan's DDL provokes, quoted in the doc comment and in the failure message.

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// TestAdv2AddedGeneratedColumnDependsOnLaterColumn: adding a generated column and the column
// it reads together is fine as one CREATE TABLE (MySQL resolves a generated column's
// expression against the whole table definition, not declaration order), but the planner adds
// new columns one at a time in the target's column order via separate `ALTER TABLE ... ADD
// COLUMN ... AFTER` statements (addParts, migrate.go:550-566). When the target orders a
// generated column *before* the plain column its expression reads (a natural layout choice --
// the computed field shown first), the planner faithfully reproduces that order in the ADD
// sequence, and the generated column's ADD runs while its dependency does not exist in the
// table yet.
//
// Measured against mysqld 8.4: the plan
//
//	ALTER TABLE `t` ADD COLUMN `g` int GENERATED ALWAYS AS ((`x` + 1)) VIRTUAL AFTER `id`;
//	ALTER TABLE `t` ADD COLUMN `x` int DEFAULT NULL AFTER `g`;
//
// fails on the first statement with Error 1054 (42S22): Unknown column 'x' in 'generated
// column function' -- the exact fragment `sqlshape diff` would print and `sqlshape apply`
// would try to run verbatim, on a schema change that is completely valid MySQL (as one CREATE
// TABLE, or as two ALTERs in the other order). sqlshape's current behavior: Plan returns this
// DDL with no error at all (nothing detects the dependency or reorders/regroups the ADDs), so
// the failure is only discovered when apply actually runs it against a live database.
func TestAdv2AddedGeneratedColumnDependsOnLaterColumn(t *testing.T) {
	ctx := start(t)
	baseSQL := "-- sqlshape: mysql 8.4\nCREATE TABLE t (id INT PRIMARY KEY);\n"
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := "-- sqlshape: mysql 8.4\n" +
		"CREATE TABLE t (id INT PRIMARY KEY, g INT GENERATED ALWAYS AS (x + 1) VIRTUAL, x INT);\n"
	to := mustCanonical(t, ctx, toSQL)

	ddl, err := Plan(base.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	// A generated column and the plain column it reads, added together, is a schema change a
	// real server accepts (as one CREATE TABLE, or as these same two ADD COLUMNs the other way
	// round): Verify should reach the target cleanly.
	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Fatalf("the plan's own DDL should apply cleanly to a real server, got: %v\nDDL:\n%s", verr, joined)
	}
	if len(changes) != 0 {
		t.Errorf("changes=%v", changes)
	}
}

// TestAdv2DroppedColumnStillReadByGeneratedSibling is the drop-side mirror of
// TestAdv2AddedGeneratedColumnDependsOnLaterColumn: dropParts (migrate.go:212-261) collects
// goneCols and emits one `DROP COLUMN` per column in the *from* table's own declaration order
// (migrate.go:258-260), with no attempt to drop a generated column ahead of the plain column
// it reads. Both columns going together in one edit is unremarkable (the whole feature is
// removed), but the from-table happened to declare the plain column before the generated one,
// so the plan drops the plain column first.
//
// Measured against mysqld 8.4: the plan
//
//	ALTER TABLE `t` DROP COLUMN `x`;
//	ALTER TABLE `t` DROP COLUMN `g`;
//
// fails on the first statement with Error 3108 (HY000): Column 'x' has a generated column
// dependency -- MySQL refuses to drop a column any live generated column still reads, even
// though the plan (correctly, per its own `-- @migrate drop` declarations) intends to drop
// both. sqlshape's current behavior: Plan returns this DDL with no error and no reordering.
func TestAdv2DroppedColumnStillReadByGeneratedSibling(t *testing.T) {
	ctx := start(t)
	baseSQL := "-- sqlshape: mysql 8.4\n" +
		"CREATE TABLE t (id INT PRIMARY KEY, x INT, g INT GENERATED ALWAYS AS (x + 1) VIRTUAL);\n"
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := "-- sqlshape: mysql 8.4\n-- @migrate drop t.x\n-- @migrate drop t.g\n" +
		"CREATE TABLE t (id INT PRIMARY KEY);\n"
	to := mustCanonical(t, ctx, toSQL)
	in, err := ParseIntents(toSQL)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}

	ddl, err := Plan(base.s, to.s, in)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	// Both columns declared droppable together (a whole feature removed): Verify should reach
	// the target cleanly.
	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Fatalf("the plan's own DDL should apply cleanly to a real server, got: %v\nDDL:\n%s", verr, joined)
	}
	if len(changes) != 0 {
		t.Errorf("changes=%v", changes)
	}
}

// TestAdv2WidenedForeignKeyColumnPairFailsMidPlan: widening a parent's primary key and the
// child column that references it (INT -> BIGINT on both sides of a foreign key) is a normal,
// reachable schema change, but the planner alters one table at a time (alters, migrate.go:386-
// 399) in the order the canonical schema lists tables -- alphabetical, since dump.Read lists
// `information_schema.TABLES ... ORDER BY TABLE_NAME` (dump.go:71). Nothing coordinates the two
// tables' MODIFY COLUMNs, so widening "acct" (parent) runs to completion while "txn" (child)
// still has the old, narrower type, even though both statements are in the very same plan.
//
// Measured against mysqld 8.4: the plan
//
//	ALTER TABLE `acct` MODIFY COLUMN `id` bigint NOT NULL;
//	ALTER TABLE `txn` MODIFY COLUMN `acct_id` bigint DEFAULT NULL;
//
// fails on the first statement with Error 3780 (HY000): Referencing column 'acct_id' and
// referenced column 'id' in foreign key constraint 'txn_ibfk_1' are incompatible -- MySQL
// checks FK type compatibility as soon as either side changes, so the parent's widening alone
// is rejected while the still-narrow child column is still attached by the foreign key.
// sqlshape's current behavior: Plan returns this DDL with no error and no note that the two
// statements must not be split (e.g. by dropping and re-adding the foreign key around them, as
// the plan already knows how to do for other reasons).
func TestAdv2WidenedForeignKeyColumnPairFailsMidPlan(t *testing.T) {
	ctx := start(t)
	baseSQL := `-- sqlshape: mysql 8.4
CREATE TABLE acct (id INT PRIMARY KEY);
CREATE TABLE txn (id INT PRIMARY KEY, acct_id INT, FOREIGN KEY (acct_id) REFERENCES acct(id));
`
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := `-- sqlshape: mysql 8.4
CREATE TABLE acct (id BIGINT PRIMARY KEY);
CREATE TABLE txn (id INT PRIMARY KEY, acct_id BIGINT, FOREIGN KEY (acct_id) REFERENCES acct(id));
`
	to := mustCanonical(t, ctx, toSQL)

	ddl, err := Plan(base.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	// Widening a parent PK and its child FK column together is a normal edit: Verify should
	// reach the target cleanly.
	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Fatalf("the plan's own DDL should apply cleanly to a real server, got: %v\nDDL:\n%s", verr, joined)
	}
	if len(changes) != 0 {
		t.Errorf("changes=%v", changes)
	}
}

// TestAdv2PrimaryKeyReplacementBreaksAutoIncrementInvariant: MySQL requires an AUTO_INCREMENT
// column to be the leading column of *some* index at all times (Error 1075 the moment it isn't
// -- an invariant checked per-statement, not only at the end of a script). Moving AUTO_INCREMENT
// off the primary key (a new business key becomes PRIMARY, the old surrogate key keeps
// AUTO_INCREMENT under its own secondary index) is a legitimate, if unusual, edit. drops()
// emits `DROP PRIMARY KEY` for the table (dropParts, migrate.go:238-248) before adds() gets to
// emit the new secondary `KEY` on the auto_increment column (addParts, migrate.go:567-574) --
// Plan never checks whether an intermediate statement would leave the auto_increment column
// without any index.
//
// Measured against mysqld 8.4: the plan
//
//	ALTER TABLE `t` DROP PRIMARY KEY;
//	ALTER TABLE `t` ADD PRIMARY KEY (`code`);
//	ALTER TABLE `t` ADD KEY `t_id_idx` (`id`);
//
// fails with Error 1075 (42000): Incorrect table definition; there can be only one auto column
// and it must be defined as a key -- the DROP PRIMARY KEY statement itself is rejected, since
// `id` (AUTO_INCREMENT) has no other index at that point. sqlshape's current behavior: Plan
// returns this DDL with no error; the "auto_increment column must be indexed" invariant is
// never checked and the plan never orders the new secondary key ahead of the old PK's drop
// (or combines them into one ALTER TABLE, which MySQL does allow).
func TestAdv2PrimaryKeyReplacementBreaksAutoIncrementInvariant(t *testing.T) {
	ctx := start(t)
	baseSQL := "-- sqlshape: mysql 8.4\nCREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, code INT NOT NULL);\n"
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := "-- sqlshape: mysql 8.4\n" +
		"CREATE TABLE t (id INT AUTO_INCREMENT, code INT NOT NULL, PRIMARY KEY (code), KEY t_id_idx (id));\n"
	to := mustCanonical(t, ctx, toSQL)

	ddl, err := Plan(base.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	// Moving AUTO_INCREMENT off the primary key onto its own secondary index is a valid end
	// state (mysqld accepts it as a single ALTER TABLE with both clauses together, measured
	// separately): Verify should reach it cleanly.
	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Fatalf("the plan's own DDL should apply cleanly to a real server, got: %v\nDDL:\n%s", verr, joined)
	}
	if len(changes) != 0 {
		t.Errorf("changes=%v", changes)
	}
}

// TestAdv2RenamedPrimaryKeyColumnDropsKeyNeedlessly: dropParts' handling of a foreign key
// translates both sides' columns through the declared renames before comparing
// (`p.renamedFK(f, fk)`, migrate.go:234), and addParts' handling of a *key* does the same
// (`renamedKey(p, f, fk)`, migrate.go:570) -- but dropParts' own key loop compares
// `diff.KeyProps(k)` (the from-side key, in the from column's own names) directly against
// `diff.KeyProps(tk)` (the target key, already in the target's post-rename names) with no
// translation at all (migrate.go:239-248). A `-- @migrate rename` of a primary-key column
// therefore makes the *same* key -- unchanged in every way but the column's declared name --
// look "changed" to dropParts, and the plan drops the primary key it should be leaving alone.
//
// Measured against mysqld 8.4, renaming customers.id -> customers.cust_id (a primary key a
// child table's foreign key still references, unchanged), the plan
//
//	ALTER TABLE `customers` DROP PRIMARY KEY;
//	ALTER TABLE `customers` RENAME COLUMN `id` TO `cust_id`;
//
// fails on the first statement with Error 1553 (HY000): Cannot drop index 'PRIMARY': needed in
// a foreign key constraint -- the primary key was never supposed to be dropped at all (the
// rename alone reaches the target: RENAME COLUMN carries the primary key and the referencing
// foreign key along with it, measured separately). sqlshape's current behavior: Plan returns
// this DDL with no error; the spurious DROP PRIMARY KEY is emitted purely because dropParts'
// key comparison, unlike its own foreign-key comparison right above it, does not know about
// `-- @migrate rename`.
func TestAdv2RenamedPrimaryKeyColumnDropsKeyNeedlessly(t *testing.T) {
	ctx := start(t)
	baseSQL := `-- sqlshape: mysql 8.4
CREATE TABLE customers (id INT PRIMARY KEY);
CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, FOREIGN KEY (customer_id) REFERENCES customers(id));
`
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := `-- sqlshape: mysql 8.4
-- @migrate rename customers.id -> customers.cust_id
CREATE TABLE customers (cust_id INT PRIMARY KEY);
CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, FOREIGN KEY (customer_id) REFERENCES customers(cust_id));
`
	to := mustCanonical(t, ctx, toSQL)
	in, err := ParseIntents(toSQL)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}

	ddl, err := Plan(base.s, to.s, in)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	// A bare column rename should not touch the primary key at all: RENAME COLUMN alone
	// carries the primary key and the referencing foreign key along with it (measured
	// separately: a schema whose only edit is the rename, applied as RENAME COLUMN with no
	// DROP/ADD PRIMARY KEY around it, reaches the target with Verify reporting no changes).
	if strings.Contains(joined, "DROP PRIMARY KEY") {
		t.Errorf("a bare column rename must not drop and re-add the primary key it renames "+
			"(a needless statement a child foreign key refuses outright -- see Verify below), got:\n%s", joined)
	}

	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Errorf("consequence of the spurious DROP PRIMARY KEY above: the plan's own DDL is "+
			"refused by a real server (%v)\nDDL:\n%s", verr, joined)
	} else if len(changes) != 0 {
		t.Errorf("changes=%v", changes)
	}
}

// TestAdv2ExplicitAutoIncrementStartSilentlyDropped: dump.normalizeTable (dump.go:284-291)
// strips the `AUTO_INCREMENT=<n>` table option from every canonicalized CREATE TABLE text --
// including a table's own `Definition`, the text migrate.adds() emits verbatim for a brand new
// table (migrate.go:530-531). That is the right call for a table that already holds rows (the
// counter is data), but it also erases an author's explicitly declared starting value for a
// table that does not exist yet, which is a schema decision (avoiding overlapping IDs across
// shards or environments is a common reason to write `AUTO_INCREMENT=1000` on a brand new
// table). The value is not merely left out of the plan's CREATE TABLE -- it is invisible to
// every later comparison too: TableProps (diff.go:292-300) has no field for it, so
// diff.Compare and verify-schema report a table created this way as matching the schema.sql
// that asked for 1000 exactly as well as one that never mentioned an AUTO_INCREMENT start at
// all.
//
// Measured against mysqld 8.4: creating `t` from a schema.sql that declares `AUTO_INCREMENT =
// 1000` yields a table whose own canonical CREATE TABLE text (SHOW CREATE TABLE, read right
// back) already has no AUTO_INCREMENT clause -- the counter reads back as 1, the server's
// default, not 1000. sqlshape's current behavior: Plan's CREATE TABLE for `t` carries no
// AUTO_INCREMENT clause, Verify (and, doing the same comparison, verify-schema) reports zero
// changes, and there is no note anywhere that the declared starting value never took effect.
func TestAdv2ExplicitAutoIncrementStartSilentlyDropped(t *testing.T) {
	ctx := start(t)
	baseSQL := "-- sqlshape: mysql 8.4\nCREATE TABLE placeholder (id INT PRIMARY KEY);\n"
	base := mustCanonical(t, ctx, baseSQL)
	toSQL := "-- sqlshape: mysql 8.4\nCREATE TABLE placeholder (id INT PRIMARY KEY);\n" +
		"CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v INT) AUTO_INCREMENT=1000;\n"
	to := mustCanonical(t, ctx, toSQL)

	ddl, err := Plan(base.s, to.s, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	joined := strings.Join(ddl, "\n")
	t.Logf("plan:\n%s", joined)

	changes, verr := Verify(ctx, dump.Local{}, base.text, joined, to.s)
	if verr != nil {
		t.Fatalf("verify: %v\nDDL:\n%s", verr, joined)
	}
	if len(changes) != 0 {
		// If this ever starts failing because AUTO_INCREMENT becomes a compared property,
		// that is the fix landing, not a new bug: update this test's expectation.
		t.Fatalf("compare already reports the counter as unmatched (fix may have landed): %v", changes)
	}

	// Ground truth: actually run the plan against a fresh mysqld holding base.text, insert a
	// row into the freshly created table with no explicit id, and read back the id the server
	// itself assigned.
	srv, err := mysqltest.Start(ctx, base.text+"\n"+joined)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer srv.Close()
	if _, err := srv.Conn().ExecContext(ctx, "INSERT INTO t (v) VALUES (1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var id int64
	if err := srv.Conn().QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}
	t.Logf("first row's assigned id: %d (schema.sql declared AUTO_INCREMENT=1000)", id)
	if id != 1000 {
		t.Errorf("expected the first row of a table created with AUTO_INCREMENT=1000 to get id "+
			"1000 (and for verify-schema to have said so, one way or the other, instead of "+
			"reporting a clean match), got %d -- verify already reported changes=%v (empty) above", id, changes)
	}
}
