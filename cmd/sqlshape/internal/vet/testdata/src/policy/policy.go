package policy

import (
	"github.com/kr9ly/sqlshape/v2"
	"github.com/kr9ly/sqlshape/postgres/v2"
)

// -no-tables: application code reads views and calls functions only

var viaView = sqlshape.Query[int64, struct{}](`SELECT id FROM order_summary`)

var viaFunc = sqlshape.Query[*int64, struct{}](`SELECT * FROM user_ids()`)

var direct = sqlshape.Query[int64, struct{}](`SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id`) // want `table orders is referenced directly; with -no-tables application code reads views and calls functions only` `table users is referenced directly`

var write = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}}") // want `table orders is referenced directly`

// interpreted string: the diagnostic lands on the reference inside the template
var escaped = sqlshape.Query[int64, struct{}]("SELECT id\n  FROM orders") // want `table orders is referenced directly`

var bulk = postgres.Copy[struct{ ID int64 }]("users", "id") // want `table users is written directly; with -no-tables application code reads views and calls functions only`
