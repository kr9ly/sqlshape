// Package ctx is judged under the ops context: the tenant pin is lifted, a DELETE must be keyed.
//
// sqlshape: context ops
package ctx

import "github.com/kr9ly/sqlshape"

var count = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM orders`)

var purge = sqlshape.Query[struct{}, struct{}](`DELETE FROM orders WHERE status = 'void'`) // want `orders requires id = \$1 here: add that predicate for orders`

var one = sqlshape.Query[struct{}, struct{ ID int64 }](`DELETE FROM orders WHERE id = {{.ID}}`)
