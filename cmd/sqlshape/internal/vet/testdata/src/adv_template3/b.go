package adv_template3

import "github.com/kr9ly/sqlshape/v2"

// Control case for a.go: the same bad column, called directly through
// `sqlshape.Query[...](...)` instead of through a var, is correctly analyzed and flagged.
// This proves the gap in a.go is specifically about the var-indirection call shape, not
// about the schema or the SQL itself.
var direct = sqlshape.Query[int64, struct{}](`SELECT nope2 FROM widgets`) // want `sqlshape: column "nope2" does not exist`
