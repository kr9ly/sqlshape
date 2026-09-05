package dbapi

import (
	"context"
	"fmt"

	"github.com/kr9ly/sqlshape"
)

// Checkout places an order through the database's functions. The limit on open orders is
// enforced by a trigger; the application only has to name the outcome.
func Checkout(ctx context.Context, db sqlshape.DB, customerID int64, shipping Yen, lines []NewLine) (int64, error) {
	id, err := PlaceOrder.Get(ctx, db, NewOrder{CustomerID: customerID, Shipping: shipping})
	switch {
	case sqlshape.Violates(err, "OS001"):
		return 0, fmt.Errorf("customer %d has too many open orders", customerID)
	case sqlshape.Violates(err, "orders_customer_id_fkey"):
		return 0, fmt.Errorf("customer %d does not exist", customerID)
	case err != nil:
		return 0, err
	}
	for _, l := range lines {
		l.OrderID = *id
		if _, err := AddLine.Get(ctx, db, l); err != nil {
			if sqlshape.Violates(err, "order_items_qty_check") {
				return 0, fmt.Errorf("%s: quantity must be positive", l.Sku)
			}
			return 0, err
		}
	}
	return *id, nil
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
