package sqlshape_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

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
-- sqlshape: expect orders_pkey, orders_user_note_key, orders_uid_active, orders_user_id_fkey, orders_total_check, P0401
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
