package dtouuid

import (
	"github.com/google/uuid"
	"github.com/kr9ly/sqlshape"
	"github.com/shopspring/decimal"
)

var _, _ = uuid.UUID{}, decimal.Decimal{}

// With uuid and decimal imported, the reverse mapping prefers them over pgtype.
type RichRow struct{}

var richRow = sqlshape.Query[RichRow, struct{}](`SELECT uid, total FROM orders`) // want `result column "uid" has no field` `result column "total" has no field`
