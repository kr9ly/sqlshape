# Runtime

The `sqlshape` package runs checked statements on pgx. `DB` is what a statement runs against;
`*pgx.Conn`, `*pgxpool.Pool` and `pgx.Tx` all satisfy it, so a statement runs on a transaction
unchanged.

## Statements

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range ListOrders.Run(ctx, db, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := ListOrders.Collect(ctx, db, p)            // []Order
first, err  := ListOrders.First(ctx, db, p)              // the first row, ErrNoRows when none
tag, err    := ListOrders.Exec(ctx, db, p)               // pgconn.CommandTag, rows discarded
```

`One[R, P]` gives a `Single` with the same `Render` and `Unprepared` and three ways to run:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := UserByEmail.Get(ctx, db, p)    // ErrNoRows when absent
u, ok, err := UserByEmail.Find(ctx, db, p)   // ok reports presence
tag, err   := MarkPaid.Exec(ctx, db, p)      // ErrNoRows when no row was touched
```

All three return `ErrManyRows` if a second row arrives: the checker proved there cannot be one,
so the database has changed under the proof.

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

A Go enum type may implement `Known() bool` (the `Labelled` interface); the mapper then rejects a
label this build does not know with `*UnknownLabelError` instead of handing the application a
value it cannot switch on.

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
	return sqlshape.LoadUserTypes(ctx, conn)
}
```

Extension scalar types that pgx's own loader does not read (`citext`, `hstore`, `ltree`, ...)
are registered too: `hstore` with pgx's hstore codec (`map[string]*string`), the others as text
(`string`, `[]string` for arrays). Tables carrying `money` and other types pgx has no codec for
still load.

A type with a declared binding (`// sqlshape: type money_amount`) is left to its own
`sql.Scanner` / `driver.Valuer`; the runtime asks PostgreSQL for the text format on those
columns, so the Scanner receives the value's text form.

## Errors

A constraint violation (SQLSTATE class 23) comes back as a `*ConstraintError` with the
`Code`, `Constraint`, `Table`, `Column` and `Detail` PostgreSQL reported, wrapping the
`*pgconn.PgError`. Its `Key()` is the violation as the template's expect line spells it: the
constraint's name, or `table.column` for NOT NULL. A SQLSTATE the expect line names (a
trigger's `P0401`, or the name given to it with `-- sqlshape: error`) is wrapped the same way.

```go
_, err := CreateCustomer.First(ctx, db, p)
if sqlshape.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

The names are the ones the checker listed, so a violation the code does not handle is one the
expect line announced ([checks.md](checks.md#failure-modes-what-can-this-write-fail-on)).
`ErrNoRows` is `pgx.ErrNoRows`; `IsNoRows(err)` tests for it.

## Batches

`Batch` sends several statements in one round trip (`pgx.Batch`):

```go
b := sqlshape.NewBatch()
orders := sqlshape.Queue(b, ListOrders, ListParams{Status: &paid})
paid   := sqlshape.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
if err := b.Send(ctx, db); err != nil { ... }
rows, err := orders.Rows()    // []Order; First() for the first row
tag, err  := paid.Tag()
```

A `BatchDB` is anything with `SendBatch`: connection, pool or transaction. Types cannot be loaded
mid-batch, so with user enums or composites in play call `LoadUserTypes` first (pools:
`AfterConnect`). A statement whose rows carry a `sql.Scanner` type cannot ride in a pgx batch,
which never asks for text format; `Send` runs it as a plain query right after the batch, in
queue order.

## Bulk loads

```go
var loadItems = sqlshape.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

n, err := loadItems.From(ctx, db, items)           // []Item
n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
```

`Copy` is `COPY ... FROM` through `pgx.CopyFrom`: each column is fed by the field of `R` that
binds to it (tag or snake_case, embedded structs flattened); with no columns given every field
feeds the column of its name; a scalar `R` feeds one column. The checker verifies the table, the
columns, each column's type against its field, and that every column left out has a default.

## Materialized views

```go
var OrderStats = sqlshape.MatView("order_stats")

err := OrderStats.Refresh(ctx, db)              // readers block until done
err := OrderStats.RefreshConcurrently(ctx, db)  // needs a unique index on the view
```

The checker verifies the name against schema.sql and, with `-strict`, that a unique index
exists when `RefreshConcurrently` is called.

## The runtime refuses SQL the checker never saw

`Render(p)` evaluates the template against `p` and returns the SQL with `$n` placeholders and the
arguments in order. It is a second implementation of the template semantics, so before running
it compares its output with the static expansion of the same branch signature, byte for byte;
a difference is an error, not a query. Two cases are trusted rather than compared: a `range`
with more than two iterations has no static twin and is checked by its two-iteration shape, and
a template the checker expanded sparsely ([templates.md](templates.md#many-branches)).

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
