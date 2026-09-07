// Package dbapi is the third sqlshape example: the database as an API. Tables are the
// database's private side; the application reads views and calls functions, and the
// meaning an ORM would keep in application code lives in schema.sql.
//
// What it adds over examples/2-views (reads were already views there; the writes move into
// the database, and the schema starts carrying meaning):
//
//   - Views fix the read models (order_view joins, aggregates and names the columns once),
//     so Go structs mirror a view, not a join the application repeats. Functions are the
//     writes; their LANGUAGE sql bodies are analyzed when the schema loads.
//   - Domains are units: a Go type that meets the yen domain (Yen) is bound to it, mixing
//     it with another domain or a plain bigint is reported, and inside SQL yen + yen is
//     fine while yen + weight is not.
//   - Closed value sets: order_status (an enum: four labels that will never go away, the
//     case where an enum beats a lookup table) and customers.tier (CHECK (tier IN ...))
//     bind to Go types the same way as the lookup table of stage 1 — constants are diffed
//     against the set and switches must be exhaustive.
//   - A composite type (address) travels as a struct in both directions: a view column
//     received as Address, a function parameter passed as Address.
//   - A trigger raising its own SQLSTATE (OS001) is annotated `-- sqlshape: error OS001 =
//     TooManyOpenOrders`; the checker adds it to the failure modes of the write that fires
//     it, and Go reads it back with sqlshape.Violates(err, "OS001").
//   - `-- sqlshape: not null` on open_orders tells the checker a function result is never
//     NULL, so Go can receive an int64 rather than a *int64.
//   - sqlshape.MatView("sales_by_day").Refresh is checked against the schema.
//   - Transactions are pgx's: Checkout runs place_order and add_line on one pgx.Tx, which
//     satisfies sqlshape.DB like a connection or a pool does.
//   - Run the checker with -no-tables to enforce the boundary: `go run ../../cmd/sqlshape
//     -no-tables .` reports any statement that reads or writes a table directly.
package dbapi
