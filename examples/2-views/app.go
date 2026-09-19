package views

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/postgres/v2"
)

// PlaceOrder creates an order with its lines in one transaction and returns its id.
func PlaceOrder(ctx context.Context, conn *pgx.Conn, customerID int64, note *string, items []NewItem) (int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	id, err := postgres.First(ctx, tx, CreateOrder, NewOrder{CustomerID: customerID, Note: note})
	if err != nil {
		if postgres.Violates(err, "orders_customer_id_fkey") {
			return 0, fmt.Errorf("customer %d does not exist", customerID)
		}
		return 0, err
	}
	for i, it := range items {
		it.OrderID, it.LineNo = id, int16(i+1)
		if _, err := postgres.Exec(ctx, tx, AddItem, it); err != nil {
			if postgres.Violates(err, "order_items_qty_check") {
				return 0, fmt.Errorf("line %d: quantity must be positive", i+1)
			}
			return 0, err
		}
	}
	if _, err := postgres.ExecOne(ctx, tx, RecomputeTotal, struct{ OrderID int64 }{id}); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// Pay moves an order to paid. A missing (or archived) order is a distinct error.
func Pay(ctx context.Context, db postgres.DB, orderID int64) error {
	_, err := postgres.ExecOne(ctx, db, SetStatus, struct {
		ID     int64
		Status OrderStatus
	}{orderID, Paid})
	if errors.Is(err, postgres.ErrNoRows) {
		return fmt.Errorf("order %d does not exist", orderID)
	}
	return err
}

// Describe renders an order for a receipt. The status label comes from the view: the
// database names the states, the application does not keep a copy of the names.
func Describe(o Order) string {
	return fmt.Sprintf("order #%d for %s: %s (%s)", o.ID, o.CustomerEmail, o.Total, o.StatusLabel)
}
