package analyze

import (
	"context"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/schema"
)

// The regress probe replays PostgreSQL's own regression corpus (src/test/regress)
// against a live PG and the analyzer side by side, and reports every statement on
// which they disagree. The corpus is the PG release the oracle runs, checked out by
// testdata/tools/fetch-regress.sh into the sqlshape cache directory (or named with
// -regress / $SQLSHAPE_REGRESS); without it the test skips.
//
// The known disagreements — the ceiling: collation environment, RLS recursion, server
// internals, and the three deliberate differences — are listed in testdata/regress_baseline_<major>.txt,
// and the test fails on any statement that joins or leaves that list (run with
// -regress-update to rewrite it after reading the report).
//
//	go test ./internal/analyze -run TestRegress [-regress /path/to/src/test/regress] \
//	    [-regress-tests select,join] [-regress-report /path/report.txt] [-regress-update]
var (
	regressDir    = flag.String("regress", "", "path to PG's src/test/regress (default: $SQLSHAPE_REGRESS, else the fetch-regress.sh checkout in the cache directory)")
	regressTests  = flag.String("regress-tests", "", "comma-separated test names to run (default: parallel_schedule order; the baseline is not checked)")
	regressReport = flag.String("regress-report", "", "write the per-statement report here (default: stderr summary only)")
	regressJobs   = flag.Int("regress-jobs", 0, "parallel workers for the non-promoted files (default: NumCPU, at most 8)")
	regressUpdate = flag.Bool("regress-update", false, "rewrite testdata/regress_baseline_<major>.txt from this run's hits")
)

// regressVersion is the PostgreSQL major version the probe runs: $SQLSHAPE_PG, else the
// default. The oracle, the corpus checkout, the catalog and the baseline all follow it.
func regressVersion(t testing.TB) pgparse.Version {
	if s := os.Getenv("SQLSHAPE_PG"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("SQLSHAPE_PG=%q: want a major version", s)
		}
		return pgparse.Version(n)
	}
	return pgparse.Default
}

// regressBaseline is the baseline file of a version's probe.
func regressBaseline(v pgparse.Version) string {
	return fmt.Sprintf("testdata/regress_baseline_%d.txt", int(v))
}

// regressDeclaration is the version declaration the probe writes above the corpus DDL, so
// the analyzer judges it with the probed version's grammar and catalog.
func regressDeclaration(v pgparse.Version) string {
	return fmt.Sprintf("-- sqlshape: postgres %d\n", int(v))
}

// regressCorpus resolves the corpus directory: the flag, the environment, or the
// fetch-regress.sh checkout under the user cache directory.
func regressCorpus(v pgparse.Version) string {
	if *regressDir != "" {
		return *regressDir
	}
	if p := os.Getenv("SQLSHAPE_REGRESS"); p != "" {
		return p
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(base, "sqlshape", fmt.Sprintf("regress-%d", int(v)), "src", "test", "regress")
	if _, err := os.Stat(filepath.Join(p, "parallel_schedule")); err != nil {
		return ""
	}
	return p
}

// reEnvCollation matches the oracle's refusal of a collation the operating system does
// not provide: which collations exist depends on the machine's locales, so these hits
// are neither in the baseline nor counted as new.
var reEnvCollation = regexp.MustCompile(`42704: collation "[^"]*" for encoding "[^"]*" does not exist`)

func (h regressHit) envDependent() bool { return reEnvCollation.MatchString(h.oracle) }

// hitID identifies a hit independently of its statement number: test, class, key and a
// hash of the statement text.
func (h regressHit) hitID() string {
	sum := sha1.Sum([]byte(h.sql))
	return fmt.Sprintf("%s\t%s %s\t%x", h.test, h.class, h.key, sum[:6])
}

// readBaseline returns the hit ids of the baseline file (nil when there is none).
func readBaseline(t *testing.T, v pgparse.Version) map[string]bool {
	data, err := os.ReadFile(regressBaseline(v))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if f := strings.Split(l, "\t"); len(f) >= 3 {
			ids[strings.Join(f[:3], "\t")] = true
		}
	}
	return ids
}

// writeBaseline writes the hits as the new baseline, one per line: id fields, then the
// first line of the statement for the reader.
func writeBaseline(t *testing.T, v pgparse.Version, hits []regressHit) {
	lines := []string{"# regress probe: the statements on which the analyzer and PostgreSQL knowingly disagree.", "# test\tclass key\tsha1(sql)\tfirst line. Rewrite with: go test ./internal/analyze -run TestRegress -regress-update"}
	var body []string
	for _, h := range hits {
		if h.envDependent() {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimSpace(h.sql), "\n")
		if len(first) > 100 {
			first = first[:100] + "…"
		}
		body = append(body, h.hitID()+"\t"+first)
	}
	sort.Strings(body)
	lines = append(lines, body...)
	if err := os.WriteFile(regressBaseline(v), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The schedule lines whose objects later tests build on. Files in these lines run
// sequentially in the shared database; every later file runs in a throwaway copy.
const regressPromotedLines = 8

type regressHit struct {
	test     string
	n        int
	class    string // DIFF / LENIENT / STRICT / CODE
	key      string
	sql      string
	oracle   string
	analyzer string
}

func TestRegress(t *testing.T) {
	version := regressVersion(t)
	corpus := regressCorpus(version)
	if corpus == "" {
		t.Skipf("no regress corpus for PostgreSQL %d: run internal/analyze/testdata/tools/fetch-regress.sh REL_%d_x, or set -regress / $SQLSHAPE_REGRESS", int(version), int(version))
	}
	dir, err := filepath.Abs(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var tests []string
	promoted := map[string]bool{}
	sched, err := os.ReadFile(filepath.Join(dir, "parallel_schedule"))
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for _, l := range strings.Split(string(sched), "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "test:") {
			continue
		}
		line++
		for _, name := range strings.Fields(strings.TrimPrefix(l, "test:")) {
			tests = append(tests, name)
			if line <= regressPromotedLines {
				promoted[name] = true
			}
		}
	}
	only := map[string]bool{}
	if *regressTests != "" {
		for _, n := range strings.Split(*regressTests, ",") {
			only[strings.TrimSpace(n)] = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	o, err := oracle.StartVersion(ctx, version, "")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()

	p := &regressProbe{t: t, ctx: ctx, o: o, dir: dir, version: version}
	p.stats = map[string]int{}
	// Promoted files build the shared database in order; the rest each run in a
	// throwaway copy of it, so they run in parallel once the promoted ones are done.
	type job struct {
		name  string
		quiet bool
	}
	var pending []job
	for _, name := range tests {
		quiet := false
		if len(only) > 0 && !only[name] {
			// Promoted tests still run so the shared database has their objects.
			if !promoted[name] {
				continue
			}
			quiet = true
		}
		if promoted[name] {
			p.hits = append(p.hits, p.runFile(o, "sqlshape", name, true, quiet)...)
		} else {
			pending = append(pending, job{name, quiet})
		}
	}
	if len(pending) > 0 {
		// CREATE DATABASE ... TEMPLATE sqlshape needs no other session on the template
		if err := o.Reconnect(ctx, "postgres"); err != nil {
			t.Fatal(err)
		}
		workers := *regressJobs
		if workers <= 0 {
			workers = min(runtime.NumCPU(), 8)
		}
		workers = min(workers, len(pending))
		results := make([][]regressHit, len(pending))
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				sess, err := o.Session(ctx, "postgres")
				if err != nil {
					t.Errorf("worker %d: %v", w, err)
					for range jobs {
					}
					return
				}
				defer sess.Close()
				for i := range jobs {
					// a fresh name per file: a database a file leaves pinned (object_address
					// leaves a logical replication subscription) must not block the next
					results[i] = p.runFile(sess, fmt.Sprintf("regress_probe_%d_%d", w, i), pending[i].name, false, pending[i].quiet)
				}
			}(w)
		}
		for i := range pending {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		for _, r := range results {
			p.hits = append(p.hits, r...)
		}
	}

	// Summary: class × key, most frequent first.
	type agg struct {
		class, key string
		n          int
	}
	counts := map[string]*agg{}
	for _, h := range p.hits {
		k := h.class + " " + h.key
		if counts[k] == nil {
			counts[k] = &agg{h.class, h.key, 0}
		}
		counts[k].n++
	}
	var aggs []*agg
	for _, a := range counts {
		aggs = append(aggs, a)
	}
	sort.Slice(aggs, func(i, j int) bool { return aggs[i].n > aggs[j].n })
	var sb strings.Builder
	fmt.Fprintf(&sb, "statements: %d compared, %d agree, %d skipped (aborted txn), %d skipped (oracle crash), %d unparsed, %d hits\n",
		p.stats["compared"], p.stats["agree"], p.stats["aborted"], p.stats["crash-skipped"], p.stats["unparsed"], len(p.hits))
	for _, a := range aggs {
		fmt.Fprintf(&sb, "%5d  %-7s %s\n", a.n, a.class, a.key)
	}
	t.Log("\n" + sb.String())

	if *regressReport != "" {
		var rb strings.Builder
		rb.WriteString(sb.String())
		rb.WriteString("\n")
		for _, a := range aggs {
			fmt.Fprintf(&rb, "\n==== %s %s (%d)\n", a.class, a.key, a.n)
			for _, h := range p.hits {
				if h.class != a.class || h.key != a.key {
					continue
				}
				fmt.Fprintf(&rb, "\n--- %s #%d\n%s\n", h.test, h.n, h.sql)
				if h.class == "DIFF" {
					fmt.Fprintf(&rb, "--- oracle\n%s--- analyzer\n%s", h.oracle, h.analyzer)
				} else {
					fmt.Fprintf(&rb, "oracle:   %s\nanalyzer: %s\n", strings.TrimSpace(h.oracle), strings.TrimSpace(h.analyzer))
				}
			}
		}
		if err := os.WriteFile(*regressReport, []byte(rb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The gate: the hits must be exactly the baseline's.
	if len(only) > 0 {
		return // a partial run cannot be compared
	}
	if *regressUpdate {
		writeBaseline(t, version, p.hits)
		t.Logf("baseline rewritten: %d hits", len(p.hits))
		return
	}
	base := readBaseline(t, version)
	if base == nil {
		t.Logf("no %s: nothing asserted (write one with -regress-update)", regressBaseline(version))
		return
	}
	seen := map[string]bool{}
	var newHits []regressHit
	env := 0
	for _, h := range p.hits {
		if h.envDependent() {
			env++
			continue
		}
		id := h.hitID()
		seen[id] = true
		if !base[id] {
			newHits = append(newHits, h)
		}
	}
	if env > 0 {
		t.Logf("%d hit(s) depend on the machine's collations and are not gated", env)
	}
	var gone []string
	for id := range base {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, h := range newHits {
		detail := fmt.Sprintf("oracle:   %s\nanalyzer: %s", strings.TrimSpace(h.oracle), strings.TrimSpace(h.analyzer))
		if h.class == "DIFF" {
			detail = "--- oracle\n" + h.oracle + "--- analyzer\n" + h.analyzer
		}
		t.Errorf("new disagreement (%s %s) in %s #%d:\n%s\n%s", h.class, h.key, h.test, h.n, h.sql, detail)
	}
	if len(gone) > 0 {
		t.Errorf("%d baseline hit(s) no longer disagree (fixed? then rewrite the baseline with -regress-update):\n  %s", len(gone), strings.Join(gone, "\n  "))
	}
}

type regressProbe struct {
	t   *testing.T
	ctx context.Context
	o   *oracle.Oracle
	dir string
	// version is the PostgreSQL major version being probed
	version pgparse.Version

	baseDDL []string // DDL accepted by PG in the promoted tests; the analyzer's shared schema
	hits    []regressHit
	mu      sync.Mutex // guards stats (hits are merged by the caller)
	stats   map[string]int
}

func (p *regressProbe) count(key string) {
	p.mu.Lock()
	p.stats[key]++
	p.mu.Unlock()
}

var (
	reMeta  = regexp.MustCompile(`(?m)^\s*\\.*$`)
	reGset  = regexp.MustCompile(`\\g[a-z]*\b[^\n]*`)
	reTemp  = regexp.MustCompile(`pg_temp_\d+\.`)
	reStdin = regexp.MustCompile(`(?i)\bfrom\s+std(in|out)\b`)
	// reCrash matches statements whose Prepare segfaults a PG 17 oracle (MERGE ... INSERT
	// into a partitioned table with a VALUES source referencing a target column; 18 takes
	// them); every crash restarts the server under all the workers, so they are skipped
	reCrash = regexp.MustCompile(`(?s)MERGE INTO measurement m\s+USING \(VALUES.*VALUES \(city_id - 1`)
)

// runFile replays one test file on the given oracle session and returns its hits.
// Promoted files run in the shared database and extend baseDDL; others run in a copy of
// it (database dbName, created here and dropped after) and leave it untouched.
func (p *regressProbe) runFile(o *oracle.Oracle, dbName, name string, promote, quiet bool) (hits []regressHit) {
	src, err := os.ReadFile(filepath.Join(p.dir, "sql", name+".sql"))
	if err != nil {
		p.t.Logf("%s: %v", name, err)
		return nil
	}
	stmts := p.split(string(src))
	if stmts == nil {
		return nil
	}
	start := time.Now()
	defer func() { p.t.Logf("%-28s %4d stmts %6.1fs", name, len(stmts), time.Since(start).Seconds()) }()
	ctx := p.ctx
	conn := o.Conn()
	const session = "SET statement_timeout = '5s'; SET lock_timeout = '2s'; SET client_min_messages = warning"
	// exec runs a statement on the current database; a crashed backend (the server
	// restarts and drops every connection, in every worker) is reconnected once.
	current := dbName
	exec := func(sql string) error {
		_, err := conn.Exec(ctx, sql)
		if err != nil && strings.Contains(err.Error(), "conn closed") {
			if rerr := o.Reconnect(ctx, current); rerr != nil {
				return rerr
			}
			conn = o.Conn()
			if current == dbName {
				conn.Exec(ctx, session)
			}
			_, err = conn.Exec(ctx, sql)
		}
		return err
	}
	if !promote {
		current = "postgres"
		if err := exec("CREATE DATABASE " + dbName + " TEMPLATE sqlshape"); err != nil {
			p.t.Logf("%s: create database: %v", name, err)
			return nil
		}
		if err := o.Reconnect(ctx, dbName); err != nil {
			p.t.Errorf("%s: %v", name, err)
			return nil
		}
		current = dbName
		defer func() {
			if err := o.Reconnect(ctx, "postgres"); err != nil {
				p.t.Error(err)
				return
			}
			conn, current = o.Conn(), "postgres"
			if err := exec("DROP DATABASE " + dbName); err != nil {
				p.t.Logf("%s: drop database: %v (left behind)", name, err)
			}
		}()
		conn = o.Conn()
	}
	conn.Exec(ctx, session)

	ddl := append([]string(nil), p.baseDDL...)
	var s *schema.Schema // the analyzer's schema; nil = rebuild from ddl on next use
	txSnap := -1
	type savepoint struct {
		name string
		n    int
	}
	var saves []savepoint
	for i, sql := range stmts {
		if reStdin.MatchString(sql) { // would wait for client data
			continue
		}
		tree, err := p.version.Parse(sql)
		if err != nil || len(tree.Stmts) == 0 {
			p.count("unparsed")
			exec(sql)
			continue
		}
		node := tree.Stmts[0].Stmt
		if isRegressQuery(node) {
			if p.version == pgparse.PG17 && reCrash.MatchString(sql) {
				p.count("crash-skipped")
				continue
			}
			if s == nil {
				s = loadRegressSchema(p.version, &ddl)
			}
			want := reTemp.ReplaceAllString(renderOracle(ctx, o, sql), "")
			if strings.Contains(want, "conn closed") || strings.Contains(want, "unexpected EOF") { // a backend crash took the connection: reconnect and retry
				if err := o.Reconnect(ctx, dbName); err != nil {
					p.t.Error(err)
					return hits
				}
				conn = o.Conn()
				conn.Exec(ctx, session)
				want = reTemp.ReplaceAllString(renderOracle(ctx, o, sql), "")
			}
			if strings.HasPrefix(want, "error: 25P02") {
				p.count("aborted")
			} else {
				got := renderAnalyzerSafe(s, sql)
				p.count("compared")
				if match(want, got) {
					p.count("agree")
				} else if !quiet {
					class, key := classify(want, got)
					hits = append(hits, regressHit{test: name, n: i + 1, class: class, key: key, sql: sql, oracle: want, analyzer: got})
				}
			}
			if _, isSel := node.Node.(*pgparse.Node_SelectStmt); !isSel || node.GetSelectStmt().IntoClause != nil {
				err := exec(sql) // keep data / SELECT INTO state moving; failures mirror psql
				if err == nil && node.GetSelectStmt().GetIntoClause() != nil {
					// SELECT INTO made a table: the analyzer's schema gets it too
					if s == nil {
						s = loadRegressSchema(p.version, &ddl)
					}
					if s.Apply(sql+";\n") == nil {
						ddl = append(ddl, sql)
					}
				}
			}
			continue
		}
		err = exec(sql)
		switch st := node.Node.(type) {
		case *pgparse.Node_DiscardStmt:
			// DISCARD ALL (a \c too) / TEMP: the temp relations made so far are gone, and
			// pgx must forget the statements the server deallocated
			if err == nil {
				conn.DeallocateAll(ctx)
				if s == nil {
					s = loadRegressSchema(p.version, &ddl)
				}
				drops := dropTemps(p.version, stmts[:i])
				if st.DiscardStmt.Target == pgparse.DiscardMode_DISCARD_ALL {
					drops = append(drops, sessionResets...)
				}
				for _, d := range drops {
					if s.Apply(d+";\n") == nil {
						ddl = append(ddl, d)
					}
				}
			}
			continue
		case *pgparse.Node_DeallocateStmt:
			if err == nil && st.DeallocateStmt.Isall {
				conn.DeallocateAll(ctx)
			}
		}
		if ts, ok := node.Node.(*pgparse.Node_TransactionStmt); ok {
			switch ts.TransactionStmt.Kind {
			case pgparse.TransactionStmtKind_TRANS_STMT_BEGIN, pgparse.TransactionStmtKind_TRANS_STMT_START:
				txSnap = len(ddl)
				saves = nil
			case pgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK, pgparse.TransactionStmtKind_TRANS_STMT_PREPARE:
				if txSnap >= 0 && len(ddl) > txSnap {
					ddl = ddl[:txSnap]
					s = nil
				}
				txSnap = -1
				saves = nil
			case pgparse.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
				saves = append(saves, savepoint{ts.TransactionStmt.SavepointName, len(ddl)})
			case pgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
				for i := len(saves) - 1; i >= 0; i-- {
					if saves[i].name == ts.TransactionStmt.SavepointName {
						if len(ddl) > saves[i].n {
							ddl = ddl[:saves[i].n]
							s = nil
						}
						saves = saves[:i+1]
						break
					}
				}
			case pgparse.TransactionStmtKind_TRANS_STMT_RELEASE:
				for i := len(saves) - 1; i >= 0; i-- {
					if saves[i].name == ts.TransactionStmt.SavepointName {
						saves = saves[:i]
						break
					}
				}
			default:
				// COMMIT: the loader drops ON COMMIT DROP tables at transaction end
				if err == nil {
					if s == nil {
						s = loadRegressSchema(p.version, &ddl)
					}
					if s.Apply(sql+";\n") == nil {
						ddl = append(ddl, sql)
					}
				}
				txSnap = -1
				saves = nil
			}
			continue
		}
		if err != nil {
			continue
		}
		// extend the analyzer's schema in place; only CREATE EXTENSION forces a rebuild
		if s == nil {
			s = loadRegressSchema(p.version, &ddl)
		}
		switch aerr := s.Apply(sql + ";\n"); {
		case aerr == nil:
			ddl = append(ddl, sql)
		case errors.Is(aerr, schema.ErrNeedsReload) && loadsAlone(p.version, sql):
			ddl = append(ddl, sql)
			s = nil
		}
	}
	if promote {
		// the file's session ends here: its temp relations and session settings go, on the
		// oracle (whose connection the next promoted file reuses) and in the loader's replay
		exec("DISCARD ALL")
		conn.DeallocateAll(ctx)
		conn.Exec(ctx, session)
		ddl = append(ddl, dropTemps(p.version, stmts)...)
		ddl = append(ddl, sessionResets...)
		p.baseDDL = ddl
	}
	return hits
}

// sessionResets undo, in the loader's replay, the settings a session ending resets.
var sessionResets = []string{"RESET search_path", "RESET datestyle", "RESET intervalstyle", "RESET timezone", "RESET xmloption", "RESET restrict_nonsystem_relation_kind"}

// dropTemps lists the DROPs for the temp tables and views the statements created and did
// not drop (what the end of a session, or DISCARD TEMP, takes with it).
func dropTemps(v pgparse.Version, stmts []string) []string {
	temps := map[string]string{} // name → TABLE / VIEW / SEQUENCE
	for _, sql := range stmts {
		tree, err := v.Parse(sql)
		if err != nil || len(tree.Stmts) != 1 {
			continue
		}
		st := tree.Stmts[0].Stmt
		if cs := st.GetCreateStmt(); cs != nil && cs.Relation.Relpersistence == "t" {
			temps[cs.Relation.Relname] = "TABLE"
		}
		if ct := st.GetCreateTableAsStmt(); ct != nil && ct.Into != nil && ct.Into.Rel.Relpersistence == "t" {
			temps[ct.Into.Rel.Relname] = "TABLE"
		}
		if vs := st.GetViewStmt(); vs != nil && vs.View.Relpersistence == "t" {
			temps[vs.View.Relname] = "VIEW"
		}
		if sq := st.GetCreateSeqStmt(); sq != nil && sq.Sequence.Relpersistence == "t" {
			temps[sq.Sequence.Relname] = "SEQUENCE"
		}
		if ds := st.GetDropStmt(); ds != nil {
			for _, on := range ds.Objects {
				items := on.GetList().GetItems()
				if len(items) > 0 {
					delete(temps, items[len(items)-1].GetString_().GetSval())
				}
			}
		}
	}
	var out []string
	for _, kind := range []string{"VIEW", "TABLE", "SEQUENCE"} { // views first: they depend on the tables
		for name, k := range temps {
			if k == kind {
				out = append(out, "DROP "+kind+" IF EXISTS "+name+" CASCADE")
			}
		}
	}
	return out
}

// loadsAlone reports whether the loader takes the statement without a hard error, so
// appending it cannot poison the accumulated DDL.
func loadsAlone(v pgparse.Version, sql string) bool {
	_, err := Load(regressDeclaration(v) + sql + ";\n")
	return err == nil
}

// loadRegressSchema loads the accumulated DDL; a statement the loader rejects
// outright (not merely a Problem) is dropped so the rest still applies.
func loadRegressSchema(v pgparse.Version, ddl *[]string) *schema.Schema {
	for {
		s, err := Load(regressDeclaration(v) + strings.Join(*ddl, ";\n") + ";\n")
		if err == nil {
			return s
		}
		if len(*ddl) == 0 {
			return s
		}
		*ddl = (*ddl)[:len(*ddl)-1]
	}
}

func isRegressQuery(n *pgparse.Node) bool {
	switch n.Node.(type) {
	case *pgparse.Node_SelectStmt, *pgparse.Node_InsertStmt, *pgparse.Node_UpdateStmt,
		*pgparse.Node_DeleteStmt, *pgparse.Node_MergeStmt:
		return true
	}
	return false
}

// split turns a regress file into statements: psql meta-commands are dropped
// (\set and \getenv are interpreted far enough to resolve the data-file paths).
func (p *regressProbe) split(src string) []string {
	vars := map[string]string{"abs_srcdir": p.dir, "abs_builddir": p.dir}
	// a variable means what it was set to at that point (a \set filename before each COPY)
	subst := func(l string) string {
		for k, v := range vars {
			l = strings.ReplaceAll(l, ":'"+k+"'", "'"+v+"'")
			l = strings.ReplaceAll(l, ":"+k, v)
		}
		return l
	}
	var lines []string
	inCopy := false
	for _, l := range strings.Split(src, "\n") {
		tl := strings.TrimSpace(l)
		if inCopy { // COPY ... FROM stdin data, up to the \. terminator
			if tl == `\.` {
				inCopy = false
			}
			continue
		}
		if reStdin.MatchString(tl) {
			inCopy = true
			continue
		}
		if strings.HasPrefix(tl, `\set `) {
			f := strings.Fields(tl)
			if len(f) >= 3 {
				var v strings.Builder
				for _, tok := range f[2:] {
					switch {
					case strings.HasPrefix(tok, ":'"):
						v.WriteString(vars[strings.Trim(tok[1:], "'")])
					case strings.HasPrefix(tok, ":"):
						v.WriteString(vars[tok[1:]])
					case strings.HasPrefix(tok, "'"):
						v.WriteString(strings.Trim(tok, "'"))
					default:
						v.WriteString(tok)
					}
				}
				vars[f[1]] = v.String()
			}
			continue
		}
		if tl == `\c` || strings.HasPrefix(tl, `\c `) || strings.HasPrefix(tl, `\connect`) {
			// a new session: temp objects, prepared statements and settings are gone
			lines = append(lines, "DISCARD ALL;")
			continue
		}
		if strings.HasPrefix(tl, `\`) {
			continue
		}
		lines = append(lines, subst(l))
	}
	text := strings.Join(lines, "\n")
	text = reGset.ReplaceAllString(text, ";")
	text = reMeta.ReplaceAllString(text, "")
	stmts, err := p.version.SplitWithScanner(text, true)
	if err != nil {
		p.t.Logf("split: %v", err)
		return nil
	}
	var out []string
	for _, s := range stmts {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// splitColumnLine breaks "  name type <- table.col null" into name, type, source, nullness.
func splitColumnLine(l string) [4]string {
	var out [4]string
	l = strings.TrimSpace(l)
	name, rest, _ := strings.Cut(l, " ")
	out[0] = name
	typ, src, hasSrc := strings.Cut(rest, " <- ")
	out[1] = typ
	if hasSrc {
		f := strings.Fields(src)
		if len(f) > 0 {
			out[2] = f[0]
			out[3] = strings.Join(f[1:], " ")
		}
	}
	return out
}

// renderAnalyzerSafe turns an analyzer panic into a reportable line instead of ending the probe.
func renderAnalyzerSafe(s *schema.Schema, sql string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("internal error: panic: %v\n", r)
		}
	}()
	return renderAnalyzer(s, sql)
}

// classify names a disagreement so the summary can group it.
func classify(want, got string) (class, key string) {
	wErr, gErr := strings.HasPrefix(want, "error:"), strings.HasPrefix(got, "error:")
	code := func(s string) string {
		s = strings.TrimPrefix(s, "error: ")
		if len(s) >= 5 {
			return s[:5]
		}
		return s
	}
	switch {
	case wErr && gErr:
		return "CODE", code(want) + "->" + code(got)
	case wErr:
		return "LENIENT", code(want)
	case gErr:
		if strings.HasPrefix(got, "internal error") {
			return "STRICT", "internal"
		}
		return "STRICT", code(got)
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) && i < len(gl); i++ {
		if wl[i] == gl[i] {
			continue
		}
		if strings.HasPrefix(wl[i], "  $") {
			return "DIFF", "param type"
		}
		w, g := splitColumnLine(wl[i]), splitColumnLine(gl[i])
		switch {
		case w[0] != g[0]:
			return "DIFF", "column name"
		case w[1] != g[1]:
			return "DIFF", "column type"
		case w[2] != g[2]:
			return "DIFF", "source"
		case w[3] != g[3]:
			return "DIFF", "nullability"
		}
		return "DIFF", "other"
	}
	return "DIFF", "column count"
}
