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
// (pgxpool: in AfterConnect). A statement whose rows carry a sql.Scanner type (a declared
// binding) cannot ride in a pgx batch, which never requests text format; Send runs it as a
// plain query right after the batch.
type Batch struct {
	b    *pgx.Batch
	err  error // the first error rendering a queued statement; Send returns it
	sent bool
	// after are statements a pgx batch cannot carry: a result received by a sql.Scanner
	// must come in text format, which only a plain Query can ask for; they run right
	// after the batch, on the same db, in queue order
	after []func(ctx context.Context, db BatchDB) error
}

// NewBatch starts an empty batch.
func NewBatch() *Batch { return &Batch{b: &pgx.Batch{}} }

// Len is the number of queued statements.
func (b *Batch) Len() int { return b.b.Len() + len(b.after) }

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
	var zero R
	if hasUserScanner(reflect.TypeOf(zero)) {
		b.after = append(b.after, func(ctx context.Context, db BatchDB) error {
			q.done = true
			if d, ok := db.(DB); ok {
				q.rows, q.err = s.Collect(ctx, d, p)
			} else {
				q.err = errors.New("sqlshape: batch db cannot run a plain query")
			}
			return q.err
		})
		return q
	}
	qq := b.b.Queue(r.SQL, r.Args...)
	if rt := reflect.TypeOf(zero); rt != nil && rt.Kind() == reflect.Struct && rt.NumField() == 0 {
		// R = struct{}: a statement without rows. Set Fn directly rather than through
		// QueuedQuery.Exec: pgx's Exec wrapper calls its callback only when br.Exec()
		// succeeds, so a failing statement would leave q.err unset and q.done false
		// (Tag reporting ErrNotSent instead of the real error).
		qq.Fn = func(br pgx.BatchResults) error {
			tag, err := br.Exec()
			q.done = true
			if err != nil {
				q.err = s.wrapErr(err)
				return q.err
			}
			q.tag = tag
			return nil
		}
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
	if b.b.Len() > 0 {
		br := db.SendBatch(ctx, b.b)
		if err := br.Close(); err != nil {
			return wrapPgErr(err, nil)
		}
	}
	for _, run := range b.after {
		if err := run(ctx, db); err != nil {
			return err
		}
	}
	return nil
}

// hasUserScanner reports whether R (or a field of it, embedded structs included) decodes
// itself through sql.Scanner, which needs text format a batch cannot request.
func hasUserScanner(rt reflect.Type) bool {
	if rt == nil {
		return false
	}
	if rt.Kind() != reflect.Struct || scalarStruct(rt) {
		return userScanner(rt)
	}
	if userScanner(rt) {
		return true
	}
	flat, err := flatFields(rt)
	if err != nil {
		return false
	}
	for _, f := range flat {
		if userScanner(f.typ) {
			return true
		}
	}
	return false
}
