package strict

import (
	"time"

	"github.com/kr9ly/sqlshape"
)

type OrderStatus string // want OrderStatus:`bound e order_status`

type OrderID int64 // want OrderID:`bound k orders.id`

type UserID int64 // want UserID:`bound k users.id`

// advisory findings, reported only with -strict

var plainEnumParam = sqlshape.Query[OrderID, struct{ S OrderStatus }](`SELECT id FROM orders WHERE status = {{.S}}`) // want `schema: materialized view order_stats has no unique index, so REFRESH MATERIALIZED VIEW CONCURRENTLY is not possible` `schema: orders.status is enum order_status: a seeded lookup table .* is easier to change` `no index on orders leads with any of \(status\): this predicate scans the whole table` `parameter .S is a non-pointer strict.OrderStatus: its zero value "" is not a label of enum order_status and fails at runtime \(SQLSTATE 22P02\) when unset`

var pointerEnumParam = sqlshape.Query[OrderID, struct{ S *OrderStatus }](`SELECT id FROM orders WHERE status = {{.S}}`) // want `no index on orders leads with any of \(status\)`

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

var defaultOwner = sqlshape.Query[OrderID, NewOrder]("-- sqlshape: expect orders_pkey, orders_user_note_key, orders_user_id_fkey, orders_total_check, P0401\nINSERT INTO orders (user_id, total, status) VALUES ({{.UserID}}, {{.Total}}, {{.Status}}) RETURNING id") // want `parameter .Status always sends a value into orders.status, so its DEFAULT never applies` `parameter .Status is a non-pointer strict.OrderStatus`

var unorderedLimit = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders LIMIT 10`) // want `LIMIT without ORDER BY: which rows are returned is unspecified`

var orderedLimit = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders ORDER BY id LIMIT 10`)

var enumOrder = sqlshape.Query[OrderID, struct{}](`SELECT id FROM orders WHERE status > 'paid' ORDER BY status`) // want `enum order_status compares in declaration order, not alphabetically` `ORDER BY enum order_status sorts in declaration order, not alphabetically` `no index on orders leads with any of \(status\)`

// a field of P the template never reads is dead or a typo
type SearchParams struct {
	ID     OrderID
	Limit  int32
	Unused string
}

var search = sqlshape.Query[struct{ ID OrderID }, SearchParams](`SELECT id FROM orders WHERE id = {{.ID}} ORDER BY id LIMIT {{.Limit}}`) // want `parameter field Unused is never used by the template`
