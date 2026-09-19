package postgres_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
)

type ItemIn struct {
	OrderID  int64
	LineNo   int32
	Sku      string
	Qty      int32
	Discount string
}

// TestNormalizeArgNilComposite covers normalizeArg / compositeArg's handling of a nil
// element inside a slice of composite pointers, and of a nil slice itself: pure
// rendering plus the runtime's argument pass, no database needed.
func TestNormalizeArgNilComposite(t *testing.T) {
	stmt := sqlshape.Query[struct{}, struct{ Items []*ItemIn }]("SELECT {{.Items}}")
	item := ItemIn{OrderID: 1, LineNo: 1, Sku: "A", Qty: 1, Discount: "0"}
	r, err := stmt.Render(struct{ Items []*ItemIn }{Items: []*ItemIn{nil, &item}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	args := postgres.Args(r.Args)
	arr, ok := args[0].([]pgtype.CompositeFields)
	if !ok || len(arr) != 2 || arr[0] != nil || arr[1] == nil {
		t.Errorf("composite slice with nil element: %#v", args[0])
	}
	r2, err := stmt.Render(struct{ Items []*ItemIn }{Items: nil})
	if err != nil {
		t.Fatalf("render nil slice: %v", err)
	}
	if a := postgres.Args(r2.Args); a[0] != nil {
		t.Errorf("nil composite slice arg = %#v, want nil", a[0])
	}
}
