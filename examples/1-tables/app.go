package tables

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/postgres"
)

// PlaceOrder creates an order with its lines in one transaction and returns its id.
// pgx.Tx satisfies postgres.DB, so statements run on the transaction unchanged.
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

// Pay moves an order to paid. A missing order is a distinct error from a database failure.
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

// Describe renders an order for a receipt; switching on Status is checked to cover every label.
func Describe(o Order) string {
	var state string
	switch o.Status {
	case Pending:
		state = "awaiting payment"
	case Paid:
		state = "paid, preparing shipment"
	case Shipped:
		state = "on its way"
	case Cancelled:
		state = "cancelled"
	}
	return fmt.Sprintf("order #%d: %s (%s)", o.ID, o.Total, state)
}
