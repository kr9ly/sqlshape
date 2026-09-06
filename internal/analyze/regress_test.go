package analyze

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/oracle"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// The regress probe replays PostgreSQL's own regression corpus (src/test/regress)
// against a live PG and the analyzer side by side, and reports every statement on
// which they disagree. It is a discovery tool, not a gate: nothing here is asserted.
//
//	go test ./internal/analyze -run TestRegress -regress /path/to/src/test/regress \
//	    [-regress-tests select,join] [-regress-report /path/report.txt]
var (
	regressDir    = flag.String("regress", "", "path to PG's src/test/regress; enables TestRegress")
	regressTests  = flag.String("regress-tests", "", "comma-separated test names to run (default: parallel_schedule order)")
	regressReport = flag.String("regress-report", "", "write the per-statement report here (default: stderr summary only)")
	regressJobs   = flag.Int("regress-jobs", 0, "parallel workers for the non-promoted files (default: NumCPU, at most 8)")
)

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
	if *regressDir == "" {
		t.Skip("-regress not set")
	}
	dir, err := filepath.Abs(*regressDir)
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
	o, err := oracle.Start(ctx, "")
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()

	p := &regressProbe{t: t, ctx: ctx, o: o, dir: dir}
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
}

type regressProbe struct {
	t   *testing.T
	ctx context.Context
	o   *oracle.Oracle
	dir string

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
	// reCrash matches statements whose Prepare segfaults the oracle (PG 17: MERGE ...
	// INSERT into a partitioned table with a VALUES source referencing a target column);
	// every crash restarts the server under all the workers, so they are skipped
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
		tree, err := pg_query.Parse(sql)
		if err != nil || len(tree.Stmts) == 0 {
			p.count("unparsed")
			exec(sql)
			continue
		}
		node := tree.Stmts[0].Stmt
		if isRegressQuery(node) {
			if reCrash.MatchString(sql) {
				p.count("crash-skipped")
				continue
			}
			if s == nil {
				s = loadRegressSchema(&ddl)
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
			if _, isSel := node.Node.(*pg_query.Node_SelectStmt); !isSel || node.GetSelectStmt().IntoClause != nil {
				err := exec(sql) // keep data / SELECT INTO state moving; failures mirror psql
				if err == nil && node.GetSelectStmt().GetIntoClause() != nil {
					// SELECT INTO made a table: the analyzer's schema gets it too
					if s == nil {
						s = loadRegressSchema(&ddl)
					}
					if s.Apply(sql+";\n") == nil {
						ddl = append(ddl, sql)
					}
				}
			}
			continue
		}
		err = exec(sql)
		if ts, ok := node.Node.(*pg_query.Node_TransactionStmt); ok {
			switch ts.TransactionStmt.Kind {
			case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN, pg_query.TransactionStmtKind_TRANS_STMT_START:
				txSnap = len(ddl)
				saves = nil
			case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK, pg_query.TransactionStmtKind_TRANS_STMT_PREPARE:
				if txSnap >= 0 && len(ddl) > txSnap {
					ddl = ddl[:txSnap]
					s = nil
				}
				txSnap = -1
				saves = nil
			case pg_query.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
				saves = append(saves, savepoint{ts.TransactionStmt.SavepointName, len(ddl)})
			case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
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
			case pg_query.TransactionStmtKind_TRANS_STMT_RELEASE:
				for i := len(saves) - 1; i >= 0; i-- {
					if saves[i].name == ts.TransactionStmt.SavepointName {
						saves = saves[:i]
						break
					}
				}
			default:
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
			s = loadRegressSchema(&ddl)
		}
		switch aerr := s.Apply(sql + ";\n"); {
		case aerr == nil:
			ddl = append(ddl, sql)
		case errors.Is(aerr, schema.ErrNeedsReload) && loadsAlone(sql):
			ddl = append(ddl, sql)
			s = nil
		}
	}
	if promote {
		// temp tables died with this file's session: drop the ones still standing
		temps := map[string]bool{}
		for _, sql := range stmts {
			tree, err := pg_query.Parse(sql)
			if err != nil || len(tree.Stmts) != 1 {
				continue
			}
			st := tree.Stmts[0].Stmt
			if cs := st.GetCreateStmt(); cs != nil && cs.Relation.Relpersistence == "t" {
				temps[cs.Relation.Relname] = true
			}
			if ct := st.GetCreateTableAsStmt(); ct != nil && ct.Into != nil && ct.Into.Rel.Relpersistence == "t" {
				temps[ct.Into.Rel.Relname] = true
			}
			if ds := st.GetDropStmt(); ds != nil && ds.RemoveType == pg_query.ObjectType_OBJECT_TABLE {
				for _, on := range ds.Objects {
					items := on.GetList().GetItems()
					delete(temps, items[len(items)-1].GetString_().GetSval())
				}
			}
		}
		for name := range temps {
			// the oracle's session lives on across files, so drop them there too
			exec("DROP TABLE IF EXISTS " + name)
			ddl = append(ddl, "DROP TABLE IF EXISTS "+name)
		}
		// and so did its session settings: the template database starts every later file
		// with the defaults, and the loader's replay must land there too
		ddl = append(ddl, "RESET search_path", "RESET datestyle", "RESET intervalstyle", "RESET timezone", "RESET xmloption", "RESET restrict_nonsystem_relation_kind")
		p.baseDDL = ddl
	}
	return hits
}

// loadsAlone reports whether the loader takes the statement without a hard error, so
// appending it cannot poison the accumulated DDL.
func loadsAlone(sql string) bool {
	_, err := schema.Load(sql + ";\n")
	return err == nil
}

// loadRegressSchema loads the accumulated DDL; a statement the loader rejects
// outright (not merely a Problem) is dropped so the rest still applies.
func loadRegressSchema(ddl *[]string) *schema.Schema {
	for {
		s, err := schema.Load(strings.Join(*ddl, ";\n") + ";\n")
		if err == nil {
			return s
		}
		if len(*ddl) == 0 {
			return s
		}
		*ddl = (*ddl)[:len(*ddl)-1]
	}
}

func isRegressQuery(n *pg_query.Node) bool {
	switch n.Node.(type) {
	case *pg_query.Node_SelectStmt, *pg_query.Node_InsertStmt, *pg_query.Node_UpdateStmt,
		*pg_query.Node_DeleteStmt, *pg_query.Node_MergeStmt:
		return true
	}
	return false
}

// split turns a regress file into statements: psql meta-commands are dropped
// (\set and \getenv are interpreted far enough to resolve the data-file paths).
func (p *regressProbe) split(src string) []string {
	vars := map[string]string{"abs_srcdir": p.dir, "abs_builddir": p.dir}
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
		if strings.HasPrefix(tl, `\`) {
			continue
		}
		lines = append(lines, l)
	}
	text := strings.Join(lines, "\n")
	text = reGset.ReplaceAllString(text, ";")
	for k, v := range vars {
		text = strings.ReplaceAll(text, ":'"+k+"'", "'"+v+"'")
		text = strings.ReplaceAll(text, ":"+k, v)
	}
	text = reMeta.ReplaceAllString(text, "")
	stmts, err := pg_query.SplitWithScanner(text, true)
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
