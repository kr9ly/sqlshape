# Migrations

[日本語](migrations.ja.md)

`schema.sql` is the only definition of the database. There are no migration files to write:
the `sqlshape` binary compares the live database with `schema.sql` and derives the DDL, checks
that the DDL really leads to `schema.sql`, and runs it. The commands work on PostgreSQL and MySQL;
the schema's declaration selects the database, and the [MySQL](#mysql) section below has what is
MySQL's about them.

```
$ sqlshape diff -db "$DSN" > up.sql         # DDL from the database's state to schema.sql
$ $EDITOR up.sql                            # reorder, split, add USING, interleave a backfill
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # drift: where a database differs from schema.sql
```

## What is compared

On PostgreSQL both sides are read through `pg_dump`, so what is compared is what PostgreSQL itself stores,
not the spelling in `schema.sql`: `'x'` and `'x'::text`, or `IN (...)` and `= ANY (ARRAY[...])`,
are not differences. `-- sqlshape:` directives are not database state and are not compared.

The comparison is object by object (tables, columns, constraints, indexes, views, functions,
types, domains, enums, triggers, rules, policies, row-security flags, sequences, extensions,
comments) and, for seeded tables, row by row.

## diff

`sqlshape diff -db DSN` prints the DDL that takes the database to `schema.sql`, in dependency
order: drops (dependents first), renames, alterations, additions, then the seed `MERGE`s. It is a
proposal: the DDL is meant to be read, and edited when the order or the form is wrong for the
data at hand (`ALTER COLUMN ... TYPE` needs a `USING`, a large table wants `CREATE INDEX
CONCURRENTLY`, a backfill belongs between two steps).

`-from other.sql` compares two schema texts instead of a database, for example the previous
release's `schema.sql`. `-packages ./...` indexes the Go statements against the current schema
and lists, as comments in the DDL, every statement that reads a column the DDL drops or retypes.

## Declaring what a diff cannot see

A diff of two schemas cannot tell a rename from a drop and an add, which label a removed enum
value should become, or what a new `NOT NULL` column should hold for existing rows. Those are
declared in `schema.sql` with `-- @migrate` lines:

```sql
-- @migrate rename orders.state -> orders.status      a column, or a table: rename old -> new
-- @migrate drop orders.legacy                        this column (or table) may go
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

The left side of a rename names things as they are now, the right as they will be. A table or
column that disappears without a `drop` or `rename` declaration is an error (the DDL is still
printed), so data loss is always announced. A declaration the diff does not bear out (a rename
whose source does not exist, a drop of a column that is still there) is an error too, so stale
declarations cannot linger: declarations describe the step from the database's current state to
`schema.sql`, and are removed once applied.

A rename becomes `RENAME TO` / `RENAME COLUMN`. A constraint or foreign key over a renamed
column is emitted as `DROP` + `ADD`, which is correct but re-validates the constraint. An enum
label removal recreates the type (PostgreSQL has no `DROP VALUE`): rename the old type, create
the new one, `ALTER COLUMN ... TYPE ... USING CASE ...` on every column carrying it, recreate the
views over those tables, drop the old type; an array column of the enum is reported as a
problem. A `backfill` is type-checked against the target schema and emitted as an `UPDATE` after
the column exists.

Partitions are schema like everything else. A new partitioned table is created with its
`PARTITION BY` and its partitions attached once they exist; a partition added to a table is a
`CREATE TABLE ... PARTITION OF ... FOR VALUES ...`; a partition that goes is a `DROP TABLE` and,
since it takes its rows, needs a `-- @migrate drop` of that table; a table becoming or ceasing
to be a partition is `ATTACH PARTITION` / `DETACH PARTITION`, a changed bound a detach and an
attach (every detach in the plan runs before any attach, so a moving bound never overlaps a
neighbour's), and the plan never alters a partition's own columns, which follow the parent's.
A table that holds rows can be partitioned after the fact, unpartitioned, or moved to another
key or strategy: the plan renames the table, creates the declared shape fresh with its
partitions, inserts every row through the parent so PostgreSQL's router places it, carries the
comments, triggers, policies, rules, inbound foreign keys and sequences over (a bigserial keeps
its sequence, an identity column continues from the highest value), and drops the old table.
Rows no partition takes are the server's error, so a `schema.sql` that does not cover the data
stops `apply` rather than losing anything.

Changes to a domain's base type, a range's subtype, `INHERITS` and `OF type` are printed as
`-- ` notes for the operator rather than as DDL.

A column's `ALTER COLUMN ... TYPE` gets one of these notes too when the new type narrows a
`numeric`'s precision or scale, a `varchar(n)` / `char(n)` length, a `time` / `timestamp` family's
fractional-second precision, or a `bit(n)` length: PostgreSQL runs a same-base-type `ALTER` without
a `USING` and without complaint, rounding or truncating the existing values to fit. The note is a
warning, not a block -- the same "proposal, edited by hand" contract as the rest of the diff -- so
running it unedited still rounds or truncates the data; add a `USING` (or fix the data first)
before applying it.

## apply

`sqlshape apply -db DSN up.sql` takes the DDL file, hand-edited or not, and checks it by its end
state: the database's current schema plus the DDL must read back as `schema.sql`. Column order is tolerated and noted (PostgreSQL cannot insert a column in
the middle, so a dropped and re-added column leaves the orders permanently different); anything
else refuses. Then the DDL runs in one transaction.

- `-packages ./...` indexes the Go statements against the current schema and refuses while any
  statement still depends on a column the DDL drops or retypes. `-force` runs anyway.
- `-dry-run` stops after the verification.
- `-no-transaction` runs the DDL as is, for `CREATE INDEX CONCURRENTLY`, `ALTER TYPE ... ADD
  VALUE` and other statements that cannot run inside a transaction.

## verify-schema

`sqlshape verify-schema -db DSN` lists where a database differs from `schema.sql`: drift, a
migration applied by hand, an environment that fell behind. Exit code 1 when there is a
difference, 0 when the database matches.

## Seeded tables (PostgreSQL)

A table whose rows are written in `schema.sql` with an ordinary `INSERT ... VALUES` is a seeded
table; its rows are part of the schema.

```sql
CREATE TABLE order_statuses (
    code       text PRIMARY KEY,
    label      text NOT NULL,
    sort_order integer NOT NULL
);
INSERT INTO order_statuses (code, label, sort_order) VALUES
    ('pending',   'Awaiting payment', 10),
    ('paid',      'Paid',             20),
    ('shipped',   'Shipped',          30),
    ('cancelled', 'Cancelled',        90);
```

The INSERT must be idempotent, so that applying the schema text twice means the same thing:
rows are identified by a key (the primary key, or a non-partial unique constraint over NOT NULL
columns) that every row gives as constants, values are constant expressions (no volatile or
stable functions, no subqueries, no `DEFAULT`), there is no `ON CONFLICT`, and no key appears
twice. Violations are schema problems; the INSERT is also type-checked like any statement.

In every comparison the declared rows are read back from the database and diffed by key, so a
row added, changed or removed in `schema.sql` shows up in `diff` and `verify-schema` like a
column would. The plan ends with one `MERGE` per table whose content differs, deleting rows the
declaration no longer lists; seeded tables joined by a foreign key are merged parent first and
their deletions child first. `-- sqlshape: seed` above the INSERT makes the seed additive: rows
the declaration does not list stay.

Removing the INSERT altogether turns the table back into an ordinary one: its rows are no longer
part of the schema, so `diff` and `verify-schema` stop reading them and the plan does not delete
them. The rows stay in the database. To empty the table, keep the seed and delete its rows first,
or write the `DELETE` yourself.

The checker reads the same rows as a value set: a Go named type that meets the key column, or a
column referencing it, is diffed against them like enum labels
([checks.md](checks.md#giving-types-a-meaning)). This is why a lookup table is the
recommended home for a value set: adding, relabelling, reordering and retiring a value are each
a one-row change and a `MERGE`, where an enum needs the type recreated under every column.

## MySQL

On MySQL both sides are read as the server's own rendering: `SHOW CREATE TABLE` and `SHOW CREATE
VIEW` for every table and view, parsed by the loader that reads `schema.sql`. What is compared is
therefore what MySQL stores, not the spelling of `schema.sql`: `INT` and `int(11)`, a default written
`0` and stored `'0'`, a key named by the server (`orders_ibfk_1`, `orders_chk_1`) are not
differences. The target's canonical form comes from applying `schema.sql` to a scratch database
(`sqlshape_scratch_<random>`) on the `-db` server, dropped when done, so it is normalized by the
very server the migration targets, version and settings (`-- sqlshape: server`) included; with
`-from` (two texts, no server) it comes from a `mysqld` on `PATH`.

Compared, object by object:

- tables: engine, charset, collation, row format, comment, and partitioning (`RANGE`, `LIST`,
  `RANGE COLUMNS`, `LIST COLUMNS`, `HASH`, `KEY`, `LINEAR`, `ALGORITHM`, subpartitioning by
  `HASH` / `KEY`, each partition's bound and comment; a form the loader does not model, such as
  a subpartition's own definition or a `TABLESPACE` option, is a problem rather than a
  difference it cannot see);
- columns: type, the whole definition as the server spells it, and their position (MySQL can
  reorder columns, so an order difference is a change the plan settles with
  `MODIFY COLUMN ... AFTER`);
- keys, foreign keys, check constraints;
- views;
- triggers, stored procedures and functions: by their definition text, read back from
  `SHOW CREATE TRIGGER` / `SHOW CREATE PROCEDURE` / `SHOW CREATE FUNCTION` with the `DEFINER`
  dropped;
- events: schedule, `STARTS` / `ENDS`, `ON COMPLETION`, status, comment and body, read back
  from `SHOW CREATE EVENT`. A time `schema.sql` leaves to the server -- a `STARTS` it omits or
  writes as an expression (`CURRENT_TIMESTAMP + INTERVAL 1 DAY`), an `AT` expression -- is
  filled in when the event is created (`SHOW CREATE EVENT` reads it back as the literal time
  of creation, measured), so it is not compared; a literal time is compared as written.

Not compared: seeded rows, which the MySQL loader does not know yet. A one-time event
(`AT ...`) without `ON COMPLETION PRESERVE` is dropped by the server once it has run, so
`verify-schema` reports it missing from then on: that is the event's own definition, not
drift. An existing table's `AUTO_INCREMENT=<n>` counter is data, not schema, and is never
compared either; a brand new table's own declared `AUTO_INCREMENT=<n>` is a schema decision
and does reach the table when it is created.

The plan uses MySQL's own definitions:

| change | DDL |
|---|---|
| a new table | the canonical `CREATE TABLE` |
| a changed column | `ALTER TABLE ... MODIFY COLUMN` with the target's definition |
| a changed key, foreign key or check | a `DROP` and an `ADD` |
| a changed view | `CREATE OR REPLACE VIEW` |
| a table gaining partitioning, or a different kind or key | `ALTER TABLE ... PARTITION BY ...` (the server redistributes the rows; a row no partition takes is its error 1526) |
| a table losing partitioning | `ALTER TABLE ... REMOVE PARTITIONING` |
| a `RANGE` partition added at the end | `ADD PARTITION` |
| a `RANGE` partition gone | `DROP PARTITION`, which takes its rows and so needs `-- @migrate drop partition orders.p0` |
| a `RANGE` bound moved, or a partition inserted before `MAXVALUE` | `REORGANIZE PARTITION ... INTO (...)` (the server moves the rows) |
| a `LIST` partition added, gone, or its value list changed | `ADD PARTITION`, `DROP PARTITION` under the same declaration, and every changed value list in one `REORGANIZE PARTITION ... INTO` (a value moving between two kept partitions never floats between statements) |
| a `HASH` or `KEY` table's partition count | `ADD PARTITION PARTITIONS n` / `COALESCE PARTITION n` |
| `LINEAR`, `KEY`'s columns or `ALGORITHM`, or the subpartitioning changed | `ALTER TABLE ... PARTITION BY ...`, the whole clause (the server redistributes the rows; none are lost) |
| a partition's `COMMENT` | `REORGANIZE PARTITION ... INTO` with the new comment (no rows move, measured) |
| a changed or removed trigger, procedure or function | a `DROP` and a `CREATE` (MySQL has no `CREATE OR REPLACE TRIGGER`) |
| a changed or removed event | a `DROP EVENT` and a `CREATE EVENT`, the target's own text (a `STARTS` it omits starts the new event when the migration runs) |

The order keeps the migration's own steps from tripping over each other:

1. a trigger's `DROP` comes before the table drops (a trigger going with a table that is itself
   dropped is not listed: `DROP TABLE` takes it silently);
2. a table that goes has the foreign keys referencing it dropped first;
3. a routine's `CREATE` comes before the views (a view may call a function) and before the tables;
4. a trigger's `CREATE` comes after the backfills, so a newly added trigger does not fire on the
   migration's own writes.

The `-- @migrate` declarations are the same, with two differences: an ENUM is a column type on
MySQL, so `enum` names the column (`-- @migrate enum orders.status: drop 'canceled' using
'cancelled'`), and the plan updates the rows before it narrows the type; and a partition is not
a table, so dropping one is declared as `-- @migrate drop partition orders.p0`.

A table's `DEFAULT CHARSET` / `COLLATE` change also re-issues `MODIFY COLUMN` for every string
column without a collation of its own: the table option alone leaves such columns in the old
encoding (measured), and the plan after `apply` would never be empty.

`apply` runs the DDL statement by statement: MySQL's DDL commits implicitly, so a script is not a
transaction and `-no-transaction` has no effect. When a statement fails, `apply` says which one
and how many before it are applied; `sqlshape diff` from that state gives what remains.

## How the plan is tested

The DDL `diff` writes is judged against a real server, the way `apply` would run it, by a
generator rather than by hand-picked cases (`check/postgres/migrate` and `check/mysql/migrate`,
`TestMigrateProbe`). From the planner's vocabulary it draws a schema, mutates it one to five
times into a target (each mutation writes its own `-- @migrate` declaration), fills every table
with three rows, and runs the plan on a server holding the source. A pair passes when the server
refuses nothing, what it reads back afterwards is the target's canonical form (column order
aside on PostgreSQL), and a second plan from there is empty. A failing pair is minimized and the
fix is pinned as a regression test with the server's error (`probe_findings_test.go`). Besides
the random pairs, every mutation is applied alone once per run, so no kind of change depends on
the draw. PostgreSQL's probe runs against 17 and against 18 (`TestMigrateProbe18`), each with
that version's own vocabulary.

What the generator has to reach is defined, not guessed: a plan's whole input is the diff, so
every kind of change the diff can report (a table added, a column's type changed, a constraint
turning deferrable, ...) is enumerated from the diff's own comparison functions, and the gate
(200 pairs of one fixed seed) fails unless each of them is produced by some pair or listed as
unreachable with a reason. Only two reasons are accepted: the planner writes no DDL for that
change and reports it instead (PostgreSQL's `INHERITS`, `OF type`, a domain's base type, a
range's subtype, and the partition key of a table holding rows), or the change cannot appear
(PostgreSQL 18 syntax against a PostgreSQL 17 server, or a change pg_dump never renders as
such). Combinations and orderings are left to the random pairs. PostgreSQL reaches 94 of 103
kinds on 17 and 98 on 18, MySQL all 48; every unreached kind is a problem the plan stops on or
a change that cannot appear. The classes of planner bugs the probe found, most of them orderings
a real server refuses, are the regression tests.

What this does not reach: schema shapes outside the generator's model (legacy spellings,
extension types, very large tables) and failures that depend on the data (a backfill's values,
lock time). Those arrive with your `schema.sql`, and `apply`'s end check before it runs anything
is what catches them.

## Requirements

PostgreSQL:

- `pg_dump` on `PATH` or named by `$SQLSHAPE_PG_DUMP`; its major version must be at least the
  database's.
- The comparisons run `schema.sql` on a private PostgreSQL of the major version the schema
  declares (`-- sqlshape: postgres 17`), which `sqlshape` downloads on first use and caches under
  `~/.cache/sqlshape` (`$SQLSHAPE_PG_CACHE`). The first run takes a few seconds for the download;
  afterwards it starts in a quarter of a second. Your database is never used for this. When the
  database runs another major version than the schema declares, the commands say so on stderr
  and go on: the DDL is judged by the declared version's rules.

MySQL:

- With `-db`, the connection's user can `CREATE DATABASE` and `DROP DATABASE` (for the scratch
  database). The server's `lower_case_table_names` must be the one the schema declares (0 when
  it declares none); the commands stop otherwise. When the server runs another MySQL version than
  the schema declares, the commands say so and go on.
- With `-from` (two schema texts), a `mysqld` on `PATH` (`nix-shell -p mysql84`, a distribution
  package, a server tarball's `bin/`), started with the schema's declared settings.

Both:

- `-schema PATH` names `schema.sql`, or a `schema/` directory whose `*.sql` files apply in name
  order; the default is the nearest one from the working directory up.

Exit codes: 0 no difference, 1 a finding (a diff, a drift, a refused apply), 2 a usage or
environment error.
