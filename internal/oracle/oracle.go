// Package oracle runs a real PostgreSQL (embedded-postgres) and answers
// "what does PG say about this statement?" via PREPARE / Describe.
//
// It is the ground truth for differential tests of the pure-Go analyzer.
// It is not used at lint time.
package oracle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Version is the PostgreSQL version the oracle runs. The generated catalog
// must be produced from the same major version.
const Version = embeddedpostgres.V17

// Oracle owns one embedded PostgreSQL instance and one connection to it.
type Oracle struct {
	pg          *embeddedpostgres.EmbeddedPostgres
	conn        *pgx.Conn
	runtimePath string
	dsn         string
}

// Type is a PostgreSQL type as PG itself prints it (format_type), plus the raw OID / typmod.
type Type struct {
	OID    uint32
	Typmod int32
	// Name is format_type(OID, Typmod), e.g. "numeric(12,2)", "order_status", "bigint[]".
	Name string
}

// Column is one result column of a described statement.
type Column struct {
	Name string
	Type Type
	// Source is set when the column comes straight from a table column.
	Source *Source
}

// Source is the table column a result column originates from (from Describe's TableOID/attnum).
type Source struct {
	Table   string // schema-qualified unless public
	Column  string
	NotNull bool
}

// Description is PG's answer for one statement.
type Description struct {
	Params  []Type
	Columns []Column
}

// PgError is the PostgreSQL error for a statement that fails to prepare.
type PgError struct {
	Code     string // SQLSTATE
	Message  string
	Position int32 // 1-based byte offset into the SQL, 0 if none
}

func (e *PgError) Error() string {
	if e.Position > 0 {
		return fmt.Sprintf("%s: %s (at %d)", e.Code, e.Message, e.Position)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Start boots a fresh PostgreSQL, applies schemaSQL, and returns a ready Oracle.
// Binaries are cached under ~/.embedded-postgres-go across runs; data lives in a temp dir.
func Start(ctx context.Context, schemaSQL string) (*Oracle, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	runtimePath, err := os.MkdirTemp("", "sqlshape-oracle-")
	if err != nil {
		return nil, err
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(Version).
		Port(uint32(port)).
		RuntimePath(runtimePath).
		Database("sqlshape").
		Username("sqlshape").
		Password("sqlshape").
		// a throwaway server: durability only costs time (the regress probe runs many
		// sessions at once, and every fsync serializes them)
		StartParameters(map[string]string{"fsync": "off", "synchronous_commit": "off", "full_page_writes": "off", "max_connections": "50"}).
		Logger(nil)
	if path := os.Getenv("SQLSHAPE_ORACLE_LOG"); path != "" {
		// the server log, for tracking down backend crashes in the regress probe
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cfg = cfg.Logger(f)
		}
	}
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		os.RemoveAll(runtimePath)
		return nil, fmt.Errorf("start embedded postgres: %w", err)
	}
	o := &Oracle{pg: pg, runtimePath: runtimePath}
	dsn := fmt.Sprintf("postgres://sqlshape:sqlshape@127.0.0.1:%d/sqlshape?sslmode=disable", port)
	o.dsn = dsn
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		o.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	o.conn = conn
	if strings.TrimSpace(schemaSQL) != "" {
		if _, err := conn.Exec(ctx, schemaSQL); err != nil {
			o.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	return o, nil
}

// Close stops PostgreSQL and removes its data directory.
func (o *Oracle) Close() error {
	var errs []error
	if o.conn != nil {
		errs = append(errs, o.conn.Close(context.Background()))
	}
	if o.pg != nil {
		errs = append(errs, o.pg.Stop())
	}
	if o.runtimePath != "" {
		errs = append(errs, os.RemoveAll(o.runtimePath))
	}
	return errors.Join(errs...)
}

// Describe prepares sql (anonymous statement, nothing persists server-side) and
// returns PG's parameter and result-column types. A statement PG rejects
// returns a *PgError.
func (o *Oracle) Describe(ctx context.Context, sql string) (*Description, error) {
	sd, err := o.conn.Prepare(ctx, "", sql)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, &PgError{Code: pgErr.Code, Message: pgErr.Message, Position: pgErr.Position}
		}
		return nil, err
	}
	d := &Description{}
	for _, oid := range sd.ParamOIDs {
		t, err := o.formatType(ctx, oid, -1)
		if err != nil {
			return nil, err
		}
		d.Params = append(d.Params, t)
	}
	for _, f := range sd.Fields {
		t, err := o.formatType(ctx, f.DataTypeOID, f.TypeModifier)
		if err != nil {
			return nil, err
		}
		c := Column{Name: f.Name, Type: t}
		if f.TableOID != 0 && f.TableAttributeNumber != 0 { // attnum 0 = whole-row reference
			src, err := o.source(ctx, f.TableOID, f.TableAttributeNumber)
			if err != nil {
				return nil, err
			}
			c.Source = src
		}
		d.Columns = append(d.Columns, c)
	}
	return d, nil
}

func (o *Oracle) formatType(ctx context.Context, oid uint32, typmod int32) (Type, error) {
	var name string
	err := o.conn.QueryRow(ctx, `SELECT format_type($1::oid, $2::int4)`, oid, typmod).Scan(&name)
	if err != nil {
		return Type{}, fmt.Errorf("format_type(%d,%d): %w", oid, typmod, err)
	}
	return Type{OID: oid, Typmod: typmod, Name: name}, nil
}

func (o *Oracle) source(ctx context.Context, tableOID uint32, attnum uint16) (*Source, error) {
	var s Source
	err := o.conn.QueryRow(ctx, `
		SELECT CASE WHEN n.nspname = 'public' THEN c.relname ELSE n.nspname || '.' || c.relname END,
		       a.attname, a.attnotnull
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE a.attrelid = $1::oid AND a.attnum = $2::int2`, tableOID, int16(attnum)).
		Scan(&s.Table, &s.Column, &s.NotNull)
	if err != nil {
		return nil, fmt.Errorf("resolve source %d.%d: %w", tableOID, attnum, err)
	}
	return &s, nil
}

// String renders a Description in the golden-file format used by differential tests.
func (d *Description) String() string {
	var b strings.Builder
	b.WriteString("params:\n")
	for i, p := range d.Params {
		fmt.Fprintf(&b, "  $%d %s\n", i+1, p.Name)
	}
	b.WriteString("columns:\n")
	for _, c := range d.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, c.Type.Name)
		if c.Source != nil {
			fmt.Fprintf(&b, " <- %s.%s", c.Source.Table, c.Source.Column)
			if c.Source.NotNull {
				b.WriteString(" not null")
			} else {
				b.WriteString(" null")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Conn exposes the underlying connection for tooling (catalog dump). Tests use Describe.
func (o *Oracle) Conn() *pgx.Conn { return o.conn }

// ConnString is the connection string of the running server (for pools and other clients).
func (o *Oracle) ConnString() string { return o.dsn }

// Session opens another connection to the same server, to the given database, for
// callers that work in parallel. Closing a session closes only its connection.
func (o *Oracle) Session(ctx context.Context, database string) (*Oracle, error) {
	s := &Oracle{dsn: o.dsn}
	if err := s.Reconnect(ctx, database); err != nil {
		return nil, err
	}
	return s, nil
}

// Reconnect closes the current connection and opens one to another database on the
// same server. Used by the regress probe, which isolates each test file in its own
// database created from a template.
func (o *Oracle) Reconnect(ctx context.Context, database string) error {
	if o.conn != nil {
		o.conn.Close(ctx)
		o.conn = nil
	}
	i := strings.LastIndex(o.dsn, "/")
	j := strings.Index(o.dsn[i:], "?")
	dsn := o.dsn[:i+1] + database + o.dsn[i+j:]
	// A crashed backend puts the server into recovery for a moment; wait it out.
	var err error
	for attempt := 0; attempt < 60; attempt++ {
		var conn *pgx.Conn
		conn, err = pgx.Connect(ctx, dsn)
		if err == nil {
			o.conn = conn
			o.dsn = dsn
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("connect %s: %w", database, err)
}
