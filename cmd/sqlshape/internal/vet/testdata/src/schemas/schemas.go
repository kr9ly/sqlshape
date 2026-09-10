package schemas

import (
	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/postgres"
)

// -schemas=other: this package's statements and Copy calls read/write "public", which is
// outside the allowed set.

var q = sqlshape.Query[int64, struct{}](`SELECT id FROM orders`) // want `orders is outside the schemas this code may reference \(other\)`

var bulk = postgres.Copy[ItemIn]("order_items") // want `order_items is outside the schemas this code may reference \(other\)`

type ItemIn struct {
	OrderID  int64
	LineNo   int16
	Sku      string
	Qty      int32
	Discount string
}
