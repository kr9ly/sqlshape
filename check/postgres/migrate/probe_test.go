package migrate

// The migrate probe, PostgreSQL side: the same oracle check/mysql/migrate has (see its
// probe_test.go for the reasoning). Schemas are generated from the planner's vocabulary
// (column types, an ENUM type, identity and generated columns, unique constraints and
// indexes, foreign keys, CHECKs, table comments, views, functions, trigger functions with
// their triggers), mutated into a target (columns added / dropped / renamed / widened /
// re-nulled, constraints and indexes attached and detached, the primary key moved onto an
// existing or a new column, tables added / dropped / renamed, ENUM labels added and dropped,
// views, triggers and functions added / dropped / changed, generated expressions changed),
// and judged the way `sqlshape apply` runs the plan: the DDL applied to a database holding
// the source must read back as the target, column order aside (PostgreSQL cannot move a
// column; Verify reports order as notes), and a second plan from there must be empty.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// Skipped without pg_dump on PATH (nix-shell -p postgresql_17).

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- judging ----------------------------------------------------------------------------

type verdict struct {
	kind    string
	detail  string
	ddl     []string
	aSQL    string
	bSQL    string
	applied []string
	// changes is diff.Compare(a, b): the planner's actual input, kept so the probe can
	// tally which (Op, Kind, Field) triples (diff.Alphabet) the pair exercised, whatever
	// judge makes of it afterward.
	changes []diff.Change
}

func judge(ctx context.Context, srv *dump.Server, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := srv.Canonical(ctx, v.aSQL, nil)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := srv.Canonical(ctx, v.bSQL, nil)
	if err != nil {
		v.kind, v.detail = "generator (target)", err.Error()
		return v
	}
	if len(a.Problems) > 0 || len(b.Problems) > 0 {
		v.kind, v.detail = "generator (problems)", fmt.Sprint(a.Problems, b.Problems)
		return v
	}
	v.changes = diff.Compare(a, b)
	intents, err := ParseIntents(v.bSQL)
	if err != nil {
		v.kind, v.detail = "generator (intents)", err.Error()
		return v
	}
	ddl, err := Plan(a, b, intents)
	v.ddl = ddl
	if err != nil {
		v.kind, v.detail = "plan refused", err.Error()
		return v
	}
	// the source database holds rows: what apply runs against is never empty
	loaded := aText + "\nRESET search_path;\n" + src.rows()
	script := loaded + "\n" + strings.Join(ddl, "\n")
	if len(target.checks) > 0 {
		// a mutation's own data assertions (so far: a partition bound shrinking past a
		// row it used to hold, "shrink partition bound") run in the same script, right
		// after the plan's DDL and before judge() ever compares schemas -- a RAISE
		// EXCEPTION here surfaces as a plain Canonical error below, same as a bad DDL
		// statement would.
		script += "\n" + strings.Join(target.checks, "\n")
	}
	got, _, err := srv.Canonical(ctx, script, b)
	if err != nil {
		if _, _, rerr := srv.Canonical(ctx, loaded, a); rerr != nil {
			v.kind, v.detail = "generator (rows)", rerr.Error()+"\n"+src.rows()
			return v
		}
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	var d []string
	for _, ch := range diff.Compare(got, b) {
		if !ch.OrderOnly() {
			d = append(d, ch.String())
		}
	}
	if len(d) > 0 {
		v.kind, v.detail = "DDL does not reach the target", strings.Join(d, "\n")
		return v
	}
	again, err := Plan(got, b, nil)
	if err != nil || len(again) > 0 {
		v.kind, v.detail = "a second plan is not empty", fmt.Sprintf("%v\n%s", err, strings.Join(again, "\n"))
		return v
	}
	return v
}

func TestMigrateProbe(t *testing.T) {
	requirePgDump(t)
	runMigrateProbe(t, server, mutations, false, "postgres", *probeSeed, *probeN)
}

func TestMigrateProbe18(t *testing.T) {
	requirePgDump18(t)
	muts := append(append([]mutation(nil), mutations...), mutations18...)
	runMigrateProbe(t, server18, muts, true, "postgres18", *probeSeed, *probeN)
}

// probeWorkers is how many pairs judge at once. Every pair has a database of its own on
// the one server (dump.Server.Canonical is safe for concurrent use), so the draw stays
// sequential -- the PRNG's stream, and with it the report, is the same as it was when the
// pairs were judged one after another -- and only the judging fans out.
var probeWorkers = min(8, runtime.GOMAXPROCS(0))

// parallel runs fn over items on probeWorkers goroutines and returns when all are done.
func parallel[T any](items []T, fn func(T)) {
	var wg sync.WaitGroup
	next := make(chan T)
	for w := 0; w < probeWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range next {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		next <- it
	}
	close(next)
	wg.Wait()
}

// pair is one random draw of the probe: a source schema, the recipe applied to it, and
// after judging its verdict (recipe and verdict shrunk together when it found something).
type pair struct {
	i       int
	src     *pSchema
	target  *pSchema
	recipe  []step
	applied []string
	v       verdict
}

func runMigrateProbe(t *testing.T, srv *dump.Server, muts []mutation, pg18 bool, reportName string, seed int64, n int) {
	ctx := context.Background()
	r := rand.New(rand.NewSource(seed))
	counts := map[string]int{}
	byMutation := map[string]int{}
	hit := map[string]bool{}
	var findings []verdict
	var pairs []*pair
	for i := 0; i < n; i++ {
		src := generate(r, pg18)
		nmut := 1 + r.Intn(5)
		var recipe []step
		for j := 0; j < nmut; j++ {
			recipe = append(recipe, step{m: r.Intn(len(muts)), seed: r.Int63()})
		}
		target, applied := mutate(muts, src, recipe)
		if len(applied) == 0 {
			continue
		}
		pairs = append(pairs, &pair{i: i, src: src, target: target, recipe: recipe, applied: applied})
	}
	parallel(pairs, func(p *pair) {
		p.v = judge(ctx, srv, p.src, p.target, p.applied)
		if p.v.kind == "" || strings.HasPrefix(p.v.kind, "generator") {
			return
		}
		// a finding: drop mutations while the failure stands
		recipe, v := p.recipe, p.v
		for j := 0; j < len(recipe); {
			shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
			tgt, app := mutate(muts, p.src, shorter)
			if len(app) == 0 {
				j++
				continue
			}
			if w := judge(ctx, srv, p.src, tgt, app); w.kind == v.kind {
				recipe, v = shorter, w
				continue
			}
			j++
		}
		p.recipe, p.v = recipe, v
	})
	for _, p := range pairs {
		v := p.v
		for _, a := range p.applied {
			byMutation[a]++
		}
		tallyHit(hit, v.changes)
		if v.kind == "" {
			counts["pass"]++
			continue
		}
		if strings.HasPrefix(v.kind, "generator") {
			counts[v.kind]++
			if counts[v.kind] <= 3 {
				t.Logf("%s (pair %d, %s): %s\n%s", v.kind, p.i, strings.Join(p.applied, "; "), v.detail, v.aSQL)
			}
			continue
		}
		counts["finding"]++
		findings = append(findings, v)
	}
	randomHit := len(hit)
	stuck := directedCoverage(t, ctx, srv, muts, pg18, seed, counts, byMutation, hit, &findings)
	combinedHit := len(hit)

	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var summary strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&summary, "%s: %d\n", k, counts[k])
	}
	var ms []string
	for m := range byMutation {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	for _, m := range ms {
		fmt.Fprintf(&summary, "  %-45s %d\n", m, byMutation[m])
	}
	t.Logf("migrate probe (%s), seed %d, %d pairs:\n%s", reportName, seed, n, summary.String())

	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe (%s), seed %d, %d pairs\n\n%s\n", reportName, seed, n, summary.String())
	distinct := 0
	for _, v := range findings {
		key := v.kind + "\n" + v.detail
		if seen[key] {
			continue
		}
		seen[key] = true
		distinct++
		fmt.Fprintf(&report, "## %d. %s\n\nmutations: %s\n\n%s\n\n### source\n\n```sql\n%s```\n\n### target\n\n```sql\n%s```\n\n### plan\n\n```sql\n%s\n```\n\n",
			distinct, v.kind, strings.Join(v.applied, "; "), v.detail, v.aSQL, v.bSQL, strings.Join(v.ddl, "\n"))
	}
	alphabet, err := diff.Alphabet()
	if err != nil {
		t.Fatalf("diff.Alphabet: %v", err)
	}
	var missed []string
	fmt.Fprintf(&report, "## alphabet coverage\n\n%d entries, %d hit (%d by the random pairs alone, %d with directed coverage added)\n\n",
		len(alphabet), len(hit), randomHit, combinedHit)
	if len(stuck) > 0 {
		fmt.Fprintf(&report, "%d mutation(s) never found a candidate alone in %d attempts: %s\n\n", len(stuck), directedAttempts, strings.Join(stuck, ", "))
	}
	for _, e := range alphabet {
		s := e.String()
		switch {
		case hit[s]:
			fmt.Fprintf(&report, "- [x] %s\n", s)
		case isKnownUnreached(s, pg18) != "":
			fmt.Fprintf(&report, "- [ ] %s -- known unreached: %s\n", s, isKnownUnreached(s, pg18))
		default:
			fmt.Fprintf(&report, "- [ ] %s -- MISSED\n", s)
			missed = append(missed, s)
		}
	}
	// a known-unreached entry diff.Alphabet no longer lists is stale (the field moved,
	// was renamed, or the planner learned to emit DDL for it): flag it so the list stays
	// honest instead of silently over-forgiving forever.
	known := map[string]bool{}
	for _, e := range alphabet {
		known[e.String()] = true
	}
	var stale []string
	for s := range alphabetKnownUnreached {
		if !known[s] {
			stale = append(stale, s)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		fmt.Fprintf(&report, "- stale known-unreached entry (not in the current alphabet): %s\n", s)
	}

	if *probeReport != "" {
		if err := os.WriteFile(*probeReport, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *probeReport)
	} else if distinct > 0 {
		t.Log(report.String())
	}
	if distinct > 0 {
		t.Errorf("%d distinct findings (%d pairs)", distinct, counts["finding"])
	}
	sort.Strings(missed)
	if len(missed) > 0 {
		t.Errorf("%d/%d alphabet entries neither hit nor known-unreached:\n%s", len(missed), len(alphabet), strings.Join(missed, "\n"))
	}
	if len(stale) > 0 {
		t.Errorf("%d stale alphabetKnownUnreached entries (not in diff.Alphabet): %s", len(stale), strings.Join(stale, ", "))
	}
	if len(stuck) > 0 {
		t.Errorf("%d mutation(s) never found a candidate alone in %d directed attempts: %s", len(stuck), directedAttempts, strings.Join(stuck, ", "))
	}
}
