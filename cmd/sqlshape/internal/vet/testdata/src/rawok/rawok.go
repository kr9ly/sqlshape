package rawok

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// listed in -raw-sql-allow: forbid does not apply here

func f(ctx context.Context, c *pgx.Conn, table string) {
	c.Query(ctx, "SELECT id FROM "+table)
}
