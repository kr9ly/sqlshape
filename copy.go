package sqlshape

import (
	"context"
	"fmt"
	"iter"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CopyDB is what Copier.From needs; pgx.Conn, pgxpool.Pool and pgx.Tx all provide it.
type CopyDB interface {
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Copier bulk-loads rows of R into a table with COPY FROM (pgx.CopyFrom). Declare it with
// Copy; the checker verifies the table and columns against the schema and each column's
// type against the field that feeds it.
type Copier[R any] struct {
	Table   string
	Columns []string
}

// Copy declares a bulk load into table (optionally schema-qualified). columns are the
// table columns to fill, each fed by the field of R that binds to it (`col:"name"` / `db`
// tag or snake_case, embedded structs flattened); with no columns given, every field of R
// feeds the column of its name. A scalar R feeds the single column.
//
//	var loadItems = sqlshape.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")
//	n, err := loadItems.From(ctx, db, items)
func Copy[R any](table string, columns ...string) Copier[R] {
	return Copier[R]{Table: table, Columns: columns}
}

// From copies rows into the table and returns how many.
func (c Copier[R]) From(ctx context.Context, db CopyDB, rows []R) (int64, error) {
	return c.FromSeq(ctx, db, func(yield func(R) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	})
}

// FromSeq copies the rows of a sequence into the table and returns how many.
func (c Copier[R]) FromSeq(ctx context.Context, db CopyDB, rows iter.Seq[R]) (int64, error) {
	cols, pick, err := c.layout()
	if err != nil {
		return 0, err
	}
	next, stop := iter.Pull(rows)
	defer stop()
	src := pgx.CopyFromFunc(func() ([]any, error) {
		r, ok := next()
		if !ok {
			return nil, nil
		}
		return pick(r), nil
	})
	return db.CopyFrom(ctx, pgx.Identifier(strings.Split(c.Table, ".")), cols, src)
}

// layout resolves the column list and how a row of R is turned into values for it.
func (c Copier[R]) layout() ([]string, func(R) []any, error) {
	var zero R
	rt := reflect.TypeOf(zero)
	if rt == nil {
		return nil, nil, fmt.Errorf("sqlshape: Copy: R must not be an interface type")
	}
	if rt.Kind() != reflect.Struct || scalarStruct(rt) {
		if len(c.Columns) != 1 {
			return nil, nil, fmt.Errorf("sqlshape: Copy[%s] into %s: a scalar R feeds exactly one column, %d given", rt, c.Table, len(c.Columns))
		}
		return c.Columns, func(r R) []any { return []any{normalizeArg(reflect.ValueOf(r))} }, nil
	}
	flat, err := flatFields(rt)
	if err != nil {
		return nil, nil, err
	}
	byCol := map[string]flatField{}
	for _, f := range flat {
		byCol[f.column] = f
	}
	cols := c.Columns
	if len(cols) == 0 {
		for _, f := range flat {
			cols = append(cols, f.column)
		}
	}
	idx := make([][]int, len(cols))
	for i, col := range cols {
		f, ok := byCol[col]
		if !ok {
			return nil, nil, fmt.Errorf("sqlshape: Copy into %s: column %q has no field in %s", c.Table, col, rt)
		}
		idx[i] = f.index
	}
	return cols, func(r R) []any {
		v := reflect.ValueOf(r)
		out := make([]any, len(idx))
		for i, ix := range idx {
			out[i] = normalizeArg(v.FieldByIndex(ix))
		}
		return out
	}, nil
}
