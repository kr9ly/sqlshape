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
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2/x/stmtprobe"
)

var (
	failN      = flag.Int("failure-probe-n", 300, "writes the failure probe judges")
	failSeed   = flag.Int64("failure-probe-seed", 1, "seed of the failure probe's generator")
	failReport = flag.String("failure-probe-report", "", "write the failure probe's report here (default: the test log)")
)

// failureKnownUnreached names the failure alphabet's entries this dialect's run does not
// reach, each with why.
var failureKnownUnreached = map[string]string{
	"insert ignore": "INSERT IGNORE is MySQL's (ON CONFLICT DO NOTHING is the PostgreSQL shape)",
	"replace":       "REPLACE is MySQL's",
}

// TestFailureProbe judges the PostgreSQL analyzer's predicted failure modes against the
// embedded server: x/stmtprobe generates writes against a schema carrying every
// constraint kind, runs them, and requires every constraint error the server raises to be
// one of the predicted violations under a key postgres.Violates matches.
func TestFailureProbe(t *testing.T) {
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
	rep, err := stmtprobe.RunFailures(ctx, stmtprobe.FailureOptions{
		Options: stmtprobe.Options{
			Dialect:        stmtprobe.Dialect{Header: "-- sqlshape: postgres 17", IntType: "integer", StrType: "text"},
			Load:           Load,
			DB:             db,
			Seed:           *failSeed,
			N:              *failN,
			KnownUnreached: failureKnownUnreached,
			Log:            t.Logf,
		},
		Wrap:     postgres.WrapError,
		Violates: postgres.Violates[string],
	})
	if err != nil {
		t.Fatal(err)
	}
	text := rep.String("failure probe (postgres)")
	if *failReport != "" {
		if err := os.WriteFile(*failReport, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *failReport)
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
