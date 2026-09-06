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
package dump

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/oracle"
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

// Load reads the database at connString into a Schema.
func Load(ctx context.Context, connString string) (*schema.Schema, error) {
	text, err := Run(ctx, connString)
	if err != nil {
		return nil, err
	}
	s, err := analyze.Load(Normalize(text))
	if err != nil {
		return nil, fmt.Errorf("load dump: %w", err)
	}
	return s, nil
}

// Canonicalizer gives schema text its canonical form. Server is the implementation;
// Canonical below is the one-shot convenience.
type Canonicalizer interface {
	Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error)
}

// Canonical applies schemaSQL to a fresh PostgreSQL and reads it back. The returned text
// is the normalized dump the Schema was loaded from. Each call boots its own server;
// for several canonical forms use one Server.
func Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error) {
	srv, err := NewServer(ctx)
	if err != nil {
		return nil, "", err
	}
	defer srv.Close()
	return srv.Canonical(ctx, schemaSQL)
}

// Server is one running PostgreSQL that canonicalizes many schema texts, each in a
// database of its own (booting a server costs seconds, CREATE DATABASE milliseconds).
type Server struct {
	o  *oracle.Oracle
	mu sync.Mutex
	n  int
}

// NewServer boots the PostgreSQL. Close it when done.
func NewServer(ctx context.Context) (*Server, error) {
	o, err := oracle.Start(ctx, "")
	if err != nil {
		return nil, err
	}
	return &Server{o: o}, nil
}

// Close stops the server and removes its data.
func (s *Server) Close() error { return s.o.Close() }

// Canonical applies schemaSQL to a fresh database on the server and reads it back. Safe
// for concurrent use.
func (s *Server) Canonical(ctx context.Context, schemaSQL string) (*schema.Schema, string, error) {
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
	sc, err := analyze.Load(text)
	if err != nil {
		return nil, "", fmt.Errorf("load dump: %w", err)
	}
	return sc, text, nil
}
