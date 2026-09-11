package a

import (
	"database/sql"
	"database/sql/driver"
	"net"
	"net/netip"
	"time"

	"b"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
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

// users.id is GENERATED ALWAYS AS IDENTITY: an INSERT that leaves it to the system cannot violate users_pkey
var insertUser = sqlshape.Query[int64, NewUser]("-- sqlshape: expect users_email_key, email_check\nINSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id")

var insertUserOK = sqlshape.Query[int64, NewUser]("-- sqlshape: expect users_email_key, email_check, users_pkey\nINSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id") // want `expects users_pkey but no expansion can violate it`

var nullableEmail = sqlshape.Query[int64, struct{ Email *string }]("-- sqlshape: expect users_email_key, email_check\nINSERT INTO users (email) VALUES ({{.Email}}) RETURNING id") // want "may violate email \\(NOT NULL on users.email, SQLSTATE 23502\\)"

var staleExpect = sqlshape.Query[struct{}, struct{ ID int64 }]("-- sqlshape: expect orders_user_note_key, P0401\nUPDATE orders SET status = 'paid' WHERE id = {{.ID}}") // want "expects orders_user_note_key but no expansion can violate it"

// \x / \u / \U escapes ahead of the bad column: litPos (vet.go) walks and decodes them to
// find the right source column; see TestLitPosEscapes for a precise, position-level check
// (this interpreted string is one physical source line, so the diagnostic still lands here)
var escapedTemplate = sqlshape.Query[int64, struct{}]("SELECT id FROM users -- \x41\u00e9\U0001F600\nWHERE nope = 1") // want `column "nope" does not exist \(SQLSTATE 42703\)`

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

var userOrders = sqlshape.Query[UserOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders: record\[\] may contain a NULL element even though the column is not NULL; \[\]a\.OrderBrief silently receives it as a zero-valued OrderBrief with no error \(use \[\]\*a\.OrderBrief\)`

type ShortBrief struct{ ID int64 }

type UserShortOrders struct {
	ID     int64
	Orders []ShortBrief
}

var badArity = sqlshape.Query[UserShortOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders: a.ShortBrief has 1 fields but the row type has 2 \("f1 bigint, f2 numeric\(12,2\)"\)` `field Orders: record\[\] may contain a NULL element even though the column is not NULL; \[\]a\.ShortBrief silently receives it as a zero-valued ShortBrief with no error \(use \[\]\*a\.ShortBrief\)`

type BadBrief struct {
	ID    int32
	Total string
}

type UserBadOrders struct {
	ID     int64
	Orders []BadBrief
}

var badNestedType = sqlshape.Query[UserBadOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders.ID: bigint into int32` `field Orders: record\[\] may contain a NULL element even though the column is not NULL; \[\]a\.BadBrief silently receives it as a zero-valued BadBrief with no error \(use \[\]\*a\.BadBrief\)`

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
var orderStats = postgres.MatView("order_stats")

var noSuchView = postgres.MatView("order_statz") // want `materialized view "order_statz" does not exist`

var notAMatView = postgres.MatView("order_summary") // want `"order_summary" is not a materialized view`

var qualifiedMatView = postgres.MatView("public.plan_counts")

var dynamicMatViewName = "order_stats"

var nonConstMatView = postgres.MatView(dynamicMatViewName) // want `MatView name must be a string constant`

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

var embeddedNested = sqlshape.Query[UserNotedOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.total, o.note)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders: record\[\] may contain a NULL element even though the column is not NULL; \[\]a\.BriefWithNote silently receives it as a zero-valued BriefWithNote with no error \(use \[\]\*a\.BriefWithNote\)`

var embeddedNestedBad = sqlshape.Query[UserNotedOrders, struct{}](`SELECT u.id, array_agg(row(o.id, o.note, o.total)) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`) // want `field Orders.OrderBase.Total is string but column "f2" may be NULL` `field Orders: record\[\] may contain a NULL element even though the column is not NULL; \[\]a\.BriefWithNote silently receives it as a zero-valued BriefWithNote with no error \(use \[\]\*a\.BriefWithNote\)`

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

var hosts = sqlshape.Query[Host, struct{}](`SELECT h.*, u.flags FROM hosts h JOIN users u ON u.id = h.id`) // want `field Uptime: interval into time\.Duration approximates months as 30 days; PostgreSQL's own calendar arithmetic on the same interval can land on a different day`

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

// bytea: only a byte slice scans / encodes it
type AvatarRow struct{ Avatar []byte }

var avatarOK = sqlshape.Query[AvatarRow, struct{}](`SELECT avatar FROM users`)

type BadAvatarRow struct{ Avatar int32 }

var avatarBad = sqlshape.Query[BadAvatarRow, struct{}](`SELECT avatar FROM users`) // want `field Avatar is int32 but column "avatar" is bytea`

var avatarParamOK = sqlshape.Query[int64, struct{ Avatar []byte }](`SELECT id FROM users WHERE avatar = {{.Avatar}}`)

var avatarParamBad = sqlshape.Query[int64, struct{ Avatar int32 }](`SELECT id FROM users WHERE avatar = {{.Avatar}}`) // want `parameter .Avatar is int32 but SQL expects bytea`

// parameters: strings encode as anything, typed values must fit
var hostParams = sqlshape.Query[struct{ ID int32 }, struct {
	Addr   string
	Net    netip.Prefix
	Uptime time.Duration
	Attrs  map[string]string
	Span   pgtype.Range[int64]
	Spans  string
	Rel    int64
}](`SELECT id FROM hosts WHERE addr = {{.Addr}} AND net = {{.Net}} AND uptime > {{.Uptime}} AND attrs @> {{.Attrs}} AND span && {{.Span}} AND spans && {{.Spans}} AND rel = {{.Rel}}`) // want `parameter .Span: int64 into integer may overflow` `parameter .Uptime: interval into time\.Duration approximates months as 30 days; PostgreSQL's own calendar arithmetic on the same interval can land on a different day`

var badHostParams = sqlshape.Query[struct{ ID int32 }, struct {
	Addr int64
	Span pgtype.Range[time.Time]
}](`SELECT id FROM hosts WHERE addr = {{.Addr}} AND span && {{.Span}}`) // want `parameter .Addr is int64 but SQL expects inet` `parameter .Span is github.com/jackc/pgx/v5/pgtype.Range\[time.Time\] but SQL expects int4range`

// composite parameters: the struct's fields line up with the type's columns, by name and order
type MoneyIn struct {
	Amount   string
	Currency string
}

type ItemIn struct {
	OrderID  int64
	LineNo   int16
	Sku      string
	Qty      int32
	Discount string
}

var saveOrder = sqlshape.Query[*int64, struct {
	Price MoneyIn
	Items []ItemIn
}](`SELECT save_order({{.Price}}, {{.Items}})`)

type ItemShort struct {
	OrderID int64
	Sku     string
}

var badSaveOrder = sqlshape.Query[*int64, struct {
	Price *Money
	Items []ItemShort
}](`SELECT save_order({{.Price}}, {{.Items}})`) // want `parameter .Price.Currency is at position 1 but the row type's column 1 is "amount"` `parameter .Price.Amount is at position 2 but the row type's column 2 is "currency"` `parameter .Items: a.ItemShort has 2 fields but the row type has 5`

var badItemType = sqlshape.Query[*int64, struct {
	Price MoneyIn
	Items []struct {
		OrderID  int64
		LineNo   bool
		Sku      string
		Qty      int32
		Discount string
	}
}](`SELECT save_order({{.Price}}, {{.Items}})`) // want `parameter .Items.LineNo is bool but column "line_no" is smallint`

// result columns must be named and distinct to bind to fields
type Sum struct{ N int64 }

var unnamedCol = sqlshape.Query[Sum, struct{}](`SELECT 1 + 1`) // want `result column 1 has no name: give it an alias \(\.\.\. AS name\) so it can bind to a field of a.Sum` `field Sum.N has no result column`

type TwoIDs struct {
	ID     int64
	UserID int64
}

var dupCol = sqlshape.Query[TwoIDs, struct{}](`SELECT o.id, u.id FROM orders o JOIN users u ON u.id = o.user_id`) // want `result columns 1 and 2 are both named "id": alias one of them \(\.\.\. AS other_name\)` `field TwoIDs.UserID has no result column`

// bulk loads: table and columns exist, each column is fed by the field of its name, and
// what is left out must have a default
var loadItems = postgres.Copy[ItemIn]("order_items")

var loadItemsQualified = postgres.Copy[ItemIn]("public.order_items", "order_id", "line_no", "sku", "qty", "discount")

var loadItemsPartial = postgres.Copy[ItemIn]("order_items", "order_id", "line_no", "sku") // want `Copy into order_items: field Qty \(column "qty"\) is not copied` `Copy into order_items: field Discount \(column "discount"\) is not copied` `Copy into order_items: column "discount" is NOT NULL without a default and is not copied`

var loadNoSuchTable = postgres.Copy[ItemIn]("order_itemz") // want `Copy: table "order_itemz" does not exist`

var loadView = postgres.Copy[ItemIn]("order_summary") // want `Copy: "order_summary" is not a table`

var loadNoSuchColumn = postgres.Copy[ItemIn]("order_items", "order_id", "line_no", "skoo", "qty", "discount") // want `Copy into order_items: column "skoo" does not exist` `field Sku \(column "sku"\) is not copied` `column "sku" is NOT NULL without a default and is not copied`

type BadItem struct {
	OrderID  int64
	LineNo   int16
	Sku      *string
	Qty      bool
	Discount string
}

var loadBadItems = postgres.Copy[BadItem]("order_items") // want `Copy: field Sku is \*string but column "sku" is NOT NULL: a nil value fails the load` `Copy: field Qty is bool but column "qty" is integer`

var loadNames = postgres.Copy[string]("users", "name", "alias") // want `Copy\[string\] into users: a scalar R feeds exactly one column, 2 given`

// scalar R that fits exactly one column: no error, and the columns left to their
// defaults that have none (NOT NULL, no default) are still reported
var loadScalarOK = postgres.Copy[bool]("flags_test", "active") // want `Copy into flags_test: column "id" is NOT NULL without a default and is not copied` `Copy into flags_test: column "tiny" is NOT NULL without a default and is not copied`

var loadScalarMissing = postgres.Copy[bool]("flags_test", "bogus") // want `Copy into flags_test: column "bogus" does not exist`

// two fields binding to the same column
type DupCols struct {
	OrderID  int64  `col:"order_id"`
	LineNo   int16  `col:"line_no"`
	SkuA     string `col:"sku"`
	SkuB     string `col:"sku"`
	Qty      int32  `col:"qty"`
	Discount string `col:"discount"`
}

var loadDup = postgres.Copy[DupCols]("order_items") // want `a.DupCols: fields SkuA and SkuB both bind to column "sku"`

// a column named in the call that exists but has no field in R
type MissingFieldItem struct {
	OrderID int64
	LineNo  int16
	Sku     string
}

var loadMissingField = postgres.Copy[MissingFieldItem]("order_items", "order_id", "line_no", "sku", "qty") // want `Copy into order_items: column "qty" has no field in a.MissingFieldItem` `Copy into order_items: column "discount" is NOT NULL without a default and is not copied`

// a generated column named in the call cannot be copied into
type FlagsCopy struct {
	ID     int64
	Active bool
	Tiny   int16
}

var loadGenerated = postgres.Copy[FlagsCopy]("flags_test", "id", "active", "tiny", "computed") // want `Copy into flags_test: column "computed" is generated and cannot be copied into`

// a lossy struct-field conversion (parameter direction): a wider Go integer into a
// narrower PG column may overflow
type LossyCopy struct {
	OrderID  int64
	LineNo   int32
	Sku      string
	Qty      int32
	Discount string
}

var loadLossy = postgres.Copy[LossyCopy]("order_items") // want `Copy: field LineNo: int32 into smallint may overflow`

// a non-constant table name
var dynamicTable = "order_items"

var loadDynamicTable = postgres.Copy[ItemIn](dynamicTable) // want `Copy table and column names must be string constants`

// shared fragments: a const concatenated into the template; diagnostics land in the fragment
const userFrag = " AND o.user_id = {{.UserID}}" // want `parameter .UserID is bool but SQL expects bigint` `parameter .UserID is bool but SQL expects bigint`

const withMissing = " AND o.status = {{.Statuz}}" // want `struct{UserID bool} has no field Statuz`

// constDecl: a const spec that inherits its predecessor's value list (no Values of its
// own) is skipped when building the name -> value-expression map
const (
	inheritedA, inheritedB = "x", "y"
	inheritedC, inheritedD
)

var _, _ = inheritedC, inheritedD

var sharedFragment = sqlshape.Query[OrderRow, struct{ UserID bool }](`SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE true` + userFrag)

var sharedMissing = sqlshape.Query[OrderRow, struct{ UserID bool }]("SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE true" + withMissing + userFrag)

// hazards: actions inside literals / comments are text; a bare ORDER BY parameter is a constant
var quotedAction = sqlshape.Query[OrderRow, struct{ Q string }]("SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.note LIKE '%{{.Q}}%' -- by {{.Q}}\n") // want `{{.Q}} is inside a string literal: it becomes text, not a parameter \(write '%' \|\| {{.Q}} \|\| '%' to concatenate\)` `{{.Q}} is inside a comment and has no effect`

var dollarQuoted = sqlshape.Query[OrderRow, struct{ Q string }]("SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.note = $q$ {{.Q}} $q$ /* {{.Q}} */") // want `{{.Q}} is inside a string literal` `{{.Q}} is inside a comment and has no effect`

var bareSort = sqlshape.Query[OrderRow, struct{ Sort string }]("SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o ORDER BY {{.Sort}}") // want `ORDER BY {{.Sort}} sorts by a constant, not by the column the value names: branch on it instead`

// hazards: the string-literal scanner's own escape handling (E'...' backslash escapes, a
// doubled ” inside a plain string, a nested /* */ comment) must not lose track of state
var equoteEscaped = sqlshape.Query[OrderRow, struct{ Q string }](`SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.note = E'\\n{{.Q}}'`) // want `{{.Q}} is inside a string literal`

var doubledQuote = sqlshape.Query[OrderRow, struct{ Q string }](`SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.note = 'it''s {{.Q}}'`) // want `{{.Q}} is inside a string literal`

var nestedComment = sqlshape.Query[OrderRow, struct{ Q string }](`SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o /* outer /* inner {{.Q}} */ still a comment */ WHERE o.id = 1`) // want `{{.Q}} is inside a comment and has no effect`

// declared bindings: a Go type names the PG type it carries and does its own encoding

// sqlshape: type money_amount
type Price struct { // want Price:`carries money_amount`
	Currency string // field order differs from the composite: irrelevant, Scan / Value own the wire format
	Amount   string
}

func (p *Price) Scan(src any) error          { return nil }
func (p Price) Value() (driver.Value, error) { return nil, nil }

var declaredResult = sqlshape.Query[struct{ Price Price }, struct{}](`SELECT price FROM orders`)

var declaredArray = sqlshape.Query[struct{ Prices []Price }, struct{}](`SELECT array_agg(price) AS prices FROM orders`)

var declaredParam = sqlshape.Query[struct{}, struct {
	ID    int64
	Price Price
}](`-- sqlshape: expect P0401
UPDATE orders SET price = {{.Price}} WHERE id = {{.ID}}`)

var declaredMisuse = sqlshape.Query[struct{ Total Price }, struct{}](`SELECT total FROM orders`) // want `field Total is a.Price but column "total" is numeric\(12,2\)`

var declaredNested = sqlshape.Query[struct {
	ID    int64
	Price Price
}, struct{}](`SELECT id, price FROM orders`)

// a declared type imported from another package: the binding travels as a fact, so it is
// checked here exactly as Price is checked locally
var declaredImported = sqlshape.Query[struct{ Price b.Money }, struct{}](`SELECT price FROM orders`)

// R itself is an imported type (a qualified selector, not a local identifier or struct
// literal): the "no field" diagnostic still fires, but with no rewrite to suggest
var qualifiedR = sqlshape.Query[b.Widget, struct{}](`SELECT id, note FROM orders`) // want `result column "note" has no field in b\.Widget`

// sqlshape: type yen
type YenBox struct{ N int64 } // want YenBox:`carries yen`

var yenNoScanner = sqlshape.Query[struct{ Balance YenBox }, struct{}](`SELECT balance FROM users`) // want `field Balance: a.YenBox carries yen but does not implement sql.Scanner: pgx cannot decode into it`

// sqlshape: type yen
type YenValuer struct{ N int64 } // want YenValuer:`carries yen`

func (y YenValuer) Value() (driver.Value, error) { return y.N, nil }

// a struct that implements driver.Valuer is accepted as a parameter for a domain (non-composite) declared type
// (an UPDATE assignment keeps the domain type; a WHERE comparison would widen it to its base)
var yenParamOK = sqlshape.Query[struct{}, struct {
	ID      int64
	Balance YenValuer
}]("-- sqlshape: expect yen_check\nUPDATE users SET balance = {{.Balance}} WHERE id = {{.ID}}")

// sqlshape: type yen
type YenNoValuer struct{ N int64 } // want YenNoValuer:`carries yen`

// a declared type used as a parameter must implement driver.Valuer, or pgx cannot encode it
var yenParamBad = sqlshape.Query[struct{}, struct {
	ID      int64
	Balance YenNoValuer
}]("-- sqlshape: expect yen_check\nUPDATE users SET balance = {{.Balance}} WHERE id = {{.ID}}") // want `parameter .Balance: a.YenNoValuer carries yen but does not implement driver.Valuer: pgx cannot encode it`

// sqlshape: type nope
type Nope string // want `type Nope: PostgreSQL type "nope" does not exist in the schema` Nope:`carries nope`

// sqlshape: type citext
type Handle string // want Handle:`carries citext`

var handleOK = sqlshape.Query[struct{ Handle Handle }, struct{ H Handle }](`SELECT handle FROM users WHERE handle = {{.H}}`)

var handleMisuse = sqlshape.Query[struct{ Name Handle }, struct{}](`SELECT name FROM users`) // want `field Name is a.Handle but column "name" is character varying\(100\)`

// a lookup table's rows are a value set like enum labels: the key column, and the
// columns referencing it, bind the Go type to the declared rows
type Plan string // want Plan:`consts free,team,trial` Plan:`bound l plans.code`

const (
	PlanFree  Plan = "free"
	PlanTeam  Plan = "team"
	PlanTrial Plan = "trial"
)

var byPlan = sqlshape.Query[int64, struct{ P Plan }](`SELECT id FROM subscriptions WHERE plan = {{.P}}`) // want `value set of plans.code \(lookup table\) has label "pro" but Plan has no constant for it` `Plan has constant "trial" which is not a label of value set of plans.code \(lookup table\)`

var planLabels = sqlshape.Query[struct {
	Code  Plan
	Label string
}, struct{}](`SELECT code, label FROM plans`)

// lookup tables whose key column is not text: the seed values still bind as a value set,
// spelled by constText (a plain integer, a cast integer, and a numeric literal that falls
// back to deparsing)
type Level int16 // want Level:`bound l priorities.level`

var byLevel = sqlshape.Query[int64, struct{ L Level }](`SELECT count(*) FROM priorities WHERE level = {{.L}}`)

type Weight string // want Weight:`bound l weights.factor`

var byWeight = sqlshape.Query[int64, struct{ W Weight }](`SELECT count(*) FROM weights WHERE factor = {{.W}}`)

func planName(p Plan) string {
	switch p { // want `switch on Plan does not handle value set of plans.code \(lookup table\) labels: pro`
	case PlanFree:
		return "f"
	case PlanTeam:
		return "t"
	}
	return ""
}

// matchValue: result-direction branches the rest of the package never exercises (a fixed
// Go array against a PG array, integer/float/numeric narrowing, UUID variants, a plain
// string reading a time column, and mismatches).

var fixedArrayOK = sqlshape.Query[struct{ Tags [4]string }, struct{}](`SELECT tags FROM users`) // want `field Tags: text\[\] may contain a NULL element even though the column is not NULL; \[4\]string cannot receive one \(use \[4\]\*string\)`

var fixedArrayMismatch = sqlshape.Query[struct{ Tags bool }, struct{}](`SELECT tags FROM users`) // want `field Tags is bool but column "tags" is text\[\]`

var int4IntoInt16 = sqlshape.Query[struct{ Qty int16 }, struct{}](`SELECT qty FROM order_items`) // want `field Qty: integer into int16`

var int8IntoInt32 = sqlshape.Query[struct{ ID int32 }, struct{}](`SELECT id FROM orders`) // want `field ID: bigint into int32`

var float4Result = sqlshape.Query[struct{ Score float64 }, struct{}](`SELECT score FROM users`) // want `field Score is float64 but column "score" may be NULL`

var float8IntoFloat32 = sqlshape.Query[struct{ Ratio float32 }, struct{}](`SELECT ratio FROM users`) // want `field Ratio: double precision into float32` `field Ratio is float32 but column "ratio" may be NULL`

var numericIntoInt = sqlshape.Query[struct{ Total int64 }, struct{}](`SELECT total FROM orders`) // want `field Total: numeric into int64 drops the fraction`

var numericMismatch = sqlshape.Query[struct{ Total bool }, struct{}](`SELECT total FROM orders`) // want `field Total is bool but column "total" is numeric\(12,2\)`

var uuidFixedArray = sqlshape.Query[struct{ UID [16]byte }, struct{}](`SELECT uid FROM orders`) // want `field UID is \[16\]byte but column "uid" may be NULL`

var uuidMismatch = sqlshape.Query[struct{ UID bool }, struct{}](`SELECT uid FROM orders`) // want `field UID is bool but column "uid" is uuid`

var timeAsString = sqlshape.Query[struct{ Wake string }, struct{}](`SELECT wake FROM users`) // want `field Wake is string but column "wake" may be NULL`

var jsonMismatch = sqlshape.Query[struct{ Meta bool }, struct{}](`SELECT meta FROM orders`) // want `field Meta is bool but column "meta" is jsonb`

var multirangeArgMismatch = sqlshape.Query[struct{ Spans pgtype.Multirange[int32] }, struct{}](`SELECT spans FROM hosts`) // want `field Spans is github.com/jackc/pgx/v5/pgtype\.Multirange\[int32\] but column "spans" is int4multirange`

// matchValue: the named uuid.UUID type, the hstore branches, and the citext string fallback
var uuidNamedType = sqlshape.Query[struct{ UID uuid.UUID }, struct{}](`SELECT uid FROM orders`) // want `field UID is github.com/google/uuid\.UUID but column "uid" may be NULL`

var hstoreBadKey = sqlshape.Query[struct{ Attrs map[int]string }, struct{}](`SELECT attrs FROM hosts`) // want `field Attrs is map\[int\]string but column "attrs" is hstore`

var hstoreNotMap = sqlshape.Query[struct{ Attrs string }, struct{}](`SELECT attrs FROM hosts`) // want `field Attrs is string but column "attrs" is hstore`

// handle is citext; a plain (undeclared) string field takes the text-codec fallback
var citextPlainString = sqlshape.Query[struct{ Handle string }, struct{}](`SELECT handle FROM users WHERE handle IS NOT NULL`)

// paramFit: a wider Go integer / float into a narrower PG parameter type may overflow
var int4ParamOverflow = sqlshape.Query[int64, struct{ ID int64 }](`SELECT id FROM hosts WHERE id = {{.ID}}`) // want `parameter .ID: int64 into integer may overflow`

var float4ParamOverflow = sqlshape.Query[int64, struct{ Score float64 }](`SELECT id FROM users WHERE score = {{.Score}}`) // want `parameter .Score: float64 into real loses precision`

// structFields / embeddedStruct / columnName / lookupTag: skipped members, tag parsing edges
type ScannedTime struct{ N int64 }

func (s *ScannedTime) Scan(src any) error { return nil }

type WithOddFields struct {
	unexported int64
	Skipped    int64  `col:"-"`
	Blank      int64  `col:",notnull"`
	Escaped    string `col:"escaped\"name"`
	Spaced     int64  `  col:"spaced"`
	DB         int64  `db:"dbname"`
	Timestamp  time.Time
	Scanned    ScannedTime
	Malformed  int64 `col`
}

var oddFields = sqlshape.Query[WithOddFields, struct{}](`SELECT id AS blank, id AS spaced, id AS dbname FROM orders`) // want `field WithOddFields.Escaped has no result column` `field WithOddFields.Timestamp has no result column` `field WithOddFields.Scanned has no result column` `field WithOddFields.Malformed has no result column`

// unwrapNullable: database/sql.Null* is checked loosely (nullable, not type-checked further)
var sqlNullField = sqlshape.Query[struct{ Note sql.NullString }, struct{}](`SELECT note FROM orders`)

// Float8 exact match (no lossy note)
var float8ExactMatch = sqlshape.Query[struct{ Ratio float64 }, struct{}](`SELECT ratio FROM users`) // want `field Ratio is float64 but column "ratio" may be NULL`

// structFields / embeddedStruct: a pointer to an embedded struct still flattens, a scalar
// embed (time.Time, a Scanner) and a non-struct embed do not, and a db-tagged embed is a
// leaf named by the tag
type NestedID struct{ ID int64 }

type MyInt int64

type TaggedBase struct{ X int64 }

type BigResult struct {
	*NestedID
	time.Time
	ScannedTime
	MyInt
	TaggedBase `db:"tagged"`
}

var bigResult = sqlshape.Query[BigResult, struct{}](`SELECT id FROM orders`) // want `field BigResult.Time has no result column` `field BigResult.ScannedTime has no result column` `field BigResult.MyInt has no result column` `field BigResult.TaggedBase has no result column`

// lookupTag: a tag that is only whitespace once trimmed, and one with an unterminated quote
type OddTags struct {
	A int64 `col:"id"`
	B int64 ` `
	C int64 `col:"unterminated`
}

var oddTags = sqlshape.Query[OddTags, struct{}](`SELECT id FROM orders`) // want `field OddTags.B has no result column` `field OddTags.C has no result column`

// owner: a call inside a plain function (no receiver) and inside a method
func plainFuncOwner() {
	_ = sqlshape.Query[int64, struct{}](`SELECT id FROM orders`)
}

type Repo struct{}

func (r *Repo) methodOwner() {
	_ = sqlshape.Query[int64, struct{}](`SELECT id FROM orders`)
}

// expand.Expand template parse error: an unclosed {{if}} action
var unclosedIf = sqlshape.Query[int64, struct{ X string }]("SELECT id FROM orders {{if .X}}") // want `sqlshape: template: `

// resolvePath: {{range $i, $v := .Items}} binds $i to the loop index, a range over a map
// resolves its element type, and selecting a field on a non-struct fails
type IndexParams struct {
	Items []struct{ Sku string }
}

var indexParam = sqlshape.Query[int64, IndexParams](`SELECT order_id FROM order_items WHERE true {{range $i, $v := .Items}} OR (line_no = {{$i}} AND sku = {{$v.Sku}}) {{end}}`) // want `parameter .#index: int into smallint may overflow`

type MapParams struct {
	M map[string]string
}

var mapRangeParam = sqlshape.Query[int64, MapParams](`SELECT id FROM orders WHERE true {{range .M}} AND note <> {{.}} {{end}}`)

type ScalarDotParams struct {
	Scalar int64
}

var scalarDotParam = sqlshape.Query[int64, ScalarDotParams](`SELECT id FROM orders WHERE id = {{.Scalar.Foo}}`) // want `\.Scalar is int64, not a struct; cannot select \.Foo`

// a result column whose case differs from the field it binds to (case-insensitive fallback)
var caseInsensitiveCol = sqlshape.Query[struct{ ID int64 }, struct{}](`SELECT id AS "ID" FROM orders`)

// resolvePath: a pointer struct mid-path is dereferenced, ranging over a fixed array
// resolves its element type, and ranging over a non-rangeable field is an error
type SubStruct struct{ X int64 }

type PtrParams struct{ Sub *SubStruct }

var ptrMidPath = sqlshape.Query[int64, PtrParams](`SELECT id FROM orders WHERE id = {{.Sub.X}}`)

type ArrParams struct{ Arr [3]string }

var arrRangeParam = sqlshape.Query[int64, ArrParams](`SELECT id FROM orders WHERE true {{range .Arr}} AND note <> {{.}} {{end}}`)

type BadRangeParams struct{ N int64 }

var badRangeParam = sqlshape.Query[int64, BadRangeParams](`SELECT id FROM orders WHERE true {{range .N}} AND note <> {{.}} {{end}}`) // want `\.N is int64, cannot range over it`

// checkNested: a plain (non-declared) struct receiving a composite column is matched
// field by field, in the row type's column order
type MoneyFields struct {
	Amount   string
	Currency string
}

var nestedOK = sqlshape.Query[struct{ Price MoneyFields }, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`) // want `field Price.Amount is string but column "amount" may be NULL` `field Price.Currency is string but column "currency" may be NULL`

type MoneyWrongCount struct {
	Amount string
}

var nestedWrongCount = sqlshape.Query[struct{ Price MoneyWrongCount }, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`) // want `field Price: a.MoneyWrongCount has 1 fields but the row type has 2 \("amount numeric\(12,2\), currency character\(3\)"\)`

type MoneyWrongName struct {
	Amount string
	Curenc string
}

var nestedWrongName = sqlshape.Query[struct{ Price MoneyWrongName }, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`) // want `field Price.Curenc is at position 2 but the row type's column 2 is "currency" \(fields are scanned in order\)` `field Price.Amount is string but column "amount" may be NULL`

type MoneyDup struct {
	A        string `col:"amount"`
	B        string `col:"amount"`
	Currency string
}

var nestedDup = sqlshape.Query[struct{ Price MoneyDup }, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`) // want `field Price: a.MoneyDup: fields A and B both bind to column "amount"` `field Price.A is string but column "amount" may be NULL` `field Price.Currency is string but column "currency" may be NULL`

// checkNested: further branches — a nested field tagged notnull, a receiving type that
// itself unwraps to nil (pgtype.*), a fixed-size array of composites, and a slice whose
// element unwraps to nil
type MoneyNotNull struct {
	Amount   string `col:",notnull"`
	Currency string
}

var nestedNotNull = sqlshape.Query[struct{ Price MoneyNotNull }, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`) // want `field Price.Currency is string but column "currency" may be NULL`

type MoneyPgtypeField struct {
	Price pgtype.Numeric
}

var nestedPgtypeDirect = sqlshape.Query[MoneyPgtypeField, struct{}](`SELECT price FROM orders WHERE price IS NOT NULL`)

type PricesArrField struct {
	Prices [1]MoneyFields
}

var nestedFixedArray = sqlshape.Query[PricesArrField, struct{}](`SELECT array_agg(price) AS prices FROM orders`) // want `field Prices is \[1\]a.MoneyFields but column "prices" may be NULL` `field Prices.Amount is string but column "amount" may be NULL` `field Prices.Currency is string but column "currency" may be NULL` `field Prices: money_amount\[\] may contain a NULL element even though the column is not NULL; \[1\]a\.MoneyFields silently receives it as a zero-valued MoneyFields with no error \(use \[1\]\*a\.MoneyFields\)`

type PricesPgtypeField struct {
	Prices []pgtype.Numeric
}

var nestedPgtypeElem = sqlshape.Query[PricesPgtypeField, struct{}](`SELECT array_agg(price) AS prices FROM orders`)
