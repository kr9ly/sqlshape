package dto

import "github.com/kr9ly/sqlshape/v2"

// role is a CHECK-based value set no Go type in this package is bound to: it falls
// through to the plain string mapping. tags is a plain array; score / ratio the two
// float kinds; flags / vflags the bit strings.
type RoleRow struct{}

var roleRow = sqlshape.Query[RoleRow, struct{}](`SELECT role, tags, score, ratio, flags, vflags FROM users`) // want `result column "role" has no field` `result column "tags" has no field` `result column "score" has no field` `result column "ratio" has no field` `result column "flags" has no field` `result column "vflags" has no field`
