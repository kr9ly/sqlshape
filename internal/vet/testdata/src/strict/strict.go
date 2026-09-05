package strict

import (
	"time"

	"github.com/kr9ly/sqlshape"
)

type OrderStatus string // want OrderStatus:`bound e order_status`

type OrderID int64 // want OrderID:`bound k orders.id`

type UserID int64 // want UserID:`bound k users.id`

// advisory findings, reported only with -strict

var plainEnumParam = sqlshape.Query[OrderID, struct{ S OrderStatus }](`SELECT id FROM orders WHERE status = {{.S}}`) // want `schema: materialized view order_stats has no unique index, so REFRESH MATERIALIZED VIEW CONCURRENTLY is not possible` `parameter .S is a non-pointer strict.OrderStatus: its zero value "" is not a label of enum order_status and fails at runtime \(SQLSTATE 22P02\) when unset`

var pointerEnumParam = sqlshape.Query[OrderID, struct{ S *OrderStatus }](`SELECT id FROM orders WHERE status = {{.S}}`)

type Times struct {
	UpdatedAt *time.Time
	Born      *time.Time
}

var fidelity = sqlshape.Query[Times, struct{}](`SELECT updated_at, born FROM users`) // want `field UpdatedAt: timestamp without time zone into time.Time` `field Born: date into time.Time`

type NewOrder struct {
	UserID UserID
	Total  string
	Status OrderStatus
}

var defaultOwner = sqlshape.Query[OrderID, NewOrder]("-- sqlshape: expect orders_pkey, orders_user_note_key, orders_uid_active, orders_user_id_fkey, orders_total_check\nINSERT INTO orders (user_id, total, status) VALUES ({{.UserID}}, {{.Total}}, {{.Status}}) RETURNING id") // want `parameter .Status always sends a value into orders.status, so its DEFAULT never applies` `parameter .Status is a non-pointer strict.OrderStatus`

var unorderedLimit = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders LIMIT 10`) // want `LIMIT without ORDER BY: which rows are returned is unspecified`

var orderedLimit = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders ORDER BY id LIMIT 10`)

var enumOrder = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders WHERE status > 'paid' ORDER BY status`) // want `enum order_status compares in declaration order, not alphabetically` `ORDER BY enum order_status sorts in declaration order, not alphabetically`
