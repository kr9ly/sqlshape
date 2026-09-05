// Package pgtype is a stub of github.com/jackc/pgx/v5/pgtype for the analyzer's fixtures:
// the checker matches these types by package path and name only.
package pgtype

type Range[T any] struct {
	Lower, Upper T
	Valid        bool
}

type Multirange[T any] []T

type Point struct{ Valid bool }

type TSVector struct{ Valid bool }

type Bits struct{ Valid bool }

type Hstore map[string]*string
