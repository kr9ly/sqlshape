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

Read this page before a change to `schema.sql` goes to a database. The first time through,
[Before you start](#before-you-start) and the commands ([diff](#diff),
[apply](#apply), [verify-schema](#verify-schema)) are enough; when several environments take
the same change, [Several environments, and rolling back](#several-environments-and-rolling-back)
says how to run it.

## Before you start

### PostgreSQL

- `pg_dump` on `PATH` or named by `$SQLSHAPE_PG_DUMP`; its major version must be at least the
  database's. `diff -from` needs it too, without a database: the comparisons read their own
  PostgreSQL back through it.
- The comparisons run `schema.sql` on a private PostgreSQL of the major version the schema
  declares (`-- sqlshape: postgres 17`). Your database is never used for this. `sqlshape`
  downloads that server on first use from Maven Central (the `io.zonky.test`
  embedded-postgres binaries) and caches it under `~/.cache/sqlshape/pg-<release>`
  (`$SQLSHAPE_PG_CACHE` moves the cache). The first run needs network access and takes a few
  seconds; afterwards the server starts in a quarter of a second.
- There is no setting for a mirror. On a machine without network access (a locked-down CI
  runner, say), put a filled `pg-<release>` directory, copied from a machine of the same OS and
  architecture that has run the commands once, under `$SQLSHAPE_PG_CACHE`: nothing is downloaded
  then.

### MySQL

- With `-db`, the connection's user can `CREATE DATABASE` and `DROP DATABASE`: the commands
  create a scratch database (`sqlshape_scratch_<random>`) on that server while they run and
  drop it when done.
- The server's `lower_case_table_names` must be the one the schema declares (0 when it
  declares none); the commands stop otherwise.
- With `-from` (two schema texts, no server), a `mysqld` on `PATH` (a distribution package, a
  server tarball's `bin/`, `nix-shell -p mysql84`), started with the schema's declared
  settings.

### Connection strings

`-db` takes the driver's own form of a connection string:

```
# PostgreSQL: a libpq connection string (the same string is handed to pg_dump)
$ sqlshape verify-schema -db "postgres://app:secret@db.example.com:5432/app?sslmode=require"

# MySQL: go-sql-driver/mysql's DSN, not a URL
$ sqlshape verify-schema -db "app:secret@tcp(db.example.com:3306)/app"
```

### The schema, and the server's version

`-schema PATH` names `schema.sql`, or a `schema/` directory whose `*.sql` files apply in name
order; the default is the nearest one from the working directory up.

When the database runs another major version than the schema declares (another MySQL version
on MySQL), the commands say so on stderr and go on: the DDL is judged by the declared version's
rules. Passing those checks does not guarantee the DDL behaves the same on that server; it can
still fail there, or act differently.

## diff

`sqlshape diff -db DSN` prints the DDL that takes the database to `schema.sql`, in dependency
order: drops (dependents first), renames, alterations, additions, then the seed `MERGE`s. It is a
proposal: the DDL is meant to be read, and edited when the order or the form is wrong for the
data at hand (`ALTER COLUMN ... TYPE` needs a `USING`, a backfill belongs between two steps).

`-from other.sql` takes the current state from a schema text instead of a database, for
example the previous release's `schema.sql`, and prints the DDL from it to `schema.sql`.
`-packages ./...` indexes the Go statements against the current schema and lists, as comments
in the DDL, every statement that reads a column the DDL drops or retypes.

## Declaring what a diff cannot see

A diff of two schemas cannot tell a rename from a drop and an add, which label a removed enum
value should become, or what a new `NOT NULL` column should hold for existing rows. Those are
declared in `schema.sql` with `-- @migrate` lines:

```sql
-- @migrate rename orders.state -> orders.status
-- @migrate drop orders.legacy
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

`rename old -> new` renames a column or a table; the left side names things as they are now,
the right as they will be. `drop` says a column (or table) may go. `enum` names the label that
goes and the one its rows become. `backfill table.column = expression [where predicate]` sets a
column in the step that changes its table: it fills a column the step adds or changes, or fixes
the rows ahead of a `CHECK`, `UNIQUE` or foreign key the table gains. Each declaration is the
whole line: nothing may follow it on the same line.

Declarations describe the step from the database's current state to `schema.sql`, and are
removed once every database has taken it. A table or column that disappears without a `drop` or
`rename` declaration is an error (the DDL is still printed), so data loss is always announced.
A declaration the diff does not bear out is an error too, so stale declarations are caught: a
rename whose source does not exist, a drop of a column that is still there, an enum that does
not change, and a backfill whose table the step does not change at all (the column is already
in the database, and so are the table's other columns, constraints, indexes and triggers as
`schema.sql` gives them). A backfill left in place after its step would otherwise run again
with every later plan and overwrite what the rows hold by then; it is reported and left out of
the DDL. A backfill on a table that some later step changes again is not caught this way, so
remove it once its step is done.

### What the declarations become

A rename becomes `RENAME TO` / `RENAME COLUMN`. A constraint or foreign key over a renamed
column is emitted as `DROP` + `ADD`, which is correct but re-validates the constraint. An enum
label removal recreates the type (PostgreSQL has no `DROP VALUE`): rename the old type, create
the new one, `ALTER COLUMN ... TYPE ... USING CASE ...` on every column carrying it, recreate the
views over those tables, drop the old type; an array column of the enum is reported as a
problem. A `backfill` is type-checked against the target schema and emitted as an `UPDATE` once
the column exists: after a new column is added, before a column turns `NOT NULL`, and before
the `CHECK`, `UNIQUE` or foreign key the table gains.

### Partitions

Partitions are schema like everything else:

| change | DDL |
|---|---|
| a new partitioned table | `CREATE TABLE ... PARTITION BY ...`, its partitions attached once they exist |
| a partition added to a table | `CREATE TABLE ... PARTITION OF ... FOR VALUES ...` |
| a partition that goes | `DROP TABLE`, which takes its rows and so needs a `-- @migrate drop` of that table |
| a table becoming or ceasing to be a partition | `ATTACH PARTITION` / `DETACH PARTITION` |
| a changed bound | a detach and an attach; every detach in the plan runs before any attach, so a moving bound never overlaps a neighbour's |
| a table holding rows partitioned after the fact, unpartitioned, or moved to another key or strategy | the table renamed, the declared shape created fresh with its partitions, every row inserted through the parent so PostgreSQL places it, the comments, triggers, policies, rules, inbound foreign keys and sequences carried over (a bigserial keeps its sequence, an identity column continues from the highest value), the old table dropped |

The plan never alters a partition's own columns, which follow the parent's. Rows no partition
takes are the server's error, so a `schema.sql` that does not cover the data stops `apply`
rather than losing anything.

### Changes printed as notes

Changes to a domain's base type, a range's subtype, `INHERITS` and `OF type` are printed as
`-- ` notes for the operator rather than as DDL: the steps are yours to decide.

A column's `ALTER COLUMN ... TYPE` gets one of these notes too when the new type narrows a
`numeric`'s precision or scale, a `varchar(n)` / `char(n)` length, a `time` / `timestamp` family's
fractional-second precision, or a `bit(n)` length: PostgreSQL runs a same-base-type `ALTER` without
a `USING` and without complaint, rounding or truncating the existing values to fit. The note is a
warning, not a block, so running the DDL unedited still rounds or truncates the data; add a
`USING` (or fix the data first) before applying it.

## apply

`sqlshape apply -db DSN up.sql` takes the DDL file, hand-edited or not, and checks it by its end
state before running anything: the database's current schema plus the DDL must read back as
`schema.sql`. Column order is tolerated and noted (PostgreSQL cannot insert a column in the
middle, so a dropped and re-added column leaves the orders permanently different); anything
else refuses. Then the DDL runs in one transaction, and a statement that fails rolls all of it
back. Do not write `BEGIN` or `COMMIT` into the file: a `COMMIT` inside it makes what came
before it permanent, even when a later statement fails.

- `-packages ./...` indexes the Go statements against the current schema and refuses while any
  statement still depends on a column the DDL drops or retypes. `-force` runs anyway.
- `-dry-run` stops after the verification.
- `-no-transaction` sends the file without apply's own transaction. PostgreSQL still runs the
  file, sent as one request, as a single implicit transaction.

A statement that cannot run inside a transaction block, such as `CREATE INDEX CONCURRENTLY`,
does not pass through `apply`, with or without `-no-transaction`: the end-state check runs the
file inside one and refuses it. Run such a statement by hand first, then `apply` the rest; the
check then finds the index in place.

## verify-schema

`sqlshape verify-schema -db DSN` lists where a database differs from `schema.sql`: drift, a
migration applied by hand, an environment that fell behind. Exit code 1 when there is a
difference, 0 when the database matches.

## Exit codes

- `diff`: 0 when it prints the DDL, including a non-empty one; 1 when a table or column
  disappears without a `drop` or `rename` declaration, or a declaration is not borne out by the
  diff (the DDL is still printed).
- `verify-schema`: 0 when the database matches `schema.sql`, 1 when it differs.
- `apply`: 0 when the DDL was applied; 1 when it refused before running anything; 2 when a
  statement failed while running (on PostgreSQL the DDL is rolled back, on MySQL the statements
  before it stay applied).
- Every command: 2 for a usage or environment error.

## Several environments, and rolling back

`sqlshape` keeps no record in the database of what was applied: there is no table of its own,
and on MySQL the scratch database exists only while a command runs. Where each environment
stands is what `verify-schema` says about it.

### One change, several databases

A `-- @migrate` declaration describes one step from one state. Keep it in `schema.sql` until
every environment has taken the step, and remove it once `verify-schema` returns 0 against every
one. While it stays, `diff -db` against an environment that has already taken the step fails on
it (exit 1): that is expected, the environments behind still need it.

On PostgreSQL the DDL can be made once per release and the same file applied to every
environment: `diff -from` with the previous release's `schema.sql` (or `diff -db` against one
environment), reviewed and edited once. The condition is that the environment returns 0 from
`verify-schema` against the previous release's `schema.sql`. An environment in any other state,
one that drifted or already took the step, is refused by `apply` before anything runs.

On MySQL, the same file fits only servers with the same settings. The DDL spells out what the
server that made it filled in, such as a new table's default character set and collation, and
`-from` fills in the defaults of the `mysqld` on `PATH`. On a server with other settings,
`apply` refuses the file before running it; make that environment's DDL with `diff -db` against
it.

In every case the check `apply` makes before running anything is the safety valve: a file that
does not take this database to `schema.sql` is not run.

### Run one apply at a time

`apply` takes no lock. Two runs against one database at once both pass the check; the later one
then fails on the server (rolled back on PostgreSQL), and a file that only changes data runs
twice. Serialize migrations, for example as one CI job per database.

### Backfills that can run twice

A backfill left in `schema.sql` after its step is an error as long as its table stays as the
step left it, so `diff` does not emit it again. A later step that changes the same table
takes it for part of that step, though. Write it with a predicate that leaves filled rows
alone (`where status is null`): an `UPDATE` that only touches the rows still to fill is
harmless if it runs a second time, from a later plan or from a DDL file run again by hand.

### Rolling back

There are no down migrations. A failed `apply` on PostgreSQL has rolled back already. On MySQL
it stops after the statements that succeeded, and `sqlshape diff` from that state gives the rest.

To undo a step that was applied, migrate forward to the previous state: put the previous
`schema.sql` back, then `diff`, review and `apply` as for any step. The step back needs its own
declarations (the reverse rename, a `drop` for a column the previous version does not have).
What the step dropped does not come back, and a seeded table's `MERGE` deletes the rows the
previous version does not list. `apply -dry-run` shows whether the file reaches the previous
state before anything runs.

## What is compared

On PostgreSQL both sides are read through `pg_dump`, so what is compared is what PostgreSQL itself stores,
not the spelling in `schema.sql`: `'x'` and `'x'::text`, or `IN (...)` and `= ANY (ARRAY[...])`,
are not differences. `-- sqlshape:` directives are not database state and are not compared.

The comparison is object by object (tables, columns, constraints, indexes, views, functions,
types, domains, enums, triggers, rules, policies, row-security flags, sequences, extensions,
comments) and, for seeded tables, row by row. Owners, privileges (`GRANT`) and tablespaces are
not compared.

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
VIEW` for every table and view, parsed the same way as `schema.sql`. What is compared is
therefore what MySQL stores, not the spelling of `schema.sql`: `INT` and `int(11)`, a default written
`0` and stored `'0'`, a key named by the server (`orders_ibfk_1`, `orders_chk_1`) are not
differences. The target side comes from applying `schema.sql` to the scratch database on the
`-db` server, so it is rendered by the very server the migration targets, version and settings
(`-- sqlshape: server`) included; with `-from` (two texts, no server) it comes from the `mysqld`
on `PATH`.

Compared, object by object:

- tables: engine, charset, collation, row format, comment, and partitioning (`RANGE`, `LIST`,
  `RANGE COLUMNS`, `LIST COLUMNS`, `HASH`, `KEY`, `LINEAR`, `ALGORITHM`, subpartitioning by
  `HASH` / `KEY`, each partition's bound, comment and explicit `SUBPARTITION` names; a form
  `sqlshape` cannot compare, such as a `TABLESPACE` or `MAX_ROWS` option on a partition or
  subpartition, is reported as a problem rather than passed over);
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
  of creation), so it is not compared; a literal time is compared as written.

Not compared: seeded rows, which `sqlshape` does not read on MySQL yet. A one-time event
(`AT ...`) without `ON COMPLETION PRESERVE` is dropped by the server once it has run, so
`verify-schema` reports it missing from then on: that is the event's own definition, not
drift. An existing table's `AUTO_INCREMENT=<n>` counter is data, not schema, and is never
compared either; a brand new table's own declared `AUTO_INCREMENT=<n>` is a schema decision
and does reach the table when it is created.

The plan uses MySQL's own definitions:

| change | DDL |
|---|---|
| a new table | the `CREATE TABLE` as the server renders it |
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
| a partition's `COMMENT` | `REORGANIZE PARTITION ... INTO` with the new comment (no rows move) |
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
encoding, and the plan after `apply` would never be empty.

`apply` runs the DDL statement by statement, on one connection under the `sql_mode` the schema
declares (8.4's default when it declares none), the mode the scratch database judged the DDL
by, whatever the server's global setting. MySQL's DDL commits implicitly, so a script is not a
transaction and `-no-transaction` has no effect. When a statement fails, `apply` says which one
and how many before it are applied; `sqlshape diff` from that state gives what remains.

## How the plan is tested

The DDL `diff` writes is tested against real servers: generated pairs of schemas, with rows in
every table, reach every kind of change the diff can report except those the plan stops on or
that cannot occur, and a pair passes only when the server accepts the plan, reads back as the
target, and a second plan from there is empty. What
this does not reach is schema shapes outside the generated ones and failures that depend on the
data (a backfill's values, lock time); those arrive with your `schema.sql`, and the check
`apply` makes before running anything is what catches them.
