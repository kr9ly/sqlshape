package tables

import (
	"time"

	"github.com/kr9ly/sqlshape"
)

// OrderStatus is the Go side of the order_status enum. The checker binds the type to the
// enum where it meets the status column and diffs these constants against the labels: a
// label added in schema.sql without a constant here is reported, and so is a constant
// that is not a label.
type OrderStatus string

const (
	Pending   OrderStatus = "pending"
	Paid      OrderStatus = "paid"
	Shipped   OrderStatus = "shipped"
	Cancelled OrderStatus = "cancelled"
)

// Known lists the labels this build knows; the row mapper rejects others with
// *sqlshape.UnknownLabelError instead of handing the application a value it cannot switch on.
func (s OrderStatus) Known() bool {
	switch s {
	case Pending, Paid, Shipped, Cancelled:
		return true
	}
	return false
}

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

// CreateCustomer returns the new id. The expect line is the statement's failure
// contract: the checker requires it to name exactly the constraints this INSERT can
// violate (here the unique email), and Violates(err, "customers_email_key") reads it back.
var CreateCustomer = sqlshape.Query[int64, NewCustomer](`
-- sqlshape: expect customers_email_key
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)

// CustomerByEmail is a One: email is UNIQUE, so the checker can prove at most one row.
var CustomerByEmail = sqlshape.One[Customer, struct{ Email string }](`
SELECT id, email, name, created_at FROM customers WHERE email = {{.Email}}`)

// DeleteCustomer fails while the customer still has orders (the FK points at us).
var DeleteCustomer = sqlshape.One[struct{}, struct{ ID int64 }](`
-- sqlshape: expect orders_customer_id_fkey
DELETE FROM customers WHERE id = {{.ID}}`)

// --- orders ------------------------------------------------------------------

type Order struct {
	ID         int64
	CustomerID int64
	Status     OrderStatus
	Total      string // numeric: a string keeps every digit; decimal types work too
	Note       *string
	CreatedAt  time.Time
}

// ListOrdersParams drives the branches of ListOrders. Nil / zero means "no filter".
type ListOrdersParams struct {
	CustomerID *int64
	Statuses   []OrderStatus
	Since      *time.Time
	Sort       string // "total" or anything else (created_at)
	Limit      int32
}

// ListOrders is one template, 2 × 2 × 2 × 2 × 2 = 32 statements: every combination of
// its branches is expanded and checked, so the else-branch typo an ORM query builder
// hides is a compile-time finding here.
var ListOrders = sqlshape.Query[Order, ListOrdersParams](`
SELECT o.id, o.customer_id, o.status, o.total, o.note, o.created_at
  FROM orders o
 WHERE true
   {{if .CustomerID}} AND o.customer_id = {{.CustomerID}}   {{end}}
   {{if .Statuses}}   AND o.status = ANY({{.Statuses}})     {{end}}
   {{if .Since}}      AND o.created_at >= {{.Since}}        {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total DESC, o.id {{else}} o.created_at DESC, o.id {{end}}
 {{with .Limit}} LIMIT {{.}} {{end}}`)

var OrderByID = sqlshape.One[Order, struct{ ID int64 }](`
SELECT id, customer_id, status, total, note, created_at FROM orders WHERE id = {{.ID}}`)

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

// RecomputeTotal stores the sum of the lines; the CHECK on total is unreachable from a
// sum of non-negative prices, but the checker cannot know that, so it stays declared.
var RecomputeTotal = sqlshape.One[struct{}, struct{ OrderID int64 }](`
-- sqlshape: expect orders_total_check
UPDATE orders o
   SET total = coalesce((SELECT sum(i.qty * i.price) FROM order_items i WHERE i.order_id = o.id), 0)
 WHERE o.id = {{.OrderID}}`)

// SetStatus is a One on the primary key: Exec returns ErrNoRows when the order does not exist.
var SetStatus = sqlshape.One[struct{}, struct {
	ID     int64
	Status OrderStatus
}](`UPDATE orders SET status = {{.Status}} WHERE id = {{.ID}}`)

// Cancel only moves a pending order; a paid one is left alone (ErrNoRows tells).
var Cancel = sqlshape.One[struct{}, struct{ ID int64 }](`
UPDATE orders SET status = 'cancelled' WHERE id = {{.ID}} AND status = 'pending'`)

type Item struct {
	LineNo int16
	Sku    string
	Qty    int32
	Price  string
}

var ItemsOf = sqlshape.Query[Item, struct{ OrderID int64 }](`
SELECT line_no, sku, qty, price FROM order_items WHERE order_id = {{.OrderID}} ORDER BY line_no`)

// --- reporting ---------------------------------------------------------------

type CustomerTotals struct {
	CustomerID int64
	Email      string
	Orders     int64
	Spent      string     // coalesce keeps it NOT NULL: a plain string is accepted
	LastOrder  *time.Time // max over a LEFT JOIN may be NULL: a pointer is required
}

var TotalsByCustomer = sqlshape.Query[CustomerTotals, struct{ MinOrders int64 }](`
SELECT c.id AS customer_id, c.email, count(o.id) AS orders,
       coalesce(sum(o.total), 0) AS spent, max(o.created_at) AS last_order
  FROM customers c
  LEFT JOIN orders o ON o.customer_id = c.id AND o.status <> 'cancelled'
 GROUP BY c.id, c.email
HAVING count(o.id) >= {{.MinOrders}}
 ORDER BY spent DESC, c.id`)
