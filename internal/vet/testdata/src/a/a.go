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

// domains are opaque units: yen (users.balance) does not meet plain bigint / numeric
type Yen int64 // want Yen:`bound d yen`

var mixedUnits = sqlshape.Query[int64, struct{}](`SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.balance > o.total`) // want `domain mismatch: yen > numeric\(12,2\): operands must share the domain`

var unitsOK = sqlshape.Query[Yen, struct{ Min Yen }](`SELECT coalesce(max(balance), 0) FROM users WHERE balance > {{.Min}}`)

var unitsCast = sqlshape.Query[int64, struct{}](`SELECT u.balance::bigint + o.user_id FROM users u JOIN orders o ON o.user_id = u.id`)

// One: the checker proves "at most one row" from unique keys fixed by equality
var userByEmail = sqlshape.One[int64, struct{ Email string }](`SELECT id FROM users WHERE email = {{.Email}}`)

var orderByUID = sqlshape.One[int64, struct{ UID string }](`SELECT id FROM orders WHERE uid = {{.UID}}`) // want `One: cannot prove at most one row: orders: no unique key is fixed by equality \(keys: \(id\), \(user_id, note\), \(uid\) WHERE \.\.\.\)`

var orderMaybeByID = sqlshape.One[int64, struct{ ID *int64 }](`SELECT id FROM orders WHERE true {{if .ID}} AND id = {{.ID}} {{end}}`) // want `One: cannot prove at most one row: orders: no unique key is fixed by equality \(keys: .*\) \[if@\d+:else\]`

var summaryByID = sqlshape.One[int64, struct{ ID int64 }](`SELECT id FROM order_summary WHERE id = {{.ID}}`)

// failure modes: the template's expect line must match what the schema says can fail
// (interpreted strings: diagnostics inside the template fall back to the literal's line)
type NewUser struct {
	Email string
	Name  *string
}

var insertUser = sqlshape.Query[int64, NewUser]("-- sqlshape: expect users_email_key, email_check\nINSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id") // want "may violate users_pkey \\(UNIQUE \\(id\\) on users, SQLSTATE 23505\\); add `-- sqlshape: expect users_pkey` to the template or make it impossible"

var insertUserOK = sqlshape.Query[int64, NewUser]("-- sqlshape: expect users_email_key, email_check, users_pkey\nINSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id")

var nullableEmail = sqlshape.Query[int64, struct{ Email *string }]("-- sqlshape: expect users_email_key, email_check, users_pkey\nINSERT INTO users (email) VALUES ({{.Email}}) RETURNING id") // want "may violate users.email \\(NOT NULL on users.email, SQLSTATE 23502\\)"

var staleExpect = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect orders_user_note_key\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}}") // want "expects orders_user_note_key but no expansion can violate it"

var branchViolation = sqlshape.Query[struct{}, struct {
	ID   int64
	Note *string
}](`UPDATE orders SET status = 'paid' {{if .Note}}, note = {{.Note}} {{end}} WHERE id = {{.ID}}`) // want "may violate orders_user_note_key \\(UNIQUE \\(user_id, note\\) on orders, SQLSTATE 23505\\); add `-- sqlshape: expect orders_user_note_key` to the template or make it impossible \\[if@\\d+:then\\]"

// nested rows: array_agg(row(...)) is positional, array_agg(t) / composite columns follow the type's column order
type OrderBrief struct {
	ID    int64
	Total string
}

type UserOrders struct {
	ID     int64
	Orders []OrderBrief
}

var userOrders = sqlshape.Query[UserOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`)

type ShortBrief struct{ ID int64 }

type UserShortOrders struct {
	ID     int64
	Orders []ShortBrief
}

var badArity = sqlshape.Query[UserShortOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders: a.ShortBrief has 1 fields but the row type has 2 \("f1 bigint, f2 numeric\(12,2\)"\)`

type BadBrief struct {
	ID    int32
	Total string
}

type UserBadOrders struct {
	ID     int64
	Orders []BadBrief
}

var badNestedType = sqlshape.Query[UserBadOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders.ID: bigint into int32`

type Money struct {
	Currency string
	Amount   string
}

var badOrder = sqlshape.Query[struct{ Price *Money }, struct{}](`SELECT price FROM orders`) // want `field Price.Currency is at position 1 but the row type's column 1 is "amount" \(fields are scanned in order\)` `field Price.Amount is at position 2 but the row type's column 2 is "currency"`

type MoneyOK struct {
	Amount   *string
	Currency *string
}

var compositeOK = sqlshape.Query[struct{ Price *MoneyOK }, struct{}](`SELECT price FROM orders`)
