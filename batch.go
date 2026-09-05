package sqlshape

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// BatchDB is what Batch.Send needs; pgx.Conn, pgxpool.Pool and pgx.Tx all provide it.
type BatchDB interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Batch sends several statements to the server in one round trip (pgx.Batch). Queue
// statements with Queue / QueueOne, Send the batch, then read each statement's rows from
// the handle Queue returned:
//
//	b := sqlshape.NewBatch()
//	orders := sqlshape.Queue(b, listOrders, ListParams{Status: &paid})
//	paid := sqlshape.QueueOne(b, markPaid, struct{ ID int64 }{id})
//	if err := b.Send(ctx, db); err != nil { ... }
//	rows, _ := orders.Rows()
//	tag, _ := paid.Tag()
//
// Result types the connection has not loaded yet cannot be loaded mid-batch: with user
// enums / composites in results or parameters, call LoadUserTypes on the connection first
// (pgxpool: in AfterConnect).
type Batch struct {
	b    *pgx.Batch
	err  error // the first error rendering a queued statement; Send returns it
	sent bool
}

// NewBatch starts an empty batch.
func NewBatch() *Batch { return &Batch{b: &pgx.Batch{}} }

// Len is the number of queued statements.
func (b *Batch) Len() int { return b.b.Len() }

// ErrNotSent is returned by a queued statement's accessors before the batch was sent.
var ErrNotSent = errors.New("sqlshape: batch not sent")

// Queued is one statement's place in a batch: its rows (or command tag) after Send.
type Queued[R any] struct {
	rows []R
	tag  pgconn.CommandTag
	err  error
	done bool
}

// Rows are the statement's result rows.
func (q *Queued[R]) Rows() ([]R, error) {
	if !q.done {
		return nil, ErrNotSent
	}
	return q.rows, q.err
}

// First is the first row, or ErrNoRows.
func (q *Queued[R]) First() (R, error) {
	var zero R
	rows, err := q.Rows()
	if err != nil {
		return zero, err
	}
	if len(rows) == 0 {
		return zero, ErrNoRows
	}
	return rows[0], nil
}

// Tag is the statement's command tag (rows affected for INSERT / UPDATE / DELETE).
func (q *Queued[R]) Tag() (pgconn.CommandTag, error) {
	if !q.done {
		return pgconn.CommandTag{}, ErrNotSent
	}
	return q.tag, q.err
}

// Queue renders s for p and adds it to the batch.
func Queue[R, P any](b *Batch, s Stmt[R, P], p P) *Queued[R] {
	q := &Queued[R]{}
	if b.sent {
		q.err, q.done = errors.New("sqlshape: batch already sent"), true
		return q
	}
	r, err := s.Render(p)
	if err != nil {
		if b.err == nil {
			b.err = err
		}
		q.err, q.done = err, true
		return q
	}
	qq := b.b.Queue(r.SQL, s.args(r.Args)...)
	var zero R
	if rt := reflect.TypeOf(zero); rt != nil && rt.Kind() == reflect.Struct && rt.NumField() == 0 {
		// R = struct{}: a statement without rows
		qq.Exec(func(tag pgconn.CommandTag) error {
			q.tag, q.done = tag, true
			return nil
		})
		return q
	}
	qq.Query(func(rows pgx.Rows) error {
		defer rows.Close()
		q.done = true
		var m *mapper[R]
		for rows.Next() {
			if m == nil {
				if m, err = newMapper[R](rows.FieldDescriptions()); err != nil {
					q.err = err
					return err
				}
			}
			row, err := m.scan(rows)
			if err != nil {
				q.err = err
				return err
			}
			q.rows = append(q.rows, row)
		}
		q.tag = rows.CommandTag()
		if err := rows.Err(); err != nil {
			q.err = s.wrapErr(err)
			return q.err
		}
		return nil
	})
	return q
}

// QueueOne renders the single-row statement s for p and adds it to the batch. After Send,
// Rows holds at most one row (the checker's proof) and First returns it or ErrNoRows.
func QueueOne[R, P any](b *Batch, s Single[R, P], p P) *Queued[R] {
	return Queue(b, s.stmt, p)
}

// Send runs the batch in one round trip. The first failing statement's error is returned
// (constraint violations mapped to ConstraintError like Run does); the statements after it
// are not executed, and their accessors return ErrNotSent.
func (b *Batch) Send(ctx context.Context, db BatchDB) error {
	if b.sent {
		return errors.New("sqlshape: batch already sent")
	}
	b.sent = true
	if b.err != nil {
		return b.err
	}
	br := db.SendBatch(ctx, b.b)
	err := br.Close()
	if err != nil {
		return wrapPgErr(err, nil)
	}
	return nil
}
