package dialect

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
	"github.com/kr9ly/sqlshape/v2/x/stmtprobe"
)

var (
	factsN      = flag.Int("facts-probe-n", 300, "statements the facts probe judges")
	factsSeed   = flag.Int64("facts-probe-seed", 1, "seed of the facts probe's generator")
	factsReport = flag.String("facts-probe-report", "", "write the facts probe's report here (default: the test log)")
)

// factsKnownUnreached names the alphabet entries the generator does not produce yet on
// MySQL, each with why; an entry reached anyway is reported as stale.
var factsKnownUnreached = map[string]string{
	"pred contains":      "range types are PostgreSQL's",
	"pred origin view":   "no producer emits FromView: a view's predicates stay in its leaf's Body (x/obligation reads the origin for a view's own pinned columns, which MySQL's producer does not mark)",
	"pred origin policy": "row-level security is PostgreSQL's",
	"leaf function":      "MySQL has no set-returning function in FROM the analyzer records",
	"leaf single":        "a scalar function in FROM is PostgreSQL's",
	"leaf key partial":   "MySQL has no partial index",
	"leaf key temporal":  "MySQL has no temporal key",
	"kind call":          "CALL takes no rows of its own to judge: the routine body's statements are the body probe's",
}

// TestFactsProbe judges the MySQL analyzer's statement facts against a running mysqld:
// x/factsprobe generates schemas, rows and statements, and refutes every claim of the
// facts (a predicate holding on every row, a fixed column, an at-most-one-row proof) with
// the rows the server returns. Skipped without a mysqld on PATH (nix-shell -p mysql84).
func TestFactsProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rep, err := stmtprobe.Run(ctx, stmtprobe.Options{
		Dialect:        stmtprobe.Dialect{Header: "-- sqlshape: mysql 8.4", IntType: "INT", StrType: "VARCHAR(20)", Positional: true},
		Load:           load,
		DB:             db.Conn(),
		Seed:           *factsSeed,
		N:              *factsN,
		KnownUnreached: factsKnownUnreached,
		Log:            t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := rep.String("facts probe (mysql)")
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
