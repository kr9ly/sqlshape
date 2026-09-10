// Package sqlshape makes SQL a first-class, statically checked citizen of a Go
// program. A query is a text/template over a parameter struct P whose finite set
// of expansions is checked at lint time against schema.sql; R describes the
// result row.
//
//	var listOrders = sqlshape.Query[OrderRow, ListOrdersParams](`
//	    SELECT o.id, o.status, o.total
//	      FROM orders o
//	     WHERE true
//	       {{if .Status}} AND o.status = {{.Status}} {{end}}
//	     LIMIT {{.Limit}}
//	`)
//
// The runtime half (executing a Stmt against pgx) is not implemented yet; the
// analyzer in cmd/sqlshape checks every expansion of every Query in a program.
package sqlshape

// Stmt is a checked SQL template. R is the result row type, P the parameter type.
type Stmt[R, P any] struct {
	Template string
}

// Query declares a statement. The argument must be a string literal so the
// analyzer can expand and check it.
func Query[R, P any](template string) Stmt[R, P] {
	return Stmt[R, P]{Template: template}
}

// Single is a checked SQL template proven to return at most one row.
type Single[R, P any] struct {
	stmt Stmt[R, P]
}

// One declares a single-row statement.
func One[R, P any](template string) Single[R, P] {
	return Single[R, P]{stmt: Stmt[R, P]{Template: template}}
}
