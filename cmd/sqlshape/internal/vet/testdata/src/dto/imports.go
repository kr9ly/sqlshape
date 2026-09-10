package dto

import "github.com/kr9ly/sqlshape"

// A fix that needs imports the file lacks adds them (a single import becomes a block).
type Stamped struct{}

var stamped = sqlshape.Query[Stamped, struct{}](`SELECT id, created_at, total FROM orders`) // want `result column "id" has no field` `result column "created_at" has no field` `result column "total" has no field`
