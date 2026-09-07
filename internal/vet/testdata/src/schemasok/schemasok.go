package schemasok

import "github.com/kr9ly/sqlshape"

// -schemas=public: this package's statements and Copy calls read/write "public", which is
// in the allowed set, so nothing is reported.

var q = sqlshape.Query[int64, struct{}](`SELECT id FROM orders`)

var bulk = sqlshape.Copy[ItemIn]("order_items")

type ItemIn struct {
	OrderID  int64
	LineNo   int16
	Sku      string
	Qty      int32
	Discount string
}
