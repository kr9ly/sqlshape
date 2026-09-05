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
// Stmt runs against pgx (Run / Collect / First / Exec); the analyzer in
// cmd/sqlshape checks every expansion of every Query in a program.
//
// One declares a statement that returns at most one row, and the checker proves it
// from the schema: the WHERE clause fixes a unique key of every FROM item by
// equality (through joins, views and subqueries), or the statement is an aggregate
// without GROUP BY, a constant LIMIT 1, or a single-row INSERT ... RETURNING.
//
//	var userByEmail = sqlshape.One[User, struct{ Email string }](`
//	    SELECT id, name FROM users WHERE email = {{.Email}}`)
//
//	u, err := userByEmail.Get(ctx, db, p)     // ErrNoRows when absent
//	u, ok, err := userByEmail.Find(ctx, db, p)
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

// One declares a single-row statement. The checker reports it when it cannot prove
// that every expansion returns at most one row.
func One[R, P any](template string) Single[R, P] {
	return Single[R, P]{stmt: Stmt[R, P]{Template: template}}
}

// Template returns the template text.
func (s Single[R, P]) Template() string { return s.stmt.Template }
