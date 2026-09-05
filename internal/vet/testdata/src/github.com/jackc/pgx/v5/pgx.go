// Package pgx is a stub of github.com/jackc/pgx/v5 for the analyzer's fixtures.
package pgx

import "context"

type Rows interface{ Next() bool }
type Row interface{ Scan(dest ...any) error }
type CommandTag struct{}
type Identifier []string
type CopyFromSource interface{}
type BatchResults interface{ Close() error }

type Conn struct{}

func (c *Conn) Query(ctx context.Context, sql string, args ...any) (Rows, error) { return nil, nil }
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) Row        { return nil }
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	return CommandTag{}, nil
}
func (c *Conn) Prepare(ctx context.Context, name, sql string) error { return nil }
func (c *Conn) CopyFrom(ctx context.Context, t Identifier, cols []string, src CopyFromSource) (int64, error) {
	return 0, nil
}
func (c *Conn) SendBatch(ctx context.Context, b *Batch) BatchResults { return nil }

type Tx interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
}

type Batch struct{}
type QueuedQuery struct{}

func (b *Batch) Queue(query string, arguments ...any) *QueuedQuery { return nil }
