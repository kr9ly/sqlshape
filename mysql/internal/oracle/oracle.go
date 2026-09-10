// Package oracle runs a real mysqld and answers what MySQL itself says about a statement:
// the result columns' types and nullability, or the error it raises. It is the ground
// truth the analyzer's type rules are checked against, and a local tool only: it needs a
// `mysqld` on PATH (`nix-shell -p mysql84`, or a server tarball's bin/), and nothing at
// lint time or in CI depends on it.
//
// The first Start initializes a data directory once per server version under
// ~/.cache/sqlshape/mysqld-<version>/template (--initialize-insecure, a few seconds); every
// Start copies it to a temporary directory and runs mysqld there on a unix socket, with
// networking off, then loads the schema into a database of its own.
package oracle

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

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/mysql/internal/placeholder"
)

// ErrNoServer is returned by Start when no mysqld is on PATH; tests skip on it.
var ErrNoServer = errors.New("oracle: no mysqld on PATH")

// Oracle is a running mysqld with the schema loaded.
type Oracle struct {
	Version string // the server's version, as SELECT VERSION() reports it
	db      *sql.DB
	cmd     *exec.Cmd
	dir     string
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

// Start boots a mysqld and loads schemaSQL into it.
func Start(ctx context.Context, schemaSQL string) (*Oracle, error) {
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
	o := &Oracle{Version: version, cmd: cmd, dir: dir}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Net, cfg.Addr = "root", "unix", sock
	cfg.ParseTime = true
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		o.Close()
		return nil, err
	}
	o.db = sql.OpenDB(conn)
	if err := o.wait(ctx); err != nil {
		o.Close()
		return nil, fmt.Errorf("oracle: mysqld did not come up: %w (see %s)", err, filepath.Join(dir, "error.log"))
	}
	if err := o.load(ctx, schemaSQL); err != nil {
		o.Close()
		return nil, err
	}
	return o, nil
}

// wait pings until the socket answers.
func (o *Oracle) wait(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := o.db.PingContext(ctx)
		if err == nil {
			return nil
		}
		if o.cmd.ProcessState != nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// load creates the oracle's database and applies the schema statement by statement.
func (o *Oracle) load(ctx context.Context, schemaSQL string) error {
	if _, err := o.db.ExecContext(ctx, "CREATE DATABASE sqlshape"); err != nil {
		return err
	}
	if _, err := o.db.ExecContext(ctx, "USE sqlshape"); err != nil {
		return err
	}
	for _, st := range mysqlparse.Split(schemaSQL) {
		text := strings.TrimSpace(st.SQL)
		if text == "" {
			continue
		}
		if _, err := o.db.ExecContext(ctx, text); err != nil {
			return fmt.Errorf("oracle: schema: %w in %q", err, firstLine(text))
		}
	}
	if err := o.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&o.Version); err != nil {
		return err
	}
	return nil
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
func (o *Oracle) Close() {
	if o.db != nil {
		o.db.Close()
	}
	if o.cmd != nil && o.cmd.Process != nil {
		o.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { o.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			o.cmd.Process.Kill()
			<-done
		}
	}
	if o.dir != "" {
		os.RemoveAll(o.dir)
	}
}

var reVersion = regexp.MustCompile(`Ver (\d+\.\d+\.\d+)`)

func serverVersion(ctx context.Context, mysqld string) (string, error) {
	out, err := exec.CommandContext(ctx, mysqld, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("oracle: mysqld --version: %w", err)
	}
	m := reVersion.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("oracle: mysqld --version: no version in %q", strings.TrimSpace(string(out)))
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
		return "", fmt.Errorf("oracle: mysqld --initialize-insecure: %w\n%s", err, out)
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}
