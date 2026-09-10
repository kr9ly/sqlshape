// Package adv_context tries to select the "ops" context, but the directive line carries
// a trailing note after the name.
//
// sqlshape: context ops (temporary, remove after the migration)
package adv_context

import "github.com/kr9ly/sqlshape/v2"

// The trailing text after the context name is not a recognized context directive, so
// no context is selected: the base obligation applies, and the unrecognized directive
// is itself flagged.
var purge = sqlshape.Query[struct{}, struct{}](`DELETE FROM orders WHERE id = 1`) // want `is not a context directive` `orders.tenant_id is not pinned`
