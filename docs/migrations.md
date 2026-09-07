# Migrations

`schema.sql` is the only definition of the database. There are no migration files to write:
the `sqlshape` binary compares the live database with `schema.sql` and derives the DDL, checks
that the DDL really leads to `schema.sql`, and runs it.

```
$ sqlshape diff -db "$DSN" > up.sql         # DDL from the database's state to schema.sql
$ $EDITOR up.sql                            # reorder, split, add USING, interleave a backfill
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # drift: where a database differs from schema.sql
```

## What is compared

Both sides are read through `pg_dump`, so what is compared is what PostgreSQL itself stores,
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

Changes to a domain's base type, a range's subtype, `INHERITS`, partitioning and `OF type` are
printed as `-- ` notes for the operator rather than as DDL.

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

## Seeded tables

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

The checker reads the same rows as a value set: a Go named type that meets the key column, or a
column referencing it, is diffed against them like enum labels
([checks.md](checks.md#meaning-what-does-the-value-stand-for)). This is why a lookup table is the
recommended home for a value set: adding, relabelling, reordering and retiring a value are each
a one-row change and a `MERGE`, where an enum needs the type recreated under every column.

## Requirements

- `pg_dump` on `PATH` or named by `$SQLSHAPE_PG_DUMP`; its major version must be at least the
  database's.
- The comparisons run `schema.sql` on a private PostgreSQL 17 that `sqlshape` downloads on first
  use and caches under `~/.cache/sqlshape` (`$SQLSHAPE_PG_CACHE`). The first run takes a few
  seconds for the download; afterwards it starts in a quarter of a second. Your database is
  never used for this.
- `-schema PATH` names `schema.sql`, or a `schema/` directory whose `*.sql` files apply in name
  order; the default is the nearest one from the working directory up.

Exit codes: 0 no difference, 1 a finding (a diff, a drift, a refused apply), 2 a usage or
environment error.
