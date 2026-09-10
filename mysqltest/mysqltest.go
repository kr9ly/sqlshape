// Package mysqltest starts a real MySQL server loaded with the application's schema.sql, for
// the application's tests: the counterpart of pgtest for MySQL. It runs the `mysqld` on
// PATH (`nix-shell -p mysql84`, a distribution package, or a server tarball's bin/) and
// returns ErrNoServer when there is none, so a test can skip.
//
// The first Start initializes a data directory once per server version under
// ~/.cache/sqlshape/mysqld-<version>/template (--initialize-insecure, a few seconds); every
// Start copies it to a temporary directory and runs mysqld there on a unix socket, with
// networking off, then loads the schema into a database named sqlshape. Close stops the
// server and removes the directory.
package mysqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ErrNoServer is returned by Start when no mysqld is on PATH; tests skip on it.
var ErrNoServer = errors.New("mysqltest: no mysqld on PATH")

// Database is the name of the database the schema is loaded into.
const Database = "sqlshape"

// DB is a running mysqld with the schema loaded.
type DB struct {
	Version string // the server's version, as SELECT VERSION() reports it
	db      *sql.DB
	cfg     *mysql.Config
	cmd     *exec.Cmd
	dir     string
}

// Start boots a mysqld and loads schemaSQL into it. schemaSQL may hold any number of
// statements; it is sent as one multi-statement script, so a compound statement (a trigger
// body with `;` inside) needs no DELIMITER.
func Start(ctx context.Context, schemaSQL string) (*DB, error) {
	mysqld, err := exec.LookPath("mysqld")
	if err != nil {
		return nil, ErrNoServer
	}
	version, err := serverVersion(ctx, mysqld)
	if err != nil {
		return nil, err
	}
	template, err := ensureTemplate(ctx, mysqld, version)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sqlshape-mysqld-")
	if err != nil {
		return nil, err
	}
	data := filepath.Join(dir, "data")
	if err := copyTree(template, data); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	sock := filepath.Join(dir, "mysql.sock")
	cmd := exec.CommandContext(ctx, mysqld, "--no-defaults", "--datadir="+data, "--socket="+sock,
		"--skip-networking", "--mysqlx=OFF", "--pid-file="+filepath.Join(dir, "mysqld.pid"),
		"--log-error="+filepath.Join(dir, "error.log"), "--secure-file-priv=", "--skip-log-bin",
		"--innodb-buffer-pool-size=32M", "--innodb-redo-log-capacity=8M", "--performance-schema=OFF")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	d := &DB{Version: version, cmd: cmd, dir: dir}
	d.cfg = mysql.NewConfig()
	d.cfg.User, d.cfg.Net, d.cfg.Addr = "root", "unix", sock
	d.cfg.ParseTime = true
	d.cfg.MultiStatements = true
	conn, err := mysql.NewConnector(d.cfg)
	if err != nil {
		d.Close()
		return nil, err
	}
	d.db = sql.OpenDB(conn)
	if err := d.wait(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("mysqltest: mysqld did not come up: %w (see %s)", err, filepath.Join(dir, "error.log"))
	}
	if err := d.load(ctx, schemaSQL); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// wait pings until the socket answers.
func (d *DB) wait(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := d.db.PingContext(ctx)
		if err == nil {
			return nil
		}
		if d.cmd.ProcessState != nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// load creates the database and applies the schema.
func (d *DB) load(ctx context.Context, schemaSQL string) error {
	if _, err := d.db.ExecContext(ctx, "CREATE DATABASE "+Database); err != nil {
		return err
	}
	d.cfg.DBName = Database
	conn, err := mysql.NewConnector(d.cfg)
	if err != nil {
		return err
	}
	d.db.Close()
	d.db = sql.OpenDB(conn)
	if text := strings.TrimSpace(schemaSQL); text != "" {
		if _, err := d.db.ExecContext(ctx, text); err != nil {
			return fmt.Errorf("mysqltest: schema: %w", err)
		}
	}
	return d.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&d.Version)
}

// Conn is a database/sql handle on the server, connected to the schema's database.
func (d *DB) Conn() *sql.DB { return d.db }

// DSN is a go-sql-driver/mysql DSN for the schema's database (parseTime=true).
func (d *DB) DSN() string {
	cfg := d.cfg.Clone()
	cfg.MultiStatements = false
	return cfg.FormatDSN()
}

// Close stops the server and removes its directory.
func (d *DB) Close() {
	if d.db != nil {
		d.db.Close()
	}
	if d.cmd != nil && d.cmd.Process != nil {
		d.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { d.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			d.cmd.Process.Kill()
			<-done
		}
	}
	if d.dir != "" {
		os.RemoveAll(d.dir)
	}
}

var reVersion = regexp.MustCompile(`Ver (\d+\.\d+\.\d+)`)

func serverVersion(ctx context.Context, mysqld string) (string, error) {
	out, err := exec.CommandContext(ctx, mysqld, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("mysqltest: mysqld --version: %w", err)
	}
	m := reVersion.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("mysqltest: mysqld --version: no version in %q", strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

// ensureTemplate initializes the per-version data directory once, under a lock against
// another process doing the same.
func ensureTemplate(ctx context.Context, mysqld, version string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(cache, "sqlshape", "mysqld-"+version)
	template := filepath.Join(base, "template")
	if _, err := os.Stat(filepath.Join(template, "mysql.ibd")); err == nil {
		return template, nil
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	unlock, err := lock(filepath.Join(base, "init.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(template, "mysql.ibd")); err == nil {
		return template, nil // another process finished first
	}
	tmp := filepath.Join(base, "template.tmp")
	os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, mysqld, "--no-defaults", "--initialize-insecure", "--datadir="+tmp,
		"--log-error="+filepath.Join(base, "initialize.log"), "--innodb-redo-log-capacity=8M")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mysqltest: mysqld --initialize-insecure: %w\n%s", err, out)
	}
	if err := os.Rename(tmp, template); err != nil {
		return "", err
	}
	return template, nil
}

// copyTree copies the template data directory (regular files and directories only).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
