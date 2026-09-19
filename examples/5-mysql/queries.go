package mysqlexample

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

// Status is the orders.status enum; Known lets the runtime reject a label this build does not know.
type Status string

const (
	Pending   Status = "pending"
	Paid      Status = "paid"
	Cancelled Status = "cancelled"
)

func (s Status) Known() bool {
	switch s {
	case Pending, Paid, Cancelled:
		return true
	}
	return false
}

type Customer struct {
	ID        uint64
	Email     string
	Name      string
	CreatedAt time.Time
}

type Order struct {
	ID         uint64
	CustomerID uint64
	Status     Status
	Total      string // DECIMAL arrives as its text; a money type of your own can wrap it
	Note       *string
}

type NewCustomer struct{ Email, Name string }

var CreateCustomer = sqlshape.Query[struct{}, NewCustomer](`
-- sqlshape: expect customers_email_key
	INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}})`)

var CustomerByEmail = sqlshape.One[Customer, struct{ Email string }](`
	SELECT id, email, name, created_at FROM customers WHERE email = {{.Email}}`)

type NewOrder struct {
	CustomerID uint64
	Total      string
	Note       *string
}

// OrderTotalTooLarge names the trigger's own 30001, from the `-- sqlshape: error` line
// above it in schema.sql.
var OrderTotalTooLarge = sqlshape.Error("30001")

// CreateOrder's failure modes reach past its own table: orders_before_insert (a BEFORE
// INSERT trigger) SIGNALs 30001 when NEW.total is too large, named OrderTotalTooLarge by
// the `-- sqlshape: error` line above the trigger -- the expect line below names it
// OrderTotalTooLarge (it could just as well say 30001; the two are interchangeable) --
// and its own INSERT INTO order_audit may violate that table's NOT NULL columns -- both
// become failure modes of the statement that fires the trigger, the way the trigger's
// own constraints do.
var CreateOrder = sqlshape.Query[struct{}, NewOrder](`
-- sqlshape: expect OrderTotalTooLarge, fk_orders_customer, orders_total_check, order_audit.customer_id, order_audit.total
	INSERT INTO orders (customer_id, total, note) VALUES ({{.CustomerID}}, {{.Total}}, {{.Note}})`)

type ListOrdersParams struct {
	CustomerID uint64
	Status     *Status
	Limit      int64
}

var ListOrders = sqlshape.Query[Order, ListOrdersParams](`
	SELECT o.id, o.customer_id, o.status, o.total, o.note
	  FROM orders o
	 WHERE o.customer_id = {{.CustomerID}}
	   {{if .Status}} AND o.status = {{.Status}} {{end}}
	 ORDER BY o.id
	 LIMIT {{.Limit}}`)

var SetStatus = sqlshape.One[struct{}, struct {
	ID     uint64
	Status Status
}](`UPDATE orders SET status = {{.Status}} WHERE id = {{.ID}}`)

var OrderTotals = sqlshape.Query[struct {
	CustomerID uint64
	N          int64
	Total      string
}, struct{}](`
	SELECT customer_id, COUNT(*) AS n, COALESCE(SUM(total), 0) AS total
	  FROM orders GROUP BY customer_id ORDER BY customer_id`)

// CustomerOrderTotal calls the schema's own stored FUNCTION: its result type comes from
// RETURNS, always nullable to the checker regardless of the declared type or the body
// (there is no static proof otherwise, and here the body's own SUM is genuinely NULL for a
// customer with no orders). The call has no failure mode of its own to expect: its body is
// a single READS SQL DATA SELECT ... INTO of an aggregate without GROUP BY, which is always
// exactly one row (never NOT FOUND, never SIGNALs).
var CustomerOrderTotal = sqlshape.One[struct{ Total *string }, struct{ CustomerID uint64 }](`
	SELECT customer_order_total({{.CustomerID}}) AS total`)
