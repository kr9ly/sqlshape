// Package dump reads a live PostgreSQL database back into a schema.Schema by way of
// `pg_dump --schema-only`, and gives schema text a canonical form: the dump of a fresh
// PostgreSQL the text was applied to.
//
// The loader reads DDL as written; PostgreSQL stores it after analysis (explicit casts,
// `IN (...)` as `= ANY (ARRAY[...])`, qualified type names). Two schemas can only be
// compared object by object when both went through PostgreSQL, so diff and apply work on
// Canonical forms, and Load reads the live side the same way. The `-- sqlshape:` directives
// live in comments and do not survive the round trip: a canonical schema carries only what
// PostgreSQL holds.
//
// Seeded tables (schema.Relation.Seed, the rows schema.sql gives a lookup table) are
// data, which pg_dump --schema-only leaves out; both readers take the seed declarations of
// a schema and read those tables' declared columns back as INSERT statements appended to
// the dump, so content compares like the rest.
package dump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/pgparse"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Binary is the pg_dump executable: $SQLSHAPE_PG_DUMP, else "pg_dump" on PATH. The
// embedded PostgreSQL ships without client tools, so the caller's installation is used;
// its major version must be at least the server's.
func Binary() string {
	if p := os.Getenv("SQLSHAPE_PG_DUMP"); p != "" {
		return p
	}
	return "pg_dump"
}

// Run dumps the schema of the database at connString as SQL text (owners, privileges and
// tablespaces left out).
func Run(ctx context.Context, connString string) (string, error) {
	cmd := exec.CommandContext(ctx, Binary(), "--schema-only", "--no-owner", "--no-privileges", "--no-tablespaces", connString)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			ee = e
		}
		if ee != nil {
			return "", fmt.Errorf("%s: %w\n%s", Binary(), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", Binary(), err)
	}
	return string(out), nil
}

var setConfig = regexp.MustCompile(`(?m)^SELECT pg_catalog\.set_config\('search_path', '([^']*)', false\);$`)

// Normalize turns pg_dump output into text the loader parses: psql meta-commands
// (`\restrict` / `\unrestrict` since 17.6) go, and the search_path set through
// set_config becomes a SET.
func Normalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, `\`) {
			continue
		}
		b.WriteString(line)
	}
	return setConfig.ReplaceAllString(b.String(), "SET search_path = '$1';")
}

// Load reads the database at connString into a Schema, and returns the normalized dump
// text it was loaded from. seeds names the tables whose rows are part of the schema (the
// relations with a schema.Seed, typically the target's): their declared columns are read
// back as INSERT statements so the content compares like the rest; nil reads no rows.
func Load(ctx context.Context, connString string, seeds *schema.Schema) (*schema.Schema, string, error) {
	text, err := Run(ctx, connString)
	if err != nil {
		return nil, "", err
	}
	text = Normalize(text)
	if seeds != nil {
		conn, err := pgx.Connect(ctx, connString)
		if err != nil {
			return nil, "", err
		}
		defer conn.Close(ctx)
		data, err := readSeeds(ctx, conn, seeds)
		if err != nil {
			return nil, "", err
		}
		text += data
	}
	text = declare(versionOf(seeds, ""), text)
	s, err := analyze.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load dump: %w", err)
	}
	adoptSeeds(s, seeds)
	return s, text, nil
}

// ServerVersion asks the database at connString which PostgreSQL it runs ("17.5").
func ServerVersion(ctx context.Context, connString string) (string, error) {
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	var v string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// versionOf is the PostgreSQL version a canonical form is judged with: the seeds schema's
// when there is one, else the version schemaSQL declares, else the default.
func versionOf(seeds *schema.Schema, schemaSQL string) pgparse.Version {
	if seeds != nil {
		return seeds.Version.Or()
	}
	if v, err := schema.DeclaredVersion(schemaSQL); err == nil {
		return v
	}
	return pgparse.Default
}

// declare puts the version declaration above a pg_dump text, which writes none, so the
// loader reads it with the right grammar and catalog.
func declare(v pgparse.Version, text string) string {
	return fmt.Sprintf("-- sqlshape: postgres %d\n", int(v)) + text
}

// readSeeds renders the current rows of every seeded table of spec as INSERT statements
// over the declared columns (values as text literals, ordered by key). A table or column
// the database does not have yet contributes nothing.
func readSeeds(ctx context.Context, conn *pgx.Conn, spec *schema.Schema) (string, error) {
	var b strings.Builder
	for _, rel := range spec.Relations {
		sd := rel.Seed
		if sd == nil {
			continue
		}
		var sel, ord []string
		for _, c := range sd.Columns {
			sel = append(sel, q(c)+"::text")
		}
		for _, k := range sd.Key {
			ord = append(ord, q(k))
		}
		sql := fmt.Sprintf("SELECT %s FROM %s.%s ORDER BY %s", strings.Join(sel, ", "), q(rel.Schema), q(rel.Name), strings.Join(ord, ", "))
		rows, err := conn.Query(ctx, sql)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42703") {
				continue // undefined table / column: not there yet
			}
			return "", fmt.Errorf("read rows of %s: %w", rel.FullName(), err)
		}
		var lines []string
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return "", err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if v == nil {
					parts[i] = "NULL"
				} else {
					parts[i] = "'" + strings.ReplaceAll(v.(string), "'", "''") + "'"
				}
			}
			lines = append(lines, "  ("+strings.Join(parts, ", ")+")")
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
		if len(lines) == 0 {
			continue
		}
		cols := make([]string, len(sd.Columns))
		for i, c := range sd.Columns {
			cols[i] = q(c)
		}
		if sd.Additive {
			b.WriteString("\n-- sqlshape: seed")
		}
		fmt.Fprintf(&b, "\nINSERT INTO %s.%s (%s) VALUES\n%s;\n", q(rel.Schema), q(rel.Name), strings.Join(cols, ", "), strings.Join(lines, ",\n"))
	}
	return b.String(), nil
}

// adoptSeeds carries what the round trip loses from spec onto s: the seed directive.
func adoptSeeds(s, spec *schema.Schema) {
	if spec == nil {
		return
	}
	for _, rel := range spec.Relations {
		if rel.Seed == nil {
			continue
		}
		if r := s.Relation(rel.Schema, rel.Name); r != nil && r.Seed != nil {
			r.Seed.Additive = rel.Seed.Additive
		}
	}
}

func q(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// Canonicalizer gives schema text its canonical form. Server is the implementation;
// Canonical below is the one-shot convenience.
type Canonicalizer interface {
	Canonical(ctx context.Context, schemaSQL string, seeds *schema.Schema) (*schema.Schema, string, error)
}

// Canonical applies schemaSQL to a fresh PostgreSQL and reads it back. The returned text
// is the normalized dump the Schema was loaded from, followed by the rows of the seeded
// tables (see Load; nil takes the seeds schemaSQL itself declares). Each call boots its
// own server; for several canonical forms use one Server.
func Canonical(ctx context.Context, schemaSQL string, seeds *schema.Schema) (*schema.Schema, string, error) {
	srv, err := NewServer(ctx, versionOf(seeds, schemaSQL))
	if err != nil {
		return nil, "", err
	}
	defer srv.Close()
	return srv.Canonical(ctx, schemaSQL, seeds)
}

// Server is one running PostgreSQL that canonicalizes many schema texts, each in a
// database of its own (booting a server costs seconds, CREATE DATABASE milliseconds).
type Server struct {
	o  *oracle.Oracle
	mu sync.Mutex
	n  int
}

// NewServer boots the PostgreSQL of major version v. Close it when done.
func NewServer(ctx context.Context, v pgparse.Version) (*Server, error) {
	o, err := oracle.StartVersion(ctx, v, "")
	if err != nil {
		return nil, err
	}
	return &Server{o: o}, nil
}

// Close stops the server and removes its data.
func (s *Server) Close() error { return s.o.Close() }

// Canonical applies schemaSQL to a fresh database on the server and reads it back. Safe
// for concurrent use.
func (s *Server) Canonical(ctx context.Context, schemaSQL string, seeds *schema.Schema) (*schema.Schema, string, error) {
	if seeds == nil {
		raw, err := analyze.Load(schemaSQL)
		if err != nil {
			return nil, "", fmt.Errorf("load schema: %w", err)
		}
		seeds = raw
	}
	s.mu.Lock()
	s.n++
	name := fmt.Sprintf("canonical_%d", s.n)
	_, err := s.o.Conn().Exec(ctx, "CREATE DATABASE "+name)
	s.mu.Unlock()
	if err != nil {
		return nil, "", fmt.Errorf("create database: %w", err)
	}
	sess, err := s.o.Session(ctx, name)
	if err != nil {
		return nil, "", err
	}
	defer sess.Close()
	if strings.TrimSpace(schemaSQL) != "" {
		if _, err := sess.Conn().Exec(ctx, schemaSQL); err != nil {
			return nil, "", fmt.Errorf("apply schema: %w", err)
		}
	}
	text, err := Run(ctx, sess.ConnString())
	if err != nil {
		return nil, "", err
	}
	text = Normalize(text)
	data, err := readSeeds(ctx, sess.Conn(), seeds)
	if err != nil {
		return nil, "", err
	}
	text += data
	text = declare(s.o.Version(), text)
	sc, err := analyze.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load dump: %w", err)
	}
	adoptSeeds(sc, seeds)
	return sc, text, nil
}
