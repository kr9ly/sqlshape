package dto

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kr9ly/sqlshape"
)

var _, _, _ = json.RawMessage{}, time.Time{}, pgtype.Numeric{}

// A query's DTO is written by declaring an empty struct and accepting the quick fix on
// any of the "no field" diagnostics: the struct is rewritten from the result columns.

type OrderStatus string // want OrderStatus:`bound e order_status`

var byStatus = sqlshape.Query[int64, struct{ S OrderStatus }](`SELECT id FROM orders WHERE status = {{.S}}`)

type OrderRow struct{}

var orderRow = sqlshape.Query[OrderRow, struct{}](`SELECT id, status, total, note, created_at FROM orders WHERE id = 1`) // want `result column "id" has no field in dto.OrderRow` `result column "status" has no field` `result column "total" has no field` `result column "note" has no field` `result column "created_at" has no field`

// A branch that adds a column makes its field a pointer; the control read by the branch
// becomes a bool field of P.
type Branchy struct{}

var branchy = sqlshape.Query[Branchy, struct{}](`SELECT id{{if .WithNote}}, note{{end}} FROM orders`) // want `struct\{\} has no field WithNote` `result column "id" has no field` `result column "note" has no field`

// Parameters: P is written from the paths, typed by what the SQL expects (the bound
// OrderStatus for the enum column).
type Params struct{}

var params = sqlshape.Query[int64, Params](`SELECT id FROM orders WHERE user_id = {{.User}} AND status = {{.Status}} AND note LIKE {{.Pattern}}`) // want `dto.Params has no field User` `dto.Params has no field Status` `dto.Params has no field Pattern`

// A struct that exists keeps the names and doc comments of the fields that already
// match a column; the rest is rewritten.
type Kept struct {
	// The order's identifier, as we call it.
	Identifier int64 `col:"id"`
	Bogus      string
	Total      float32
}

var kept = sqlshape.Query[Kept, struct{}](`SELECT id, total, meta, uid, matrix FROM orders`) // want `field Kept.Bogus has no result column` `field Total: numeric into float32 loses precision` `result column "meta" has no field` `result column "uid" has no field` `result column "matrix" has no field`
