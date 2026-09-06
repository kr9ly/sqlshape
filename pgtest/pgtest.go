// Package pgtest starts a real PostgreSQL with a schema.sql applied, for testing the
// database side of an application (views, functions, triggers, constraints) from Go the
// same way the checker's oracle does. Nothing persists: the server lives in a temporary
// directory and dies with Close.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/schema"
	"github.com/kr9ly/sqlshape/internal/verify"
)

// DB is a running PostgreSQL with the schema applied.
type DB struct {
	o         *oracle.Oracle
	schemaSQL string
	schema    *schema.Schema
}

// Start launches PostgreSQL (embedded binary, downloaded on first use) and applies schemaSQL.
func Start(ctx context.Context, schemaSQL string) (*DB, error) {
	o, err := oracle.Start(ctx, schemaSQL)
	if err != nil {
		return nil, err
	}
	return &DB{o: o, schemaSQL: schemaSQL}, nil
}

// ReadSchema reads a schema.sql file, or a directory of *.sql files applied in name order,
// the same way the checker does, for passing to Start.
func ReadSchema(path string) (string, error) { return schema.ReadSource(path) }

// Statement is what Verify accepts: sqlshape.Stmt and sqlshape.Single.
type Statement interface{ SQLTemplate() string }

// Verify checks that the analyzer agrees with this PostgreSQL about each statement: every
// expansion of the template (each combination of its branches) is prepared by PG, and PG's
// parameter types, result column names and types, or its rejection, are compared with what
// the checker concluded. A disagreement means the checker's verdict on that statement
// cannot be trusted; the error lists each one with the SQL and both descriptions. Put it
// in a test next to Start so the application carries the evidence that its static checks
// hold for the PostgreSQL it runs on.
func (d *DB) Verify(ctx context.Context, stmts ...Statement) error {
	if d.schema == nil {
		s, err := analyze.Load(d.schemaSQL)
		if err != nil {
			return fmt.Errorf("pgtest: schema: %w", err)
		}
		for _, p := range s.Problems {
			return fmt.Errorf("pgtest: schema: %s", p)
		}
		d.schema = s
	}
	var errs []error
	for _, st := range stmts {
		ms, err := verify.Template(ctx, d.o, d.schema, st.SQLTemplate())
		if err != nil {
			errs = append(errs, fmt.Errorf("pgtest: template %q: %w", firstLine(st.SQLTemplate()), err))
			continue
		}
		for _, m := range ms {
			errs = append(errs, m)
		}
	}
	return errors.Join(errs...)
}

func firstLine(s string) string {
	return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
}

// Conn is a connection to the database; it satisfies sqlshape.DB.
func (d *DB) Conn() *pgx.Conn { return d.o.Conn() }

// ConnString connects other clients (a pgxpool.Pool, psql) to the same server.
func (d *DB) ConnString() string { return d.o.ConnString() }

// Close stops the server and removes its data.
func (d *DB) Close() error { return d.o.Close() }
