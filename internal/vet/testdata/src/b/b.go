// Package b declares an enum-like type used by package a's queries; it never imports sqlshape.
package b

type Status string

const (
	Pending   Status = "pending"
	Paid      Status = "paid"
	Shipped   Status = "shipped"
	Cancelled Status = "cancelled"
	Refunded  Status = "refunded" // not in the DB enum
)
