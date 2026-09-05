// Package pgtest starts a real PostgreSQL with a schema.sql applied, for testing the
// database side of an application (views, functions, triggers, constraints) from Go the
// same way the checker's oracle does. Nothing persists: the server lives in a temporary
// directory and dies with Close.
package pgtest

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/internal/oracle"
)

// DB is a running PostgreSQL with the schema applied.
type DB struct {
	o *oracle.Oracle
}

// Start launches PostgreSQL (embedded binary, downloaded on first use) and applies schemaSQL.
func Start(ctx context.Context, schemaSQL string) (*DB, error) {
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		return nil, err
	}
	return &DB{o: o}, nil
}

// Conn is a connection to the database; it satisfies sqlshape.DB.
func (d *DB) Conn() *pgx.Conn { return d.o.Conn() }

// Close stops the server and removes its data.
func (d *DB) Close() error { return d.o.Close() }
