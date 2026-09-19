// Package mysqltest starts a real MySQL server loaded with a schema.sql: the counterpart of
// pgtest for MySQL, and like it sqlshape's own test tooling (the MySQL oracle, the examples and
// the conformance tests run on it) rather than part of the API an application is meant to use.
// It carries no compatibility promise. It runs the `mysqld` on PATH (`nix-shell -p mysql84`, a
// distribution package, or a server tarball's bin/) and returns ErrNoServer when there is none,
// so a test can skip.
//
// The first Start initializes a data directory once per server version under
// ~/.cache/sqlshape/mysqld-<version>/template (--initialize-insecure, a few seconds); every
// Start copies it to a temporary directory and runs mysqld there on a unix socket, with
// networking off, then loads the schema into a database named sqlshape. Close stops the
// server and removes the directory. A package whose TestMain runs its tests through Main
// boots one server per set of settings instead and hands it from test to test (Close drops
// the database): a start is over a second, a CREATE DATABASE milliseconds.
//
// The server runs as the schema declares it: every `-- sqlshape: server <variable> =
// <value>` line becomes a --<variable>=<value> option of mysqld (a variable mysqld does not
// know keeps it from starting), so the checker, the application's tests and the schema
// agree on sql_mode and lower_case_table_names. lower_case_table_names is fixed when the
// data directory is initialized, so a value other than 0 has a template of its own
// (mysqld-<version>-lctn<N>).
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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
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
	srv     *server
	owned   bool // Close stops the server; otherwise it hands the server back to the pool
}

// server is one mysqld process: its options, socket and data directory.
type server struct {
	key    string // the options it was started with, joined; two Starts share a server only when these agree
	cfg    *mysql.Config
	cmd    *exec.Cmd
	exited chan struct{} // closed when mysqld has exited (cmd.Wait returned)
	dir    string
	busy   bool // a DB is using it (the pool's, under pool.mu)
}

// pool is the process's shared servers, in effect after Main. A Start finds a free server
// started with the same options, or boots one and adds it; Close hands the server back with
// its database dropped. Servers never stop until Main's m.Run returns.
var pool struct {
	mu      sync.Mutex
	sharing bool
	servers []*server
}

// Main is m.Run with servers shared across the package's tests: from TestMain,
//
//	func TestMain(m *testing.M) { os.Exit(mysqltest.Main(m)) }
//
// Without it every Start boots a mysqld of its own (copying the template data directory
// and waiting for InnoDB, over a second), and Close stops it. Under Main the first Start of
// a given set of `-- sqlshape: server` settings boots one, and every later Start with the
// same settings takes it over once its previous user has Closed (Close drops the database,
// so the schema starts from nothing each time); a Start while that server is in use, or with
// other settings, boots another. Server-global state a test changes (SET GLOBAL, users) is
// visible to the tests after it, which is what the tests give up for the shared process.
// Every server stops, its directory removed, when m.Run returns.
func Main(m *testing.M) int {
	pool.mu.Lock()
	pool.sharing = true
	pool.mu.Unlock()
	code := m.Run()
	pool.mu.Lock()
	servers := pool.servers
	pool.servers, pool.sharing = nil, false
	pool.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
	return code
}

// Start boots a mysqld and loads schemaSQL into it. schemaSQL may hold any number of
// statements; it is sent as one multi-statement script, so a compound statement (a trigger
// body with `;` inside) needs no DELIMITER. Under Main (see there) the server may be one an
// earlier test already booted.
func Start(ctx context.Context, schemaSQL string) (*DB, error) { return start(ctx, schemaSQL, false) }

// StartOwn is Start on a server of its own even under Main: for a test that changes what a
// shared server would carry over to the next test (accounts, global variables, other
// databases). Close stops it.
func StartOwn(ctx context.Context, schemaSQL string) (*DB, error) { return start(ctx, schemaSQL, true) }

func start(ctx context.Context, schemaSQL string, own bool) (*DB, error) {
	mysqld, err := exec.LookPath("mysqld")
	if err != nil {
		return nil, ErrNoServer
	}
	version, err := serverVersion(ctx, mysqld)
	if err != nil {
		return nil, err
	}
	settings, err := dialect.Settings(schemaSQL)
	if err != nil {
		return nil, fmt.Errorf("mysqltest: %w", err)
	}
	var options []string
	lctn := "0"
	for _, s := range settings {
		options = append(options, "--"+strings.ReplaceAll(s.Name, "_", "-")+"="+s.Value)
		if s.Name == "lower_case_table_names" {
			lctn = s.Value
		}
	}
	srv, owned, err := acquire(ctx, mysqld, version, options, lctn, own)
	if err != nil {
		return nil, err
	}
	d := &DB{Version: version, srv: srv, owned: owned, cfg: srv.cfg.Clone()}
	if err := d.load(ctx, schemaSQL); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// acquire is a server started with options: a free one of the pool when sharing (owned
// false), else a new one (owned true when not sharing, or when the caller wants its own; a
// new shared server is added to the pool and marked busy).
func acquire(ctx context.Context, mysqld, version string, options []string, lctn string, own bool) (*server, bool, error) {
	key := strings.Join(options, "\x00")
	pool.mu.Lock()
	sharing := pool.sharing && !own
	if sharing {
		for _, s := range pool.servers {
			if s.key == key && !s.busy {
				s.busy = true
				pool.mu.Unlock()
				return s, false, nil
			}
		}
	}
	pool.mu.Unlock()
	// a shared server outlives the Start that booted it, so it is not tied to ctx (which the
	// caller cancels when its test ends)
	cmdCtx := ctx
	if sharing {
		cmdCtx = context.Background()
	}
	s, err := boot(ctx, cmdCtx, mysqld, version, options, lctn)
	if err != nil {
		return nil, false, err
	}
	s.key = key
	if !sharing {
		return s, true, nil
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if !pool.sharing { // Main returned meanwhile: nobody will stop it for us
		return s, true, nil
	}
	s.busy = true
	pool.servers = append(pool.servers, s)
	return s, false, nil
}

// boot copies the template data directory and starts mysqld on it, returning once the
// socket answers and lower_case_table_names is what the schema declared. ctx bounds the
// wait; cmdCtx bounds the process.
func boot(ctx, cmdCtx context.Context, mysqld, version string, options []string, lctn string) (*server, error) {
	template, err := ensureTemplate(ctx, mysqld, version, lctn)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(scratchRoot(), "sqlshape-mysqld-")
	if err != nil {
		return nil, err
	}
	data := filepath.Join(dir, "data")
	if err := copyTree(template, data); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	sock := filepath.Join(dir, "mysql.sock")
	cmd := exec.CommandContext(cmdCtx, mysqld, append([]string{"--no-defaults", "--datadir=" + data, "--socket=" + sock,
		"--skip-networking", "--mysqlx=OFF", "--pid-file=" + filepath.Join(dir, "mysqld.pid"),
		"--log-error=" + filepath.Join(dir, "error.log"), "--secure-file-priv=", "--skip-log-bin",
		"--innodb-buffer-pool-size=32M", "--innodb-redo-log-capacity=8M", "--performance-schema=OFF"}, options...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s := &server{cmd: cmd, dir: dir, exited: make(chan struct{})}
	go func() { cmd.Wait(); close(s.exited) }()
	s.cfg = mysql.NewConfig()
	s.cfg.User, s.cfg.Net, s.cfg.Addr = "root", "unix", sock
	s.cfg.ParseTime = true
	s.cfg.MultiStatements = true
	conn, err := mysql.NewConnector(s.cfg)
	if err != nil {
		s.stop()
		return nil, err
	}
	admin := sql.OpenDB(conn)
	defer admin.Close()
	if err := s.wait(ctx, admin); err != nil {
		log := errorLog(filepath.Join(dir, "error.log"))
		s.stop()
		return nil, fmt.Errorf("mysqltest: mysqld did not come up: %w%s", err, log)
	}
	// lower_case_table_names = 2 is only honored on a case-insensitive filesystem: on a
	// case-sensitive one mysqld warns and starts at 0 instead (measured), silently out of
	// step with what the schema declared. A caller loading a schema declaring 2 on such a
	// filesystem needs to know its tests are not actually exercising 2, not a server that
	// silently answers as 0.
	var gotLCTN string
	if err := admin.QueryRowContext(ctx, "SELECT @@GLOBAL.lower_case_table_names").Scan(&gotLCTN); err != nil {
		s.stop()
		return nil, fmt.Errorf("mysqltest: reading lower_case_table_names: %w", err)
	}
	if gotLCTN != lctn {
		s.stop()
		return nil, fmt.Errorf("mysqltest: schema declares lower_case_table_names = %s, but the running mysqld started with %s (2 needs a case-insensitive filesystem)", lctn, gotLCTN)
	}
	return s, nil
}

// wait pings until the socket answers, or mysqld has exited (an option it rejects, a data
// directory it cannot use), or a minute has passed.
func (s *server) wait(ctx context.Context, admin *sql.DB) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := admin.PingContext(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-s.exited:
			return fmt.Errorf("mysqld exited: %w", err)
		default:
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.exited:
			return fmt.Errorf("mysqld exited: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stop ends the process and removes its directory.
func (s *server) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(30 * time.Second):
			s.cmd.Process.Kill()
			<-s.exited
		}
	}
	if s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

// release drops the schema's database and marks the server free for the next Start. A
// server whose database will not drop (a connection still holding its tables) is stopped
// and taken out of the pool instead, so the next Start boots a clean one.
func (s *server) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := errors.New("mysqltest: no connector")
	if conn, cerr := mysql.NewConnector(s.cfg); cerr == nil {
		admin := sql.OpenDB(conn)
		_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+Database)
		admin.Close()
	}
	pool.mu.Lock()
	if err == nil {
		s.busy = false
		pool.mu.Unlock()
		return
	}
	for i, t := range pool.servers {
		if t == s {
			pool.servers = append(pool.servers[:i], pool.servers[i+1:]...)
			break
		}
	}
	pool.mu.Unlock()
	s.stop()
}

// errorLog is the [ERROR] lines of mysqld's log, for the message of a server that did not
// come up; "" when there are none (the path is named instead).
func errorLog(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return " (see " + path + ")"
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "[ERROR]") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) == 0 {
		return " (see " + path + ")"
	}
	return "\n  " + strings.Join(lines, "\n  ")
}

// load creates the database and applies the schema.
func (d *DB) load(ctx context.Context, schemaSQL string) error {
	conn, err := mysql.NewConnector(d.cfg)
	if err != nil {
		return err
	}
	admin := sql.OpenDB(conn)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+Database)
	admin.Close()
	if err != nil {
		return err
	}
	d.cfg.DBName = Database
	conn, err = mysql.NewConnector(d.cfg)
	if err != nil {
		return err
	}
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

// Close stops the server and removes its directory; under Main, drops the database and
// hands the server back for the next Start.
func (d *DB) Close() {
	if d.db != nil {
		d.db.Close()
		d.db = nil
	}
	if d.srv == nil {
		return
	}
	if d.owned {
		d.srv.stop()
	} else {
		d.srv.release()
	}
	d.srv = nil
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
// another process doing the same. lctn is the lower_case_table_names the directory is
// initialized with ("0" for the default), part of its name when not the default.
func ensureTemplate(ctx context.Context, mysqld, version, lctn string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(cache, "sqlshape", "mysqld-"+version)
	if lctn != "0" {
		base += "-lctn" + lctn
	}
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
		"--log-error="+filepath.Join(base, "initialize.log"), "--innodb-redo-log-capacity=8M",
		"--lower-case-table-names="+lctn)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mysqltest: mysqld --initialize-insecure: %w\n%s", err, out)
	}
	if err := os.Rename(tmp, template); err != nil {
		return "", err
	}
	return template, nil
}

// scratchRoot is where a server's data directory goes: SQLSHAPE_MYSQLTEST_DIR when set,
// else /dev/shm when it is a writable directory (tmpfs on Linux: copying the 95 MB template
// and InnoDB's own writes leave the disk out of it; a start measured 2.05 s on ext4 and
// 1.45 s there), else the system temp directory. TMPDIR alone does not opt out, since
// nix-shell and other wrappers set it to a disk path as a matter of course. The directory
// is removed on Close either way.
func scratchRoot() string {
	if d := os.Getenv("SQLSHAPE_MYSQLTEST_DIR"); d != "" {
		return d
	}
	const shm = "/dev/shm"
	if st, err := os.Stat(shm); err == nil && st.IsDir() {
		if f, err := os.CreateTemp(shm, "sqlshape-probe-"); err == nil {
			f.Close()
			os.Remove(f.Name())
			return shm
		}
	}
	return ""
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
