package a

import (
	"net"
	"net/netip"
	"time"

	"b"

	"github.com/jackc/pgx/v5/pgtype"
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

var mixedUnits = sqlshape.Query[int64, struct{}](`SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.balance > o.total`)                   // want `domain mismatch: yen > numeric\(12,2\): operands must share the domain`
var mixedCollations = sqlshape.Query[int64, struct{}](`SELECT u.id FROM users u, (SELECT id, name COLLATE "POSIX" AS p FROM users) s WHERE u.alias = s.p`) // want `could not determine which collation to use for string comparison: implicit collations "C" and "POSIX" conflict`

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

var staleExpect = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect orders_user_note_key, P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}}") // want "expects orders_user_note_key but no expansion can violate it"

var branchViolation = sqlshape.Query[struct{}, struct {
	ID   int64
	Note *string
}](`UPDATE orders SET status = 'paid' {{if .Note}}, note = {{.Note}} {{end}} WHERE id = {{.ID}}`) // want `may violate P0401 \(raised by trigger orders_size on orders as OrderTooLarge, SQLSTATE P0401\)` "may violate orders_user_note_key \\(UNIQUE \\(user_id, note\\) on orders, SQLSTATE 23505\\); add `-- sqlshape: expect orders_user_note_key` to the template or make it impossible \\[if@\\d+:then\\]"

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

// -- sqlshape: not null in the template overrides the analyzer's nullability for named result columns
type NotedOrder struct {
	ID   int64
	Note string
}

var notNullOverride = sqlshape.Query[NotedOrder, struct{}]("-- sqlshape: not null note\nSELECT id, note FROM orders WHERE note IS NOT NULL")

var notNullTypo = sqlshape.Query[NotedOrder, struct{}]("-- sqlshape: not null note, nope\nSELECT id, note FROM orders") // want `not null: the query has no result column "nope"`

var countByFunc = sqlshape.Query[int64, struct{ U int64 }](`SELECT order_count({{.U}})`)

// materialized view handles are checked against the schema
var orderStats = sqlshape.MatView("order_stats")

var noSuchView = sqlshape.MatView("order_statz") // want `materialized view "order_statz" does not exist`

var notAMatView = sqlshape.MatView("order_summary") // want `"order_summary" is not a materialized view`

// CHECK (role IN (...)) is a value set like an enum: constants are diffed both ways
type Role string // want Role:`consts admin,guest,member` Role:`bound v users.role`

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleGuest  Role = "guest"
)

var byRole = sqlshape.Query[int64, struct{ R Role }](`SELECT id FROM users WHERE role = {{.R}}`) // want `value set of users.role \(CHECK\) has label "owner" but Role has no constant for it` `Role has constant "guest" which is not a label of value set of users.role \(CHECK\)`

// visibility policy: memos rows are visible where deleted_at IS NULL
var liveMemos = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM memos WHERE user_id = {{.U}} AND deleted_at IS NULL`)

var allMemos = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM memos WHERE user_id = {{.U}}`) // want "rows of memos are visible where deleted_at IS NULL: add that predicate for memos, or opt out with `-- sqlshape: unfiltered memos`"

var trashMemos = sqlshape.Query[int64, struct{ U int64 }]("-- sqlshape: unfiltered memos\nSELECT id FROM memos WHERE user_id = {{.U}} AND deleted_at IS NOT NULL")

var viaLiveView = sqlshape.Query[int64, struct{ U int64 }](`SELECT id FROM live_memos WHERE user_id = {{.U}}`)

// optional projection: a field only some branches select must be nullable
type DetailRow struct {
	ID    int64
	Total *string
}

var optionalTotal = sqlshape.Query[DetailRow, struct{ Detailed bool }](`SELECT id {{if .Detailed}}, total {{end}} FROM orders`)

type DetailRowBad struct {
	ID    int64
	Total string
}

var optionalTotalBad = sqlshape.Query[DetailRowBad, struct{ Detailed bool }](`SELECT id {{if .Detailed}}, total {{end}} FROM orders`) // want `field DetailRowBad.Total is not selected in every branch \[if@\d+:else\]: make it a pointer so those branches leave it nil`

var neverSelected = sqlshape.Query[DetailRow, struct{ Detailed bool }](`SELECT id FROM orders`) // want `field DetailRow.Total has no result column`

// composite keys: each column of order_items' primary key is an identity; order_id follows its FK to orders.id
type LineNo int16 // want LineNo:`bound k order_items.line_no`

var itemByLine = sqlshape.Query[string, struct {
	O UserID
	L LineNo
}](`SELECT sku FROM order_items WHERE order_id = {{.O}} AND line_no = {{.L}}`) // want `parameter .O is a.UserID, which stands for key users.id elsewhere, but here meets key orders.id`

// embedded structs flatten into the parent: OrderBase's columns are OrderWithNote's columns
type OrderBase struct {
	ID    int64
	Total string
}

type OrderWithNote struct {
	OrderBase
	Note *string
}

var embeddedRow = sqlshape.Query[OrderWithNote, struct{}](`SELECT id, total, note FROM orders`)

var embeddedMissing = sqlshape.Query[OrderWithNote, struct{}](`SELECT id, note FROM orders`) // want `field OrderWithNote.OrderBase.Total has no result column`

type MistypedRow struct {
	OrderBase
	Note int64
}

var embeddedMistyped = sqlshape.Query[MistypedRow, struct{}](`SELECT id, total, note FROM orders`) // want `field Note is int64 but column "note" is text`

type DupRow struct {
	OrderBase
	ID int64
}

var embeddedDup = sqlshape.Query[DupRow, struct{}](`SELECT id, total FROM orders`) // want `a.DupRow: fields OrderBase.ID and ID both bind to column "id"`

// an embedded struct with a column tag is a nested row, not flattened
type PriceRow struct {
	*Money `col:"price"`
	ID     int64
}

var embeddedTagged = sqlshape.Query[PriceRow, struct{}](`SELECT id, price FROM orders`) // want `field Money.Currency is at position 1 but the row type's column 1 is "amount"` `field Money.Amount is at position 2 but the row type's column 2 is "currency"`

// embedded structs inside a nested row flatten too
type BriefWithNote struct {
	OrderBase
	Note *string
}

type UserNotedOrders struct {
	ID     int64
	Orders []BriefWithNote
}

var embeddedNested = sqlshape.Query[UserNotedOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total, o.note)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`)

var embeddedNestedBad = sqlshape.Query[UserNotedOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.note, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders.OrderBase.Total is string but column "f2" may be NULL`

// promoted fields of P resolve in the template, as text/template does
type ByID struct{ ID int64 }

var embeddedParam = sqlshape.Query[OrderWithNote, struct {
	ByID
	Limit int32
}](`SELECT id, total, note FROM orders WHERE id = {{.ID}} LIMIT {{.Limit}}`)

// the Go type table: what pgx scans each PG type into (results) and encodes (parameters)
type Host struct {
	ID     int32
	Addr   netip.Addr
	Net    netip.Prefix
	Mac    net.HardwareAddr
	Uptime time.Duration
	Attrs  map[string]*string
	Span   pgtype.Range[int32]
	Spans  pgtype.Multirange[pgtype.Range[int32]]
	Seen   pgtype.Range[time.Time]
	Pos    pgtype.Point
	Doc    pgtype.TSVector
	Body   string
	Fee    string
	AtTz   string
	Rel    uint32
	Flags  pgtype.Bits
}

var hosts = sqlshape.Query[Host, struct{}](`SELECT h.*, u.flags FROM hosts h JOIN users u ON u.id = h.id`)

type BadHost struct {
	Addr   string
	Net    netip.Addr
	Uptime string
	Attrs  map[string]string
	Span   pgtype.Range[string]
	Spans  []pgtype.Range[int32]
	Pos    string
	Doc    string
	AtTz   time.Time
	Rel    int64
	Flags  string
	Vflags []byte
}

var badHosts = sqlshape.Query[BadHost, struct{}](`SELECT h.addr, h.net, h.uptime, h.attrs, h.span, h.spans, h.pos, h.doc, h.at_tz, h.rel, u.flags, u.vflags FROM hosts h JOIN users u ON u.id = h.id`) // want `field Addr is string but column "addr" is inet` `field Net is net/netip.Addr but column "net" is cidr` `field Uptime is string but column "uptime" is interval` `field Attrs: hstore into map\[string\]string fails at scan time when a value is NULL; use map\[string\]\*string` `field Span is github.com/jackc/pgx/v5/pgtype.Range\[string\] but column "span" is int4range` `field Spans is \[\]github.com/jackc/pgx/v5/pgtype.Range\[int32\] but column "spans" is int4multirange` `field Pos is string but column "pos" is point` `field Doc is string but column "doc" is tsvector` `field AtTz is time.Time but column "at_tz" is time with time zone` `field Rel is int64 but column "rel" is oid` `field Flags is string but column "flags" is bit\(4\)` `field Vflags is \[\]byte but column "vflags" is bit varying\(8\)`

var lossyRange = sqlshape.Query[struct{ Span pgtype.Range[int32] }, struct{}](`SELECT int8range(1, 5) AS span`) // want `field Span: bigint into int32`

// parameters: strings encode as anything, typed values must fit
var hostParams = sqlshape.Query[struct{ ID int32 }, struct {
	Addr   string
	Net    netip.Prefix
	Uptime time.Duration
	Attrs  map[string]string
	Span   pgtype.Range[int64]
	Spans  string
	Rel    int64
}](`SELECT id FROM hosts WHERE addr = {{.Addr}} AND net = {{.Net}} AND uptime > {{.Uptime}} AND attrs @> {{.Attrs}} AND span && {{.Span}} AND spans && {{.Spans}} AND rel = {{.Rel}}`)

var badHostParams = sqlshape.Query[struct{ ID int32 }, struct {
	Addr int64
	Span pgtype.Range[time.Time]
}](`SELECT id FROM hosts WHERE addr = {{.Addr}} AND span && {{.Span}}`) // want `parameter .Addr is int64 but SQL expects inet` `parameter .Span is github.com/jackc/pgx/v5/pgtype.Range\[time.Time\] but SQL expects int4range`
