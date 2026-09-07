package otherschema

import "github.com/kr9ly/sqlshape"

// A relation outside the public schema: checkReferences qualifies its name with the
// schema, unlike a public-schema reference.
var q = sqlshape.Query[int64, struct{}](`SELECT id FROM priv.thing`)
