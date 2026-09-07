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

import (
	"context"
	"strings"
)

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

// SQLTemplate is the template text; pgtest.Verify expands and checks it against a real PG.
func (s Stmt[R, P]) SQLTemplate() string { return s.Template }

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


// MatView is a handle on a materialized view; the checker verifies the name against
// schema.sql (and, with -strict, that a unique index allows RefreshConcurrently).
//
//	var OrderStats = sqlshape.MatView("order_stats")
//	err := OrderStats.Refresh(ctx, db)
type MatView string

// Refresh runs REFRESH MATERIALIZED VIEW: readers block until it completes.
func (m MatView) Refresh(ctx context.Context, db DB) error {
	_, err := db.Exec(ctx, "REFRESH MATERIALIZED VIEW "+quoteQualified(string(m)))
	return err
}

// RefreshConcurrently refreshes without blocking readers; the view needs a unique index.
func (m MatView) RefreshConcurrently(ctx context.Context, db DB) error {
	_, err := db.Exec(ctx, "REFRESH MATERIALIZED VIEW CONCURRENTLY "+quoteQualified(string(m)))
	return err
}

// quoteQualified quotes a possibly schema-qualified identifier.
func quoteQualified(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}
