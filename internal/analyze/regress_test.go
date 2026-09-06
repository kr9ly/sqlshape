package analyze

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	for _, name := range tests {
		if len(only) > 0 && !only[name] {
			// Promoted tests still run so the shared database has their objects.
			if !promoted[name] {
				continue
			}
			p.quiet = true
		} else {
			p.quiet = false
		}
		p.runFile(name, promoted[name])
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
	fmt.Fprintf(&sb, "statements: %d compared, %d agree, %d skipped (aborted txn), %d unparsed, %d hits\n",
		p.stats["compared"], p.stats["agree"], p.stats["aborted"], p.stats["unparsed"], len(p.hits))
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
	t     *testing.T
	ctx   context.Context
	o     *oracle.Oracle
	dir   string
	quiet bool

	baseDDL []string // DDL accepted by PG in the promoted tests; the analyzer's shared schema
	hits    []regressHit
	stats   map[string]int
}

var (
	reMeta  = regexp.MustCompile(`(?m)^\s*\\.*$`)
	reGset  = regexp.MustCompile(`\\g[a-z]*\b[^\n]*`)
	reTemp  = regexp.MustCompile(`pg_temp_\d+\.`)
	reStdin = regexp.MustCompile(`(?i)\bfrom\s+std(in|out)\b`)
)

// runFile replays one test file. Promoted files run in the shared database and
// extend baseDDL; others run in a copy of it and leave it untouched.
func (p *regressProbe) runFile(name string, promote bool) {
	src, err := os.ReadFile(filepath.Join(p.dir, "sql", name+".sql"))
	if err != nil {
		p.t.Logf("%s: %v", name, err)
		return
	}
	stmts := p.split(string(src))
	if stmts == nil {
		return
	}
	start := time.Now()
	defer func() { p.t.Logf("%-28s %4d stmts %6.1fs", name, len(stmts), time.Since(start).Seconds()) }()
	ctx := p.ctx
	conn := p.o.Conn()
	dbName := "sqlshape"
	if !promote {
		dbName = "regress_probe"
		if _, err := conn.Exec(ctx, "CREATE DATABASE regress_probe TEMPLATE sqlshape"); err != nil {
			p.t.Logf("%s: create database: %v", name, err)
			return
		}
		if err := p.o.Reconnect(ctx, "regress_probe"); err != nil {
			p.t.Fatalf("%s: %v", name, err)
		}
		defer func() {
			if err := p.o.Reconnect(ctx, "sqlshape"); err != nil {
				p.t.Fatal(err)
			}
			if _, err := p.o.Conn().Exec(ctx, "DROP DATABASE regress_probe"); err != nil {
				p.t.Fatalf("%s: drop database: %v", name, err)
			}
		}()
		conn = p.o.Conn()
	}
	conn.Exec(ctx, "SET statement_timeout = '5s'; SET lock_timeout = '2s'; SET client_min_messages = warning")

	ddl := append([]string(nil), p.baseDDL...)
	if !promote {
		// the template database carries the promoted files' objects but not their session
		// settings (SET search_path ...), so the loader must not replay those either
		ddl = ddl[:0]
		for _, d := range p.baseDDL {
			if tree, err := pg_query.Parse(d); err == nil && len(tree.Stmts) == 1 && tree.Stmts[0].Stmt.GetVariableSetStmt() != nil {
				continue
			}
			ddl = append(ddl, d)
		}
	}
	var s *schema.Schema
	dirty := true
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
			p.stats["unparsed"]++
			conn.Exec(ctx, sql)
			continue
		}
		node := tree.Stmts[0].Stmt
		if isRegressQuery(node) {
			if dirty {
				s = loadRegressSchema(&ddl)
				dirty = false
			}
			want := reTemp.ReplaceAllString(renderOracle(ctx, p.o, sql), "")
			if strings.Contains(want, "conn closed") { // a backend crash took the connection: reconnect and retry
				if err := p.o.Reconnect(ctx, dbName); err != nil {
					p.t.Fatal(err)
				}
				conn = p.o.Conn()
				conn.Exec(ctx, "SET statement_timeout = '5s'; SET lock_timeout = '2s'; SET client_min_messages = warning")
				want = reTemp.ReplaceAllString(renderOracle(ctx, p.o, sql), "")
			}
			if strings.HasPrefix(want, "error: 25P02") {
				p.stats["aborted"]++
			} else {
				got := renderAnalyzerSafe(s, sql)
				p.stats["compared"]++
				if match(want, got) {
					p.stats["agree"]++
				} else if !p.quiet {
					class, key := classify(want, got)
					p.hits = append(p.hits, regressHit{test: name, n: i + 1, class: class, key: key, sql: sql, oracle: want, analyzer: got})
				}
			}
			if _, isSel := node.Node.(*pg_query.Node_SelectStmt); !isSel || node.GetSelectStmt().IntoClause != nil {
				conn.Exec(ctx, sql) // keep data / SELECT INTO state moving; failures mirror psql
			}
			continue
		}
		_, err = conn.Exec(ctx, sql)
		if ts, ok := node.Node.(*pg_query.Node_TransactionStmt); ok {
			switch ts.TransactionStmt.Kind {
			case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN, pg_query.TransactionStmtKind_TRANS_STMT_START:
				txSnap = len(ddl)
				saves = nil
			case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK, pg_query.TransactionStmtKind_TRANS_STMT_PREPARE:
				if txSnap >= 0 && len(ddl) > txSnap {
					ddl = ddl[:txSnap]
					dirty = true
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
							dirty = true
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
		if err == nil && loadsAlone(sql) {
			ddl = append(ddl, sql)
			dirty = true
		}
	}
	if promote {
		p.baseDDL = ddl
	}
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
