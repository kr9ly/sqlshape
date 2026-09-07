package tables

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/pgtest"
)

func TestOrderBook(t *testing.T) {
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

	// the static checks, confirmed against this PostgreSQL for every statement
	if err := db.Verify(ctx, CreateCustomer, CustomerByEmail, DeleteCustomer, ListOrders, OrderByID,
		CreateOrder, AddItem, RecomputeTotal, SetStatus, Cancel, ItemsOf, TotalsByCustomer); err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()

	alice, err := CreateCustomer.First(ctx, conn, NewCustomer{Email: "alice@example.com", Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCustomer.First(ctx, conn, NewCustomer{Email: "alice@example.com", Name: "Alice again"}); !sqlshape.Violates(err, "customers_email_key") {
		t.Fatalf("duplicate email: %v", err)
	}

	note := "gift wrap"
	id, err := PlaceOrder(ctx, conn, alice, &note, []NewItem{{Sku: "BOOK", Qty: 2, Price: "12.50"}, {Sku: "PEN", Qty: 1, Price: "3.00"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlaceOrder(ctx, conn, alice, nil, []NewItem{{Sku: "BOOK", Qty: 0, Price: "1"}}); err == nil || err.Error() != "line 1: quantity must be positive" {
		t.Fatalf("qty check: %v", err)
	}
	if _, err := PlaceOrder(ctx, conn, 999, nil, nil); err == nil || err.Error() != "customer 999 does not exist" {
		t.Fatalf("fk: %v", err)
	}

	o, err := OrderByID.Get(ctx, conn, struct{ ID int64 }{id})
	if err != nil || o.Total != "28.00" || o.Status != Pending || o.Note == nil || *o.Note != "gift wrap" {
		t.Fatalf("order: %v %+v", err, o)
	}
	if err := Pay(ctx, conn, id); err != nil {
		t.Fatal(err)
	}
	if err := Pay(ctx, conn, id+100); err == nil {
		t.Fatal("paying a missing order should fail")
	}
	// cancelling a paid order changes nothing: One.Exec says so with ErrNoRows
	if _, err := Cancel.Exec(ctx, conn, struct{ ID int64 }{id}); !errors.Is(err, sqlshape.ErrNoRows) {
		t.Fatalf("cancel paid: %v", err)
	}

	paid := []OrderStatus{Paid, Shipped}
	rows, err := ListOrders.Collect(ctx, conn, ListOrdersParams{CustomerID: &alice, Statuses: paid, Sort: "total", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("list: %v %+v", err, rows)
	}
	if got := Describe(rows[0]); got != "order #1: 28.00 (paid, preparing shipment)" {
		t.Errorf("describe: %q", got)
	}
	items, err := ItemsOf.Collect(ctx, conn, struct{ OrderID int64 }{id})
	if err != nil || len(items) != 2 || items[1].Sku != "PEN" {
		t.Fatalf("items: %v %+v", err, items)
	}
	totals, err := TotalsByCustomer.Collect(ctx, conn, struct{ MinOrders int64 }{0})
	if err != nil || len(totals) != 1 || totals[0].Spent != "28.00" || totals[0].LastOrder == nil {
		t.Fatalf("totals: %v %+v", err, totals)
	}

	// the database knows a status this build does not: the mapper refuses the row
	if _, err := conn.Exec(ctx, `INSERT INTO order_statuses VALUES ('refunded', 'Refunded', 50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE orders SET status = 'refunded' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var unknown *sqlshape.UnknownLabelError
	if _, err := OrderByID.Get(ctx, conn, struct{ ID int64 }{id}); !errors.As(err, &unknown) || unknown.Value != "refunded" {
		t.Fatalf("unknown label: %v", err)
	}

	if _, err := DeleteCustomer.Exec(ctx, conn, struct{ ID int64 }{alice}); !sqlshape.Violates(err, "orders_customer_id_fkey") {
		t.Fatalf("delete customer with orders: %v", err)
	}
}
