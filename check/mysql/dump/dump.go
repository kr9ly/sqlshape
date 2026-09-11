// Package dump reads a MySQL database back into a schema.Schema by way of the server's own
// rendering of it: `SHOW CREATE TABLE` and `SHOW CREATE VIEW` for every table and view of
// the database, parsed by the same loader that reads schema.sql. A schema text is given its
// canonical form by applying it to a scratch database and reading it back the same way, so
// both sides of a comparison are what MySQL stores (types in their canonical spelling,
// defaults quoted as the server quotes them, generated constraint names, explicit charset
// and collation), not the spelling of schema.sql.
//
// The scratch database is, when a live server is at hand, a database created on that server
// (`sqlshape_scratch_<random>`, dropped when done): it is normalized by the very server the
// migration targets, version and settings included. Without a server (two schema texts, the
// tests) it is a mysqld started from PATH with the schema's declared settings (mysqltest).
package dump

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// Canonicalizer gives schema text its canonical form: the schema the server holds after
// applying the text, as the loader reads it back, and the text it was read from.
type Canonicalizer interface {
	Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error)
}

// Header is the schema text's declarations (`-- sqlshape: mysql 8.4` and the
// `-- sqlshape: server` lines), which a text read back from a server needs in front of it
// to load the way the declaring text does.
func Header(schemaSQL string) string {
	var b strings.Builder
	if d, err := dialect.Declared(schemaSQL); err == nil && d.Name != "" {
		fmt.Fprintf(&b, "-- sqlshape: %s %s\n", d.Name, d.Version)
	} else {
		b.WriteString("-- sqlshape: mysql 8.4\n")
	}
	if settings, err := dialect.Settings(schemaSQL); err == nil {
		for _, s := range settings {
			fmt.Fprintf(&b, "-- sqlshape: server %s = '%s'\n", s.Name, strings.ReplaceAll(s.Value, "'", "''"))
		}
	}
	return b.String()
}

// Querier runs queries: *sql.DB and *sql.Conn.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Read renders the database db is connected to as DDL text: SET FOREIGN_KEY_CHECKS=0, the
// tables in name order (their AUTO_INCREMENT counters left out), then the views in
// dependency order (their DEFINER left out). The text is what Load parses.
func Read(ctx context.Context, db Querier) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME")
	if err != nil {
		return "", err
	}
	var tables, views []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			rows.Close()
			return "", err
		}
		if kind == "VIEW" {
			views = append(views, name)
		} else {
			tables = append(tables, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("SET FOREIGN_KEY_CHECKS=0;\n")
	for _, t := range tables {
		var name, ddl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE `"+t+"`").Scan(&name, &ddl); err != nil {
			return "", fmt.Errorf("SHOW CREATE TABLE %s: %w", t, err)
		}
		b.WriteString(normalizeTable(ddl))
		b.WriteString(";\n")
	}
	defs := map[string]string{}
	for _, v := range views {
		var name, ddl, cs, cl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE VIEW `"+v+"`").Scan(&name, &ddl, &cs, &cl); err != nil {
			return "", fmt.Errorf("SHOW CREATE VIEW %s: %w", v, err)
		}
		defs[v] = normalizeView(ddl)
	}
	for _, v := range viewOrder(views, defs) {
		b.WriteString(defs[v])
		b.WriteString(";\n")
	}
	return b.String(), nil
}

var (
	autoIncrement = regexp.MustCompile(` AUTO_INCREMENT=\d+`)
	definer       = regexp.MustCompile(` DEFINER=\x60[^\x60]*\x60@\x60[^\x60]*\x60`)
)

// normalizeTable drops the AUTO_INCREMENT counter from the table options: it is data, not
// schema.
func normalizeTable(ddl string) string {
	if i := strings.LastIndex(ddl, "\n)"); i >= 0 {
		return ddl[:i] + autoIncrement.ReplaceAllString(ddl[i:], "")
	}
	return ddl
}

// normalizeView drops the DEFINER, which names the user that created the view on that
// server.
func normalizeView(ddl string) string { return definer.ReplaceAllString(ddl, "") }

// viewOrder sorts views so that a view comes after the views its text names.
func viewOrder(views []string, defs map[string]string) []string {
	isView := map[string]bool{}
	for _, v := range views {
		isView[v] = true
	}
	deps := map[string][]string{}
	for _, v := range views {
		for _, m := range regexp.MustCompile("\x60([^\x60]+)\x60").FindAllStringSubmatch(defs[v], -1) {
			if m[1] != v && isView[m[1]] {
				deps[v] = append(deps[v], m[1])
			}
		}
	}
	var out []string
	done := map[string]bool{}
	var visit func(v string, stack map[string]bool)
	visit = func(v string, stack map[string]bool) {
		if done[v] || stack[v] {
			return
		}
		stack[v] = true
		sort.Strings(deps[v])
		for _, d := range deps[v] {
			visit(d, stack)
		}
		done[v] = true
		out = append(out, v)
	}
	for _, v := range views {
		visit(v, map[string]bool{})
	}
	return out
}

// Load reads the database at dsn into a Schema, and returns the text it was loaded from.
// header is the declaring schema's declarations (Header), so the live side loads under the
// same version and settings; the server's lower_case_table_names must agree with them.
func Load(ctx context.Context, dsn, header string) (*schema.Schema, string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, "", err
	}
	db, err := open(cfg)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	if err := checkSettings(ctx, db, header); err != nil {
		return nil, "", err
	}
	text, err := Read(ctx, db)
	if err != nil {
		return nil, "", err
	}
	text = header + text
	s, err := schema.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load dump: %w", err)
	}
	return s, text, nil
}

// ServerVersion asks the database at dsn which MySQL it runs ("8.4.6").
func ServerVersion(ctx context.Context, dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	db, err := open(cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	err = db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v)
	return v, err
}

// checkSettings refuses a server whose lower_case_table_names is not the declared one: the
// value is fixed when the server is initialized, so the names the two sides compare would
// never agree.
func checkSettings(ctx context.Context, db Querier, header string) error {
	want := 0
	if settings, err := dialect.Settings(header); err == nil {
		for _, s := range settings {
			if s.Name == "lower_case_table_names" {
				want, _ = strconv.Atoi(s.Value)
			}
		}
	}
	var got int
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.lower_case_table_names").Scan(&got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the server runs with lower_case_table_names = %d but schema.sql declares %d: declare `-- sqlshape: server lower_case_table_names = %d` if the server is the one meant", got, want, got)
	}
	return nil
}

// sqlMode is the declared sql_mode, "" when the header declares none.
func sqlMode(header string) (string, bool) {
	if settings, err := dialect.Settings(header); err == nil {
		for _, s := range settings {
			if s.Name == "sql_mode" {
				return s.Value, true
			}
		}
	}
	return "", false
}

func open(cfg *mysql.Config) (*sql.DB, error) {
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// Scratch canonicalizes on a live server: each Canonical creates a database of its own
// there, applies the text under the declared sql_mode, reads it back and drops the
// database. It needs the CREATE and DROP DATABASE privileges.
type Scratch struct {
	cfg *mysql.Config
}

// NewScratch prepares canonicalization on the server at dsn.
func NewScratch(dsn string) (*Scratch, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &Scratch{cfg: cfg}, nil
}

// Canonical applies schemaSQL to a fresh scratch database on the server and reads it back.
func (s *Scratch) Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error) {
	var buf [6]byte
	rand.Read(buf[:])
	name := "sqlshape_scratch_" + hex.EncodeToString(buf[:])
	admin, err := open(s.cfg)
	if err != nil {
		return nil, "", err
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		return nil, "", fmt.Errorf("scratch database: %w", err)
	}
	defer admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
	cfg := s.cfg.Clone()
	cfg.DBName = name
	cfg.MultiStatements = true
	db, err := open(cfg)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	return canonicalOn(ctx, db, schemaSQL, Header(schemaSQL))
}

// canonicalOn applies schemaSQL to the database db is connected to (one connection, so the
// session settings hold) and reads it back.
func canonicalOn(ctx context.Context, db *sql.DB, schemaSQL, header string) (*schema.Schema, string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close()
	if mode, ok := sqlMode(header); ok {
		if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = '"+strings.ReplaceAll(mode, "'", "''")+"'"); err != nil {
			return nil, "", err
		}
	}
	if err := checkSettings(ctx, conn, header); err != nil {
		return nil, "", err
	}
	if text := strings.TrimSpace(schemaSQL); text != "" {
		if _, err := conn.ExecContext(ctx, text); err != nil {
			return nil, "", err
		}
	}
	text, err := Read(ctx, conn)
	if err != nil {
		return nil, "", err
	}
	text = header + text
	s, err := schema.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load canonical: %w", err)
	}
	return s, text, nil
}

// Local canonicalizes on a mysqld started from PATH for each text, with the text's
// declared settings (mysqltest). ErrNoServer when there is none.
type Local struct{}

// ErrNoServer is mysqltest's: no mysqld on PATH.
var ErrNoServer = mysqltest.ErrNoServer

// Canonical starts a server with schemaSQL loaded and reads it back.
func (Local) Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error) {
	srv, err := mysqltest.Start(ctx, schemaSQL)
	if err != nil {
		return nil, "", err
	}
	defer srv.Close()
	text, err := Read(ctx, srv.Conn())
	if err != nil {
		return nil, "", err
	}
	text = Header(schemaSQL) + text
	s, err := schema.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load canonical: %w", err)
	}
	return s, text, nil
}

// Problems lists what the loader could not apply of a schema text (a target must be
// clean: a statement the loader skipped would be missing from the canonical form too).
func Problems(schemaSQL string) error {
	s, err := schema.Load(schemaSQL)
	if err != nil {
		return err
	}
	if len(s.Problems) == 0 {
		return nil
	}
	var lines []string
	for _, p := range s.Problems {
		lines = append(lines, p.String())
	}
	return fmt.Errorf("schema has problems:\n  %s", strings.Join(lines, "\n  "))
}

// Schema is the loaded schema the canonicalizers and Load return: the MySQL loader's, named
// here so that a caller outside the module can hold and pass it (to diff.Compare,
// migrate.Plan).
type Schema = schema.Schema
