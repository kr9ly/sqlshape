package dialect

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/v2/x/factsprobe"
)

var (
	factsN      = flag.Int("facts-probe-n", 300, "statements the facts probe judges")
	factsSeed   = flag.Int64("facts-probe-seed", 1, "seed of the facts probe's generator")
	factsReport = flag.String("facts-probe-report", "", "write the facts probe's report here (default: the test log)")
)

// factsKnownUnreached names the alphabet entries the generator does not produce yet on
// PostgreSQL, each with why; an entry reached anyway is reported as stale.
var factsKnownUnreached = map[string]string{
	"pred contains":      "range columns and Contains predicates are not generated yet",
	"pred origin view":   "no producer emits FromView: a view's predicates stay in its leaf's Body",
	"pred origin policy": "row-level security policies are not generated yet",
	"leaf function":      "a set-returning function in FROM is not generated yet",
	"leaf single":        "a scalar function in FROM is not generated yet",
	"leaf key partial":   "partial unique indexes are not generated yet",
	"leaf key temporal":  "WITHOUT OVERLAPS keys are PostgreSQL 18's and not generated yet",
	"kind call":          "CALL takes no rows of its own to judge",
}

// TestFactsProbe judges the PostgreSQL analyzer's statement facts against the embedded
// server: x/factsprobe generates schemas, rows and statements, and refutes every claim of
// the facts (a predicate holding on every row, a fixed column, an at-most-one-row proof)
// with the rows the server returns.
func TestFactsProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	db, err := sql.Open("pgx", o.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rep, err := factsprobe.Run(ctx, factsprobe.Options{
		Dialect:        factsprobe.Dialect{Header: "-- sqlshape: postgres 17", IntType: "integer", StrType: "text"},
		Load:           Load,
		DB:             db,
		Seed:           *factsSeed,
		N:              *factsN,
		KnownUnreached: factsKnownUnreached,
		Log:            t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := rep.String("postgres")
	if *factsReport != "" {
		if err := os.WriteFile(*factsReport, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *factsReport)
	} else {
		t.Log(text)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("%d finding(s), see the report", len(rep.Findings))
	}
	if len(rep.Missed) > 0 {
		t.Errorf("alphabet entries never reached: %s", strings.Join(rep.Missed, ", "))
	}
	if len(rep.Stale) > 0 {
		t.Errorf("known-unreached entries that were reached (drop them from the list): %s", strings.Join(rep.Stale, ", "))
	}
}
