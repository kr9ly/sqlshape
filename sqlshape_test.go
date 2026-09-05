package sqlshape_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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
	if err := sqlshape.LoadUserTypes(ctx, db); err != nil {
		t.Fatalf("LoadUserTypes: %v", err)
	}
	sp, err := statusPairs.First(ctx, db, struct{}{})
	if err != nil || len(sp) != 2 || sp[1].Status != "paid" {
		t.Errorf("enum in record: %v %+v", err, sp)
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
