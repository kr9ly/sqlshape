package views

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/pgtest"
)

func TestOrderBookThroughViews(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgtest.Start(ctx, string(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Verify(ctx, ListStatuses, CreateCustomer, CustomerByEmail, DeleteCustomer, ListOrders, OrderByID,
		CreateOrder, AddItem, RecomputeTotal, SetStatus, Cancel, Archive, ArchivedOrders, ItemsOf, TotalsByCustomer); err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()

	statuses, err := ListStatuses.Collect(ctx, conn, struct{}{})
	if err != nil || len(statuses) != 4 || statuses[0].Code != Pending || statuses[0].Label != "Awaiting payment" {
		t.Fatalf("statuses: %v %+v", err, statuses)
	}
	alice, err := CreateCustomer.First(ctx, conn, NewCustomer{Email: "alice@example.com", Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CustomerByEmail.Get(ctx, conn, struct{ Email string }{"alice@example.com"})
	if err != nil || c.ID != alice {
		t.Fatalf("by email: %v %+v", err, c)
	}

	note := "gift wrap"
	id, err := PlaceOrder(ctx, conn, alice, &note, []NewItem{{Sku: "BOOK", Qty: 2, Price: "12.50"}, {Sku: "PEN", Qty: 1, Price: "3.00"}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := OrderByID.Get(ctx, conn, struct{ ID int64 }{id})
	if err != nil || o.Total != "28.00" || o.Status != Pending || o.StatusLabel != "Awaiting payment" || o.CustomerEmail != "alice@example.com" {
		t.Fatalf("order: %v %+v", err, o)
	}
	if err := Pay(ctx, conn, id); err != nil {
		t.Fatal(err)
	}
	if _, err := Cancel.Exec(ctx, conn, struct{ ID int64 }{id}); !errors.Is(err, sqlshape.ErrNoRows) {
		t.Fatalf("cancel paid: %v", err)
	}
	rows, err := ListOrders.Collect(ctx, conn, ListOrdersParams{CustomerID: &alice, Statuses: []OrderStatus{Paid, Shipped}, Sort: "total", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("list: %v %+v", err, rows)
	}
	if got := Describe(rows[0]); got != "order #1 for alice@example.com: 28.00 (Paid)" {
		t.Errorf("describe: %q", got)
	}
	items, err := ItemsOf.Collect(ctx, conn, struct{ OrderID int64 }{id})
	if err != nil || len(items) != 2 || items[0].Amount != "25.00" {
		t.Fatalf("items: %v %+v", err, items)
	}
	totals, err := TotalsByCustomer.Collect(ctx, conn, struct{ MinOrders int64 }{1})
	if err != nil || len(totals) != 1 || totals[0].Spent != "28.00" {
		t.Fatalf("totals: %v %+v", err, totals)
	}

	// archived: gone from every view but archived_orders, and not there to be paid
	if _, err := Archive.Exec(ctx, conn, struct{ ID int64 }{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := OrderByID.Get(ctx, conn, struct{ ID int64 }{id}); !errors.Is(err, sqlshape.ErrNoRows) {
		t.Fatalf("archived order still visible: %v", err)
	}
	if err := Pay(ctx, conn, id); err == nil {
		t.Fatal("paying an archived order should fail")
	}
	archived, err := ArchivedOrders.Collect(ctx, conn, struct{}{})
	if err != nil || len(archived) != 1 || archived[0].ID != id || archived[0].Status != Paid {
		t.Fatalf("archived: %v %+v", err, archived)
	}
	totals, err = TotalsByCustomer.Collect(ctx, conn, struct{ MinOrders int64 }{0})
	if err != nil || len(totals) != 1 || totals[0].Orders != 0 {
		t.Fatalf("totals after archive: %v %+v", err, totals)
	}

	// a status this build does not know: the mapper refuses the row
	id2, err := PlaceOrder(ctx, conn, alice, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO order_statuses VALUES ('refunded', 'Refunded', 50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE orders SET status = 'refunded' WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	var unknown *sqlshape.UnknownLabelError
	if _, err := OrderByID.Get(ctx, conn, struct{ ID int64 }{id2}); !errors.As(err, &unknown) || unknown.Value != "refunded" {
		t.Fatalf("unknown label: %v", err)
	}
	if _, err := DeleteCustomer.Exec(ctx, conn, struct{ ID int64 }{alice}); !sqlshape.Violates(err, "orders_customer_id_fkey") {
		t.Fatalf("delete customer with orders: %v", err)
	}
}
