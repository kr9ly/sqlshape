# Runtime

[日本語](runtime.ja.md)

A statement is declared with the `sqlshape` package (`Query`, `One`: no dependencies) and run
with a runtime module for its database. `github.com/kr9ly/sqlshape/postgres/v2` runs it on pgx:
`postgres.DB` is what a statement runs against, and `*pgx.Conn`, `*pgxpool.Pool` and `pgx.Tx`
all satisfy it, so a statement runs on a transaction unchanged. The checker reads only the
declarations, so a program may also run them through a runtime of its own; what such a runtime
does not get is listed at the end.

## Statements

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // the first row, ErrNoRows when none
tag, err    := postgres.Exec(ctx, db, ListOrders, p)               // pgconn.CommandTag, rows discarded
```

`One[R, P]` gives a `Single` with the same `Render` and `Unprepared` and three ways to run:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := postgres.Get(ctx, db, UserByEmail, p)    // ErrNoRows when absent
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)   // ok reports presence
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)      // ErrNoRows when no row was touched
```

All three return `ErrManyRows` if a second row arrives. The checker proved from the schema that
there cannot be one, so this happens only when the unique constraint the proof rested on is not
actually in place in the database.

`Stmt.Unprepared()` returns a copy that runs without a server-side prepared statement, so the
planner makes a custom plan for the actual values every time. Use it for statements whose
parameters have skewed distributions, where pgx's statement cache would settle on a generic
plan. Otherwise prepared-statement caching is pgx's, per expansion.

## Row mapping

Result columns are mapped to fields by name: the `col:"..."` tag, then `db:"..."`, then the field
name in snake_case. Embedded structs flatten. A nullable field (pointer, slice, map, `sql.Null*`,
`pgtype.*`) receives NULL as its zero value; a nullable field whose column is absent from this
expansion's result stays zero (a column only some branches select). A scalar `R` receives the
single column. `numeric` into `string` keeps every digit.

A Go enum type may implement `Known() bool` (the `sqlshape.Labelled` interface); the mapper then
rejects a label this build does not know with `*sqlshape.UnknownLabelError` instead of handing
the application a value it cannot switch on. The binding rules (`sqlshape.Fields`) are the root
module's, shared by the checker and every runtime.

## Nested rows and user types

`array_agg(row(o.id, o.total))`, `array_agg(o)`, `row(...)` and composite-typed columns are
scanned into a struct or a slice of structs field by field, positionally for an anonymous record
and by the composite's column order for a named one. Composite parameters (`{{.Price}}` where
SQL expects `money_amount`, `{{.Items}}` where it expects `order_items[]`) are encoded from a
struct or a slice of structs the same way.

pgx has to know a user-defined type (enum, composite, domain, range, multirange, and their
arrays) before it can decode it. `Run` loads the types a result needs on the connection the
first time it meets them; types nested inside an anonymous record cannot be seen before
scanning, so for those, and for pools, register everything once:

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
	return postgres.LoadUserTypes(ctx, conn)
}
```

Extension scalar types that pgx's own loader does not read (`citext`, `hstore`, `ltree`, ...)
are registered too: `hstore` with pgx's hstore codec (`map[string]*string`), the others as text
(`string`, `[]string` for arrays). Tables carrying `money` and other types pgx has no codec for
still load.

A type with a declared binding (`// sqlshape: type money_amount`) is left to its own
`sql.Scanner` / `driver.Valuer`; the runtime asks PostgreSQL for the text format on those
columns, so the Scanner receives the value's text form. That request is remembered per
statement so later runs skip the extra round trip; if the type was dropped and recreated
since (a migration, same name, new OID), the runtime notices PostgreSQL's resulting
"cached plan must not change result type" and re-derives the request once, so the
statement keeps working without a restart.

## Errors

A constraint violation (SQLSTATE class 23) comes back as a `*ConstraintError` with the
`Code`, `Constraint`, `Table`, `Column` and `Detail` PostgreSQL reported, wrapping the
`*pgconn.PgError`. Its `Key()` is the violation as the template's expect line spells it: the
constraint's name, or `table.column` for NOT NULL. A SQLSTATE the expect line names (a
trigger's `P0401`, or the name given to it with `-- sqlshape: error`) is wrapped the same way.

```go
_, err := postgres.First(ctx, db, CreateCustomer, p)
if postgres.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

The names are the ones the checker listed, so a violation the code does not handle is one the
expect line announced ([checks.md](checks.md#preparing-for-a-write-to-fail)).
`postgres.ErrNoRows` is `pgx.ErrNoRows`; `postgres.IsNoRows(err)` tests for it.

## Batches

`Batch` sends several statements in one round trip (`pgx.Batch`):

```go
b := postgres.NewBatch()
orders := postgres.Queue(b, ListOrders, ListParams{Status: &paid})
paid   := postgres.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
if err := b.Send(ctx, db); err != nil { ... }
rows, err := orders.Rows()    // []Order; First() for the first row
tag, err  := paid.Tag()
```

A `BatchDB` is anything with `SendBatch`: connection, pool or transaction. Types cannot be loaded
mid-batch, so with user enums or composites in play call `LoadUserTypes` first (pools:
`AfterConnect`). A statement whose rows carry a `sql.Scanner` type cannot ride in a pgx batch,
which never asks for text format; `Send` runs it as a plain query right after the batch, in
queue order.

A queued statement's constraint violations and `-- sqlshape: expect` SQLSTATEs are mapped to
`*ConstraintError` the same way `Run` maps them, whether it was queued as a row-returning
statement or as an `Exec` (no `RETURNING`). `Send` returns the first such error; the queued
statements after the failing one were not executed, and their `Rows` / `Tag` return `ErrNotSent`.

## Bulk loads

```go
var loadItems = postgres.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

n, err := loadItems.From(ctx, db, items)           // []Item
n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
```

`Copy` is `COPY ... FROM` through `pgx.CopyFrom`: each column is fed by the field of `R` that
binds to it (tag or snake_case, embedded structs flattened); with no columns given every field
feeds the column of its name; a scalar `R` feeds one column. The checker verifies the table, the
columns, each column's type against its field, and that every column left out has a default.

## Materialized views

```go
var OrderStats = postgres.MatView("order_stats")

err := OrderStats.Refresh(ctx, db)              // readers block until done
err := OrderStats.RefreshConcurrently(ctx, db)  // needs a unique index on the view
```

The checker verifies the name against schema.sql and, with `-strict`, that a unique index
exists when `RefreshConcurrently` is called.

## Only checked SQL runs

On every execution, the SQL built from the template is compared, character for character, with the
SQL the checker verified for the same combination of branches. If they differ, no query is sent and
an error is returned:

```
sqlshape: rendered SQL differs from the checked expansion [if@64:then]: the runtime evaluator and the checker disagree; please report this
```

This indicates a bug in sqlshape; please report it. It does not occur in normal use.

Two cases cannot be compared: a `{{range}}` with three or more elements (checking covers up to two),
and a template whose branch combinations exceeded 256 so that only a representative set was checked
([templates.md](templates.md#many-branches)). In those, only the shape of the branches is confirmed
before running.

## MySQL

`github.com/kr9ly/sqlshape/mysql/v2` runs the same declarations on MySQL through
`database/sql`, with go-sql-driver/mysql as the driver. `mysql.DB` is `*sql.DB`, `*sql.Tx` or
`*sql.Conn`.

```go
for o, err := range mysql.Run(ctx, db, ListOrders, p) { ... }
orders, err := mysql.Collect(ctx, db, ListOrders, p)
first, err  := mysql.First(ctx, db, ListOrders, p)              // ErrNoRows (sql.ErrNoRows) when none
res, err    := mysql.Exec(ctx, db, MarkPaid, p)                 // sql.Result
u, err      := mysql.Get(ctx, db, UserByEmail, p)               // One: Get / Find / ExecOne as on PostgreSQL
```

The template's `{{.X}}` become MySQL's positional `?` on the wire, the arguments lined up in
the order the placeholders appear (a parameter used twice is sent twice). Rows map to fields
by the same rules; the receive types are the driver's (`int64` / `uint64`, `string` for
`DECIMAL`, `time.Time` for temporal columns with `parseTime=true`, `[]byte` for binary strings
and JSON), which is the table the checker uses. A constraint violation comes back as a
`*mysql.ConstraintError` whose `Key` is the schema's name for it — the UNIQUE key's name,
the FOREIGN KEY's `CONSTRAINT` name, the CHECK constraint's name, the column for NOT NULL —
and `mysql.Violates(err, key)` tests for it (`users.name`, as the expect line spells a NOT NULL
column, matches too). `ExecOne` judges `RowsAffected`, which MySQL
counts as changed rows: an UPDATE to the values a row already has reports `ErrNoRows`
unless the DSN sets `clientFoundRows=true`.

There is no Batch, Copy or MatView on MySQL. `mysqltest.Start(ctx, schemaSQL)` boots the
`mysqld` on PATH with the schema loaded, for the application's tests, and returns
`mysqltest.ErrNoServer` when there is none. It starts the server with the settings the schema
declares (`-- sqlshape: server sql_mode = '...'`, `lower_case_table_names = N`;
[checks.md](checks.md#the-schema-names-the-servers-settings-server)), so the tests run under
the mode the checker judged by.

```go
if err := mysql.Verify(ctx, db, schemaSQL); err != nil { ... }
```

`mysql.Verify` asks the connection for its session `@@sql_mode` and the server's
`lower_case_table_names` and returns an error when they differ from what the schema declares
(the server's defaults when it declares none): a DSN's `sql_mode=...`, a pool's session setup
or a server configured otherwise would run the statements under rules the checker did not
judge them by. Call it once at start-up, after opening the pool.

## What a runtime of your own does not get

The checker recognizes the declarations, not the runtime (`-query` registers a marker function
of your own, see [flags.md](flags.md)). Three promises are the runtime's, and hold only when the
statement runs through `sqlshape/postgres`: that the SQL sent is byte for byte the SQL the checker
verified (the section above), that a violation comes back as a `ConstraintError` under the name
the expect line spells, and that a `One` statement returning a second row is an error.

## Tests on a real PostgreSQL

`pgtest` starts an embedded PostgreSQL (downloaded on first use, cached under
`~/.cache/sqlshape`) with schema.sql applied, in a temporary directory that dies with `Close`:

```go
schemaSQL, _ := pgtest.ReadSchema("schema.sql")   // or a schema/ directory
db, err := pgtest.Start(ctx, schemaSQL)
defer db.Close()

if err := db.Verify(ctx, ListOrders, UserByEmail, CreateCustomer); err != nil {
	t.Fatal(err)
}
conn := db.Conn()   // *pgx.Conn, also db.ConnString()
```

`Verify` prepares every expansion of each statement on the server and compares PostgreSQL's
parameter types, result column names and types, or its rejection, with what the checker
concluded. A disagreement means the checker's verdict on that statement cannot be trusted; the
error lists each with the SQL and both descriptions. Put it in a test next to the application's
own tests, so the suite carries the evidence that the static checks hold for the PostgreSQL the
code runs on. `db.Conn()` is a plain connection for exercising views, functions and triggers.
