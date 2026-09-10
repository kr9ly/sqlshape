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
	INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}})`)

var CustomerByEmail = sqlshape.One[Customer, struct{ Email string }](`
	SELECT id, email, name, created_at FROM customers WHERE email = {{.Email}}`)

type NewOrder struct {
	CustomerID uint64
	Total      string
	Note       *string
}

var CreateOrder = sqlshape.Query[struct{}, NewOrder](`
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
