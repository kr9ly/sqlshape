package app

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID        uint64
	Name      string
	Email     *string
	Active    bool
	CreatedAt time.Time `col:"created_at"`
}

type ByID struct{ ID uint64 }

var getUser = sqlshape.Query[User, ByID](`SELECT id, name, email, active, created_at FROM users WHERE id = {{.ID}}`)

type OrderRow struct {
	ID    uint64
	Total string
	Note  *string
	Name  string
}

type ListOrders struct {
	UserID uint64
	Limit  int64
}

var listOrders = sqlshape.Query[OrderRow, ListOrders](`
SELECT o.id, o.total, o.note, u.name
  FROM orders o JOIN users u ON u.id = o.user_id
 WHERE o.user_id = {{.UserID}}
 LIMIT {{.Limit}}
`)

type Wrong struct {
	ID    int32
	Email string
	Total string
}

var wrong = sqlshape.Query[Wrong, struct{ ID string }](`SELECT id, email, total FROM users WHERE id = {{.ID}}`) // want `Unknown column 'total' in 'field list' \(MySQL error 1054\)`

var wrong2 = sqlshape.Query[Wrong, struct{ ID string }](`SELECT id, email FROM users WHERE id = {{.ID}}`) // want `field ID is int32 but column "id" is bigint unsigned` `field Email is string but column "email" may be NULL` `field Wrong.Total has no result column` `parameter .ID is string but SQL expects bigint unsigned`

var noTable = sqlshape.Query[int64, struct{}](`SELECT id FROM customers`) // want `Table 'customers' doesn't exist \(MySQL error 1146\)`

var syntax = sqlshape.Query[int64, struct{}](`SELECT id FROM users WHERE`) // want `MySQL error 1064`

type Insert struct {
	Name  string
	Email *string
}

var insert = sqlshape.Query[struct{}, Insert](`INSERT INTO users (name, email) VALUES ({{.Name}}, {{.Email}})`)

var insertWrong = sqlshape.Query[struct{}, struct{ Name int64 }](`INSERT INTO users (name) VALUES ({{.Name}})`) // want `parameter .Name is int64 but SQL expects varchar\(100\)`

var update = sqlshape.Query[int64, struct{ Name string }](`UPDATE users SET name = {{.Name}} WHERE id = 1`) // want `R is int64 but the query returns 0 columns`

type Count struct{ N int64 }

var count = sqlshape.Query[Count, struct{}](`SELECT count(*) AS n FROM users`)

var expr = sqlshape.Query[string, struct{}](`SELECT COALESCE(email, name) FROM users`)

var exprNull = sqlshape.Query[string, struct{}](`SELECT COALESCE(email, NULL) FROM users`) // want `column COALESCE\(email, NULL\) is string but column "COALESCE\(email, NULL\)" may be NULL`

type Stats struct {
	N      int64
	Total  string
	Active bool
	Oldest *time.Time
}

var stats = sqlshape.Query[Stats, struct{}](`SELECT count(*) AS n, SUM(o.total) AS total, u.active = 1 AS active, MIN(u.created_at) AS oldest FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = 1`) // want `field Total is string but column "total" may be NULL`

// ONLY_FULL_GROUP_BY: a column outside the aggregates is not determined by the group
var mixed = sqlshape.Query[Stats, struct{}](`SELECT count(*) AS n, SUM(o.total) AS total, u.active = 1 AS active, MIN(u.created_at) AS oldest FROM users u JOIN orders o ON o.user_id = u.id`) // want `In aggregated query without GROUP BY, expression #3 of SELECT list contains nonaggregated column 'u.active'; this is incompatible with sql_mode=only_full_group_by \(MySQL error 1140\)`

var notDependent = sqlshape.Query[Stats, struct{}](`SELECT count(*) AS n, SUM(o.total) AS total, u.active = 1 AS active, MIN(u.created_at) AS oldest FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.name`) // want `Expression #3 of SELECT list is not in GROUP BY clause and contains nonaggregated column 'u.active' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by \(MySQL error 1055\)`

var unknownFn = sqlshape.Query[int64, struct{}](`SELECT NOPE(id) FROM users`) // want `FUNCTION NOPE does not exist \(MySQL error 1305\)`

var union = sqlshape.Query[int64, struct{}](`SELECT 1 UNION SELECT 2`)

var show = sqlshape.Query[int64, struct{}](`SHOW TABLES`) // want `is not supported yet`

// One: proved from the schema's keys through the facts the MySQL analyzer records
var userByID = sqlshape.One[User, struct{ ID uint64 }](`SELECT id, name, email, active, created_at FROM users WHERE id = {{.ID}}`)

var userByName = sqlshape.One[User, struct{ Name string }](`SELECT id, name, email, active, created_at FROM users WHERE name = {{.Name}}`) // want `One: cannot prove at most one row: users: no unique key is fixed by equality \(keys: \(id\)\)`

var orderOfUser = sqlshape.One[int64, struct{ ID uint64 }](`SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id WHERE u.id = {{.ID}}`) // want `One: cannot prove at most one row: orders o: no unique key is fixed by equality`

var countUsers = sqlshape.One[int64, struct{}](`SELECT count(*) FROM users`)

var markActive = sqlshape.One[struct{}, struct{ ID uint64 }](`UPDATE users SET active = 1 WHERE id = {{.ID}}`)

// Failure modes: the expect line names the constraints a write may violate, as the server
// names them (the key's name, the CONSTRAINT's, table.column for NOT NULL)
type NewOrder struct {
	ID     uint64
	UserID uint64
	Total  string
}

var insertOrder = sqlshape.Query[struct{}, NewOrder]("-- sqlshape: expect PRIMARY, fk_orders_user\nINSERT INTO orders (id, user_id, total) VALUES ({{.ID}}, {{.UserID}}, {{.Total}})")

var insertOrderBare = sqlshape.Query[struct{}, NewOrder](`INSERT INTO orders (id, user_id, total) VALUES ({{.ID}}, {{.UserID}}, {{.Total}})`) // want `may violate PRIMARY \(PRIMARY KEY \(id\) on orders, MySQL error 1062\)` `may violate fk_orders_user \(FOREIGN KEY fk_orders_user \(user_id\) on orders REFERENCES users, MySQL error 1452\)`

var insertOrderNullable = sqlshape.Query[struct{}, struct {
	ID     uint64
	UserID *uint64
	Total  string
}]("-- sqlshape: expect PRIMARY, fk_orders_user\nINSERT INTO orders (id, user_id, total) VALUES ({{.ID}}, {{.UserID}}, {{.Total}})") // want `may violate orders.user_id \(NOT NULL on orders.user_id, MySQL error 1048\)`

var insertOrderIgnore = sqlshape.Query[struct{}, NewOrder](`INSERT IGNORE INTO orders (id, user_id, total) VALUES ({{.ID}}, {{.UserID}}, {{.Total}})`)

var staleExpect = sqlshape.Query[struct{}, struct {
	ID   uint64
	Name string
}]("-- sqlshape: expect fk_orders_user\nUPDATE users SET name = {{.Name}} WHERE id = {{.ID}}") // want `expects fk_orders_user but no expansion can violate it`

var deleteUser = sqlshape.Query[struct{}, struct{ ID uint64 }](`DELETE FROM users WHERE id = {{.ID}}`) // want `may violate fk_orders_user \(FOREIGN KEY fk_orders_user \(user_id\) on orders REFERENCES users: a row of orders still refers to the one changed, MySQL error 1451\)`

// Obligations: `require pinned(tenant_id)` on tenant_notes, judged through the contract
var noteByID = sqlshape.Query[string, struct{ ID uint64 }](`SELECT body FROM tenant_notes WHERE id = {{.ID}}`) // want `tenant_notes.tenant_id is not pinned: every statement on tenant_notes must fix tenant_id by equality \(or assign it\)`

var noteByTenant = sqlshape.Query[string, struct {
	ID       uint64
	TenantID uint64
}](`SELECT body FROM tenant_notes WHERE id = {{.ID}} AND tenant_id = {{.TenantID}}`)

var noteViaView = sqlshape.Query[string, struct{ ID uint64 }](`SELECT body FROM all_notes WHERE id = {{.ID}}`)

var noteWaived = sqlshape.Query[string, struct{ ID uint64 }]("-- sqlshape: waive tenant_notes pinned(tenant_id)\nSELECT body FROM tenant_notes WHERE id = {{.ID}}")

type NewNote struct {
	ID       uint64
	TenantID uint64
	Body     string
}

var insertNote = sqlshape.Query[struct{}, NewNote]("-- sqlshape: expect PRIMARY, tenant_notes_tenant_body\nINSERT INTO tenant_notes (id, tenant_id, body) VALUES ({{.ID}}, {{.TenantID}}, {{.Body}})") // want `expects tenant_notes_tenant_body but no expansion can violate it`

var moveNote = sqlshape.Query[struct{}, struct {
	ID       uint64
	TenantID uint64
}]("-- sqlshape: expect PRIMARY\nUPDATE tenant_notes SET id = {{.ID}} WHERE tenant_id = {{.TenantID}}")
