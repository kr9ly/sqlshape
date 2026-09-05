package rawforbid

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// -raw-sql=forbid: no driver call at all, constant or not

const byID = "SELECT id FROM users WHERE id = $1"

func f(ctx context.Context, c *pgx.Conn, id int64) {
	c.Query(ctx, byID, id)                                        // want `pgx.Query executes SQL outside sqlshape; with -raw-sql=forbid every statement goes through sqlshape.Query / One / Copy \(or list the package in -raw-sql-allow\)`
	c.CopyFrom(ctx, pgx.Identifier{"users"}, []string{"id"}, nil) // want `pgx.CopyFrom executes SQL outside sqlshape`
	var b pgx.Batch
	b.Queue(byID, id)    // want `pgx.Queue executes SQL outside sqlshape`
	c.SendBatch(ctx, &b) // want `pgx.SendBatch executes SQL outside sqlshape`
}
