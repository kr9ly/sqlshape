package docs

import "github.com/kr9ly/sqlshape/v2"

// -sync-comments: COMMENT ON TABLE orders / COLUMN orders.status become doc comment suggestions

type OrderStatus string // want OrderStatus:`bound e order_status`

type Order struct { // want `type Order has no doc comment; the schema says: One purchase\.`
	ID     int64
	Status OrderStatus // want `field Status has no doc comment; the schema says: Lifecycle state; see order_status\.`
}

var orders = sqlshape.Query[Order, struct{}](`SELECT id, status FROM orders`)

// Documented already: nothing to suggest.
type Documented struct {
	ID int64
	// The state.
	Status OrderStatus
}

var documented = sqlshape.Query[Documented, struct{}](`SELECT id, status FROM orders`)

// A computed column has no source: nothing to look up in the schema's COMMENT ON.
type Count struct {
	Cnt int64
}

var counted = sqlshape.Query[Count, struct{}](`SELECT count(*) AS cnt FROM orders`)

// A table without its own COMMENT ON: no type-level suggestion, even though the type is
// otherwise eligible (undocumented, every column from one relation).
type UserRow struct {
	ID   int64
	Name *string
}

var byUser = sqlshape.Query[UserRow, struct{}](`SELECT id, name FROM users`)

// Rows drawn from more than one relation: no single table to attribute the type to.
type MixedRow struct {
	ID   int64
	Name *string
}

var mixed = sqlshape.Query[MixedRow, struct{}](`SELECT o.id, u.name FROM orders o JOIN users u ON u.id = o.user_id`)

// An anonymous struct literal is not a declared type: nothing to attach a doc comment to.
var anon = sqlshape.Query[struct{ ID int64 }, struct{}](`SELECT id FROM orders`)
