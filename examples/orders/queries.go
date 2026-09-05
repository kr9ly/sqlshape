// Package orders shows sqlshape queries; run `go run ./cmd/sqlshape ./examples/...` to check them.
package orders

import (
	"time"

	"github.com/kr9ly/sqlshape"
)

type OrderStatus string

type OrderRow struct {
	ID        int64
	Status    OrderStatus
	Total     string
	Note      *string
	CreatedAt time.Time
}

type ListOrdersParams struct {
	Status  *OrderStatus
	UserIDs []int64
	Sort    string
	Limit   int32
}

var ListOrders = sqlshape.Query[OrderRow, ListOrdersParams](`
SELECT o.id, o.status, o.total, o.note, o.created_at
  FROM orders o
 WHERE true
   {{if .Status}}  AND o.status = {{.Status}}          {{end}}
   {{if .UserIDs}} AND o.user_id = ANY({{.UserIDs}})   {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total DESC {{else}} o.created_at DESC {{end}}
 LIMIT {{.Limit}}
`)

type UserSummary struct {
	Email      string
	OrderCount int64
	Spent      string
	LastOrder  *time.Time
}

// A deliberate mistake: last_order is nullable (LEFT JOIN + max) — LastOrder is a pointer, good;
// but Spent is declared string while coalesce(sum(total), 0) is numeric: fine. Nothing to report.
var UserSummaries = sqlshape.Query[UserSummary, struct{ MinOrders int64 }](`
SELECT u.email, count(o.id) AS order_count, coalesce(sum(o.total), 0) AS spent, max(o.created_at) AS last_order
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
 GROUP BY u.id, u.email
HAVING count(o.id) >= {{.MinOrders}}
`)

type NewOrder struct {
	UserID int64
	Total  string
	Note   *string
}

// The expect line is the statement's failure contract: the checker requires it to list
// exactly the constraints the schema says this INSERT can violate.
var InsertOrder = sqlshape.Query[int64, NewOrder](`
-- sqlshape: expect orders_pkey, orders_user_note_key, orders_uid_active, orders_user_id_fkey, orders_total_check
INSERT INTO orders (user_id, total, note) VALUES ({{.UserID}}, {{.Total}}, {{.Note}}) RETURNING id
`)
