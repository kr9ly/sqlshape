package a

import (
	"time"

	"b"

	"github.com/kr9ly/sqlshape"
)

type OrderStatus string // want OrderStatus:`bound e order_status`

type OrderRow struct {
	ID        int64
	Status    OrderStatus
	Total     string
	Note      *string
	CreatedAt time.Time `col:"created_at"`
}

type ListParams struct {
	Status  *OrderStatus
	UserIDs []int64
	Limit   int32
}

var listOrders = sqlshape.Query[OrderRow, ListParams](`
SELECT o.id, o.status, o.total, o.note, o.created_at
  FROM orders o
 WHERE true
   {{if .Status}} AND o.status = {{.Status}} {{end}}
   {{if .UserIDs}} AND o.user_id = ANY({{.UserIDs}}) {{end}}
 LIMIT {{.Limit}}
`)

// --- findings -------------------------------------------------------------

type BadRow struct {
	ID    int32  // bigint into int32
	Total string // fine
	Note  string // nullable column into plain string
	Extra bool   // no such column
}

var badTypes = sqlshape.Query[BadRow, struct{}](`SELECT id, total, note, status FROM orders`) // want `field ID: bigint into int32` `field Note is string but column "note" may be NULL` `field BadRow.Extra has no result column` `result column "status" has no field in a.BadRow`

type P2 struct{ Email int }

var badParam = sqlshape.Query[int64, P2](`SELECT id FROM users WHERE email = {{.Email}}`) // want `parameter .Email is int but SQL expects text`

var badColumn = sqlshape.Query[int64, struct{}](`SELECT idd FROM users`) // want `column "idd" does not exist \(SQLSTATE 42703\)`

type P3 struct{ Status string }

var badBranch = sqlshape.Query[int64, P3](`SELECT id FROM orders WHERE true {{if .Status}} AND status = {{.Status}} AND nope = 1 {{end}}`) // want `column "nope" does not exist \(SQLSTATE 42703\) \[if@\d+:then\]`

var badPath = sqlshape.Query[int64, P3](`SELECT id FROM orders WHERE status = {{.Missing}}`) // want `a.P3 has no field Missing`

var scalarOK = sqlshape.Query[int64, struct{ Min string }](`SELECT count(*) FROM orders WHERE total > {{.Min}}`)

var scalarBad = sqlshape.Query[string, struct{}](`SELECT id, email FROM users`) // want `R is string but the query returns 2 columns`

// --- interpretation sharing -----------------------------------------------

type LocalStatus string // want LocalStatus:`consts oops,paid,pending` LocalStatus:`bound e order_status`

const (
	LocalPending LocalStatus = "pending"
	LocalPaid    LocalStatus = "paid"
	LocalOops    LocalStatus = "oops"
)

// binds LocalStatus to enum order_status: labels shipped/cancelled have no constant, "oops" is not a label
var byLocalStatus = sqlshape.Query[int64, struct{ S LocalStatus }](`SELECT id FROM orders WHERE status = {{.S}}`) // want `enum order_status has label "shipped" but LocalStatus has no constant for it` `enum order_status has label "cancelled" but LocalStatus has no constant for it` `LocalStatus has constant "oops" which is not a label of enum order_status`

// cross-package constants via facts: b.Status has "refunded" which the enum lacks
var byImportedStatus = sqlshape.Query[int64, struct{ S b.Status }](`SELECT id FROM orders WHERE status = {{.S}}`) // want `Status has constant "refunded" which is not a label of enum order_status`

func conv() LocalStatus {
	switch s := LocalStatus("paid"); s { // want `switch on LocalStatus does not handle enum order_status labels: shipped, cancelled`
	case LocalPending:
		return s
	case LocalPaid:
		return s
	}
	return LocalStatus("bogus") // want `LocalStatus\("bogus"\) is not a label of enum order_status`
}

// key identities: UserID is bound to users.id by the first query, then meets orders.id
type UserID int64 // want UserID:`bound k users.id`

var byUser = sqlshape.Query[int64, struct{ U UserID }](`SELECT id FROM orders WHERE user_id = {{.U}}`)

var wrongKey = sqlshape.Query[int64, struct{ U UserID }](`SELECT id FROM orders WHERE id = {{.U}}`) // want `parameter .U is a.UserID, which stands for key users.id elsewhere, but here meets key orders.id`

type OrderIDRow struct {
	ID UserID // result column orders.id received as UserID
}

var wrongKeyResult = sqlshape.Query[OrderIDRow, struct{}](`SELECT id FROM orders`) // want `field ID is a.UserID, which stands for key users.id elsewhere, but here meets key orders.id`
