package dto

import "github.com/kr9ly/sqlshape"

var blank = sqlshape.Query[Blank, struct{}](`SELECT id, created_at FROM orders`) // want `result column "id" has no field` `result column "created_at" has no field`

var grouped = sqlshape.Query[Grouped, struct{}](`SELECT id, total FROM orders`) // want `result column "id" has no field` `result column "total" has no field`
