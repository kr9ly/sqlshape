# PostgreSQL

[日本語](postgres.ja.md)

Everything about sqlshape that is PostgreSQL's: the version the schema declares, what the checker
embeds and how its verdicts are verified, the runtime on pgx, and the migration commands. The rules themselves are in [checks.md](checks.md) and are the same for every database;
this page is where the names, numbers and types in those rules come from when the schema declares
`postgres`.

## The schema declares the version

```sql
-- sqlshape: postgres 17
CREATE TABLE ...
```

`schema.sql` names, once, the PostgreSQL major version it is written for: 17 or 18. The
declaration decides how everything else is judged: the grammar that parses the schema and every
statement, the catalog of types, functions and operators they resolve against, and the PostgreSQL
that the migration commands boot. A schema without it is not read.

A statement that only a newer version accepts (`RETURNING old.*`, `WITHOUT OVERLAPS`, `NOT
ENFORCED`, `VIRTUAL` generated columns are 18's) is a syntax error under an older declaration, as
it is on that server. Moving to a new PostgreSQL is changing the number and reading what the
checker reports.

## What the checker embeds

The parser is libpg_query of the declared version, embedded as WebAssembly and run on wazero, so
everything the server parses, the checker parses the same way: SELECT and DML, MERGE, CTEs,
window functions, GROUPING SETS, SQL/JSON, ranges, `RETURNING old` / `new` and temporal keys on
18, extensions such as citext and hstore, and DDL including views, functions (SQL and PL/pgSQL
bodies), triggers and policies. The analyzer is a pure-Go implementation built from that
version's `pg_catalog`; checking never connects to a PostgreSQL. There is no C compiler to
install and no library to link; the first run compiles the module (about a second) and caches
the result under the user cache directory (`~/.cache/sqlshape` on Linux).

The verdicts are backed by PostgreSQL's own regression suite. The statements of
`src/test/regress` are run through the analyzer and a real PostgreSQL of the same version side by
side, and parameter types, result columns and errors must agree. On 17 they disagree on 19 of
22,103 statements, on 18 on 31 of 23,384; every one is listed
(`check/postgres/analyze/testdata/regress_baseline_<version>.txt`), and each is either something
static analysis cannot decide (row-level security recursion, permissions, server internals) or a
case where the checker is right and the server's Describe cannot say (the NULLs of `RETURNING old`
after an INSERT). The comparison is part of `go test ./...`, so a new disagreement fails the
build.

## What the rules use

- Types. The Go type for each PostgreSQL type, as pgx scans and encodes it, is
  [the Go type table below](#the-go-type-table). A type not in the table, or one you receive
  with a type of your own, is bound with `// sqlshape: type <pg type>` on the Go type.
- Constraint names. A failure mode is named as PostgreSQL names the constraint
  ([the table below](#constraint-names)); the rules are in
  [Preparing for a write to fail](checks.md#preparing-for-a-write-to-fail).
- The `One` proof rests on primary keys, `UNIQUE` constraints, unique indexes, partial unique
  indexes whose predicate the statement repeats, and temporal keys (`WITHOUT OVERLAPS`, 18) fixed
  at a known point; a `DEFERRABLE` key never proves anything
  ([Returning one row](checks.md#returning-one-row-one)).
- The schema itself is checked: the bodies of SQL and PL/pgSQL functions, views, triggers and
  row-level security policies are analyzed at load, and a PL/pgSQL `RAISE` joins the failure modes
  of the statements that call the function.
- PostgreSQL schemas (`CREATE SCHEMA app`) are part of a relation's name; `-schemas=a_api,b_private`
  restricts the schemas a package may reference, a service boundary over one database
  ([checks.md](checks.md#a-package-references-only-its-schemas--schemas-postgresql)).

Only on PostgreSQL: `postgres.Copy` and `postgres.MatView` ([below](#the-runtime-pgx)), PL/pgSQL,
domains, composite types and arrays as first-class types, `-schemas`, `// sqlshape: type`, seeded
tables in migrations.

## The Go type table

What pgx actually scans and encodes, verified against a running PostgreSQL. The same table applies
to receiving columns and to passing parameters.

| PostgreSQL | Go |
|---|---|
| `bool` | `bool` |
| `smallint` / `integer` / `bigint` | `int16` / `int32` / `int64` / `int` (a narrower Go type is accepted with a note such as `bigint into int32`) |
| `real` / `double precision` | `float32` / `float64` (`double precision into float32` is noted) |
| `numeric` | `string` (keeps every digit), `pgtype.Numeric`, `big.Rat`, `shopspring/decimal.Decimal`, `apd.Decimal`; a float or an integer is accepted with a precision note |
| `text` / `varchar` / `char` / `name` / `citext` and other text-like extension types | `string` (a `string` also encodes as a parameter of any type) |
| `bytea` | `[]byte` |
| `uuid` | `uuid.UUID` (any package), `[16]byte`, `string` |
| `timestamptz` / `timestamp` / `date` | `time.Time` (`-strict` notes that `timestamp` and `date` lose the zone or the time) |
| `time` | `time.Time`, `string` |
| `interval` | `pgtype.Interval` (keeps months, days and microseconds apart); `time.Duration` flattens them into a fixed span and carries a standing note that this can disagree with PostgreSQL's own calendar arithmetic on the same interval by whole days |
| `json` / `jsonb` | `[]byte`, `json.RawMessage`, `string`, or any struct, slice or map pgx unmarshals into |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)` (each element is checked the same way a plain `T` parameter or column is, notes included). PostgreSQL never guarantees an array's elements are themselves not null -- a column's own `NOT NULL` only forbids the array value as a whole from being `NULL` -- so a result element type that cannot itself carry `NULL` (not already a pointer) carries a standing note, and `-strict` additionally reports it as rejected: `[]int32` for `integer[]` and `[]Item` for a composite array both need `[]*int32` / `[]*Item` to receive a `NULL` element safely (a composite element is worse: pgx silently decodes a `NULL` one as a zero-valued struct instead of erroring) |
| ranges | `pgtype.Range[T]`, with `T` checked against the subtype (user-defined ranges too) |
| multiranges | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | the `pgtype` value |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enums, seeded lookup keys, CHECK value sets | a Go named string type ([Giving types a meaning](checks.md#giving-types-a-meaning)) |
| domains | the base type's Go type, or a named type bound to the domain |
| composites, records | a struct |

A type not in the table, or one you want to receive with your own type, is declared on the Go type
in its doc comment:

```go
// sqlshape: type money_amount
type Money struct{ ... }   // implements sql.Scanner / driver.Valuer
```

The checker then accepts `Money` exactly where the SQL has `money_amount` (its arrays and domains
over it included) and reports it anywhere else. Conversion is left to the type's own
`sql.Scanner` / `driver.Valuer`; the Scanner receives the text form.

## Constraint names

A named constraint goes by its name. An unnamed one goes by the name PostgreSQL gives it, so the
diagnostic, the expect line and the run-time error all carry the same string.

| constraint | key | example |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| `REFERENCES` on `(a)` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| table `CHECK` referencing exactly one column `(a)` | `<table>_<a>_check` (a CHECK on several columns, or on none, is `<table>_check`) | `orders_total_check` |
| domain `CHECK` | `<domain>_check` | `yen_check` |
| `EXCLUDE (a, b)` | `<table>_<a>_<b>_excl` | `reservations_room_during_excl` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| domain `NOT NULL` | the domain's name, schema-qualified unless `public` | `email` |
| an error raised by a trigger | the SQLSTATE, or the name given with `-- sqlshape: error` | `P0401`, `OrderTooLarge` |
| `WITH CHECK OPTION` on a view | the SQLSTATE (PostgreSQL's own 44000 error names no constraint) | `44000` |

A second constraint that would get the same name is numbered, as PostgreSQL does
(`orders_total_check1`). A generated name over PostgreSQL's 63-byte identifier limit is cut down the
same way PostgreSQL cuts it, without splitting a multibyte character.

## The runtime: pgx

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/postgres/v2   # running them on pgx
```

`github.com/kr9ly/sqlshape/postgres/v2` runs a declared statement on pgx. `postgres.DB` is what a
statement runs against, and `*pgx.Conn`, `*pgxpool.Pool` and `pgx.Tx` all satisfy it, so a
statement runs on a transaction unchanged. `{{.X}}` becomes a `$n` parameter on the wire.

```go
for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // the first row, ErrNoRows when none
tag, err    := postgres.Exec(ctx, db, MarkPaid, p)                 // pgconn.CommandTag, rows discarded

u, err     := postgres.Get(ctx, db, UserByEmail, p)                // One: ErrNoRows when absent
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)               // One: ok reports presence
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)               // One: ErrNoRows when no row was touched
```

What every runtime does the same way (row mapping, `One`, the guarantee that only checked SQL
runs, errors under the expect line's names) is in [runtime.md](runtime.md). What pgx adds:

- `Stmt.Unprepared()` returns a copy that runs without a server-side prepared statement, so the
  planner makes a custom plan for the actual values every time. Use it for statements whose
  parameters have skewed distributions, where pgx's statement cache would settle on a generic
  plan. Otherwise prepared-statement caching is pgx's, per expansion.
- Nested rows and user types. `array_agg(row(o.id, o.total))`, `array_agg(o)`, `row(...)` and
  composite-typed columns are scanned into a struct or a slice of structs field by field,
  positionally for an anonymous record and by the composite's column order for a named one.
  Composite parameters are encoded from a struct or a slice of structs the same way. pgx has to
  know a user-defined type (enum, composite, domain, range, multirange, and their arrays) before
  it can decode it: `Run` loads the types a result needs on the connection the first time it
  meets them; types nested inside an anonymous record cannot be seen before scanning, so for
  those, and for pools, register everything once:

  ```go
  cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
  	return postgres.LoadUserTypes(ctx, conn)
  }
  ```

  Extension scalar types pgx's own loader does not read (`citext`, `hstore`, `ltree`, ...) are
  registered too: `hstore` with pgx's hstore codec (`map[string]*string`), the others as text.
  A type with a declared binding (`// sqlshape: type money_amount`) is left to its own
  `sql.Scanner` / `driver.Valuer`; the runtime asks PostgreSQL for the text format on those
  columns and remembers the request per statement. If the type was dropped and recreated since
  (a migration, same name, new OID), the runtime notices PostgreSQL's "cached plan must not
  change result type" and re-derives the request once.
- Errors. A constraint violation (SQLSTATE class 23) comes back as a `*postgres.ConstraintError`
  with the `Code`, `Constraint`, `Table`, `Column` and `Detail` PostgreSQL reported, wrapping the
  `*pgconn.PgError`. `Key()` is the violation as the expect line spells it; a `NOT NULL` a domain
  raises is the one case PostgreSQL reports with no table or column, so `Key()` is the domain's
  name there. `postgres.Violates(err, key)` tests for it. `postgres.ErrNoRows` is `pgx.ErrNoRows`.
- `Batch` sends several statements in one round trip (`pgx.Batch`):

  ```go
  b := postgres.NewBatch()
  orders := postgres.Queue(b, ListOrders, ListParams{Status: &paid})
  paid   := postgres.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
  if err := b.Send(ctx, db); err != nil { ... }
  rows, err := orders.Rows()    // []Order; First() for the first row
  tag, err  := paid.Tag()
  ```

  A `BatchDB` is anything with `SendBatch`: connection, pool or transaction. Types cannot be
  loaded mid-batch, so with user enums or composites in play call `LoadUserTypes` first. A
  statement whose rows carry a `sql.Scanner` type cannot ride in a pgx batch, which never asks
  for text format; `Send` runs it as a plain query right after the batch, in queue order. A
  queued statement's violations are mapped to `*ConstraintError` the same way `Run` maps them;
  `Send` returns the first, and the statements after it were not executed (`ErrNotSent`).
- `Copy` is `COPY ... FROM` through `pgx.CopyFrom`:

  ```go
  var loadItems = postgres.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

  n, err := loadItems.From(ctx, db, items)           // []Item
  n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
  ```

  Each column is fed by the field of `R` that binds to it; with no columns given every field feeds
  the column of its name; a scalar `R` feeds one column. The checker verifies the table, the
  columns, each column's type against its field, and that every column left out has a default
  ([checks.md](checks.md#bulk-loading-with-copy-postgresql)).
- `MatView` names a materialized view: `Refresh` and `RefreshConcurrently` (the latter needs a
  unique index on the view, which `-strict` verifies).

  ```go
  var OrderStats = postgres.MatView("order_stats")

  err := OrderStats.Refresh(ctx, db)
  err := OrderStats.RefreshConcurrently(ctx, db)
  ```

## Migrations

`sqlshape diff`, `apply` and `verify-schema` derive the DDL from the difference between a
database and `schema.sql`, check that the DDL leads to `schema.sql`, and run it. They compare
both sides as `pg_dump` output, so they need the declared version's `pg_dump` on `PATH`, and they
boot an embedded PostgreSQL of the declared version (downloaded on first use, cached under
`~/.cache/sqlshape`) to read `schema.sql` through it. [migrations.md](migrations.md) has the
commands; its MySQL section says what differs there.

## License

The PostgreSQL side of the checker (`check/postgres`) embeds `pg_catalog` data and validation
rules ported from PostgreSQL under the PostgreSQL License, see
[check/postgres/NOTICE](../check/postgres/NOTICE). The runtime (`postgres`) is Apache License 2.0,
like the declarations.
