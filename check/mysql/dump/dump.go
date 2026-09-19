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
// tables in name order (their AUTO_INCREMENT counters left out), then the stored procedures
// and functions in name order (a view may call a function, so routines come first), then
// the views in dependency order, then the triggers (their DEFINER left out throughout).
// Triggers on the same table, action time and event keep the order the server runs them in
// (information_schema.TRIGGERS' ACTION_ORDER), reproduced as an explicit FOLLOWS clause
// (SHOW CREATE TRIGGER's own text never has one, measured against mysqld: it always reads
// back as if the trigger were declared alone). The text is what Load parses.
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
	if err := readRoutines(ctx, db, &b); err != nil {
		return "", err
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
	if err := readTriggers(ctx, db, &b); err != nil {
		return "", err
	}
	if err := readEvents(ctx, db, &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

// readEvents renders every event of the database in name order, last: an event's body is
// bound late (the server checks nothing of it at CREATE time), so nothing depends on its
// position. SHOW CREATE EVENT spells the schedule out in full: a STARTS the source omitted
// or wrote as an expression comes back as the literal time the server computed when the
// event was created (measured), which pinEvents marks for diff to skip.
func readEvents(ctx context.Context, db Querier, b *strings.Builder) error {
	rows, err := db.QueryContext(ctx, "SELECT EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA = DATABASE() ORDER BY EVENT_NAME")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			// defensive: see readRoutines' own note.
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		// defensive: see readRoutines' own note.
		return err
	}
	for _, name := range names {
		// SHOW CREATE EVENT returns (Event, sql_mode, time_zone, Create Event,
		// character_set_client, collation_connection, Database Collation), measured.
		var ev, sqlMode, tz, ddl, cs, cl, dbCollation string
		q := "SHOW CREATE EVENT `" + name + "`"
		if err := db.QueryRowContext(ctx, q).Scan(&ev, &sqlMode, &tz, &ddl, &cs, &cl, &dbCollation); err != nil {
			// defensive: see readRoutines' own note (the event was just listed).
			return fmt.Errorf("%s: %w", q, err)
		}
		b.WriteString(normalizeRoutine(ddl))
		b.WriteString(";\n")
	}
	return nil
}

// pinEvents carries the source text's own knowledge of each event's schedule into its
// canonical form: whether AT / STARTS / ENDS were written as literal times. The canonical
// text (SHOW CREATE EVENT's) always spells them as literals, so on its own it cannot tell a
// time the schema fixed from one the server filled in at creation; the source can. An event
// the source does not have (it cannot happen: the canonical form came from the source) stays
// as read, every time literal.
func pinEvents(canon *schema.Schema, source string) {
	if len(canon.Events) == 0 {
		return
	}
	src, err := schema.Load(source)
	if err != nil {
		return
	}
	for _, e := range canon.Events {
		if se := src.Event(e.Name); se != nil {
			e.AtLiteral, e.StartsLiteral, e.EndsLiteral = se.AtLiteral, se.StartsLiteral, se.EndsLiteral
		}
	}
}

// pinAutoIncrement carries a table's own explicit `AUTO_INCREMENT=<n>` (the author's chosen
// starting value for a table's first creation, a schema decision) from the source text into
// the canonical schema: normalizeTable strips the counter from every canonicalized CREATE
// TABLE unconditionally (a live counter is data, not schema), so schema.Load(text) never sees
// it there. The source can still have it (schema.Table.Definition is the statement's own text
// as written), so it is read back here and kept on Table.AutoIncrementStart for a fresh CREATE
// TABLE (migrate.adds) to reproduce; diff.TableProps has no field for it, so this makes no
// difference to any comparison of a table that already exists on both sides.
func pinAutoIncrement(canon *schema.Schema, source string) {
	src, err := schema.Load(source)
	if err != nil {
		return
	}
	for _, t := range canon.Tables {
		if st := src.Table(t.Name); st != nil {
			t.AutoIncrementStart = st.AutoIncrementStart
		}
	}
}

// readRoutines renders every stored procedure and function of the database, in name (then
// kind, so a PROCEDURE and a FUNCTION of the same name sort deterministically) order.
func readRoutines(ctx context.Context, db Querier, b *strings.Builder) error {
	rows, err := db.QueryContext(ctx, "SELECT ROUTINE_NAME, ROUTINE_TYPE FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE() ORDER BY ROUTINE_NAME, ROUTINE_TYPE")
	if err != nil {
		return err
	}
	type routine struct{ name, kind string }
	var list []routine
	for rows.Next() {
		var r routine
		if err := rows.Scan(&r.name, &r.kind); err != nil {
			// defensive: information_schema.ROUTINES' own two columns always scan into
			// two strings; only a genuine connection failure reaches this in practice.
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		// defensive: a driver-level failure partway through the result set (a dropped
		// connection); not deterministically reproducible without one.
		return err
	}
	for _, r := range list {
		// SHOW CREATE PROCEDURE / FUNCTION both return (name, sql_mode, create text,
		// character_set_client, collation_connection, Database Collation), measured
		// against mysqld.
		var name, sqlMode, ddl, cs, cl, dbCollation string
		q := "SHOW CREATE FUNCTION `" + r.name + "`"
		if r.kind == "PROCEDURE" {
			q = "SHOW CREATE PROCEDURE `" + r.name + "`"
		}
		if err := db.QueryRowContext(ctx, q).Scan(&name, &sqlMode, &ddl, &cs, &cl, &dbCollation); err != nil {
			// defensive: the routine was just listed by information_schema.ROUTINES; only
			// a concurrent DROP between the two queries, or a connection failure, reaches
			// this (not deterministically reproducible without one).
			return fmt.Errorf("%s: %w", q, err)
		}
		b.WriteString(normalizeRoutine(ddl))
		b.WriteString(";\n")
	}
	return nil
}

// readTriggers renders every trigger of the database, grouped by its table, action time and
// event in the server's own firing order (information_schema.TRIGGERS' ACTION_ORDER), a
// FOLLOWS clause added to every trigger after the first of its group so the order survives a
// reload.
func readTriggers(ctx context.Context, db Querier, b *strings.Builder) error {
	rows, err := db.QueryContext(ctx, "SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = DATABASE() ORDER BY EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION, ACTION_ORDER")
	if err != nil {
		return err
	}
	type trig struct{ name, table, timing, event string }
	var list []trig
	for rows.Next() {
		var t trig
		if err := rows.Scan(&t.name, &t.table, &t.timing, &t.event); err != nil {
			// defensive: see readRoutines' own note.
			rows.Close()
			return err
		}
		list = append(list, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		// defensive: see readRoutines' own note.
		return err
	}
	last := map[string]string{}
	for _, t := range list {
		key := t.table + "|" + t.timing + "|" + t.event
		follows := last[key]
		// SHOW CREATE TRIGGER returns (name, sql_mode, SQL Original Statement,
		// character_set_client, collation_connection, Database Collation, Created), one
		// more column than SHOW CREATE PROCEDURE/FUNCTION, measured against mysqld.
		var name, sqlMode, ddl, cs, cl, dbCollation, created string
		q := "SHOW CREATE TRIGGER `" + t.name + "`"
		if err := db.QueryRowContext(ctx, q).Scan(&name, &sqlMode, &ddl, &cs, &cl, &dbCollation, &created); err != nil {
			// defensive: see readRoutines' own note (the trigger was just listed by
			// information_schema.TRIGGERS).
			return fmt.Errorf("%s: %w", q, err)
		}
		b.WriteString(normalizeTrigger(ddl, follows))
		b.WriteString(";\n")
		last[key] = t.name
	}
	return nil
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

// normalizeRoutine drops the DEFINER of a CREATE PROCEDURE / CREATE FUNCTION.
func normalizeRoutine(ddl string) string {
	return dropTrailingSemicolon(definer.ReplaceAllString(ddl, ""))
}

var forEachRow = regexp.MustCompile(`FOR EACH ROW`)

// dropTrailingSemicolon removes one trailing ';' (Read appends its own): a trigger or
// routine whose body is a single simple statement, not a BEGIN ... END block, ends its SHOW
// CREATE text with that statement's own ';' (measured against mysqld: a BEGIN ... END body
// does not, its text ends at END), which would otherwise double up.
func dropTrailingSemicolon(ddl string) string {
	return strings.TrimSuffix(strings.TrimRight(ddl, " \t\r\n"), ";")
}

// normalizeTrigger drops the DEFINER, and inserts a FOLLOWS clause naming follows (the
// trigger the server runs just before this one, for its table/action time/event) when
// follows is not "": SHOW CREATE TRIGGER's own text never carries FOLLOWS/PRECEDES, so a
// reload would otherwise put every trigger of a group last (measured against mysqld).
func normalizeTrigger(ddl, follows string) string {
	ddl = dropTrailingSemicolon(definer.ReplaceAllString(ddl, ""))
	if follows == "" {
		return ddl
	}
	loc := forEachRow.FindStringIndex(ddl)
	if loc == nil {
		// defensive: SHOW CREATE TRIGGER's own text always has "FOR EACH ROW" (the only
		// row-level trigger MySQL has); follows is only ever "" when there is nothing to
		// insert anyway, but this guards a change in the server's own wording.
		return ddl
	}
	return ddl[:loc[1]] + " FOLLOWS `" + strings.ReplaceAll(follows, "`", "``") + "`" + ddl[loc[1]:]
}

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
	pinEvents(s, schemaSQL)
	pinAutoIncrement(s, schemaSQL)
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
	pinEvents(s, schemaSQL)
	pinAutoIncrement(s, schemaSQL)
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
