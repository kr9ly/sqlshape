package sqlshape_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/internal/oracle"
)

type OrderStatus string

// Known lists the labels this build knows; the row mapper rejects others.
func (s OrderStatus) Known() bool {
	switch s {
	case "pending", "paid", "shipped":
		return true
	}
	return false
}

type OrderRow struct {
	ID        int64
	Status    OrderStatus
	Total     string
	Note      *string
	CreatedAt time.Time
}

type ListParams struct {
	Status   *OrderStatus
	Statuses []OrderStatus
	IDs      []int64
	Sort     string
	Limit    int32
}

var listOrders = sqlshape.Query[OrderRow, ListParams](`
SELECT o.id, o.status, o.total, o.note, o.created_at
  FROM orders o
 WHERE true
   {{if .Status}}   AND o.status = {{.Status}}          {{end}}
   {{if .Statuses}} AND o.status = ANY({{.Statuses}})   {{end}}
   {{range .IDs}}   AND o.id <> {{.}}                   {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total DESC {{else}} o.id {{end}}
 {{with .Limit}} LIMIT {{.}} {{end}}
`)

type NewOrder struct {
	UserID int64
	Total  string
	Note   *string
}

var insertOrder = sqlshape.Query[int64, NewOrder](`
-- sqlshape: expect orders_pkey, orders_user_note_key, orders_user_id_fkey, orders_total_check, P0401
INSERT INTO orders (user_id, total, note) VALUES ({{.UserID}}, {{.Total}}, {{.Note}}) RETURNING id`)

var markPaid = sqlshape.Query[struct{}, struct{ ID int64 }](`UPDATE orders SET status = 'paid' WHERE id = {{.ID}}`)

var orderByID = sqlshape.One[OrderRow, struct{ ID int64 }](`
SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.id = {{.ID}}`)

var markPaidProc = sqlshape.Query[struct{}, struct{ ID int64 }](`CALL mark_paid({{.ID}})`)

var settleProc = sqlshape.One[struct{ PTotal string }, struct{ ID int64 }](`CALL settle({{.ID}}, NULL)`)

var countByStatus = sqlshape.Query[struct {
	Status OrderStatus
	N      int64
}, struct{}](`SELECT status, count(*) AS n FROM orders GROUP BY status ORDER BY status`)

// nested rows
type OrderBrief struct {
	ID    int64
	Total string
}

type UserOrders struct {
	ID     int64
	Orders []OrderBrief
}

var userOrders = sqlshape.Query[UserOrders, struct{}](`
SELECT u.id, array_agg(row(o.id, o.total) ORDER BY o.id) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id ORDER BY u.id`)

type FullOrder struct {
	ID        int64
	UserID    int64
	Status    OrderStatus
	Total     string
	Price     *Money
	Note      *string
	Meta      *string
	UID       *string
	Matrix    [][]int32
	CreatedAt time.Time
}

type Money struct {
	Amount   *string
	Currency *string
}

type UserFullOrders struct {
	ID     int64
	Orders []FullOrder
}

// array_agg(o) is orders[]: a user composite type the connection must load first
var userFullOrders = sqlshape.Query[UserFullOrders, struct{}](`
SELECT u.id, array_agg(o ORDER BY o.id) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id ORDER BY u.id`)

type StatusPair struct {
	ID     int64
	Status OrderStatus
}

// an enum inside an anonymous record: only decodable once the enum type is registered
var statusPairs = sqlshape.Query[[]StatusPair, struct{}](`SELECT array_agg(row(o.id, o.status) ORDER BY o.id) FROM orders o`)

// extension types (citext, hstore) decode as text before and after LoadUserTypes
var extTypes = sqlshape.Query[struct {
	Handle *string
	Attrs  map[string]*string
	Tags   []string
}, struct{ H string }](`SELECT handle, attrs, array_agg(handle) OVER () AS tags FROM users WHERE handle = {{.H}} OR handle IS NULL LIMIT 1`)

// embedded structs flatten: OrderBase's columns are scanned into the promoted fields
type OrderBase struct {
	ID    int64
	Total string
}

type OrderWithNote struct {
	OrderBase
	Note *string
}

type UserNotedOrders struct {
	ByUser
	Orders []OrderWithNote
}

type ByUser struct{ ID int64 }

var embeddedRows = sqlshape.Query[UserNotedOrders, ByUser](`
SELECT u.id, array_agg(row(o.id, o.total, o.note) ORDER BY o.id) AS orders FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = {{.ID}} GROUP BY u.id`)

// the Go type table, end to end: network, interval, hstore, ranges, bit, geometry, text-only types
type Host struct {
	ID     int32
	Addr   netip.Addr
	Net    *netip.Prefix
	Mac    net.HardwareAddr
	Uptime time.Duration
	Attrs  map[string]*string
	Span   pgtype.Range[int32]
	Spans  pgtype.Multirange[pgtype.Range[int32]]
	Seen   pgtype.Range[time.Time]
	Fr     pgtype.Range[float64]
	Pos    pgtype.Point
	Flags  pgtype.Bits
	Doc    pgtype.TSVector
	Fee    *string
	AtTz   *string
}

var insertHost = sqlshape.Query[struct{}, Host](`
-- sqlshape: expect hosts_pkey
INSERT INTO hosts (id, addr, net, mac, uptime, attrs, span, spans, seen, fr, pos, flags, doc, fee, at_tz)
VALUES ({{.ID}}, {{.Addr}}, {{.Net}}, {{.Mac}}, {{.Uptime}}, {{.Attrs}}, {{.Span}}, {{.Spans}}, {{.Seen}}, {{.Fr}}, {{.Pos}}, {{.Flags}}, {{.Doc}}, {{.Fee}}, {{.AtTz}})`)

var hostByAddr = sqlshape.One[Host, struct {
	Addr string
	Span pgtype.Range[int32]
}](`SELECT * FROM hosts WHERE addr = {{.Addr}} AND span && {{.Span}}`)

// composite parameters: a struct encodes as the composite, a slice of structs as its array;
// the connection loads the types lazily on the first use
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

var setPrice = sqlshape.Query[struct{}, struct {
	ID    int64
	Price *MoneyIn
}](`UPDATE orders SET price = {{.Price}} WHERE id = {{.ID}}`)

var itemSkus = sqlshape.Query[string, struct{ Items []ItemIn }](`SELECT sku FROM unnest({{.Items}}::order_items[]) ORDER BY line_no`)

var loadItems = sqlshape.Copy[ItemIn]("order_items", "order_id", "line_no", "sku", "qty", "discount")

var itemCount = sqlshape.Query[int64, struct{ OrderID int64 }](`SELECT count(*) FROM order_items WHERE order_id = {{.OrderID}}`)

var markPaidOne = sqlshape.One[struct{}, struct{ ID int64 }](`UPDATE orders SET status = 'paid' WHERE id = {{.ID}}`)

// a NOT NULL violation (ConstraintError.Column, as opposed to a named constraint)
var insertItemNoDiscount = sqlshape.Query[struct{}, struct{ OrderID int64 }](`
INSERT INTO order_items (order_id, line_no, sku, qty) VALUES ({{.OrderID}}, 199, 'ND', 1)`)

// a result struct with an optional field the query does not select (optionalKind: true)
type PartialOrder struct {
	ID   int64
	Note *string
}

var partialOrder = sqlshape.One[PartialOrder, struct{ ID int64 }](`SELECT id FROM orders WHERE id = {{.ID}}`)

// a result struct with a required field the query does not select (optionalKind: false, an error)
type BadPartialOrder struct {
	ID    int64
	Extra string
}

var badPartialOrder = sqlshape.Query[BadPartialOrder, struct{ ID int64 }](`SELECT id FROM orders WHERE id = {{.ID}}`)

// array_agg(o) with a LEFT JOIN: a user with no orders makes one array element a NULL
// composite row (rowDest.ScanNull), not a row of NULL fields.
var userAnyOrders = sqlshape.Query[UserFullOrders, struct{}](`
SELECT u.id, array_agg(o ORDER BY o.id) AS orders FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.id ORDER BY u.id`)

// a declared binding: the type owns the wire format through Scan / Value
//
// sqlshape: type money_amount
type PriceTag struct{ Text string }

func (p *PriceTag) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		p.Text = ""
	case string:
		p.Text = v
	case []byte:
		p.Text = string(v)
	default:
		return fmt.Errorf("PriceTag: cannot scan %T", src)
	}
	return nil
}

func (p PriceTag) Value() (driver.Value, error) { return p.Text, nil }

var priceTagOf = sqlshape.One[struct{ Price PriceTag }, struct{ ID int64 }](`SELECT price FROM orders WHERE id = {{.ID}}`)

var setPriceTag = sqlshape.Query[struct{}, struct {
	ID    int64
	Price PriceTag
}](`UPDATE orders SET price = {{.Price}} WHERE id = {{.ID}}`)

// ScalarTag is a non-struct sql.Scanner: newMapper's scalar (R itself, not a field)
// userScanner branch only fires for a scalar kind, since a plain struct R is never
// marked scalar (only R == time.Time is, as a special case).
type ScalarTag string

func (s *ScalarTag) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = ""
	case string:
		*s = ScalarTag(v)
	case []byte:
		*s = ScalarTag(v)
	default:
		return fmt.Errorf("ScalarTag: cannot scan %T", src)
	}
	return nil
}

var priceOf = sqlshape.One[struct{ Price *Money }, struct{ ID int64 }](`SELECT price FROM orders WHERE id = {{.ID}}`)

func TestRender(t *testing.T) {
	st := OrderStatus("paid")
	r, err := listOrders.Render(ListParams{Status: &st, IDs: []int64{7, 8}, Sort: "total", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := "\nSELECT o.id, o.status, o.total, o.note, o.created_at\n  FROM orders o\n WHERE true\n      AND o.status = $1          \n   \n      AND o.id <> $2                      AND o.id <> $3                   \n ORDER BY  o.total DESC \n  LIMIT $4 \n"
	if r.SQL != want {
		t.Errorf("sql:\n%q", r.SQL)
	}
	if len(r.Args) != 4 || r.Args[0] != "paid" || r.Args[1] != int64(7) || r.Args[3] != int32(10) {
		t.Errorf("args: %#v", r.Args)
	}
	// nil pointer → nil arg, named slice → []string
	r, _ = insertOrder.Render(NewOrder{UserID: 1, Total: "1.00"})
	if r.Args[2] != nil {
		t.Errorf("nil pointer arg = %#v", r.Args[2])
	}
	r, _ = listOrders.Render(ListParams{Statuses: []OrderStatus{"paid", "shipped"}})
	if ss, ok := r.Args[0].([]string); !ok || len(ss) != 2 {
		t.Errorf("enum slice arg = %#v", r.Args[0])
	}
}

func TestAgainstPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("internal/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()
	if _, err := db.Exec(ctx, `INSERT INTO users (email, name) VALUES ('a@x', 'A'), ('b@x', NULL)`); err != nil {
		t.Fatal(err)
	}
	// a third user with no orders: a LEFT JOIN array_agg(o) for it holds one NULL row
	if _, err := db.Exec(ctx, `INSERT INTO users (email, name) VALUES ('c@x', 'C')`); err != nil {
		t.Fatal(err)
	}

	note := "first"
	id1, err := insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "10.50", Note: &note})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id2, err := insertOrder.First(ctx, db, NewOrder{UserID: 2, Total: "99.00"})
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if id1 == 0 || id2 != id1+1 {
		t.Errorf("ids: %d %d", id1, id2)
	}
	if tag, err := markPaid.Exec(ctx, db, struct{ ID int64 }{id2}); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("exec: %v %v", tag, err)
	}

	all, err := listOrders.Collect(ctx, db, ListParams{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(all) != 2 || all[0].ID != id1 || *all[0].Note != "first" || all[1].Note != nil || all[1].Status != "paid" || all[1].Total != "99.00" {
		t.Errorf("rows: %+v", all)
	}
	if all[0].CreatedAt.IsZero() {
		t.Error("created_at not scanned")
	}

	paid := OrderStatus("paid")
	rows, err := listOrders.Collect(ctx, db, ListParams{Status: &paid, Sort: "total"})
	if err != nil || len(rows) != 1 || rows[0].ID != id2 {
		t.Errorf("enum scalar param: %v %+v", err, rows)
	}
	rows, err = listOrders.Collect(ctx, db, ListParams{Statuses: []OrderStatus{"paid", "shipped"}})
	if err != nil || len(rows) != 1 {
		t.Errorf("enum array param: %v %+v", err, rows)
	}
	rows, err = listOrders.Collect(ctx, db, ListParams{IDs: []int64{id1}, Limit: 5})
	if err != nil || len(rows) != 1 || rows[0].ID != id2 {
		t.Errorf("range + with: %v %+v", err, rows)
	}

	_, err = insertOrder.First(ctx, db, NewOrder{UserID: 999, Total: "1"})
	if !sqlshape.Violates(err, "orders_user_id_fkey") {
		t.Errorf("fk violation not mapped: %v", err)
	}
	_, err = insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "1", Note: &note})
	var ce *sqlshape.ConstraintError
	if !errors.As(err, &ce) || ce.Code != "23505" || ce.Constraint != "orders_user_note_key" || ce.Table != "orders" {
		t.Errorf("unique violation not mapped: %v", err)
	}
	if msg := ce.Error(); !strings.Contains(msg, "orders_user_note_key") {
		t.Errorf("ConstraintError.Error() (named constraint): %s", msg)
	}
	if ce.Unwrap() != ce.Err {
		t.Errorf("ConstraintError.Unwrap() = %v, want %v", ce.Unwrap(), ce.Err)
	}
	// a NOT NULL violation: ConstraintError.Column set, Constraint empty (the other Error() form)
	_, err = insertItemNoDiscount.Exec(ctx, db, struct{ OrderID int64 }{id1})
	var ceNN *sqlshape.ConstraintError
	if !errors.As(err, &ceNN) || ceNN.Column != "discount" || ceNN.Table != "order_items" || ceNN.Constraint != "" {
		t.Errorf("not null violation not mapped: %v", err)
	}
	if msg := ceNN.Error(); !strings.Contains(msg, "order_items") || !strings.Contains(msg, "discount") {
		t.Errorf("ConstraintError.Error() (NOT NULL): %s", msg)
	}
	if ceNN.Unwrap() != ceNN.Err {
		t.Errorf("ConstraintError.Unwrap() = %v, want %v", ceNN.Unwrap(), ceNN.Err)
	}
	_, err = insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "-1"})
	if !sqlshape.Violates(err, "orders_total_check") {
		t.Errorf("check violation not mapped: %v", err)
	}
	_, err = insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "2000000"})
	if !sqlshape.Violates(err, "P0401") {
		t.Errorf("trigger SQLSTATE not mapped: %v", err)
	}

	one, err := orderByID.Get(ctx, db, struct{ ID int64 }{id2})
	if err != nil || one.ID != id2 || one.Status != "paid" {
		t.Errorf("One.Get: %v %+v", err, one)
	}
	if _, err := orderByID.Get(ctx, db, struct{ ID int64 }{id2 + 100}); !sqlshape.IsNoRows(err) {
		t.Errorf("One.Get missing: %v", err)
	}
	if _, ok, err := orderByID.Find(ctx, db, struct{ ID int64 }{id2 + 100}); err != nil || ok {
		t.Errorf("One.Find missing: %v %v", ok, err)
	}
	if r, err := orderByID.Render(struct{ ID int64 }{id2}); err != nil || !strings.Contains(r.SQL, "$1") {
		t.Errorf("Single.Render: %v %+v", err, r)
	}
	if row, err := orderByID.Unprepared().Get(ctx, db, struct{ ID int64 }{id2}); err != nil || row.ID != id2 {
		t.Errorf("Single.Unprepared: %v %+v", err, row)
	}
	if listOrders.SQLTemplate() == "" || orderByID.SQLTemplate() == "" {
		t.Error("SQLTemplate: empty")
	}
	// optionalKind: a field with no matching column is fine when it is optional (a pointer)...
	if po, err := partialOrder.Get(ctx, db, struct{ ID int64 }{id2}); err != nil || po.Note != nil {
		t.Errorf("partial result (optional field unset): %v %+v", err, po)
	}
	// ...and an error when it is not.
	if _, err := badPartialOrder.Collect(ctx, db, struct{ ID int64 }{id2}); err == nil || !strings.Contains(err.Error(), "no result column") {
		t.Errorf("partial result (required field unset): %v", err)
	}

	// composite parameters, before any type is loaded (lazy registration on encode failure)
	items := []ItemIn{{OrderID: id1, LineNo: 2, Sku: "B", Qty: 1, Discount: "0.00"}, {OrderID: id1, LineNo: 1, Sku: "A", Qty: 2, Discount: "1.50"}}
	if n, err := saveOrder.First(ctx, db, struct {
		Price MoneyIn
		Items []ItemIn
	}{MoneyIn{"1.00", "JPY"}, items}); err != nil || n == nil || *n != 1 {
		t.Fatalf("composite params: %v %d", err, n)
	}
	if skus, err := itemSkus.Collect(ctx, db, struct{ Items []ItemIn }{items}); err != nil || len(skus) != 2 || skus[0] != "A" || skus[1] != "B" {
		t.Errorf("composite[] param round trip: %v %v", err, skus)
	}
	if _, err := setPrice.Exec(ctx, db, struct {
		ID    int64
		Price *MoneyIn
	}{id2, &MoneyIn{"7.25", "USD"}}); err != nil {
		t.Fatalf("composite param in UPDATE: %v", err)
	}
	if pr, err := priceOf.Get(ctx, db, struct{ ID int64 }{id2}); err != nil || pr.Price == nil || *pr.Price.Amount != "7.25" || *pr.Price.Currency != "USD" {
		t.Errorf("composite param read back: %v %+v", err, pr.Price)
	}
	if _, err := setPrice.Exec(ctx, db, struct {
		ID    int64
		Price *MoneyIn
	}{id2, nil}); err != nil {
		t.Fatalf("NULL composite param: %v", err)
	}

	// COPY FROM
	if n, err := loadItems.From(ctx, db, items); err != nil || n != 2 {
		t.Fatalf("copy: %v %d", err, n)
	}
	if n, err := itemCount.First(ctx, db, struct{ OrderID int64 }{id1}); err != nil || n != 2 {
		t.Errorf("copied rows: %v %d", err, n)
	}

	// One.Exec: exactly one row, or ErrNoRows
	if _, err := markPaidOne.Exec(ctx, db, struct{ ID int64 }{id1}); err != nil {
		t.Errorf("One.Exec: %v", err)
	}
	if _, err := markPaidOne.Exec(ctx, db, struct{ ID int64 }{id1 + 100}); !sqlshape.IsNoRows(err) {
		t.Errorf("One.Exec on no row: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'pending' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}

	// batch: one round trip, results per statement
	b := sqlshape.NewBatch()
	qAll := sqlshape.Queue(b, listOrders, ListParams{})
	qPaid := sqlshape.QueueOne(b, markPaidOne, struct{ ID int64 }{id1})
	qOne := sqlshape.QueueOne(b, orderByID, struct{ ID int64 }{id1})
	qNone := sqlshape.QueueOne(b, orderByID, struct{ ID int64 }{id1 + 100})
	if _, err := qAll.Rows(); !errors.Is(err, sqlshape.ErrNotSent) {
		t.Errorf("before send: %v", err)
	}
	if err := b.Send(ctx, db); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if rows, err := qAll.Rows(); err != nil || len(rows) != 2 {
		t.Errorf("batch rows: %v %d", err, len(rows))
	}
	if tag, err := qPaid.Tag(); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("batch exec: %v %v", err, tag)
	}
	if row, err := qOne.First(); err != nil || row.Status != "paid" {
		t.Errorf("batch one: %v %+v", err, row)
	}
	if _, err := qNone.First(); !sqlshape.IsNoRows(err) {
		t.Errorf("batch one missing: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'pending' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	// Batch.Len: the pgx-batch statements plus the ones that must run as a plain query
	// after it (a sql.Scanner result needs text format, which a pgx batch never asks for)
	bLen := sqlshape.NewBatch()
	if bLen.Len() != 0 {
		t.Errorf("empty batch len: %d", bLen.Len())
	}
	sqlshape.Queue(bLen, listOrders, ListParams{})
	if bLen.Len() != 1 {
		t.Errorf("batch len after Queue: %d", bLen.Len())
	}
	sqlshape.QueueOne(bLen, priceTagOf, struct{ ID int64 }{id1})
	if bLen.Len() != 2 {
		t.Errorf("batch len after scanner Queue: %d", bLen.Len())
	}

	// a failing statement surfaces as the same ConstraintError as Run
	b = sqlshape.NewBatch()
	sqlshape.Queue(b, insertOrder, NewOrder{UserID: 999, Total: "1"})
	if err := b.Send(ctx, db); !sqlshape.Violates(err, "orders_user_id_fkey") {
		t.Errorf("batch fk violation: %v", err)
	}

	// unknown enum label: the database knows 'cancelled', this build's Known() does not
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'cancelled' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	var ule *sqlshape.UnknownLabelError
	if _, err := listOrders.Collect(ctx, db, ListParams{}); !errors.As(err, &ule) || ule.Value != "cancelled" {
		t.Errorf("unknown label: %v", err)
	}
	if msg := ule.Error(); !strings.Contains(msg, "cancelled") || !strings.Contains(msg, "OrderStatus") {
		t.Errorf("UnknownLabelError.Error(): %s", msg)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'pending' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}

	// unprepared execution (custom plan every time)
	if rows, err := listOrders.Unprepared().Collect(ctx, db, ListParams{}); err != nil || len(rows) != 2 {
		t.Errorf("unprepared: %v %d", err, len(rows))
	}

	// materialized view refresh
	if err := sqlshape.MatView("order_stats").Refresh(ctx, db); err != nil {
		t.Errorf("refresh: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX order_stats_user_idx ON order_stats (user_id)`); err != nil {
		t.Fatal(err)
	}
	if err := sqlshape.MatView("order_stats").RefreshConcurrently(ctx, db); err != nil {
		t.Errorf("refresh concurrently: %v", err)
	}

	// procedures
	if _, err := markPaidProc.Exec(ctx, db, struct{ ID int64 }{id1}); err != nil {
		t.Errorf("CALL: %v", err)
	}
	if st, err := settleProc.Get(ctx, db, struct{ ID int64 }{id1}); err != nil || st.PTotal != "10.50" {
		t.Errorf("CALL with INOUT: %v %+v", err, st)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'pending' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}

	// nested rows
	uo, err := userOrders.Collect(ctx, db, struct{}{})
	if err != nil || len(uo) != 2 || len(uo[0].Orders) != 1 || uo[0].Orders[0].ID != id1 || uo[0].Orders[0].Total != "10.50" {
		t.Fatalf("array_agg(row): %v %+v", err, uo)
	}
	ufo, err := userFullOrders.Collect(ctx, db, struct{}{})
	if err != nil || len(ufo) != 2 || ufo[1].Orders[0].ID != id2 || ufo[1].Orders[0].Status != "paid" || ufo[1].Orders[0].Price != nil || ufo[1].Orders[0].CreatedAt.IsZero() {
		t.Fatalf("array_agg(o): %v %+v", err, ufo)
	}
	// LEFT JOIN: the user with no orders gets one NULL composite element (rowDest.ScanNull),
	// not a row of NULL fields
	uao, err := userAnyOrders.Collect(ctx, db, struct{}{})
	if err != nil || len(uao) != 3 {
		t.Fatalf("array_agg(o) with LEFT JOIN: %v %+v", err, uao)
	}
	var sawNullRow bool
	for _, u := range uao {
		if len(u.Orders) != 1 {
			continue
		}
		o := u.Orders[0]
		if o.ID == 0 && o.UserID == 0 && o.Status == "" && o.Total == "" && o.Price == nil &&
			o.Note == nil && o.Meta == nil && o.UID == nil && o.Matrix == nil && o.CreatedAt.IsZero() {
			sawNullRow = true
		}
	}
	if !sawNullRow {
		t.Errorf("expected a NULL nested row: %+v", uao)
	}
	eo, err := embeddedRows.First(ctx, db, ByUser{1})
	if err != nil || eo.ID != 1 || len(eo.Orders) != 1 || eo.Orders[0].ID != id1 || eo.Orders[0].Total != "10.50" || eo.Orders[0].Note == nil || *eo.Orders[0].Note != "first" {
		t.Errorf("embedded structs: %v %+v", err, eo)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET price = ROW(1.5, 'JPY') WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	pr, err := priceOf.Get(ctx, db, struct{ ID int64 }{id1})
	if err != nil || pr.Price == nil || *pr.Price.Amount != "1.50" || *pr.Price.Currency != "JPY" {
		t.Errorf("composite column: %v %+v", err, pr.Price)
	}
	pr, err = priceOf.Get(ctx, db, struct{ ID int64 }{id2})
	if err != nil || pr.Price != nil {
		t.Errorf("NULL composite column: %v %+v", err, pr.Price)
	}
	// declared binding round trip: Value encodes the composite's text form, Scan receives it
	if _, err := setPriceTag.Exec(ctx, db, struct {
		ID    int64
		Price PriceTag
	}{id2, PriceTag{"(7.25,USD)"}}); err != nil {
		t.Fatalf("declared type param: %v", err)
	}
	if pt, err := priceTagOf.Get(ctx, db, struct{ ID int64 }{id2}); err != nil || pt.Price.Text != "(7.25,USD)" {
		t.Errorf("declared type result: %v %+v", err, pt.Price)
	}
	if pt, err := priceTagOf.Get(ctx, db, struct{ ID int64 }{id1 + 1000}); !sqlshape.IsNoRows(err) {
		t.Errorf("declared type missing row: %v %+v", err, pt)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET handle = 'Alice', attrs = 'k=>v'`); err != nil {
		t.Fatal(err)
	}
	et, err := extTypes.First(ctx, db, struct{ H string }{"alice"})
	if err != nil || et.Handle == nil || *et.Handle != "Alice" || et.Attrs == nil || et.Attrs["k"] == nil || *et.Attrs["k"] != "v" || len(et.Tags) == 0 {
		t.Errorf("extension types before LoadUserTypes: %v %+v", err, et)
	}
	if err := sqlshape.LoadUserTypes(ctx, db); err != nil {
		t.Fatalf("LoadUserTypes: %v", err)
	}
	et, err = extTypes.First(ctx, db, struct{ H string }{"ALICE"})
	if err != nil || et.Handle == nil || *et.Handle != "Alice" {
		t.Errorf("extension types after LoadUserTypes: %v %+v", err, et)
	}
	sp, err := statusPairs.First(ctx, db, struct{}{})
	if err != nil || len(sp) != 2 || sp[1].Status != "paid" {
		t.Errorf("enum in record: %v %+v", err, sp)
	}

	// the Go type table round trip (floatrange as a parameter needs the type loaded: LoadUserTypes above)
	oneStr := "1"
	net24 := netip.MustParsePrefix("10.0.0.0/24")
	fee, atTz := "$12.34", "12:00:00+00"
	h := Host{
		ID: 7, Addr: netip.MustParseAddr("10.0.0.9"), Net: &net24, Mac: net.HardwareAddr{8, 0, 0x2b, 1, 2, 3},
		Uptime: 90 * time.Minute, Attrs: map[string]*string{"a": &oneStr, "b": nil},
		Span:  pgtype.Range[int32]{Lower: 1, Upper: 5, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		Spans: pgtype.Multirange[pgtype.Range[int32]]{{Lower: 1, Upper: 3, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}},
		Seen:  pgtype.Range[time.Time]{Lower: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Upper: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		Fr:    pgtype.Range[float64]{Lower: 0.5, Upper: 1.5, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		Pos:   pgtype.Point{P: pgtype.Vec2{X: 1, Y: 2}, Valid: true},
		Flags: pgtype.Bits{Bytes: []byte{0xa0}, Len: 4, Valid: true},
		Doc:   pgtype.TSVector{Lexemes: []pgtype.TSVectorLexeme{{Word: "cat"}}, Valid: true},
		Fee:   &fee, AtTz: &atTz,
	}
	if _, err := insertHost.Exec(ctx, db, h); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	got, err := hostByAddr.Get(ctx, db, struct {
		Addr string
		Span pgtype.Range[int32]
	}{"10.0.0.9", pgtype.Range[int32]{Lower: 4, Upper: 6, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}})
	if err != nil {
		t.Fatalf("host round trip: %v", err)
	}
	if got.Addr != h.Addr || got.Net == nil || *got.Net != net24 || got.Mac.String() != h.Mac.String() || got.Uptime != h.Uptime ||
		got.Attrs["a"] == nil || *got.Attrs["a"] != "1" || got.Attrs["b"] != nil || got.Span.Upper != 5 || len(got.Spans) != 1 || got.Spans[0].Upper != 3 ||
		!got.Seen.Upper.Equal(h.Seen.Upper) || got.Fr.Upper != 1.5 || got.Pos.P.X != 1 || got.Flags.Len != 4 || got.Flags.Bytes[0] != 0xa0 ||
		len(got.Doc.Lexemes) != 1 || got.Fee == nil || *got.Fee != "$12.34" || got.AtTz == nil || *got.AtTz != "12:00:00+00" {
		t.Errorf("host round trip: %+v", got)
	}

	first, err := listOrders.First(ctx, db, ListParams{IDs: []int64{id1, id2}})
	if !sqlshape.IsNoRows(err) {
		t.Errorf("First on empty: %v %+v", err, first)
	}

	n := 0
	for row, err := range listOrders.Run(ctx, db, ListParams{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		_ = row
		break // early exit must release the connection
	}
	counts, err := countByStatus.Collect(ctx, db, struct{}{})
	// enum ORDER BY follows declaration order: pending before paid
	if err != nil || len(counts) != 2 || counts[0].Status != "pending" || counts[1].Status != "paid" {
		t.Errorf("after early break: %v %+v", err, counts)
	}
}

// TestConstraintErrorDirect exercises ConstraintError's methods directly (Error()'s
// unnamed / generic branch, and Key()'s table.column form), with no database needed:
// every field is exported, so a violation can be built by hand.
func TestConstraintErrorDirect(t *testing.T) {
	generic := &sqlshape.ConstraintError{Err: &pgconn.PgError{Message: "boom"}}
	if got := generic.Error(); got != "sqlshape: boom" {
		t.Errorf("Error() (no constraint, no column) = %q", got)
	}
	if got := generic.Key(); got != "." {
		t.Errorf("Key() (no constraint, no column) = %q", got)
	}

	notNull := &sqlshape.ConstraintError{Table: "order_items", Column: "discount", Err: &pgconn.PgError{Message: "not null"}}
	if got := notNull.Key(); got != "order_items.discount" {
		t.Errorf("Key() (table.column) = %q", got)
	}
	if !sqlshape.Violates(notNull, "order_items.discount") {
		t.Error("Violates should match the table.column key")
	}
}

// TestCopyLayoutErrors covers Copier.layout's error paths, none of which touch the
// database (they are all detected before FromSeq calls db.CopyFrom), so From is called
// with a nil CopyDB.
func TestCopyLayoutErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := sqlshape.Copy[any]("t").From(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "must not be an interface") {
		t.Errorf("interface R: %v", err)
	}
	if _, err := sqlshape.Copy[int64]("t", "a", "b").From(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "feeds exactly one column") {
		t.Errorf("scalar R, wrong column count: %v", err)
	}
	if _, err := sqlshape.Copy[ItemIn]("order_items", "nope").From(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "has no field") {
		t.Errorf("unknown column: %v", err)
	}
	type dupCol struct {
		A string `col:"x"`
		B string `col:"x"`
	}
	if _, err := sqlshape.Copy[dupCol]("t").From(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "both bind to column") {
		t.Errorf("duplicate column tags: %v", err)
	}
}

// TestNormalizeArgNilComposite covers normalizeArg / compositeArg's handling of a nil
// element inside a slice of composite pointers, and of a nil slice itself: pure
// rendering, no database needed.
func TestNormalizeArgNilComposite(t *testing.T) {
	stmt := sqlshape.Query[struct{}, struct{ Items []*ItemIn }]("SELECT {{.Items}}")
	item := ItemIn{OrderID: 1, LineNo: 1, Sku: "A", Qty: 1, Discount: "0"}
	r, err := stmt.Render(struct{ Items []*ItemIn }{Items: []*ItemIn{nil, &item}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	arr, ok := r.Args[0].([]pgtype.CompositeFields)
	if !ok || len(arr) != 2 || arr[0] != nil || arr[1] == nil {
		t.Errorf("composite slice with nil element: %#v", r.Args[0])
	}
	r2, err := stmt.Render(struct{ Items []*ItemIn }{Items: nil})
	if err != nil {
		t.Fatalf("render nil slice: %v", err)
	}
	if r2.Args[0] != nil {
		t.Errorf("nil composite slice arg = %#v, want nil", r2.Args[0])
	}
}

// noConnDB wraps a *pgx.Conn without exposing Conn(), so sqlshape's connOf cannot reach
// the underlying connection through it: withParamTypes then cannot register a missing
// user type on the fly and reports the LoadUserTypes advice instead.
type noConnDB struct{ c *pgx.Conn }

func (d noConnDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return d.c.Query(ctx, sql, args...)
}
func (d noConnDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.c.Exec(ctx, sql, args...)
}

// noQueryDB implements sqlshape.BatchDB (SendBatch) but not sqlshape.DB: a batch
// statement that must run after the batch as a plain query (a sql.Scanner result)
// then cannot, and Batch.Send surfaces that error.
type noQueryDB struct{ c *pgx.Conn }

func (d noQueryDB) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return d.c.SendBatch(ctx, b)
}

// TestRuntimeGaps exercises the harder-to-reach corners of runtime.go, render.go,
// nested.go, batch.go and copy.go: mapper edge cases, embedded-pointer allocation,
// nested-row scan overflow / NULL arrays, ConstraintError's other code paths, and
// Batch's error / already-sent / scanner-in-batch branches. It uses its own embedded
// Postgres instance (a fresh, type-registration-empty connection matters for some of
// these), separate from TestAgainstPostgres.
func TestRuntimeGaps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("internal/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	if _, err := db.Exec(ctx, `INSERT INTO users (email, name) VALUES ('a@x', 'A')`); err != nil {
		t.Fatal(err)
	}
	id1, err := insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "10.00"})
	if err != nil {
		t.Fatalf("insert order 1: %v", err)
	}
	id2, err := insertOrder.First(ctx, db, NewOrder{UserID: 1, Total: "20.00"})
	if err != nil {
		t.Fatalf("insert order 2: %v", err)
	}

	// --- newMapper edge cases -----------------------------------------------

	// R an interface type: reflect.TypeOf(zero) is nil.
	anyStmt := sqlshape.Query[any, struct{}]("SELECT 1")
	if _, err := anyStmt.First(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "must not be an interface") {
		t.Errorf("interface R: %v", err)
	}

	// scalar R, wrong result column count.
	twoColStmt := sqlshape.Query[string, struct{}]("SELECT 'a', 'b'")
	if _, err := twoColStmt.First(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "returns 2 columns") {
		t.Errorf("scalar wrong column count: %v", err)
	}

	// scalar R decoded by sql.Scanner: newMapper's scalar + userScanner branch (only a
	// non-struct R, or R == time.Time, is ever marked scalar).
	scalarTagStmt := sqlshape.One[ScalarTag, struct{}](`SELECT 'hello'`)
	if tag, err := scalarTagStmt.Get(ctx, db, struct{}{}); err != nil || tag != "hello" {
		t.Errorf("scalar Scanner result: %v %+v", err, tag)
	}

	// case-insensitive column fallback (byName misses, lower map hits).
	type idOnly struct{ ID int64 }
	lowerFallback := sqlshape.Query[idOnly, struct{}](`SELECT id AS "ID" FROM orders LIMIT 1`)
	if rows, err := lowerFallback.Collect(ctx, db, struct{}{}); err != nil || len(rows) == 0 {
		t.Errorf("lowercase column fallback: %v %+v", err, rows)
	}

	// a result column with no matching field.
	extraCol := sqlshape.Query[idOnly, struct{}](`SELECT id, total FROM orders LIMIT 1`)
	if _, err := extraCol.Collect(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "has no field") {
		t.Errorf("extra result column: %v", err)
	}

	// the same result column bound twice.
	dupCol := sqlshape.Query[idOnly, struct{}](`SELECT id, id FROM orders LIMIT 1`)
	if _, err := dupCol.Collect(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Errorf("column bound twice: %v", err)
	}

	// R with a duplicate col tag: flatFields errors, surfaced through newMapper.
	type dupTagRow struct {
		A string `col:"x"`
		B string `col:"x"`
	}
	dupTag := sqlshape.Query[dupTagRow, struct{}](`SELECT 'a' AS x`)
	if _, err := dupTag.Collect(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "both bind to column") {
		t.Errorf("duplicate col tag result: %v", err)
	}

	// --- embedded structs: tagged / non-struct / pointer leaves --------------

	// the anonymous fields' type names must be exported (capitalized): an unexported
	// anonymous field is dropped by flatFields regardless of embeddedStruct's answer.
	type ColTaggedID int64
	type DbTaggedID int64
	type embedVariantsRow struct {
		OrderBase
		ColTaggedID `col:"cv"`
		DbTaggedID  `db:"dv"`
		time.Time
		Extra string
	}
	embedVariants := sqlshape.Query[embedVariantsRow, struct{}](
		`SELECT 1::bigint AS id, '1.00'::numeric AS total, 5::bigint AS cv, 6::bigint AS dv, now() AS time, 'x' AS extra`)
	if rows, err := embedVariants.Collect(ctx, db, struct{}{}); err != nil || len(rows) != 1 || rows[0].ID != 1 || rows[0].Extra != "x" {
		t.Errorf("embedded variants (tagged / scalar leaves): %v %+v", err, rows)
	}

	// an embedded *struct: fieldByIndex must allocate it on the way to a promoted field.
	// The type must be exported: reflect never allows Set on an unexported field, even
	// from code in the same package.
	type PtrBase struct{ ID int64 }
	type rowWithPtrEmbed struct {
		*PtrBase
		Total string
	}
	ptrEmbedStmt := sqlshape.Query[rowWithPtrEmbed, struct{ ID int64 }](`SELECT id, total FROM orders WHERE id = {{.ID}}`)
	if row, err := ptrEmbedStmt.First(ctx, db, struct{ ID int64 }{id1}); err != nil || row.PtrBase == nil || row.ID != id1 {
		t.Errorf("embedded pointer allocation: %v %+v", err, row)
	}

	// --- nested rows: pointer elements, scan overflow, NULL array -----------

	type orderBriefPtr = *OrderBrief
	type userOrdersPtrs struct {
		ID     int64
		Orders []orderBriefPtr
	}
	userOrdersPtr := sqlshape.Query[userOrdersPtrs, struct{}](`
SELECT u.id, array_agg(row(o.id, o.total) ORDER BY o.id) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id ORDER BY u.id`)
	if rows, err := userOrdersPtr.Collect(ctx, db, struct{}{}); err != nil || len(rows) != 1 || len(rows[0].Orders) != 2 || rows[0].Orders[0] == nil {
		t.Errorf("nested []*struct: %v %+v", err, rows)
	}

	// a nested-row struct with fewer fields than the composite has columns:
	// rowDest.ScanIndex(i) for the extra index returns nil (dropped, not an error).
	type oneFieldOrder struct{ ID int64 }
	type userOneFieldOrders struct {
		ID     int64
		Orders []oneFieldOrder
	}
	oneFieldStmt := sqlshape.Query[userOneFieldOrders, struct{}](`
SELECT u.id, array_agg(row(o.id, o.total) ORDER BY o.id) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id ORDER BY u.id`)
	if rows, err := oneFieldStmt.Collect(ctx, db, struct{}{}); err != nil || len(rows) != 1 || len(rows[0].Orders) != 2 || rows[0].Orders[0].ID == 0 {
		t.Errorf("nested row with dropped extra column: %v %+v", err, rows)
	}

	// array_agg(...) FILTER (WHERE false) on an INNER JOIN with a match: the group is
	// non-empty before the filter, so the result is a NULL array, not an empty one.
	type userFullOrdersNull struct {
		ID     int64
		Orders []FullOrder
	}
	nullArrayStmt := sqlshape.Query[userFullOrdersNull, struct{}](`
SELECT u.id, array_agg(o ORDER BY o.id) FILTER (WHERE false) AS orders FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.id`)
	if rows, err := nullArrayStmt.Collect(ctx, db, struct{}{}); err != nil || len(rows) != 1 || rows[0].Orders != nil {
		t.Errorf("NULL array_agg: %v %+v", err, rows)
	}

	// --- ConstraintError.Key() the table.column form, from a real violation --

	_, err = insertItemNoDiscount.Exec(ctx, db, struct{ OrderID int64 }{id1})
	if !sqlshape.Violates(err, "order_items.discount") {
		t.Errorf("Violates table.column key: %v", err)
	}

	// --- Single: ErrManyRows (Find and Exec), and an error mid-iteration -----

	ordersByUser := sqlshape.One[OrderRow, struct{ UserID int64 }](`
SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.user_id = {{.UserID}}`)
	if _, _, err := ordersByUser.Find(ctx, db, struct{ UserID int64 }{1}); !errors.Is(err, sqlshape.ErrManyRows) {
		t.Errorf("Single.Find ErrManyRows: %v", err)
	}
	if _, err := ordersByUser.Get(ctx, db, struct{ UserID int64 }{1}); !errors.Is(err, sqlshape.ErrManyRows) {
		t.Errorf("Single.Get ErrManyRows: %v", err)
	}
	markPaidByUser := sqlshape.One[struct{}, struct{ UserID int64 }](`UPDATE orders SET status = 'paid' WHERE user_id = {{.UserID}}`)
	if _, err := markPaidByUser.Exec(ctx, db, struct{ UserID int64 }{1}); !errors.Is(err, sqlshape.ErrManyRows) {
		t.Errorf("Single.Exec ErrManyRows: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'pending' WHERE user_id = $1`, int64(1)); err != nil {
		t.Fatal(err)
	}
	// an error yielded mid-iteration (not the empty / many-rows cases): Find propagates it.
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'cancelled' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	orderByIDErr := sqlshape.One[OrderRow, struct{ ID int64 }](`
SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.id = {{.ID}}`)
	var ule *sqlshape.UnknownLabelError
	if _, _, err := orderByIDErr.Find(ctx, db, struct{ ID int64 }{id1}); !errors.As(err, &ule) {
		t.Errorf("Single.Find mid-iteration error: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'paid' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}

	// --- checkLabels: nil pointer (short-circuits) and an error inside a slice ---

	type optStatusRow struct{ Status *OrderStatus }
	optStatus := sqlshape.Query[optStatusRow, struct{}](`SELECT NULL::order_status AS status`)
	if rows, err := optStatus.Collect(ctx, db, struct{}{}); err != nil || len(rows) != 1 || rows[0].Status != nil {
		t.Errorf("checkLabels nil pointer: %v %+v", err, rows)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'cancelled' WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := statusPairs.First(ctx, db, struct{}{}); !errors.As(err, &ule) {
		t.Errorf("checkLabels error inside a slice: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'paid' WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}

	// --- Run / Exec: a Render error, and a plain (non-constraint) query error ---

	badField := sqlshape.Query[struct{}, struct{}]("SELECT {{.NoSuchField}}")
	if _, err := badField.Collect(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "has no field") {
		t.Errorf("Run Render error: %v", err)
	}
	if _, err := badField.Exec(ctx, db, struct{}{}); err == nil || !strings.Contains(err.Error(), "has no field") {
		t.Errorf("Exec Render error: %v", err)
	}
	badSQL := sqlshape.Query[struct{}, struct{}]("SELECT no_such_column_xyz FROM orders LIMIT 1")
	var pgErr *pgconn.PgError
	if _, err := badSQL.Collect(ctx, db, struct{}{}); !errors.As(err, &pgErr) || strings.HasPrefix(pgErr.Code, "23") {
		t.Errorf("Run plain query error (not mapped to ConstraintError): %v", err)
	}

	// wrapPgErr: an error that is not a *pgconn.PgError at all passes through unchanged.
	cancelledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if _, err := markPaid.Exec(cancelledCtx, db, struct{ ID int64 }{id1}); !errors.Is(err, context.Canceled) {
		t.Errorf("wrapPgErr non-PgError passthrough: %v", err)
	}

	// --- connOf: a DB that cannot expose *pgx.Conn ---------------------------

	// same composite-param scenario as TestAgainstPostgres's saveOrder (a fresh
	// connection encoding a not-yet-registered composite type fails client-side with
	// "unknown type (OID n)"), but through a DB that cannot expose its *pgx.Conn:
	// withParamTypes cannot register the type and reports the LoadUserTypes advice.
	nc := noConnDB{c: db}
	ncItems := []ItemIn{{OrderID: id1, LineNo: 9, Sku: "N", Qty: 1, Discount: "0.00"}}
	if _, err := saveOrder.First(ctx, nc, struct {
		Price MoneyIn
		Items []ItemIn
	}{MoneyIn{"1.00", "JPY"}, ncItems}); err == nil || !strings.Contains(err.Error(), "LoadUserTypes") {
		t.Errorf("withParamTypes with no reachable *pgx.Conn: %v", err)
	}

	// --- Copy: the no-columns-given path (every field's own column) ---------

	loadItemsAllCols := sqlshape.Copy[ItemIn]("order_items")
	moreItems := []ItemIn{{OrderID: id2, LineNo: 1, Sku: "Z", Qty: 3, Discount: "0.00"}}
	if n, err := loadItemsAllCols.From(ctx, db, moreItems); err != nil || n != 1 {
		t.Errorf("Copy with no columns given: %v %d", err, n)
	}

	// a scalar R actually copied (layout's scalar row-picker func invoked, not just built).
	loadNames := sqlshape.Copy[string]("users", "name")
	if n, err := loadNames.From(ctx, db, []string{"Eve", "Frank"}); err != nil || n != 2 {
		t.Errorf("scalar Copy: %v %d", err, n)
	}

	// --- Batch: not-yet-sent accessors, queue-after-send, Render error in Queue,
	//     already-sent Send, Send with a pending render error, a scanner statement
	//     actually running (and one that fails to, on a BatchDB that cannot Query) ---

	b := sqlshape.NewBatch()
	qBad := sqlshape.Queue(b, badField, struct{}{})
	if _, err := qBad.Tag(); err == nil {
		t.Errorf("Tag before send should be the render error, got nil")
	}
	qOK := sqlshape.Queue(b, listOrders, ListParams{})
	if _, err := qOK.Tag(); !errors.Is(err, sqlshape.ErrNotSent) {
		t.Errorf("Tag before send: %v", err)
	}
	if err := b.Send(ctx, db); err == nil || !strings.Contains(err.Error(), "has no field") {
		t.Errorf("Send with a pending render error: %v", err)
	}
	if err := b.Send(ctx, db); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Errorf("Send twice: %v", err)
	}
	if q := sqlshape.Queue(b, listOrders, ListParams{}); q == nil {
		t.Error("Queue after send should still return a handle")
	} else if _, err := q.Rows(); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Errorf("Queue after send: %v", err)
	}

	// a scanner-result statement actually run through Send's "after" path (kept in its
	// own batch: a failing pgx-batch statement would otherwise abort it before the
	// after-loop runs at all).
	if _, err := db.Exec(ctx, `UPDATE orders SET price = ROW(3, 'JPY') WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	b2 := sqlshape.NewBatch()
	qScanner := sqlshape.QueueOne(b2, priceTagOf, struct{ ID int64 }{id1})
	if err := b2.Send(ctx, db); err != nil {
		t.Fatalf("batch send (scanner after-path): %v", err)
	}
	if pt, err := qScanner.First(); err != nil || pt.Price.Text == "" {
		t.Errorf("batch scanner statement (after path): %v %+v", err, pt)
	}

	// a normal statement whose result mismatches its struct: the mapper error surfaces
	// from inside the pgx batch's own Query callback.
	b2b := sqlshape.NewBatch()
	qMismatch := sqlshape.Queue(b2b, badPartialOrder, struct{ ID int64 }{id1})
	if err := b2b.Send(ctx, db); err == nil || !strings.Contains(err.Error(), "no result column") {
		t.Errorf("batch mapper mismatch (send): %v", err)
	}
	if _, err := qMismatch.First(); err == nil {
		t.Errorf("batch mapper mismatch (queued handle): want an error, got nil")
	}

	// hasUserScanner: R nil (interface) - Queue itself must not panic on a nil
	// reflect.Type, and routes through the normal pgx-batch path; the mapper then
	// fails the same way it does outside a batch (R must not be an interface type).
	b3 := sqlshape.NewBatch()
	sqlshape.Queue(b3, anyStmt, struct{}{})
	if err := b3.Send(ctx, db); err == nil || !strings.Contains(err.Error(), "must not be an interface") {
		t.Errorf("batch with R=any: %v", err)
	}

	// hasUserScanner: R with a duplicate col tag - flatFields errors, so hasUserScanner
	// reports false (routes through the normal pgx-batch path) rather than propagating
	// the error itself; the mapper then fails the same way it does outside a batch.
	b3b := sqlshape.NewBatch()
	sqlshape.Queue(b3b, dupTag, struct{}{})
	if err := b3b.Send(ctx, db); err == nil || !strings.Contains(err.Error(), "both bind to column") {
		t.Errorf("batch with a duplicate-col-tag result type: %v", err)
	}

	// hasUserScanner: R itself (not merely a field of it) is a struct implementing
	// sql.Scanner.
	sqlshape.Queue(sqlshape.NewBatch(), sqlshape.Query[PriceTag, struct{}]("SELECT 1"), struct{}{})

	// a normal (non-scalar) batched query whose mapper builds fine but a row fails to
	// scan (UnknownLabelError): batch.go's own row-scan-error branch inside the pgx
	// Query callback (a separate implementation from Run's).
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'cancelled' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	b3c := sqlshape.NewBatch()
	qScanErr := sqlshape.Queue(b3c, listOrders, ListParams{})
	if err := b3c.Send(ctx, db); !errors.As(err, &ule) {
		t.Errorf("batch row scan error: %v", err)
	}
	if _, err := qScanErr.Rows(); err == nil {
		t.Error("batch row scan error (queued handle): want an error, got nil")
	}
	if _, err := db.Exec(ctx, `UPDATE orders SET status = 'paid' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}

	// a BatchDB that cannot run a plain query: the scanner "after" statement fails,
	// and Batch.Send surfaces that error from its after-loop.
	nq := noQueryDB{c: db}
	b4 := sqlshape.NewBatch()
	sqlshape.QueueOne(b4, priceTagOf, struct{ ID int64 }{id1})
	if err := b4.Send(ctx, nq); err == nil || !strings.Contains(err.Error(), "cannot run a plain query") {
		t.Errorf("batch after-statement on a query-less BatchDB: %v", err)
	}
}
