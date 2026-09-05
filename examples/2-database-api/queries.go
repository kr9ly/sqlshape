package dbapi

import (
	"time"

	"github.com/kr9ly/sqlshape"
)

// Yen meets the yen domain and is bound to it: a Yen passed where a plain bigint or
// another domain is expected is reported, even though both are int64 in Go.
type Yen int64

// OrderStatus is the enum; Tier is the CHECK value set on customers.tier. Both are closed
// sets the checker diffs against the constants declared here.
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

type Tier string

const (
	Free       Tier = "free"
	Pro        Tier = "pro"
	Enterprise Tier = "enterprise"
)

// Address receives the address composite: fields match its columns by name and order.
type Address struct {
	Street     *string
	City       *string
	PostalCode *string
}

// --- reads: views --------------------------------------------------------------

type Customer struct {
	ID        int64
	Email     string
	Name      string
	Tier      Tier
	ShipsTo   *Address // the column is nullable: a nil pointer is a NULL address
	CreatedAt time.Time
}

var CustomerByEmail = sqlshape.One[Customer, struct{ Email string }](`
SELECT id, email, name, tier, ships_to, created_at FROM customer_view WHERE email = {{.Email}}`)

var CustomersByTier = sqlshape.Query[Customer, struct{ Tier Tier }](`
SELECT id, email, name, tier, ships_to, created_at FROM customer_view WHERE tier = {{.Tier}} ORDER BY id`)

// Order mirrors order_view: the projection was decided in the schema, once.
type Order struct {
	ID            int64
	CustomerID    int64
	CustomerEmail string
	Status        OrderStatus
	Note          *string
	CreatedAt     time.Time
	Subtotal      Yen
	Shipping      Yen
	Total         Yen
	Lines         int32
}

type ListOrdersParams struct {
	CustomerID *int64
	Statuses   []OrderStatus
	MinTotal   *Yen
	Limit      int32
}

var ListOrders = sqlshape.Query[Order, ListOrdersParams](`
SELECT id, customer_id, customer_email, status, note, created_at, subtotal, shipping, total, lines
  FROM order_view
 WHERE true
   {{if .CustomerID}} AND customer_id = {{.CustomerID}} {{end}}
   {{if .Statuses}}   AND status = ANY({{.Statuses}})   {{end}}
   {{if .MinTotal}}   AND total >= {{.MinTotal}}        {{end}}
 ORDER BY created_at DESC, id
 {{with .Limit}} LIMIT {{.}} {{end}}`)

var OrderByID = sqlshape.One[Order, struct{ ID int64 }](`
SELECT id, customer_id, customer_email, status, note, created_at, subtotal, shipping, total, lines
  FROM order_view WHERE id = {{.ID}}`)

type Line struct {
	LineNo    int16
	Sku       string
	Qty       int32
	UnitPrice Yen
	Amount    Yen
}

var LinesOf = sqlshape.Query[Line, struct{ OrderID int64 }](`
SELECT line_no, sku, qty, unit_price, amount FROM order_line_view WHERE order_id = {{.OrderID}} ORDER BY line_no`)

// OpenOrders receives int64, not *int64: the function is annotated not null.
var OpenOrders = sqlshape.One[int64, struct{ CustomerID int64 }](`SELECT open_orders({{.CustomerID}})`)

type DailySales struct {
	Day     time.Time
	Orders  int64
	Revenue Yen
}

var SalesSince = sqlshape.Query[DailySales, struct{ Since time.Time }](`
SELECT day, orders, revenue FROM sales_by_day WHERE day >= {{.Since}} ORDER BY day`)

var Sales = sqlshape.MatView("sales_by_day")

// --- writes: functions -------------------------------------------------------------

type NewCustomer struct {
	Email string
	Name  string
	Tier  Tier
}

// The failure modes of a function call are those of the statements in its body: the
// unique email, the domain's CHECK on the email format, the tier value set.
var CreateCustomer = sqlshape.One[*int64, NewCustomer](`
-- sqlshape: expect customers_email_key, email_check, customers_tier_check
SELECT create_customer({{.Email}}, {{.Name}}, {{.Tier}})`)

var SetAddress = sqlshape.One[struct{}, struct {
	CustomerID int64
	Address    Address
}](`SELECT set_address({{.CustomerID}}, {{.Address}})`)

type NewOrder struct {
	CustomerID int64
	Note       *string
	Shipping   Yen
}

// OS001 is the trigger's SQLSTATE: the order limit for free-tier customers.
var PlaceOrder = sqlshape.One[*int64, NewOrder](`
-- sqlshape: expect orders_customer_id_fkey, yen_check, OS001
SELECT place_order({{.CustomerID}}, {{.Note}}, {{.Shipping}})`)

type NewLine struct {
	OrderID   int64
	Sku       string
	Qty       int32
	UnitPrice Yen
}

var AddLine = sqlshape.One[*int16, NewLine](`
-- sqlshape: expect order_items_pkey, order_items_order_id_fkey, order_items_qty_check, yen_check
SELECT add_line({{.OrderID}}, {{.Sku}}, {{.Qty}}, {{.UnitPrice}})`)

// PayOrder returns the new status, or nil when the order was not pending.
var PayOrder = sqlshape.One[*OrderStatus, struct{ OrderID int64 }](`SELECT pay_order({{.OrderID}})`)
