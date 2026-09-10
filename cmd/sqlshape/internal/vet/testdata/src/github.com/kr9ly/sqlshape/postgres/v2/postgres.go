// Package postgres is the vet test stub of the PostgreSQL runtime: the declarations the
// checker recognizes (MatView, Copy), without pgx.
package postgres

// MatView is a handle on a materialized view.
type MatView string

type Copier[R any] struct {
	Table   string
	Columns []string
}

func Copy[R any](table string, columns ...string) Copier[R] {
	return Copier[R]{Table: table, Columns: columns}
}
