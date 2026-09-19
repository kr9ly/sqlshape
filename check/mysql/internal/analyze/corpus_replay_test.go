package analyze

// The corpus probe replays MySQL's own test corpus (mysql-test/t, the server source the
// function catalog is generated from, checked out under the sqlshape cache directory)
// against a running mysqld and the analyzer side by side, and reports every statement on
// which they disagree -- the MySQL counterpart of check/postgres/analyze's regress probe.
// Each .test file runs on a mysqld of its own (mysqltest.StartOwn, over a second per file: the
// corpus creates and drops databases by name, changes global variables and accounts, so
// files cannot share a server), connected to a fresh `test` database the way mysqltest
// connects, over one connection (so SET and USE hold); after a statement that may have
// changed the schema, the analyzer's
// schema is rebuilt from the server's own SHOW CREATE output (dump.Read) under the
// session's sql_mode, so the analyzer judges what the server holds. A SELECT's columns are
// compared by name, type family and nullability; an error by number; a statement the
// analyzer does not read (DDL, SHOW, SET ...) is tallied, not judged.
//
// The known disagreements are listed in testdata/corpus_baseline.txt; the test fails on a
// statement that joins or leaves that list (-corpus-update rewrites it after reading the
// report). Skipped without the corpus or a mysqld on PATH (nix-shell -p mysql84).
//
//	go test ./check/mysql/internal/analyze -run TestCorpusReplay -corpus-run [-corpus-files select,join] \
//	    [-corpus-report /path/report.md] [-corpus-update] [-corpus-jobs 8]
//
// It is not part of a plain `go test` (five minutes on eight cores, a mysqld per file):
// -corpus-run or $SQLSHAPE_CORPUS=1 turns it on.

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/parsegen"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

var (
	corpusFiles  = flag.String("corpus-files", "", "comma-separated .test names (without extension) to replay (default: every file; the baseline is not checked)")
	corpusReport = flag.String("corpus-report", "", "write the per-statement report here (default: the test log summary only)")
	corpusUpdate = flag.Bool("corpus-update", false, "rewrite testdata/corpus_baseline.txt from this run's hits")
	corpusJobs   = flag.Int("corpus-jobs", 0, "files replayed at once (default: NumCPU, at most 8)")
	corpusLimit  = flag.Int("corpus-limit", 0, "replay only the first N files (0: all; the baseline is not checked)")
	corpusRun    = flag.Bool("corpus-run", false, "run the corpus probe (five minutes on eight cores; also $SQLSHAPE_CORPUS=1)")
)

const corpusBaseline = "testdata/corpus_baseline.txt"

// corpusHit is one disagreement.
type corpusHit struct {
	file   string
	line   int
	class  string // LENIENT / STRICT / CODE / DIFF / RUNTIME / SCHEMA
	key    string
	sql    string
	detail string
}

func (h corpusHit) id() string {
	sum := sha1.Sum([]byte(h.sql))
	return fmt.Sprintf("%s\t%s %s\t%x", h.file, h.class, h.key, sum[:6])
}

func corpusDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "sqlshape", "mysql-server", "mysql-test", "t")
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

func TestCorpusReplay(t *testing.T) {
	if !*corpusRun && os.Getenv("SQLSHAPE_CORPUS") == "" && *corpusFiles == "" && *corpusLimit == 0 {
		t.Skip("the corpus probe runs with -corpus-run (or $SQLSHAPE_CORPUS=1): five minutes on eight cores")
	}
	dir := corpusDir()
	if dir == "" {
		t.Skip("no MySQL source under the cache directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	probeServer, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	probeServer.Close()
	stmts, _, err := parsegen.SplitTestDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string][]parsegen.Statement{}
	var files []string
	for _, s := range stmts {
		if _, ok := byFile[s.File]; !ok {
			files = append(files, s.File)
		}
		byFile[s.File] = append(byFile[s.File], s)
	}
	partial := false
	if *corpusFiles != "" {
		want := map[string]bool{}
		for _, f := range strings.Split(*corpusFiles, ",") {
			want[strings.TrimSuffix(strings.TrimSpace(f), ".test")+".test"] = true
		}
		var keep []string
		for _, f := range files {
			if want[f] {
				keep = append(keep, f)
			}
		}
		files, partial = keep, true
	}
	if *corpusLimit > 0 && *corpusLimit < len(files) {
		files, partial = files[:*corpusLimit], true
	}
	jobs := *corpusJobs
	if jobs <= 0 {
		jobs = min(runtime.NumCPU(), 8)
	}
	p := &corpusProbe{t: t, ctx: ctx, counts: map[string]int{}, unsupported: map[string]int{}}
	var (
		mu      sync.Mutex
		hits    []corpusHit
		wg      sync.WaitGroup
		next    = make(chan int)
		started = time.Now()
	)
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				srv, err := mysqltest.StartOwn(ctx, "-- sqlshape: mysql 8.4\nCREATE DATABASE test;\n")
				if err != nil {
					t.Errorf("%s: %v", files[i], err)
					continue
				}
				fh := p.runFile(srv, files[i], byFile[files[i]])
				srv.Close()
				mu.Lock()
				hits = append(hits, fh...)
				mu.Unlock()
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})

	// summary
	var b strings.Builder
	fmt.Fprintf(&b, "# corpus probe (mysql): %d files, %.0fs\n\n", len(files), time.Since(started).Seconds())
	var keys []string
	for k := range p.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %d\n", k, p.counts[k])
	}
	byClass := map[string]int{}
	byKey := map[string]int{}
	for _, h := range hits {
		byClass[h.class]++
		byKey[h.class+" "+h.key]++
	}
	fmt.Fprintf(&b, "\nhits: %d\n", len(hits))
	for _, c := range []string{"LENIENT", "STRICT", "CODE", "DIFF", "RUNTIME", "SCHEMA", "PANIC"} {
		if byClass[c] > 0 {
			fmt.Fprintf(&b, "  %s: %d\n", c, byClass[c])
		}
	}
	type kv struct {
		k string
		n int
	}
	var top []kv
	for k, n := range byKey {
		top = append(top, kv{k, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n || top[i].n == top[j].n && top[i].k < top[j].k })
	fmt.Fprintf(&b, "\ntop keys:\n")
	for i, kv := range top {
		if i >= 40 {
			break
		}
		fmt.Fprintf(&b, "  %5d  %s\n", kv.n, kv.k)
	}
	var un []kv
	for k, n := range p.unsupported {
		un = append(un, kv{k, n})
	}
	sort.Slice(un, func(i, j int) bool { return un[i].n > un[j].n })
	fmt.Fprintf(&b, "\nnot read by the analyzer (top):\n")
	for i, kv := range un {
		if i >= 15 {
			break
		}
		fmt.Fprintf(&b, "  %5d  %s\n", kv.n, kv.k)
	}
	t.Log(b.String())
	if *corpusReport != "" {
		for _, h := range hits {
			fmt.Fprintf(&b, "\n## %s:%d %s %s\n\n```sql\n%s\n```\n\n%s\n", h.file, h.line, h.class, h.key, h.sql, h.detail)
		}
		if err := os.WriteFile(*corpusReport, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *corpusReport)
	}

	// the baseline
	if partial {
		return
	}
	if *corpusUpdate {
		var lines []string
		lines = append(lines, "# corpus probe: the statements on which the analyzer and MySQL knowingly disagree.", "# file\tclass key\tsha1(sql)\tfirst line. Rewrite with: go test ./check/mysql/internal/analyze -run TestCorpusReplay -corpus-update")
		seen := map[string]bool{}
		for _, h := range hits {
			id := h.id()
			if seen[id] {
				continue
			}
			seen[id] = true
			first := strings.SplitN(strings.TrimSpace(h.sql), "\n", 2)[0]
			if len(first) > 100 {
				first = first[:100]
			}
			lines = append(lines, id+"\t"+first)
		}
		if err := os.WriteFile(corpusBaseline, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("baseline rewritten: %d hits", len(seen))
		return
	}
	known := map[string]bool{}
	if data, err := os.ReadFile(corpusBaseline); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if f := strings.Split(l, "\t"); len(f) >= 3 {
				known[strings.Join(f[:3], "\t")] = true
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	current := map[string]bool{}
	var joined []string
	for _, h := range hits {
		id := h.id()
		current[id] = true
		if !known[id] {
			joined = append(joined, id)
		}
	}
	var left []string
	for id := range known {
		if !current[id] {
			left = append(left, id)
		}
	}
	sort.Strings(joined)
	sort.Strings(left)
	if len(joined) > 0 {
		t.Errorf("%d statement(s) newly disagree (not in %s):\n%s", len(joined), corpusBaseline, strings.Join(joined, "\n"))
	}
	if len(left) > 0 {
		t.Errorf("%d baseline statement(s) no longer disagree (drop them, or -corpus-update):\n%s", len(left), strings.Join(left, "\n"))
	}
}

type corpusProbe struct {
	t   *testing.T
	ctx context.Context
	mu  sync.Mutex
	// counts tallies statements by outcome; unsupported the analyzer's own limits by message
	counts      map[string]int
	unsupported map[string]int
}

func (p *corpusProbe) count(key string, n int) {
	p.mu.Lock()
	p.counts[key] += n
	p.mu.Unlock()
}

var (
	reCreateTemp   = regexp.MustCompile("(?i)^\\s*CREATE\\s+TEMPORARY\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?(\\w+)`?")
	reSystemSchema = regexp.MustCompile("(?i)\\b(information_schema|performance_schema|mysql|sys)\\s*\\.\\s*`?\\w")
)

// mentionsAny: the statement names one of the tables (as a bare word).
func mentionsAny(sql string, names map[string]bool) bool {
	if len(names) == 0 {
		return false
	}
	for _, w := range regexp.MustCompile("`?(\\w+)`?").FindAllStringSubmatch(sql, -1) {
		if names[strings.ToLower(w[1])] {
			return true
		}
	}
	return false
}

var reFirstWord = regexp.MustCompile(`(?is)^(?:\s|/\*.*?\*/|--[^\n]*\n|#[^\n]*\n)*\(*\s*(\w+)`)

// firstWord is a statement's leading keyword, upper-cased.
func firstWord(sql string) string {
	m := reFirstWord.FindStringSubmatch(sql)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// schemaChanging: after this statement the analyzer's schema is rebuilt from the server.
func schemaChanging(word string) bool {
	switch word {
	case "CREATE", "ALTER", "DROP", "RENAME", "SET", "USE", "TRUNCATE", "FLUSH", "IMPORT":
		return true
	}
	return false
}

// notUnsupported is the analyzer's own limit on a statement kind, not a verdict.
func notUnsupported(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if _, ok := err.(*Error); ok {
		return "", false
	}
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 && strings.HasPrefix(msg, "analyze") {
		msg = strings.TrimSpace(msg[i+1:])
	}
	if len(msg) > 60 {
		msg = msg[:60]
	}
	return msg, true
}

// runFile replays one .test file on its own srv, in the `test` database.
func (p *corpusProbe) runFile(srv *mysqltest.DB, file string, stmts []parsegen.Statement) []corpusHit {
	ctx := p.ctx
	name := strings.TrimSuffix(file, ".test")
	cfg, err := driver.ParseDSN(srv.DSN())
	if err != nil {
		p.t.Fatal(err)
	}
	cfg.DBName = "test"
	cfg.MultiStatements = false
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		p.t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		p.t.Logf("%s: connect: %v", file, err)
		return nil
	}
	defer conn.Close()
	// the dump reads SHOW CREATE on a connection of its own (the file's connection may hold
	// LOCK TABLES, under which SHOW CREATE of another table is refused), in whatever
	// database the file's connection is in
	dumpDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		p.t.Fatal(err)
	}
	defer dumpDB.Close()
	reconnect := func() {
		conn.Close()
		conn, err = db.Conn(ctx)
		if err != nil {
			p.t.Logf("%s: reconnect: %v", file, err)
		}
	}
	start := time.Now()
	var hits []corpusHit
	var s *schema.Schema // nil: rebuild before the next judged statement
	schemaBroken := ""
	rebuild := func() {
		s = nil
		schemaBroken = ""
		var current sql.NullString
		var mode string
		if err := conn.QueryRowContext(ctx, "SELECT DATABASE(), @@SESSION.sql_mode").Scan(&current, &mode); err != nil {
			schemaBroken = "session: " + err.Error()
			return
		}
		if !current.Valid {
			schemaBroken = "no database selected"
			return
		}
		// the file's connection may hold a metadata lock (an open transaction, LOCK
		// TABLES, HANDLER OPEN) that SHOW CREATE on another connection waits for: bound
		// the wait, and give up on the schema for now
		dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
		defer dcancel()
		dc, err := dumpDB.Conn(dctx)
		if err != nil {
			schemaBroken = "dump connection: " + err.Error()
			return
		}
		defer dc.Close()
		if _, err := dc.ExecContext(dctx, "SET SESSION lock_wait_timeout = 3, SESSION autocommit = 1"); err != nil {
			schemaBroken = "dump session: " + err.Error()
			return
		}
		if _, err := dc.ExecContext(dctx, "COMMIT"); err != nil {
			// the file may have set autocommit off globally (locking_readonly_db): a pooled
			// connection would then read the dictionary from a transaction's stale snapshot
			schemaBroken = "dump commit: " + err.Error()
			return
		}
		if _, err := dc.ExecContext(dctx, "USE `"+current.String+"`"); err != nil {
			schemaBroken = "dump USE: " + err.Error()
			return
		}
		text, err := dump.Read(dctx, dc)
		if err != nil {
			schemaBroken = "dump: " + err.Error()
			return
		}
		// the session's sql_mode, an empty one included (SET sql_mode = '' is the corpus's
		// way out of strict mode; without the line the schema would assume the default)
		header := "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = '" + mode + "'\n"
		sc, err := schema.Load(header + text)
		if err != nil {
			schemaBroken = "load: " + err.Error()
			return
		}
		if len(sc.Problems) > 0 {
			var ps []string
			for _, pr := range sc.Problems {
				ps = append(ps, pr.Message)
			}
			schemaBroken = "problems: " + strings.Join(ps, "; ")
			return
		}
		s = sc
	}
	dirty := true
	locked := false // LOCK TABLES held by the file's connection
	judged, total := 0, 0
	temps := map[string]bool{} // CREATE TEMPORARY TABLE names: not in SHOW CREATE, so not the analyzer's
	const fileBudget = 3 * time.Minute
	for _, st := range stmts {
		total++
		if time.Since(start) > fileBudget {
			p.count("skipped (file budget)", 1)
			continue
		}
		sql := strings.TrimSpace(st.SQL)
		word := firstWord(sql)
		if m := reCreateTemp.FindStringSubmatch(sql); m != nil {
			temps[strings.ToLower(m[1])] = true
		}
		switch word {
		case "SHUTDOWN", "RESTART", "KILL":
			p.count("skipped (server control)", 1)
			continue
		}
		if !utf8.ValidString(sql) {
			// a file in a legacy encoding (gb18030, sjis, koi8r ...): the harness sends the
			// bytes as utf8mb4 and reads SHOW CREATE back as utf8mb4, so the names it
			// compares are not the file's -- the file's own character set, not a verdict
			p.count("not judged (encoding)", 1)
			continue
		}
		// the server
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var desc *description
		var serr error
		if isQueryWord(word) {
			desc, serr = describeOn(sctx, conn, sql)
		} else {
			_, serr = conn.ExecContext(sctx, sql)
		}
		cancel()
		if serr != nil && (errors.Is(serr, context.DeadlineExceeded) || strings.Contains(serr.Error(), "bad connection") || strings.Contains(serr.Error(), "invalid connection")) {
			p.count("skipped (timeout / connection)", 1)
			reconnect()
			dirty = true
			continue
		}
		var me *driver.MySQLError
		scode := 0
		if serr != nil {
			if !errors.As(serr, &me) {
				p.count("skipped (driver error)", 1)
				continue
			}
			scode = int(me.Number)
		}
		if serr == nil && schemaChanging(word) {
			dirty = true
		}
		if serr == nil {
			switch word {
			case "LOCK":
				locked = true
			case "UNLOCK", "START", "BEGIN": // UNLOCK TABLES; a transaction start releases LOCK TABLES too
				if locked {
					dirty = true // a DDL under the lock is visible only now
				}
				locked = false
			}
		}
		if reSystemSchema.MatchString(sql) {
			p.count("not judged (system schema)", 1)
			continue
		}
		if mentionsAny(sql, temps) {
			p.count("not judged (temporary table)", 1)
			continue
		}
		if scode == 1046 || scode == 1049 {
			// no database selected, or a database the file expects and this server lacks:
			// the file's environment, not the statement's meaning (a MySQL schema is one
			// database to the analyzer)
			p.count("not judged (no database)", 1)
			continue
		}
		// the analyzer: only the statement kinds it reads
		switch word {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "CALL", "WITH", "TABLE", "VALUES", "LOCK", "UNLOCK", "LOAD":
		default:
			p.count("not judged (statement kind)", 1)
			continue
		}
		if dirty && locked {
			// a DDL ran under LOCK TABLES: SHOW CREATE from the dump's connection waits on
			// (or does not yet see) what the locking connection did -- ALTER TABLE ...
			// RENAME under LOCK TABLES leaves the new name locked exclusively (measured:
			// the dump read an inventory without it); judged again after UNLOCK
			p.count("not judged (locked)", 1)
			continue
		}
		if dirty {
			rebuild()
			dirty = false
		}
		if s == nil {
			p.count("not judged (schema)", 1)
			if schemaBroken != "" {
				// a one-shot event the scheduler ran and dropped between the statement and
				// the dump is timing, not a disagreement
				if !strings.Contains(schemaBroken, "Unknown event") {
					hits = append(hits, corpusHit{file: name, line: st.Line, class: "SCHEMA", key: schemaKey(schemaBroken), sql: sql, detail: schemaBroken})
				}
				schemaBroken = "" // once per rebuild
			}
			continue
		}
		if word == "CALL" {
			body := calledBody(s, sql)
			if serr == nil && reBodyDDL.MatchString(body) {
				dirty = true // the body ran DDL: the schema is not what the CALL was judged on
			}
			if body != "" && (reSystemSchema.MatchString(body) || mentionsAny(body, temps) || reCreateTempAny.MatchString(body)) {
				// the routine's body reads information_schema / performance_schema (the
				// corpus's own p_verify_reprepare_count) or a TEMPORARY table, or creates
				// one: the same exclusions as for a top-level statement, one level down
				p.count("not judged (routine body)", 1)
				continue
			}
		}
		if scode == 3024 || scode == 1317 {
			// ER_QUERY_TIMEOUT / ER_QUERY_INTERRUPTED: whether the server raises these is
			// the machine's speed, not the statement's shape -- the same corpus flapped
			// max_statement_time's rows in and out of the baseline run to run
			p.count("not judged (timing)", 1)
			continue
		}
		res, aerr := analyzeSafe(s, sql)
		if aerr != nil && strings.HasPrefix(aerr.Error(), "panic:") {
			hits = append(hits, corpusHit{file: name, line: st.Line, class: "PANIC", key: panicKey(aerr.Error()), sql: sql, detail: aerr.Error()})
			continue
		}
		if msg, ok := notUnsupported(aerr); ok {
			p.mu.Lock()
			p.unsupported[msg]++
			p.mu.Unlock()
			p.count("not judged (analyzer limit)", 1)
			continue
		}
		if acode := errCode(aerr); acode == 1146 && otherDatabase(sql, aerr.Error()) || acode == 1305 && reQualifiedCall.MatchString(sql) {
			// the table the analyzer misses is named with a database qualifier (or the
			// routine it misses is called as db.routine): another database's, which the
			// analyzer (one database) does not model
			p.count("not judged (other database)", 1)
			continue
		}
		judged++
		acode := errCode(aerr)
		switch {
		case scode != 0 && acode != 0:
			if scode == acode {
				p.count("agree (error)", 1)
			} else {
				hits = append(hits, corpusHit{file: name, line: st.Line, class: "CODE", key: fmt.Sprintf("%d->%d", scode, acode), sql: sql, detail: fmt.Sprintf("server: %v\nanalyzer: %v", serr, aerr)})
			}
		case scode != 0 && runtimeCode(scode):
			// a failure the data decides (a constraint, a conversion, a subquery's row
			// count): the analyzer's verdict on it is a predicted violation, not an error
			if predictsCode(res, scode) {
				p.count("agree (predicted failure)", 1)
			} else {
				hits = append(hits, corpusHit{file: name, line: st.Line, class: "RUNTIME", key: fmt.Sprint(scode), sql: sql, detail: fmt.Sprintf("server: %v\nanalyzer accepts and predicts %v", serr, predictedCodes(res))})
			}
		case scode != 0:
			hits = append(hits, corpusHit{file: name, line: st.Line, class: "LENIENT", key: fmt.Sprint(scode), sql: sql, detail: fmt.Sprintf("server: %v\nanalyzer accepts", serr)})
		case acode != 0:
			hits = append(hits, corpusHit{file: name, line: st.Line, class: "STRICT", key: fmt.Sprint(acode), sql: sql, detail: fmt.Sprintf("analyzer: %v\nserver accepts", aerr)})
		default:
			if desc == nil {
				p.count("agree (write)", 1)
				break
			}
			if key, detail := compareColumns(res, desc); key != "" {
				hits = append(hits, corpusHit{file: name, line: st.Line, class: "DIFF", key: key, sql: sql, detail: detail})
			} else {
				p.count("agree (columns)", 1)
			}
		}
	}
	p.t.Logf("%-40s %5d stmts %4d judged %6.1fs", name, total, judged, time.Since(start).Seconds())
	return hits
}

// runtimeCode: an error the data decides at run time, which the analyzer predicts as a
// violation (an expect line's key) rather than raises: constraints (1048 / 1062 / 1451 /
// 1452 / 3819 / 1369 / 1364 / 1423 / 1263), conversions and ranges (1264 / 1265 / 1292 / 1366 /
// 1406 / 3854 / 1690 / 1441), a scalar subquery's or SELECT INTO's row count (1242 / 1172),
// SIGNALs (1644 / 1643), a division by zero (1365), a duplicate under a locking read.
func runtimeCode(code int) bool {
	switch code {
	case 1048, 1062, 1451, 1452, 3819, 1369, 1364, 1423, 1263, 1264, 1265, 1292, 1366, 1406, 3854, 1690, 1441, 1242, 1172, 1644, 1643, 1365, 1329, 1213, 1205, 1105, 3105, 1216, 1217, 1586, 1416:
		return true
	}
	return false
}

// predictsCode: the analyzer lists a violation of the server's error number.
func predictsCode(r *Result, code int) bool {
	if r == nil {
		return false
	}
	for _, v := range r.Violations {
		if v.Code == code {
			return true
		}
	}
	return false
}

func predictedCodes(r *Result) []int {
	if r == nil {
		return nil
	}
	var out []int
	for _, v := range r.Violations {
		out = append(out, v.Code)
	}
	sort.Ints(out)
	return out
}

var (
	reBodyDDL       = regexp.MustCompile(`(?i)\b(CREATE|DROP|ALTER|RENAME)\s+(TABLE|VIEW|TEMPORARY)\b`)
	reCreateTempAny = regexp.MustCompile(`(?i)\bCREATE\s+TEMPORARY\s+TABLE\b`)
	reMissingTable  = regexp.MustCompile(`Table '([^']+)' doesn't exist`)
	reQualifiedCall = regexp.MustCompile("(?i)`?\\w+`?\\s*\\.\\s*`?\\w+`?\\s*\\(")
	reCallName      = regexp.MustCompile("(?is)^CALL\\s+`?([\\w$]+)`?")
)

// calledBody is the CREATE text of the routine a CALL statement names, "" when the schema
// has no such routine.
func calledBody(s *schema.Schema, sql string) string {
	m := reCallName.FindStringSubmatch(sql)
	if m == nil {
		return ""
	}
	if r := s.RoutineOf(schema.Procedure, m[1]); r != nil {
		return r.Definition
	}
	return ""
}

// otherDatabase: the table a 1146 names appears database-qualified in the statement (the
// name is matched as bytes: the corpus is read as latin-1, and a name need not be UTF-8).
func otherDatabase(sql, msg string) bool {
	m := reMissingTable.FindStringSubmatch(msg)
	if m == nil {
		return false
	}
	name := m[1]
	lower := strings.ToLower(sql)
	for i := 0; i < len(lower); {
		j := strings.Index(lower[i:], strings.ToLower(name))
		if j < 0 {
			return false
		}
		k := i + j
		// walk back over an optional backquote, spaces, a dot, spaces, and a word
		b := k - 1
		if b >= 0 && lower[b] == '`' {
			b--
		}
		for b >= 0 && (lower[b] == ' ' || lower[b] == '\t') {
			b--
		}
		if b >= 0 && lower[b] == '.' {
			b--
			for b >= 0 && (lower[b] == ' ' || lower[b] == '\t') {
				b--
			}
			if b >= 0 && (lower[b] == '_' || lower[b] == '`' || lower[b] >= 'a' && lower[b] <= 'z' || lower[b] >= '0' && lower[b] <= '9') {
				return true
			}
		}
		i = k + len(name)
	}
	return false
}

// analyzeSafe is Analyze with a panic turned into an error, so one crashing statement
// does not end the run (and is reported as a hit of its own).
func analyzeSafe(s *schema.Schema, sql string) (r *Result, err error) {
	defer func() {
		if e := recover(); e != nil {
			var trace []string
			for _, l := range strings.Split(string(debug.Stack()), "\n") {
				if strings.Contains(l, "/check/mysql/") && !strings.Contains(l, "_test.go") {
					trace = append(trace, strings.TrimSpace(l))
				}
			}
			r, err = nil, fmt.Errorf("panic: %v\n%s", e, strings.Join(trace, "\n"))
		}
	}()
	return Analyze(s, sql)
}

// panicKey names a panic by its message and the analyzer frame it came from.
func panicKey(msg string) string {
	lines := strings.Split(msg, "\n")
	key := strings.TrimPrefix(lines[0], "panic: ")
	if len(key) > 50 {
		key = key[:50]
	}
	if len(lines) > 1 {
		if i := strings.LastIndex(lines[1], "/"); i >= 0 {
			key += " @ " + lines[1][i+1:]
		}
	}
	return key
}

func schemaKey(msg string) string {
	i := strings.Index(msg, ":")
	if i < 0 {
		return msg
	}
	rest := strings.TrimSpace(msg[i+1:])
	if j := strings.IndexAny(rest, ";\n"); j >= 0 {
		rest = rest[:j]
	}
	// strip the object's name: the class of problem is the key
	rest = regexp.MustCompile("`[^`]*`|'[^']*'|\"[^\"]*\"").ReplaceAllString(rest, "?")
	rest = regexp.MustCompile(`\d+`).ReplaceAllString(rest, "N")
	if len(rest) > 60 {
		rest = rest[:60]
	}
	return msg[:i] + ": " + rest
}

func isQueryWord(word string) bool {
	switch word {
	case "SELECT", "TABLE", "VALUES", "WITH":
		return true
	}
	return false
}

// description is the server's result-set metadata for a query.
type description struct {
	cols []struct {
		name, typ string
		nullable  bool
	}
}

// describeOn runs a query and reads its column metadata (rows are drained, since the
// statement is the corpus's own and its side effects are wanted).
func describeOn(ctx context.Context, conn *sql.Conn, sql string) (*description, error) {
	rows, err := conn.QueryContext(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	d := &description{}
	for _, ct := range cts {
		n, _ := ct.Nullable()
		d.cols = append(d.cols, struct {
			name, typ string
			nullable  bool
		}{ct.Name(), ct.DatabaseTypeName(), n})
	}
	for rows.Next() {
	}
	return d, rows.Err()
}

// compareColumns judges the analyzer's columns against the server's metadata: count,
// names, type families (typeKey / oracleKey's grouping), nullability.
func compareColumns(r *Result, d *description) (key, detail string) {
	if len(r.Columns) != len(d.cols) {
		return "column count", fmt.Sprintf("analyzer %d columns, server %d", len(r.Columns), len(d.cols))
	}
	for i, c := range r.Columns {
		oc := d.cols[i]
		if !sameColumnName(c.Name, oc.name) {
			return "column name", fmt.Sprintf("column %d: analyzer %q, server %q", i+1, c.Name, oc.name)
		}
		if !c.Known {
			continue
		}
		got, want := typeKey(c.Type), familyOf(oc.typ)
		if got != want {
			return "column type " + got + "->" + want, fmt.Sprintf("column %q: analyzer %s, server %s", c.Name, got, want)
		}
		if c.Nullable != oc.nullable {
			return fmt.Sprintf("nullable %v->%v", c.Nullable, oc.nullable), fmt.Sprintf("column %q: analyzer nullable=%v, server %v", c.Name, c.Nullable, oc.nullable)
		}
	}
	return "", ""
}

// sameColumnName compares a result column's name with the server's, allowing for what the
// connection's character set did to the server's: under a file's SET NAMES utf8mb3 a
// character outside the BMP in the name comes back as '?', and under a single-byte SET
// NAMES (koi8r, latin1) the name is not UTF-8 at all -- the file's connection settings,
// not the analyzer's naming.
func sameColumnName(analyzer, server string) bool {
	if analyzer == server {
		return true
	}
	if !utf8.ValidString(server) {
		return true
	}
	if strings.ContainsRune(server, '?') {
		folded := strings.Map(func(r rune) rune {
			if r > 0xFFFF {
				return '?'
			}
			return r
		}, analyzer)
		return folded == server
	}
	return false
}

// familyOf is oracleKey over the driver's DatabaseTypeName.
func familyOf(t string) string {
	name := strings.ToLower(t)
	if name == "geometry" {
		return "geometry"
	}

	unsigned := strings.HasPrefix(name, "unsigned ")
	name = strings.TrimPrefix(name, "unsigned ")
	switch name {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		name = "string"
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		name = "binary"
	}
	if unsigned {
		return name + " unsigned"
	}
	return name
}
