package raw

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -raw-sql=constant (the default): driver calls may run constant SQL only

const byID = "SELECT id FROM users WHERE id = $1"

func ok(ctx context.Context, c *pgx.Conn, p *pgxpool.Pool, db *sql.DB, tx pgx.Tx, id int64) {
	c.Query(ctx, byID, id)
	c.QueryRow(ctx, "SELECT 1")
	c.Exec(ctx, byID+" AND true", id)
	c.Prepare(ctx, "q", byID)
	p.Query(ctx, byID, id)
	tx.Exec(ctx, byID, id)
	db.QueryContext(ctx, byID, id)
	db.Exec(byID, id)
	var b pgx.Batch
	b.Queue(byID, id)
	c.SendBatch(ctx, &b)
	c.CopyFrom(ctx, pgx.Identifier{"users"}, []string{"id"}, nil)
}

func bad(ctx context.Context, c *pgx.Conn, p *pgxpool.Pool, db *sql.DB, tx pgx.Tx, table, order string, id int64) {
	c.Query(ctx, "SELECT id FROM "+table, id)                           // want `SQL passed to Query must be a constant: a string built at run time can carry injected SQL; write the dynamic parts as a sqlshape.Query template \(or pass -raw-sql=allow\)`
	c.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = $1", table), id) // want `SQL passed to Exec must be a constant`
	c.QueryRow(ctx, byID+" ORDER BY "+order)                            // want `SQL passed to QueryRow must be a constant`
	c.Prepare(ctx, "q", byID+order)                                     // want `SQL passed to Prepare must be a constant`
	p.Exec(ctx, "TRUNCATE "+table)                                      // want `SQL passed to Exec must be a constant`
	tx.Query(ctx, "SELECT * FROM "+table)                               // want `SQL passed to Query must be a constant`
	db.Query("SELECT * FROM " + table)                                  // want `SQL passed to Query must be a constant`
	db.ExecContext(ctx, "DELETE FROM "+table)                           // want `SQL passed to ExecContext must be a constant`
	db.QueryRowContext(ctx, "SELECT count(*) FROM "+table)              // want `SQL passed to QueryRowContext must be a constant`
	db.PrepareContext(ctx, "SELECT * FROM "+table)                      // want `SQL passed to PrepareContext must be a constant`
	var b pgx.Batch
	b.Queue("SELECT * FROM "+table, id) // want `SQL passed to Queue must be a constant`
}
