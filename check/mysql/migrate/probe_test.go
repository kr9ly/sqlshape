package migrate

// The migrate probe: an oracle for Plan that a hand-written adversarial round cannot be. The
// second adversarial round's migrate lane found six problems, all of one class -- the order
// the plan's DDL runs in against a real server -- and a hand-picked case only ever catches
// the one instance its author thought of. This probe generates schemas from the vocabulary
// the planner handles (column types, generated columns, keys, foreign keys, AUTO_INCREMENT,
// CHECKs, views, triggers, functions), mutates each into a target (columns added / dropped /
// renamed / retyped / moved, keys and foreign keys attached and detached, the primary key
// moved, tables added / dropped / renamed, ENUM labels, views, triggers, functions), and
// judges the plan the way `sqlshape apply` would live: the DDL runs on a server holding the
// source, the result must read back as the target's canonical form, and a second Plan from
// there must be empty. Every pair is a change a real mysqld accepts written as one schema
// (the target canonicalizes cleanly before the plan is judged), so a refused statement or a
// leftover difference is the planner's.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// A failing pair is minimized (mutations removed while the failure stands) and written to
// the report with its source, target, DDL and what went wrong; the test fails on any finding.
// Skipped without a mysqld on PATH (nix-shell -p mysql84).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- judging --------------------------------------------------------------------------

type verdict struct {
	kind    string // "" when the pair passes
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

// judge plans src -> target and runs the plan on a server holding src. The generator's own
// mistakes (a target the server refuses as one schema) are "generator" verdicts, kept apart
// from the planner's.
func judge(ctx context.Context, c dump.Canonicalizer, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := c.Canonical(ctx, v.aSQL)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := c.Canonical(ctx, v.bSQL)
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
	got, _, err := c.Canonical(ctx, aText+"\n"+src.rows()+"\nSET FOREIGN_KEY_CHECKS=1;\n"+strings.Join(ddl, "\n"))
	if err != nil {
		if strings.Contains(err.Error(), "Error 1062") || strings.Contains(err.Error(), "Error 1452") || strings.Contains(err.Error(), "Error 3819") {
			// the rows themselves (a duplicate, a dangling reference, a CHECK): a rows()
			// mistake unless the plan's own statement raised it -- told apart below by
			// loading the rows alone
			if _, _, rerr := c.Canonical(ctx, aText+"\n"+src.rows()); rerr != nil {
				v.kind, v.detail = "generator (rows)", rerr.Error()+"\n"+src.rows()
				return v
			}
		}
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	if changes := diff.Compare(got, b); len(changes) > 0 {
		var d []string
		for _, ch := range changes {
			d = append(d, ch.String())
		}
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

// probeWorkers is how many pairs judge at once. Every Canonical runs in a scratch database
// of its own on the one server, so the draw stays sequential -- the PRNG's stream, and with
// it the report, is what it was when the pairs were judged one after another -- and only
// the judging fans out.
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

// judgePair applies recipe to src and judges the pair; a real finding is minimized (steps
// dropped while the failure stands). applied is what the recipe changed before minimizing,
// nil when no step found anything to apply -- the caller's cue that this attempt did not
// exercise its mutation(s) at all and, for a directed attempt, should be retried against a
// freshly generated schema.
func judgePair(ctx context.Context, scratch dump.Canonicalizer, src *pSchema, recipe []step) (v verdict, applied []string) {
	target, applied := mutate(src, recipe)
	if len(applied) == 0 {
		return verdict{}, nil
	}
	v = judge(ctx, scratch, src, target, applied)
	if v.kind == "" || strings.HasPrefix(v.kind, "generator") {
		return v, applied
	}
	for j := 0; j < len(recipe); {
		shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
		tgt, app := mutate(src, shorter)
		if len(app) == 0 {
			j++
			continue
		}
		if w := judge(ctx, scratch, src, tgt, app); w.kind == v.kind {
			recipe, v = shorter, w
			continue
		}
		j++
	}
	return v, applied
}

// tally records a judged pair: byMutation by what it applied, hit by which (Op, Kind,
// Field) triples (diff.Alphabet) it exercised whatever judge made of it afterward, counts by
// the verdict; a failure the generator itself produced is logged (the first three) and
// dropped, a real finding kept.
func tally(t *testing.T, label string, v verdict, applied []string, counts, byMutation map[string]int, hit map[string]bool, findings *[]verdict) {
	for _, a := range applied {
		byMutation[a]++
	}
	for _, ch := range v.changes {
		if ch.Op == diff.Alter {
			for _, f := range ch.Fields {
				hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind, Field: f.Name}.String()] = true
			}
			continue
		}
		hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind}.String()] = true
	}
	switch {
	case v.kind == "":
		counts["pass"]++
	case strings.HasPrefix(v.kind, "generator"):
		counts[v.kind]++
		if counts[v.kind] <= 3 {
			t.Logf("%s (%s, %s): %s\n%s", v.kind, label, strings.Join(applied, "; "), v.detail, v.aSQL)
		}
	default:
		counts["finding"]++
		*findings = append(*findings, v)
	}
}

// runPair is judgePair followed by tally; the zero verdict, untallied, when nothing applied.
func runPair(ctx context.Context, scratch dump.Canonicalizer, label string, src *pSchema, recipe []step,
	counts map[string]int, byMutation map[string]int, hit map[string]bool, findings *[]verdict, t *testing.T) verdict {
	v, applied := judgePair(ctx, scratch, src, recipe)
	if applied == nil {
		return verdict{}
	}
	tally(t, label, v, applied, counts, byMutation, hit, findings)
	return v
}

func TestMigrateProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	srv, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	} else if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	scratch, err := dump.NewScratch(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewSource(*probeSeed))
	counts := map[string]int{}
	byMutation := map[string]int{}
	hit := map[string]bool{}
	var findings []verdict
	type pair struct {
		i       int
		src     *pSchema
		recipe  []step
		applied []string
		v       verdict
	}
	var pairs []*pair
	for i := 0; i < *probeN; i++ {
		src := generate(r)
		n := 1 + r.Intn(5)
		var recipe []step
		for j := 0; j < n; j++ {
			recipe = append(recipe, step{m: r.Intn(len(mutations)), seed: r.Int63()})
		}
		pairs = append(pairs, &pair{i: i, src: src, recipe: recipe})
	}
	parallel(pairs, func(p *pair) { p.v, p.applied = judgePair(ctx, scratch, p.src, p.recipe) })
	for _, p := range pairs {
		if p.applied == nil {
			continue
		}
		tally(t, fmt.Sprintf("pair %d", p.i), p.v, p.applied, counts, byMutation, hit, &findings)
	}
	hitRandom := len(hit)

	// directed coverage: the random draw above is what an earlier round's gate relied on
	// entirely (200 pairs of seed 1 "happening" to reach every alphabet entry), which breaks
	// the moment a new mutation shifts the PRNG's draws out from under an existing one (measured:
	// adding a mutation here once made an existing "~ domain check <name>" stop being hit at
	// seed 1). This applies each mutation completely alone, against a schema fresh enough for
	// it, at least once, regardless of what the random pairs above happened to draw -- so the
	// gate's coverage no longer depends on chance. A mutation that finds nothing to apply to a
	// given fresh schema (untouched candidates the generator did not happen to produce) just
	// draws another; one that still finds nothing after directedTries schemas is reported as
	// unable to run standalone and fails the gate below (as opposed to reachable only combined
	// with another mutation first, which this loop does not claim to rule out).
	const directedTries = 30
	var unusable []string
	for m, mut := range mutations {
		ok := false
		for try := 0; try < directedTries && !ok; try++ {
			src := generate(r)
			recipe := []step{{m: m, seed: r.Int63()}}
			label := fmt.Sprintf("directed %q try %d", mut.name, try)
			v := runPair(ctx, scratch, label, src, recipe, counts, byMutation, hit, &findings, t)
			if v.applied == nil {
				continue // this schema had no candidate for the mutation: draw another
			}
			if strings.HasPrefix(v.kind, "generator") {
				continue // a bad pairing, not this mutation's fault: draw another
			}
			ok = true
		}
		if !ok {
			unusable = append(unusable, mut.name)
		}
	}
	sort.Strings(unusable)

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
	t.Logf("migrate probe, seed %d, %d pairs:\n%s", *probeSeed, *probeN, summary.String())

	// the same failure minimized from different pairs reads the same: dedupe by kind + DDL
	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe, seed %d, %d pairs\n\n%s\n", *probeSeed, *probeN, summary.String())
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
	fmt.Fprintf(&report, "## alphabet coverage\n\n%d entries, %d hit by the random pairs alone, %d hit once directed is added\n\n", len(alphabet), hitRandom, len(hit))
	var newlyUnusable []string
	fmt.Fprintf(&report, "### directed coverage: applying every mutation alone\n\n")
	for _, name := range unusable {
		if reason := directedKnownUnusable[name]; reason != "" {
			fmt.Fprintf(&report, "- [ ] %s -- known unusable standalone: %s\n", name, reason)
			continue
		}
		fmt.Fprintf(&report, "- [ ] %s -- MISSED (could not apply alone after %d fresh schemas)\n", name, directedTries)
		newlyUnusable = append(newlyUnusable, name)
	}
	if len(unusable) == 0 {
		fmt.Fprintf(&report, "every mutation applied standalone at least once\n")
	}
	fmt.Fprintln(&report)
	for _, e := range alphabet {
		s := e.String()
		switch {
		case hit[s]:
			fmt.Fprintf(&report, "- [x] %s\n", s)
		case alphabetKnownUnreached[s] != "":
			fmt.Fprintf(&report, "- [ ] %s -- known unreached: %s\n", s, alphabetKnownUnreached[s])
		default:
			fmt.Fprintf(&report, "- [ ] %s -- MISSED\n", s)
			missed = append(missed, s)
		}
	}
	// a known-unreached entry diff.Alphabet no longer lists is stale (the field moved, was
	// renamed, or the planner learned to emit DDL for it): flag it so the list stays
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
	// a directedKnownUnusable entry this run's directed pass did apply standalone is stale
	// the same way: the mutation (or a companion one earlier in mutations) changed and the
	// allowance no longer describes reality.
	unusableNow := map[string]bool{}
	for _, name := range unusable {
		unusableNow[name] = true
	}
	var staleUnusable []string
	for name := range directedKnownUnusable {
		if !unusableNow[name] {
			staleUnusable = append(staleUnusable, name)
		}
	}
	sort.Strings(staleUnusable)
	for _, s := range staleUnusable {
		fmt.Fprintf(&report, "- stale directedKnownUnusable entry (applied standalone this run): %s\n", s)
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
	sort.Strings(newlyUnusable)
	if len(newlyUnusable) > 0 {
		t.Errorf("%d mutation(s) could not be applied standalone by directed coverage and are not in directedKnownUnusable: %s", len(newlyUnusable), strings.Join(newlyUnusable, ", "))
	}
	if len(staleUnusable) > 0 {
		t.Errorf("%d stale directedKnownUnusable entries (applied standalone this run): %s", len(staleUnusable), strings.Join(staleUnusable, ", "))
	}
}
