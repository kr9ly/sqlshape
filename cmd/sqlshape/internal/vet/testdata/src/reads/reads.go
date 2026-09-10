package reads

import "github.com/kr9ly/sqlshape"

// -no-table-reads: reads go through views; tables are written, not read

var viaView = sqlshape.Query[int64, struct{}](`SELECT id FROM order_summary`)

var direct = sqlshape.Query[int64, struct{}](`SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id`) // want `table orders is read directly; with -no-table-reads application code reads views \(tables are written, not read\)` `table users is read directly`

var write = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}}")

var writeReadingAnother = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND user_id IN (SELECT id FROM users)") // want `table users is read directly`

var writeFromView = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND id IN (SELECT id FROM order_summary)")

var insertSelect = sqlshape.Query[struct{}, struct{}]("-- sqlshape: expect orders_pkey, orders_user_note_key, orders_user_id_fkey, orders_total_check, P0401\nINSERT INTO orders (user_id, total) SELECT id, 0 FROM users") // want `table users is read directly`
