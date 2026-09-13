package errnameunknown

import "github.com/kr9ly/sqlshape/v2"

// Ghost names a code the schema does not declare at all.
var Ghost = sqlshape.Error("P9999") // want `the schema declares no error "P9999"` Ghost:`sqlshape.Error\(P9999\)`

// a read, so the trigger's own P0501 is not itself a possible violation to report here
// (that is errnamemissing's own case): this file is only about Ghost's own code.
var read = sqlshape.Query[int64, struct{}]("SELECT id FROM widgets")
