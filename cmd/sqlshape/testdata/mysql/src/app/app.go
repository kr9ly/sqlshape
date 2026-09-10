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

var stats = sqlshape.Query[Stats, struct{}](`SELECT count(*) AS n, SUM(o.total) AS total, u.active = 1 AS active, MIN(u.created_at) AS oldest FROM users u JOIN orders o ON o.user_id = u.id`) // want `field Total is string but column "total" may be NULL`

var unknownFn = sqlshape.Query[int64, struct{}](`SELECT NOPE(id) FROM users`) // want `FUNCTION NOPE does not exist \(MySQL error 1305\)`

var union = sqlshape.Query[int64, struct{}](`SELECT 1 UNION SELECT 2`)

var show = sqlshape.Query[int64, struct{}](`SHOW TABLES`) // want `is not supported yet`
