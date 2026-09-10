// Package pgxpool is a stub for the analyzer's fixtures.
package pgxpool

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type Pool struct{}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) { return nil, nil }
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgx.CommandTag, error) {
	return pgx.CommandTag{}, nil
}
