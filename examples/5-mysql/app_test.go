package mysqlexample

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

func TestOrders(t *testing.T) {
	ctx := context.Background()
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := mysqltest.Start(ctx, string(schema))
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	db := srv.Conn()

	if _, err := mysql.Exec(ctx, db, CreateCustomer, NewCustomer{Email: "alice@example.com", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mysql.Exec(ctx, db, CreateCustomer, NewCustomer{Email: "alice@example.com", Name: "Twin"}); !mysql.Violates(err, "customers_email_key") {
		t.Fatalf("duplicate email: %v", err)
	}
	alice, err := mysql.First(ctx, db, CustomerByEmail, struct{ Email string }{"alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mysql.Exec(ctx, db, CreateOrder, NewOrder{CustomerID: alice.ID, Total: "12.50"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mysql.Exec(ctx, db, CreateOrder, NewOrder{CustomerID: 999, Total: "1.00"}); !mysql.Violates(err, "fk_orders_customer") {
		t.Fatalf("unknown customer: %v", err)
	}
	orders, err := mysql.Collect(ctx, db, ListOrders, ListOrdersParams{CustomerID: alice.ID, Limit: 10})
	if err != nil || len(orders) != 1 || orders[0].Status != Pending || orders[0].Total != "12.50" || orders[0].Note != nil {
		t.Fatalf("orders: %+v %v", orders, err)
	}
	if _, err := mysql.Exec(ctx, db, SetStatus, struct {
		ID     uint64
		Status Status
	}{orders[0].ID, Paid}); err != nil {
		t.Fatal(err)
	}
	paid := Paid
	if got, err := mysql.Collect(ctx, db, ListOrders, ListOrdersParams{CustomerID: alice.ID, Status: &paid, Limit: 10}); err != nil || len(got) != 1 {
		t.Fatalf("paid orders: %d %v", len(got), err)
	}
	totals, err := mysql.Collect(ctx, db, OrderTotals, struct{}{})
	if err != nil || len(totals) != 1 || totals[0].N != 1 || totals[0].Total != "12.50" {
		t.Fatalf("totals: %+v %v", totals, err)
	}
}
