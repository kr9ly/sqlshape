package docs

import "github.com/kr9ly/sqlshape"

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
