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
-- sqlshape: expect orders_pkey, orders_user_note_key, orders_uid_active, orders_user_id_fkey, orders_total_check
INSERT INTO orders (user_id, total, note) VALUES ({{.UserID}}, {{.Total}}, {{.Note}}) RETURNING id`)

var markPaid = sqlshape.Query[struct{}, struct{ ID int64 }](`UPDATE orders SET status = 'paid' WHERE id = {{.ID}}`)

var orderByID = sqlshape.One[OrderRow, struct{ ID int64 }](`
SELECT o.id, o.status, o.total, o.note, o.created_at FROM orders o WHERE o.id = {{.ID}}`)

var countByStatus = sqlshape.Query[struct {
	Status OrderStatus
	N      int64
}, struct{}](`SELECT status, count(*) AS n FROM orders GROUP BY status ORDER BY status`)

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
