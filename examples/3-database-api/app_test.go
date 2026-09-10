package dbapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/pgtest"
	"github.com/kr9ly/sqlshape/postgres"
)

func TestDatabaseAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := pgtest.ReadSchema("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgtest.Start(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Verify(ctx, CustomerByEmail, CustomersByTier, ListOrders, OrderByID, LinesOf, OpenOrders,
		SalesSince, CreateCustomer, SetAddress, PlaceOrder, AddLine, PayOrder); err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()

	alice, err := postgres.Get(ctx, conn, CreateCustomer, NewCustomer{Email: "alice@example.com", Name: "Alice", Tier: Free})
	if err != nil || alice == nil {
		t.Fatalf("create customer: %v", err)
	}
	if _, err := postgres.Get(ctx, conn, CreateCustomer, NewCustomer{Email: "not-an-email", Name: "X", Tier: Free}); !postgres.Violates(err, "email_check") {
		t.Fatalf("email domain: %v", err)
	}
	street, city, zip := "1-1 Chiyoda", "Tokyo", "100-0001"
	if _, err := postgres.ExecOne(ctx, conn, SetAddress, struct {
		CustomerID int64
		Address    Address
	}{*alice, Address{&street, &city, &zip}}); err != nil {
		t.Fatalf("set address: %v", err)
	}
	c, err := postgres.Get(ctx, conn, CustomerByEmail, struct{ Email string }{"alice@example.com"})
	if err != nil || c.Tier != Free || c.ShipsTo == nil || *c.ShipsTo.City != "Tokyo" {
		t.Fatalf("customer: %v %+v", err, c)
	}

	id, err := Checkout(ctx, conn, *alice, 500, []NewLine{{Sku: "BOOK", Qty: 2, UnitPrice: 1200}, {Sku: "PEN", Qty: 1, UnitPrice: 300}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := postgres.Get(ctx, conn, OrderByID, struct{ ID int64 }{id})
	if err != nil || o.Subtotal != 2700 || o.Total != 3200 || o.Lines != 2 || o.CustomerEmail != "alice@example.com" {
		t.Fatalf("order view: %v %+v", err, o)
	}
	if Format(o.Total) != "¥3200" {
		t.Errorf("format: %s", Format(o.Total))
	}
	lines, err := postgres.Collect(ctx, conn, LinesOf, struct{ OrderID int64 }{id})
	if err != nil || len(lines) != 2 || lines[0].Amount != 2400 {
		t.Fatalf("lines: %v %+v", err, lines)
	}

	if _, err := Checkout(ctx, conn, 999, 0, nil); err == nil || err.Error() != "customer 999 does not exist" {
		t.Fatalf("fk: %v", err)
	}
	// shipping is a yen: the domain's CHECK (>= 0) is not one of Checkout's named
	// outcomes, so the generic path returns the raw error
	if _, err := Checkout(ctx, conn, *alice, -1, nil); !postgres.Violates(err, "yen_check") {
		t.Fatalf("negative shipping: %v", err)
	}
	// a context already done fails at Begin, before any statement runs; use a
	// throwaway connection since a cancelled context poisons the one it was used on
	deadConn, err := pgx.Connect(ctx, db.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	doneCtx, doneCancel := context.WithCancel(ctx)
	doneCancel()
	if _, err := Checkout(doneCtx, deadConn, *alice, 0, nil); err == nil {
		t.Fatal("checkout on a cancelled context should fail")
	}
	deadConn.Close(ctx)

	// a failing line rolls the whole Checkout back: neither of these leaves a
	// pending order behind for the trigger test below to trip over
	if _, err := postgres.Get(ctx, conn, AddLine, NewLine{OrderID: id, Sku: "BOOK", Qty: 0, UnitPrice: 100}); !postgres.Violates(err, "order_items_qty_check") {
		t.Fatalf("add line qty check: %v", err)
	}
	if _, err := Checkout(ctx, conn, *alice, 0, []NewLine{{Sku: "BOOK", Qty: 0, UnitPrice: 100}}); err == nil || err.Error() != "BOOK: quantity must be positive" {
		t.Fatalf("checkout qty check: %v", err)
	}
	// a negative unit price is a yen too: not a quantity problem, so the generic
	// path returns the raw error
	if _, err := Checkout(ctx, conn, *alice, 0, []NewLine{{Sku: "BOOK", Qty: 1, UnitPrice: -1}}); !postgres.Violates(err, "yen_check") {
		t.Fatalf("checkout negative unit price: %v", err)
	}

	// the trigger: a free customer may hold three open orders, not four
	for i := 0; i < 2; i++ {
		if _, err := Checkout(ctx, conn, *alice, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Checkout(ctx, conn, *alice, 0, nil); err == nil || err.Error() != "customer 1 has too many open orders" {
		t.Fatalf("open order limit: %v", err)
	}
	if n, err := postgres.Get(ctx, conn, OpenOrders, struct{ CustomerID int64 }{*alice}); err != nil || n != 3 {
		t.Fatalf("open orders: %v %d", err, n)
	}

	st, err := postgres.Get(ctx, conn, PayOrder, struct{ OrderID int64 }{id})
	if err != nil || st == nil || *st != Paid {
		t.Fatalf("pay: %v %v", err, st)
	}
	if st, err := postgres.Get(ctx, conn, PayOrder, struct{ OrderID int64 }{id}); err != nil || st != nil {
		t.Fatalf("pay twice: %v %v", err, st)
	}
	min := Yen(3000)
	rows, err := postgres.Collect(ctx, conn, ListOrders, ListOrdersParams{CustomerID: alice, Statuses: []OrderStatus{Paid}, MinTotal: &min})
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("list: %v %+v", err, rows)
	}

	if err := Sales.Refresh(ctx, conn); err != nil {
		t.Fatal(err)
	}
	sales, err := postgres.Collect(ctx, conn, SalesSince, struct{ Since time.Time }{time.Now().AddDate(0, 0, -1)})
	if err != nil || len(sales) != 1 || sales[0].Revenue != 2700 {
		t.Fatalf("sales: %v %+v", err, sales)
	}
	if got := Label(Free); got != "Free" {
		t.Errorf("label free: %q", got)
	}
	if got := Label(Pro); got != "Pro" {
		t.Errorf("label pro: %q", got)
	}
	if got := Label(Enterprise); got != "Enterprise" {
		t.Errorf("label enterprise: %q", got)
	}
	// a tier the switch does not name falls back to the raw value
	if got := Label(Tier("gold")); got != "gold" {
		t.Errorf("label unknown: %q", got)
	}
	raw := "refunded" // a value that arrives at run time, not a constant the checker would diff
	if OrderStatus(raw).Known() {
		t.Error("refunded should not be a known status")
	}
}
