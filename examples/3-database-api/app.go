package dbapi

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/postgres/v2"
)

// Beginner starts transactions: pgx.Conn and pgxpool.Pool both do.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Checkout places an order with its lines through the database's functions, in one
// transaction: a rejected line leaves no half-built order behind. pgx.Tx satisfies
// postgres.DB, so the statements run on the transaction unchanged. The limit on open
// orders is enforced by a trigger; the application only has to name the outcome.
func Checkout(ctx context.Context, db Beginner, customerID int64, shipping Yen, lines []NewLine) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // a no-op after Commit

	id, err := postgres.Get(ctx, tx, PlaceOrder, NewOrder{CustomerID: customerID, Shipping: shipping})
	switch {
	case postgres.Violates(err, "OS001"):
		return 0, fmt.Errorf("customer %d has too many open orders", customerID)
	case postgres.Violates(err, "orders_customer_id_fkey"):
		return 0, fmt.Errorf("customer %d does not exist", customerID)
	case err != nil:
		return 0, err
	}
	for _, l := range lines {
		l.OrderID = *id
		if _, err := postgres.Get(ctx, tx, AddLine, l); err != nil {
			if postgres.Violates(err, "order_items_qty_check") {
				return 0, fmt.Errorf("%s: quantity must be positive", l.Sku)
			}
			return 0, err
		}
	}
	return *id, tx.Commit(ctx)
}

// Label is the customer-facing tier name; the switch must cover every value of the set.
func Label(t Tier) string {
	switch t {
	case Free:
		return "Free"
	case Pro:
		return "Pro"
	case Enterprise:
		return "Enterprise"
	}
	return string(t)
}

// Format prints yen the Japanese way. Yen is a unit: the checker keeps it apart from
// other bigint columns, this function keeps the formatting in one place.
func Format(y Yen) string { return fmt.Sprintf("¥%d", int64(y)) }
