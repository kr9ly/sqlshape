// Package tables is the first sqlshape example: an order book on plain tables, the
// ground an ORM also covers, written as checked SQL instead.
//
// What it shows:
//
//   - sqlshape.Query[R, P] and sqlshape.One[R, P]: a SQL template, the row type it
//     produces and the parameter type it reads. `go vet -vettool=sqlshape ./...` proves
//     the three agree with schema.sql — column names and types, nullability (a nullable
//     column needs a pointer), parameter types, and that One really returns one row.
//   - Templates: {{.Field}} becomes a $n parameter, never text; {{if}} / {{range}}
//     branches are expanded and every combination is checked.
//   - A value set as a seeded lookup table: order_statuses' rows are written in schema.sql
//     with a plain INSERT, OrderStatus is bound to the table's key by use, its constants
//     are diffed against the rows, and Known() lets the mapper reject a status this build
//     does not know. (An enum binds the same way; a table can carry a label and retire a
//     value, so it is the default here.)
//   - Failure modes: an INSERT / UPDATE / DELETE declares in `-- sqlshape: expect` the
//     constraints it may violate; the checker keeps the list exact, and at run time a
//     violation arrives as a ConstraintError keyed by the same names.
//   - Transactions are pgx's: PlaceOrder runs its statements on a pgx.Tx, which satisfies
//     sqlshape.DB the same way a connection or a pool does.
//   - Tests on a real PostgreSQL: pgtest.Start applies schema.sql to an embedded server,
//     and db.Verify checks every statement against it, so the test suite carries the
//     evidence that the static checks hold for the PostgreSQL the code runs on.
//
// Run the checker with `go run ../../cmd/sqlshape .` and the tests with `go test .`.
package tables
