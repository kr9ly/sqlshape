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
// This package is the declaration: it has no database dependency. A runtime executes a
// Stmt — sqlshape/postgres over pgx, sqlshape/mysql over database/sql — and the analyzer
// in cmd/sqlshape checks every expansion of every Query in a program. The markers the
// checker recognizes are configurable, so a program may run its statements through a
// runtime of its own.
//
// One declares a statement that returns at most one row, and the checker proves it
// from the schema: the WHERE clause fixes a unique key of every FROM item by
// equality (through joins, views and subqueries), or the statement is an aggregate
// without GROUP BY, a constant LIMIT 1, or a single-row INSERT ... RETURNING.
//
//	var userByEmail = sqlshape.One[User, struct{ Email string }](`
//	    SELECT id, name FROM users WHERE email = {{.Email}}`)
//
//	u, err := postgres.Get(ctx, db, userByEmail, p)     // ErrNoRows when absent
//	u, ok, err := postgres.Find(ctx, db, userByEmail, p)
package sqlshape

// Stmt is a checked SQL template. R is the result row type, P the parameter type.
type Stmt[R, P any] struct {
	Template string
	// unprepared: run without a prepared statement (see Unprepared)
	unprepared bool
}

// Unprepared returns a copy that runs without a server-side prepared statement, so the
// planner makes a custom plan for the actual parameter values every time. Use it for
// statements whose parameters have skewed value distributions, where pgx's automatic
// statement cache would switch to a generic plan after a few executions.
func (s Stmt[R, P]) Unprepared() Stmt[R, P] {
	s.unprepared = true
	return s
}

// Unprepared returns a copy that runs without a prepared statement (see Stmt.Unprepared).
func (s Single[R, P]) Unprepared() Single[R, P] {
	s.stmt.unprepared = true
	return s
}

// IsUnprepared reports whether Unprepared was applied (a runtime that prepares reads it).
func (s Stmt[R, P]) IsUnprepared() bool { return s.unprepared }

// SQLTemplate is the template text; pgtest.Verify expands and checks it against a real PG.
func (s Stmt[R, P]) SQLTemplate() string { return s.Template }

// Stmt is the statement under the single-row declaration.
func (s Single[R, P]) Stmt() Stmt[R, P] { return s.stmt }

// Render renders the template for p.
func (s Single[R, P]) Render(p P) (Rendered, error) { return s.stmt.Render(p) }

// SQLTemplate is the template text (see Stmt.SQLTemplate).
func (s Single[R, P]) SQLTemplate() string { return s.stmt.Template }

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
