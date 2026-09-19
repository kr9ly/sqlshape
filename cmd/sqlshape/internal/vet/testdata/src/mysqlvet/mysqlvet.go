// Package mysqlvet runs the analyzer against a MySQL schema in-process: the obligation
// declared on a view, and the type-binding directive MySQL has nothing to bind to.
package mysqlvet

import (
	"github.com/kr9ly/sqlshape/v2"
)

// sqlshape: type point
type Geo string // want `type Geo: .*binds to a named type of the schema, and mysql has none` Geo:`carries point`

var pinned = sqlshape.Query[uint64, struct{ Tenant uint64 }](`SELECT id FROM v_orders WHERE tenant_id = {{.Tenant}}`)

var unpinned = sqlshape.Query[uint64, struct{}](`SELECT id FROM v_orders`) // want `v_orders.tenant_id is not pinned: every statement on v_orders must fix tenant_id by equality \(or assign it\)`

var insertAssigns = sqlshape.Query[struct{}, struct {
	ID     uint64
	Tenant uint64
	Total  int32
}](`-- sqlshape: expect PRIMARY
INSERT INTO v_orders (id, tenant_id, total) VALUES ({{.ID}}, {{.Tenant}}, {{.Total}})`)
