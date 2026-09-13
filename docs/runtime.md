# Runtime

[日本語](runtime.ja.md)

A statement is declared with the `sqlshape` package (`Query`, `One`: no dependencies) and run
with the runtime module for its database: `github.com/kr9ly/sqlshape/postgres/v2` on pgx
([postgres.md](postgres.md#the-runtime-pgx)), `github.com/kr9ly/sqlshape/mysql/v2` on
`database/sql` ([mysql.md](mysql.md#the-runtime-databasesql)). This page is what both do the
same way; the examples use the postgres functions, and the mysql ones have the same names and
shapes. The checker reads only the declarations, so a program may also run them through a
runtime of its own; what such a runtime does not get is listed at the end.

## Statements

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // the first row, ErrNoRows when none
tag, err    := postgres.Exec(ctx, db, MarkPaid, p)                 // the driver's result (pgconn.CommandTag / sql.Result), rows discarded
```

`One[R, P]` gives a `Single` with the same `Render` and three ways to run:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := postgres.Get(ctx, db, UserByEmail, p)    // ErrNoRows when absent
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)   // ok reports presence
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)      // ErrNoRows when no row was touched
```

All three return `ErrManyRows` if a second row arrives. The checker proved from the schema that
there cannot be one, so this happens only when the unique constraint the proof rested on is not
actually in place in the database.

## Row mapping

Result columns are mapped to fields by name: the `col:"..."` tag, then `db:"..."`, then the field
name in snake_case. Embedded structs flatten. A nullable field (pointer, slice, map, `sql.Null*`,
the driver's nullable value types such as `pgtype.*`) receives NULL as its zero value; a nullable field whose column is absent from this
expansion's result stays zero (a column only some branches select). A scalar `R` receives the
single column. A `numeric` / `DECIMAL` into `string` keeps every digit. Which Go types a column or
a parameter may take is the database's table: [postgres.md](postgres.md#what-the-rules-use),
[mysql.md](mysql.md#the-go-type-table).

A Go enum type may implement `Known() bool` (the `sqlshape.Labelled` interface); the mapper then
rejects a label this build does not know with `*sqlshape.UnknownLabelError` instead of handing
the application a value it cannot switch on. The binding rules (`sqlshape.Fields`) are the root
module's, shared by the checker and every runtime.

## Errors

A constraint violation comes back as a `*ConstraintError` of the runtime (`postgres.ConstraintError`
wrapping the `*pgconn.PgError`, `mysql.ConstraintError` wrapping the driver's error) with what
the server reported. Its `Key()` is the violation as the template's expect line spells it: the
constraint's name as the database names it, or `table.column` for NOT NULL
([postgres.md](postgres.md#what-the-rules-use), [mysql.md](mysql.md#constraint-names-and-failure-modes)).
A SQLSTATE the expect line names (a PostgreSQL trigger's `P0401`, or a MySQL SIGNAL's own
code) is wrapped the same way; on PostgreSQL, since the runtime does not read schema.sql and so
cannot resolve a `-- sqlshape: error` annotation's Name back to the code the checker matched it
against, it wraps any custom SQLSTATE a statement with an expect line at all receives, not only
one the checker is known to have predicted for it.

```go
_, err := postgres.First(ctx, db, CreateCustomer, p)
if postgres.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

The names are the ones the checker listed, so a violation the code does not handle is one the
expect line announced ([checks.md](checks.md#preparing-for-a-write-to-fail)). `ErrNoRows` is the
driver's (`pgx.ErrNoRows`, `sql.ErrNoRows`); `IsNoRows(err)` tests for it.

A `-- sqlshape: error <code> = <Name>` annotation's Name is mirrored into Go with
`sqlshape.Error(code)` ([checks.md](checks.md#name-the-errors-a-trigger-raises)), giving a
`sqlshape.Failure` -- a `~string` holding the code itself. `Violates` takes a `Failure` or a
plain string identically (`Violates[K ~string](err error, key K) bool`), judging only by the
code it is given; it does not read the schema, so it cannot tell a Name from an arbitrary
string that happens to equal it, and never resolves one to the other. `Violates(err,
OrderTooLarge)` and `Violates(err, "P0401")` are exactly the same call once `OrderTooLarge` is
`sqlshape.Error("P0401")`.

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

## What the database's runtime adds

Beyond the functions above, each runtime has what its driver and database offer and the other
does not: on pgx, `Unprepared`, nested rows and user-defined types with `LoadUserTypes`,
`Batch`, `Copy` and `MatView` ([postgres.md](postgres.md#the-runtime-pgx)); on `database/sql`,
`ExecOne`'s reading of `RowsAffected` and `mysql.Verify` for the server's settings
([mysql.md](mysql.md#the-runtime-databasesql)).

## What a runtime of your own does not get

The checker recognizes the declarations, not the runtime (`-query` registers a marker function
of your own, see [flags.md](flags.md)). Three promises are the runtime's, and hold only when the
statement runs through `sqlshape/postgres` or `sqlshape/mysql`: that the SQL sent is byte for byte the SQL the checker
verified (the section above), that a violation comes back as a `ConstraintError` under the name
the expect line spells, and that a `One` statement returning a second row is an error.
