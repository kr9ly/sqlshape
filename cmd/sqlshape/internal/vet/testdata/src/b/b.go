// Package b declares an enum-like type used by package a's queries; it never imports sqlshape.
package b

import "database/sql/driver"

type Status string

const (
	Pending   Status = "pending"
	Paid      Status = "paid"
	Shipped   Status = "shipped"
	Cancelled Status = "cancelled"
	Refunded  Status = "refunded" // not in the DB enum
)

// Money declares the PG type it carries without ever calling a sqlshape query itself; the
// binding still travels as a fact to package a, which uses it in a query.

// sqlshape: type money_amount
type Money struct { // want Money:`carries money_amount`
	Amount   string
	Currency string
}

func (m *Money) Scan(src any) error          { return nil }
func (m Money) Value() (driver.Value, error) { return nil, nil }

// Widget is a plain struct with no binding, used as R from another package: its type
// argument is a qualified selector (b.Widget), not a local identifier or struct literal,
// so a diagnostic about it fitting the query carries no suggested rewrite.
type Widget struct {
	ID int64
}
