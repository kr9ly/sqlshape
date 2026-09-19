package dump

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
)

func TestNormalize(t *testing.T) {
	in := "--\n\\restrict abc\nSET a = 1;\nSELECT pg_catalog.set_config('search_path', '', false);\nCREATE TABLE t (id int);\n\\unrestrict abc\n"
	want := "--\nSET a = 1;\nSET search_path = '';\nCREATE TABLE t (id int);\n"
	if got := Normalize(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBinary covers Binary's env-var override and its "pg_dump" fallback.
func TestBinary(t *testing.T) {
	t.Setenv("SQLSHAPE_PG_DUMP", "")
	if got, want := Binary(), "pg_dump"; got != want {
		t.Errorf("Binary() = %q, want %q", got, want)
	}
	t.Setenv("SQLSHAPE_PG_DUMP", "/opt/custom/pg_dump")
	if got, want := Binary(), "/opt/custom/pg_dump"; got != want {
		t.Errorf("Binary() = %q, want %q", got, want)
	}
}

// TestRunErrors covers Run's two error branches: the process never starting at all
// (exec.CommandContext's own error, not an *exec.ExitError - Binary itself doesn't
// exist), and the process starting but exiting nonzero (an *exec.ExitError, whose
// stderr is folded into the message).
func TestRunErrors(t *testing.T) {
	t.Run("binary not found", func(t *testing.T) {
		t.Setenv("SQLSHAPE_PG_DUMP", "/no/such/pg_dump_binary_xyz")
		_, err := Run(context.Background(), "postgres://ignored")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("a start failure should have no stderr suffix: %v", err)
		}
	})
	t.Run("process exits nonzero", func(t *testing.T) {
		requirePgDump(t)
		_, err := Run(context.Background(), "postgres://nosuchhost.invalid:1/nope")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), Binary()+":") {
			t.Errorf("error should be prefixed with the binary name: %v", err)
		}
	})
}

func requirePgDump(t *testing.T) {
	if _, err := exec.LookPath(Binary()); err != nil {
		t.Skipf("%s not found: %v", Binary(), err)
	}
}

// The examples' schema.sql each round-trip through PostgreSQL: the dump loads without
// problems, and a second round trip of the dump is a fixed point (no changes).
func TestCanonicalFixedPoint(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	srv, err := NewServer(ctx, pgparse.Default)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, ex := range []string{"1-tables", "2-views", "3-database-api", "4-everything"} {
		t.Run(ex, func(t *testing.T) {
			sql, err := os.ReadFile(filepath.Join("../../../examples", ex, "schema.sql"))
			if err != nil {
				t.Fatal(err)
			}
			s1, text, err := srv.Canonical(ctx, string(sql), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range s1.Problems {
				t.Errorf("problem loading dump: %s", p)
			}
			s2, _, err := srv.Canonical(ctx, text, nil)
			if err != nil {
				t.Fatalf("re-applying the dump: %v", err)
			}
			if changes := diff.Compare(s1, s2); len(changes) > 0 {
				var lines []string
				for _, c := range changes {
					lines = append(lines, c.String())
				}
				t.Errorf("dump is not a fixed point:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// Edits to a schema show up as changes between the two canonical forms, in PostgreSQL's
// spelling of the expressions.
func TestCanonicalChanges(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	base, err := os.ReadFile("../../../examples/1-tables/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	edit := string(base) + `
ALTER TABLE customers ADD COLUMN nickname text DEFAULT 'anon';
ALTER TABLE orders ALTER COLUMN total SET DEFAULT 1;
CREATE INDEX orders_status_idx ON orders (status) WHERE status <> 'paid';
INSERT INTO order_statuses (code, label, sort_order) VALUES ('refunded', 'Refunded', 50);
COMMENT ON TABLE orders IS 'orders placed';
`
	from, _, err := Canonical(ctx, string(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := Canonical(ctx, edit, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range diff.Compare(from, to) {
		lines = append(lines, c.String())
	}
	got := strings.Join(lines, "\n")
	want := strings.TrimSpace(`
~ table customers
    column order: id, email, name, created_at -> id, email, name, created_at, nickname
+ column customers.nickname
~ rows order_statuses
    row 'refunded':  -> 'refunded', 'Refunded', '50'
~ column orders.total
    default: 0 -> 1
+ index orders.orders_status_idx
+ comment orders
`)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// newSession boots a fresh Server and a database on it, applying schemaSQL, returning
// its own connString for direct Load calls (as opposed to Server.Canonical, which always
// creates its own fresh database from scratch).
func newSession(t *testing.T, schemaSQL string) (srv *Server, connString string) {
	t.Helper()
	ctx := context.Background()
	srv, err := NewServer(ctx, pgparse.Default)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	name := "loadtest"
	if _, err := srv.o.Conn().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	sess, err := srv.o.Session(ctx, name)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if strings.TrimSpace(schemaSQL) != "" {
		if _, err := sess.Conn().Exec(ctx, schemaSQL); err != nil {
			t.Fatalf("apply schema: %v", err)
		}
	}
	return srv, sess.ConnString()
}

// TestLoad covers Load end to end: without seeds (the plain dump/analyze round trip),
// with seeds (its rows folded in as INSERT statements), and its propagation of a Run
// failure for a bad connString.
func TestLoad(t *testing.T) {
	requirePgDump(t)
	t.Run("no seeds", func(t *testing.T) {
		_, connString := newSession(t, "CREATE TABLE t (id int, name text);")
		s, text, err := Load(context.Background(), connString, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if s.Relation("", "t") == nil {
			t.Errorf("loaded schema missing table t:\n%s", text)
		}
	})
	t.Run("with seeds", func(t *testing.T) {
		schemaSQL := "CREATE TABLE statuses (code text PRIMARY KEY, label text NOT NULL);\nINSERT INTO statuses (code, label) VALUES ('a', 'Alpha'), ('b', 'Beta');\n"
		_, connString := newSession(t, schemaSQL)
		seeds, err := analyze.Load(schemaSQL)
		if err != nil {
			t.Fatal(err)
		}
		s, text, err := Load(context.Background(), connString, seeds)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !strings.Contains(text, "INSERT INTO") {
			t.Errorf("expected the seed rows folded into the dump text, got:\n%s", text)
		}
		r := s.Relation("", "statuses")
		if r == nil || r.Seed == nil {
			t.Errorf("loaded schema's statuses table should carry a Seed directive")
		}
	})
	t.Run("bad connString propagates Run's error", func(t *testing.T) {
		_, _, err := Load(context.Background(), "postgres://nosuchhost.invalid:1/nope", nil)
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestServerCanonicalErrors covers Server.Canonical's own error branches: the target
// database already existing (a name collision on CREATE DATABASE), and schemaSQL that
// PostgreSQL itself refuses to apply.
func TestServerCanonicalErrors(t *testing.T) {
	requirePgDump(t)
	ctx := context.Background()
	srv, err := NewServer(ctx, pgparse.Default)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	t.Run("create database collision", func(t *testing.T) {
		// Server names its databases "canonical_<n>" from an internal, per-instance
		// counter starting at 1; pre-creating the name its very first call will pick
		// forces that CREATE DATABASE to fail.
		if _, err := srv.o.Conn().Exec(ctx, "CREATE DATABASE canonical_1"); err != nil {
			t.Fatalf("pre-create: %v", err)
		}
		_, _, err := srv.Canonical(ctx, "CREATE TABLE t (id int);", nil)
		if err == nil || !strings.Contains(err.Error(), "create database") {
			t.Errorf("got %v, want a create-database error", err)
		}
	})
	t.Run("schema PostgreSQL refuses", func(t *testing.T) {
		// this parses fine as far as sqlshape's own loader is concerned (it does not
		// check for a duplicate constraint name), so seeds is derived successfully and
		// the failure only shows up when PostgreSQL itself applies it.
		bad := "CREATE TABLE t (id int, CONSTRAINT dup CHECK (id > 0), CONSTRAINT dup CHECK (id < 100));"
		_, _, err := srv.Canonical(ctx, bad, nil)
		if err == nil || !strings.Contains(err.Error(), "apply schema") {
			t.Errorf("got %v, want an apply-schema error", err)
		}
	})
}

// TestCanonicalOneShotNewServerError covers the one-shot Canonical's propagation of a
// NewServer failure. Forcing embedded-postgres itself to fail to start is impractical
// (it depends on extracting/caching real PostgreSQL binaries), so this documents that
// branch rather than exercising it: see dump.go's `if err != nil { return nil, "", err }`
// right after `srv, err := NewServer(ctx, pgparse.Default)`.
func TestCanonicalOneShotNewServerError(t *testing.T) {
	t.Skip("forcing embedded-postgres to fail to start is impractical in a unit test; see comment")
}

// TestReadSeedsBranches covers readSeeds' remaining branches through Load: a seeded
// table that does not exist yet in the live database (skipped, not an error), a seeded
// table that exists but currently has none of its declared rows (skipped, no INSERT
// emitted), a NULL value in a seeded column, and an additive seed's leading directive
// comment.
func TestReadSeedsBranches(t *testing.T) {
	requirePgDump(t)
	// "gone" is declared seeded in spec but never created in the live database (42P01);
	// "empty" is created but has no rows at all; "statuses" is populated, additive, and
	// has a NULL in its optional "note" column.
	liveSQL := `
CREATE TABLE empty (code text PRIMARY KEY, label text NOT NULL);
CREATE TABLE statuses (code text PRIMARY KEY, label text NOT NULL, note text);
INSERT INTO statuses (code, label, note) VALUES ('a', 'Alpha', NULL);
`
	specSQL := `
CREATE TABLE gone (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO gone (code, label) VALUES ('x', 'X');
CREATE TABLE empty (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO empty (code, label) VALUES ('y', 'Y');
CREATE TABLE statuses (code text PRIMARY KEY, label text NOT NULL, note text);
-- sqlshape: seed
INSERT INTO statuses (code, label, note) VALUES ('a', 'Alpha', NULL);
`
	spec, err := analyze.Load(specSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, connString := newSession(t, liveSQL)
	_, text, err := Load(context.Background(), connString, spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(text, `"gone"`) {
		t.Errorf("a seeded table missing from the live database should be skipped entirely, got:\n%s", text)
	}
	if strings.Contains(text, `"empty"`) {
		t.Errorf("a seeded table with no matching rows should emit no INSERT, got:\n%s", text)
	}
	if !strings.Contains(text, "-- sqlshape: seed\nINSERT INTO") {
		t.Errorf("want the additive directive right before statuses' INSERT, got:\n%s", text)
	}
	if !strings.Contains(text, "'Alpha', NULL") {
		t.Errorf("want a NULL rendered for the note column, got:\n%s", text)
	}
}
