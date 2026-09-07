// Package views is the second sqlshape example: the same order book as examples/1-tables,
// read through views. The tables stay the application's to write with plain INSERT and
// UPDATE; what it reads is a view, so the database decides once what things are called,
// how they join and aggregate, and which rows exist for the application.
//
// What it adds over examples/1-tables:
//
//   - Views as the read model: order_view resolves the customer's email and the status
//     label (from the lookup table) and joins once; customer_totals aggregates once; the
//     Go row types mirror a view each, not a join the application repeats.
//   - A soft-delete predicate the views absorb: orders is annotated `-- sqlshape: visible
//     where archived_at IS NULL`, the views carry the predicate, the writes on orders carry
//     it too, and the one view that lists archived orders opts out with `-- sqlshape:
//     unfiltered orders`. A statement that forgets it is reported.
//   - `-no-table-reads`: a SELECT from a table is reported (a write's WHERE reading another
//     table too), so reads cannot drift back to the tables. RecomputeTotal sums
//     order_line_view rather than order_items for that reason.
//   - Names from the database: Describe prints the status label the view supplies; the Go
//     constants are still bound to order_statuses and diffed against its rows.
//   - One through a view: OrderByID is proved single because order_view's rows are keyed
//     by orders.id, which the view exposes.
//
// Run the checker with `go run ../../cmd/sqlshape -no-table-reads .` and the tests with
// `go test .`.
package views
