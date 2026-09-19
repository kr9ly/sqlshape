package dto

import "github.com/kr9ly/sqlshape/v2"

// The lone boolean and smallint columns the rest of the package never exercises.
type FlagRow struct{}

var flagRow = sqlshape.Query[FlagRow, struct{}](`SELECT active, tiny FROM flags_test`) // want `result column "active" has no field` `result column "tiny" has no field`
