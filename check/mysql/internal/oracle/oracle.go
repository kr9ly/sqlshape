// Package oracle runs a real mysqld and answers what MySQL itself says about a statement:
// the result columns' types and nullability, or the error it raises. It is the ground
// truth the analyzer's type rules are checked against, and a local tool only: it needs a
// `mysqld` on PATH (`nix-shell -p mysql84`, or a server tarball's bin/), and nothing at
// lint time or in CI depends on it. The server is the one mysqltest boots.
package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/placeholder"
)

// ErrNoServer is returned by Start when no mysqld is on PATH; tests skip on it.
var ErrNoServer = mysqltest.ErrNoServer

// Oracle is a running mysqld with the schema loaded.
type Oracle struct {
	Version string // the server's version, as SELECT VERSION() reports it
	server  *mysqltest.DB
	db      *sql.DB
}

// Column is one result column as the server describes it in the result set metadata.
type Column struct {
	Name     string
	Type     string // the driver's spelling of the wire type: BIGINT, UNSIGNED BIGINT, DECIMAL, VARCHAR, VARBINARY, TEXT, DATETIME, JSON ...
	Nullable bool
	Length   int64 // for string types; 0 when the driver does not report one
	Prec     int64 // for DECIMAL
	Scale    int64
}

// Description is what the server says about a statement.
type Description struct {
	Columns []Column
}

// Error is a statement the server rejects.
type Error struct {
	Number  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (MySQL error %d)", e.Message, e.Number) }

// Start boots a mysqld (mysqltest.Start) and loads schemaSQL into it.
func Start(ctx context.Context, schemaSQL string) (*Oracle, error) {
	server, err := mysqltest.Start(ctx, schemaSQL)
	if err != nil {
		return nil, err
	}
	return &Oracle{Version: server.Version, server: server, db: server.Conn()}, nil
}

// Describe asks the server about sql, whose placeholders are `$n`. A SELECT runs with NULL
// for every parameter (no row need come back: the metadata does); any other statement is
// prepared only, so nothing is written. A statement the server rejects returns *Error.
func (o *Oracle) Describe(ctx context.Context, sql string) (*Description, error) {
	text, pm := placeholder.Rewrite(sql)
	args := make([]any, pm.Marks()) // one NULL per `?`, the same $n written twice included
	d := &Description{}
	if !isQuery(text) {
		stmt, err := o.db.PrepareContext(ctx, text)
		if err != nil {
			return nil, wrap(err)
		}
		stmt.Close()
		return d, nil
	}
	rows, err := o.db.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	for _, ct := range cts {
		c := Column{Name: ct.Name(), Type: ct.DatabaseTypeName()}
		if n, ok := ct.Nullable(); ok {
			c.Nullable = n
		}
		if l, ok := ct.Length(); ok {
			c.Length = l
		}
		if p, s, ok := ct.DecimalSize(); ok {
			c.Prec, c.Scale = p, s
		}
		d.Columns = append(d.Columns, c)
	}
	return d, nil
}

var reLeading = regexp.MustCompile(`^(?:\s|/\*.*?\*/|--[^\n]*\n|#[^\n]*\n)*`)

// isQuery reports a statement that returns rows: SELECT, TABLE, VALUES, WITH, or one in parentheses.
func isQuery(text string) bool {
	rest := strings.ToUpper(strings.TrimLeft(reLeading.ReplaceAllString(text, ""), "("))
	for _, kw := range []string{"SELECT", "TABLE", "VALUES", "WITH"} {
		if strings.HasPrefix(rest, kw) {
			return true
		}
	}
	return false
}

func wrap(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return &Error{Number: int(me.Number), Message: me.Message}
	}
	return err
}

// Close stops the server and removes its directory.
func (o *Oracle) Close() { o.server.Close() }
