package strict

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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

// a plain (undeclared) Go type that receives a domain: advisory, since the domain's
// constraint is not carried by an unnamed type
var plainDomain = sqlshape.Query[struct{ Balance int64 }, struct{}](`SELECT balance FROM users`) // want `field Balance carries domain yen as a plain int64; declare a named type to have it checked`

// adviseParam: a parameter fed into a timestamp/date column as time.Time carries the same
// implicit zone question as a result field, and a parameter assigned into an identity
// column is one the database generates itself
var paramFidelity = sqlshape.Query[struct{}, struct {
	T time.Time
}]("UPDATE users SET updated_at = {{.T}} WHERE id = 1") // want `parameter .T: timestamp without time zone into time.Time`

var identityAssign = sqlshape.Query[struct{}, struct {
	Seq int64
}]("UPDATE flags_test SET seq = {{.Seq}} WHERE id = 1") // want `parameter .Seq sends a value into flags_test.seq, which the database generates`

// reportUnusedParams: an embedded struct's fields are promoted, so an unused one inside it
// is still reported by name
type EmbeddedFilter struct {
	Extra string
}

type EmbedSearchParams struct {
	ID OrderID
	EmbeddedFilter
}

var embedSearch = sqlshape.Query[struct{ ID OrderID }, EmbedSearchParams](`SELECT id FROM orders WHERE id = {{.ID}}`) // want `parameter field Extra is never used by the template`

// reportUnusedParams: a P that unwraps to nil (a pgtype.* value carries Valid, no fields
// to check) or that is a scalar (not a struct at all) returns without walking any fields
var pgtypeParam = sqlshape.Query[int64, pgtype.Numeric]("SELECT id FROM orders WHERE total = {{.}}") // want `no index on orders leads with any of \(total\)` `R carries key orders.id as a plain int64; declare a named type to have it checked`

var scalarParam = sqlshape.Query[int64, int64]("SELECT id FROM orders WHERE id = {{.}}") // want `R carries key orders.id as a plain int64; declare a named type to have it checked` `parameter . carries key orders.id as a plain int64; declare a named type to have it checked`

// reportUnusedParams: res.Controls ({{if}}) also marks a field used
type ControlParams struct {
	Flag    bool
	Unused2 string
}

var controlParam = sqlshape.Query[int64, ControlParams](`SELECT id FROM orders WHERE true {{if .Flag}} AND true {{end}}`) // want `parameter field Unused2 is never used by the template` `R carries key orders.id as a plain int64; declare a named type to have it checked` `R carries key orders.id as a plain int64; declare a named type to have it checked`
