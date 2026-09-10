package owner

import "github.com/kr9ly/sqlshape/v2"

// -require-columns=user_id: every statement on a table with user_id must pin it

var pinned = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM orders WHERE user_id = {{.U}} AND status = 'paid'`)

var pinnedViaJoin = sqlshape.Query[int64, struct{ U int64 }](`SELECT o.id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = {{.U}}`)

var pinnedInSubquery = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM users WHERE id IN (SELECT user_id FROM orders WHERE user_id = {{.U}})`)

var unpinned = sqlshape.Query[int64, struct{}](`SELECT id FROM orders WHERE status = 'paid'`) // want `orders.user_id is not pinned: every statement on orders must fix user_id by equality \(or assign it\)`

var unpinnedSubquery = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM users WHERE id = {{.U}} AND id IN (SELECT user_id FROM orders)`) // want `orders.user_id is not pinned`

var inserted = sqlshape.Query[int64, struct {
	U int64
	T string
}]("-- sqlshape: expect orders_pkey, orders_user_note_key, orders_user_id_fkey, orders_total_check, P0401\nINSERT INTO orders (user_id, total) VALUES ({{.U}}, {{.T}}) RETURNING id")

var deleted = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect order_items_order_fk\nDELETE FROM orders WHERE id = {{.ID}}") // want `orders.user_id is not pinned`

var noSuchColumn = sqlshape.Query[int64, struct{}](`SELECT id FROM users`)
