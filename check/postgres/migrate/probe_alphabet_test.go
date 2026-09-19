package migrate

// Split from probe_test.go (see its header): the diff.Alphabet() coverage unit -- every
// (Op, Kind, Field) triple diff.Compare can report, the ones the probe accepts as
// structurally unreachable instead of requiring a mutation to produce, and directed
// coverage (one pass applying every mutation alone, so the alphabet's hit set does not
// depend on which mutations 200 random pairs happen to draw). Pure move.

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
)

var alphabetKnownUnreached = map[string]string{
	"~ table inherits":              "migrate.go's alterTable reports inherits/of type changes as a problem (\"has no lossless DDL\", the same ruling as a partition key change); never DDL. Regular table INHERITS (distinct from PARTITION OF, in this round's vocabulary) is still out of this probe's vocabulary",
	"~ table of type":               "same problem-only path as table inherits; typed tables (CREATE TABLE OF) are also not in this probe's vocabulary",
	"~ domain base":                 "migrate.go's alterType domain case reports a base type change as a problem (\"has no lossless DDL\"); never DDL",
	"~ range subtype":               "migrate.go's alterType range case reports any change as a problem (\"has no lossless DDL\"); never DDL. The generator also has no range type vocabulary",
	"~ constraint without overlaps": "a PostgreSQL 18 constraint attribute (WITHOUT OVERLAPS). Under the 17 gate the server refuses the syntax outright, so this stays an accepted excuse there; under 18, generate() itself seeds a temporal key from the start (mutations18's \"toggle key without overlaps\" then flips its flag in place) and pg18OnlyUnreached drops this excuse, so 18 requires an actual hit",
	"~ constraint period":           "a PostgreSQL 18 constraint attribute (FOREIGN KEY ... PERIOD, needing a WITHOUT OVERLAPS primary key on the referenced side): under the 17 gate the server refuses the syntax outright, so this stays an accepted excuse there; under 18, generate() seeds an ordinary foreign key resting on that same temporal key from the start (mutations18's \"toggle foreign key period\" widens it onto the key's own range column, PERIOD) and pg18OnlyUnreached drops this excuse, so 18 requires an actual hit",
	"~ column generated kind":       "a PostgreSQL 18 generated-column form (VIRTUAL, versus this probe's STORED); same PostgreSQL 17 server ceiling as \"without overlaps\"",
	"~ constraint not enforced":     "a PostgreSQL 18 constraint attribute (NOT ENFORCED); same PostgreSQL 17 server ceiling as \"without overlaps\"",
	"~ index unique":                "structurally unreachable (measured): pg_dump always renders a named UNIQUE constraint as ALTER TABLE ... ADD CONSTRAINT, never a CREATE INDEX + ADD CONSTRAINT ... UNIQUE USING INDEX pair, so toggling a key between UNIQUE and a plain index changes its diff.Change Kind (constraint <-> index) rather than the same-named index's \"unique\" property -- no vocabulary addition reaches it, since the generator would have to reproduce a pg_dump form pg_dump itself never produces",
}

// pg18OnlyUnreached is the subset of alphabetKnownUnreached that stops being an accepted
// excuse under TestMigrateProbe18 (brief-holes-pg.md item E): PostgreSQL 18 syntax
// mutations18 exercises there, so alphabetKnownUnreached's own text (written for the 17
// gate, where the server itself refuses the syntax) does not apply -- isKnownUnreached
// reports these as needing an actual hit instead once pg18 is true.
var pg18OnlyUnreached = map[string]bool{
	"~ column generated kind":       true,
	"~ constraint not enforced":     true,
	"~ constraint without overlaps": true,
	"~ constraint period":           true,
}

// isKnownUnreached is alphabetKnownUnreached's excuse for s, "" when none applies --
// except a pg18OnlyUnreached entry under TestMigrateProbe18, whose whole excuse was the 17
// server's syntax ceiling: gone once pg18 is true, so it must be hit like anything else.
func isKnownUnreached(s string, pg18 bool) string {
	if pg18 && pg18OnlyUnreached[s] {
		return ""
	}
	return alphabetKnownUnreached[s]
}

// tallyHit folds one judged pair's diff.Compare output into the alphabet coverage set:
// every Alter's Fields as their own (Op, Kind, Field) entries, every Add / Drop as one
// (Op, Kind) entry with no Field (matching diff.Alphabet's own grouping for rows and
// comment's single field, and for schema / extension which are never Alter).
func tallyHit(hit map[string]bool, changes []diff.Change) {
	for _, ch := range changes {
		if ch.Op == diff.Alter {
			for _, f := range ch.Fields {
				hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind, Field: diff.NormalizeField(ch.Kind, f.Name)}.String()] = true
			}
			continue
		}
		hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind}.String()] = true
	}
}

// directedAttempts bounds how many freshly generated schemas one mutation gets to find a
// candidate against on its own, in directedCoverage, before it is reported unable to
// apply alone.
const directedAttempts = 30

// directedCoverage runs one pair per mutation -- that mutation alone against a freshly
// generated schema, retried against a new schema up to directedAttempts times if it finds
// no candidate -- so the alphabet coverage gate no longer depends on TestMigrateProbe's
// random pairs happening to draw every mutation at least once. That dependency is real:
// adding a mutation shifts every later draw's position in the PRNG sequence, so a vocabulary
// addition can silently stop a *different*, already-covered alphabet entry from being hit
// by the random pairs at the same seed (brief-holes-common.md's whole reason for this
// pass). judge and the finding / hit bookkeeping are exactly what the random loop does;
// unlike a random pair (up to 7 mutations), a directed pair is already minimal, so there is
// nothing to shrink if it turns up a finding.
func directedCoverage(t *testing.T, ctx context.Context, srv *dump.Server, muts []mutation, pg18 bool, seed int64, counts, byMutation map[string]int, hit map[string]bool, findings *[]verdict) []string {
	// every mutation draws from a PRNG of its own, so the mutations are independent and
	// are judged side by side (see parallel); the tally below runs in their order
	type directed struct {
		applied []string
		v       verdict
	}
	results := make([]directed, len(muts))
	indices := make([]int, len(muts))
	for i := range indices {
		indices[i] = i
	}
	parallel(indices, func(mi int) {
		r := rand.New(rand.NewSource(seed*100003 + int64(mi)))
		for attempt := 0; attempt < directedAttempts; attempt++ {
			src := generate(r, pg18)
			target, applied := mutate(muts, src, []step{{m: mi, seed: r.Int63()}})
			if len(applied) > 0 {
				results[mi] = directed{applied: applied, v: judge(ctx, srv, src, target, applied)}
				return
			}
		}
	})
	var stuck []string
	for mi, m := range muts {
		res := results[mi]
		if len(res.applied) == 0 {
			stuck = append(stuck, m.name)
			continue
		}
		v := res.v
		for _, a := range res.applied {
			byMutation[a]++
		}
		tallyHit(hit, v.changes)
		switch {
		case v.kind == "":
			counts["pass"]++
		case strings.HasPrefix(v.kind, "generator"):
			counts[v.kind]++
			if counts[v.kind] <= 3 {
				t.Logf("%s (directed, %s): %s\n%s", v.kind, m.name, v.detail, v.aSQL)
			}
		default:
			counts["finding"]++
			*findings = append(*findings, v)
		}
	}
	return stuck
}
