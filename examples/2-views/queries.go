package views

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

// OrderStatus is bound to order_statuses.code by use (through the views' status column
// and the writes' parameters) and its constants are diffed against the seeded rows.
type OrderStatus string

const (
	Pending   OrderStatus = "pending"
	Paid      OrderStatus = "paid"
	Shipped   OrderStatus = "shipped"
	Cancelled OrderStatus = "cancelled"
)

func (s OrderStatus) Known() bool {
	switch s {
	case Pending, Paid, Shipped, Cancelled:
		return true
	}
	return false
}

// Status is a row of the statuses view: what the UI lists, in the database's order.
type Status struct {
	Code      OrderStatus
	Label     string
	SortOrder int32
}

var ListStatuses = sqlshape.Query[Status, struct{}](`
SELECT code, label, sort_order FROM statuses ORDER BY sort_order`)

// --- customers -------------------------------------------------------------

type Customer struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

type NewCustomer struct {
	Email string
	Name  string
}

var CreateCustomer = sqlshape.Query[int64, NewCustomer](`
-- sqlshape: expect customers_email_key
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)

// CustomerByEmail reads the view; the One proof follows the view to customers.email.
var CustomerByEmail = sqlshape.One[Customer, struct{ Email string }](`
SELECT id, email, name, created_at FROM customer_view WHERE email = {{.Email}}`)

var DeleteCustomer = sqlshape.One[struct{}, struct{ ID int64 }](`
-- sqlshape: expect orders_customer_id_fkey
DELETE FROM customers WHERE id = {{.ID}}`)

// --- orders ------------------------------------------------------------------

// Order mirrors order_view: the customer's email and the status label come resolved.
type Order struct {
	ID            int64
	CustomerID    int64
	CustomerEmail string
	Status        OrderStatus
	StatusLabel   string
	Total         string
	Note          *string
	CreatedAt     time.Time
}

type ListOrdersParams struct {
	CustomerID *int64
	Statuses   []OrderStatus
	Since      *time.Time
	Sort       string
	Limit      int32
}

// ListOrders reads order_view: no join to write, no archived rows to exclude.
var ListOrders = sqlshape.Query[Order, ListOrdersParams](`
SELECT id, customer_id, customer_email, status, status_label, total, note, created_at
  FROM order_view
 WHERE true
   {{if .CustomerID}} AND customer_id = {{.CustomerID}}   {{end}}
   {{if .Statuses}}   AND status = ANY({{.Statuses}})     {{end}}
   {{if .Since}}      AND created_at >= {{.Since}}        {{end}}
 ORDER BY {{if eq .Sort "total"}} total DESC, id {{else}} created_at DESC, id {{end}}
 {{with .Limit}} LIMIT {{.}} {{end}}`)

// OrderByID is single through the view: order_view's rows are keyed by orders.id.
var OrderByID = sqlshape.One[Order, struct{ ID int64 }](`
SELECT id, customer_id, customer_email, status, status_label, total, note, created_at
  FROM order_view WHERE id = {{.ID}}`)

type NewOrder struct {
	CustomerID int64
	Note       *string
}

var CreateOrder = sqlshape.Query[int64, NewOrder](`
-- sqlshape: expect orders_customer_id_fkey
INSERT INTO orders (customer_id, note) VALUES ({{.CustomerID}}, {{.Note}}) RETURNING id`)

type NewItem struct {
	OrderID int64
	LineNo  int16
	Sku     string
	Qty     int32
	Price   string
}

var AddItem = sqlshape.Query[struct{}, NewItem](`
-- sqlshape: expect order_items_pkey, order_items_order_id_fkey, order_items_qty_check, order_items_price_check
INSERT INTO order_items (order_id, line_no, sku, qty, price)
VALUES ({{.OrderID}}, {{.LineNo}}, {{.Sku}}, {{.Qty}}, {{.Price}})`)

// RecomputeTotal writes orders but reads the lines through order_line_view: with
// -no-table-reads a subquery on order_items would be reported.
var RecomputeTotal = sqlshape.One[struct{}, struct{ OrderID int64 }](`
-- sqlshape: expect orders_total_check
UPDATE orders o
   SET total = coalesce((SELECT sum(l.amount) FROM order_line_view l WHERE l.order_id = o.id), 0)
 WHERE o.id = {{.OrderID}} AND o.archived_at IS NULL`)

// Writes on orders carry the visibility predicate like every other statement on it: an
// archived order is not there to be paid or cancelled.
var SetStatus = sqlshape.One[struct{}, struct {
	ID     int64
	Status OrderStatus
}](`
-- sqlshape: expect orders_status_fkey
UPDATE orders SET status = {{.Status}} WHERE id = {{.ID}} AND archived_at IS NULL`)

var Cancel = sqlshape.One[struct{}, struct{ ID int64 }](`
-- sqlshape: expect orders_status_fkey
UPDATE orders SET status = 'cancelled' WHERE id = {{.ID}} AND status = 'pending' AND archived_at IS NULL`)

// Archive hides an order from every view but archived_orders.
var Archive = sqlshape.One[struct{}, struct{ ID int64 }](`
UPDATE orders SET archived_at = now() WHERE id = {{.ID}} AND archived_at IS NULL`)

type ArchivedOrder struct {
	ID         int64
	CustomerID int64
	Status     OrderStatus
	Total      string
	ArchivedAt time.Time
}

var ArchivedOrders = sqlshape.Query[ArchivedOrder, struct{}](`
SELECT id, customer_id, status, total, archived_at FROM archived_orders ORDER BY archived_at, id`)

type Item struct {
	LineNo int16
	Sku    string
	Qty    int32
	Price  string
	Amount string
}

var ItemsOf = sqlshape.Query[Item, struct{ OrderID int64 }](`
SELECT line_no, sku, qty, price, amount FROM order_line_view WHERE order_id = {{.OrderID}} ORDER BY line_no`)

// --- reporting ---------------------------------------------------------------

type CustomerTotals struct {
	CustomerID int64
	Email      string
	Orders     int64
	Spent      string
	LastOrder  *time.Time
}

// TotalsByCustomer filters the aggregate view: the GROUP BY lives in the database.
var TotalsByCustomer = sqlshape.Query[CustomerTotals, struct{ MinOrders int64 }](`
SELECT customer_id, email, orders, spent, last_order
  FROM customer_totals
 WHERE orders >= {{.MinOrders}}
 ORDER BY spent DESC, customer_id`)
