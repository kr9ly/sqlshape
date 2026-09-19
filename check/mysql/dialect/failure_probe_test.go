package dialect

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	mysqlrt "github.com/kr9ly/sqlshape/mysql/v2"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
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
	"upsert nothing":         "ON CONFLICT DO NOTHING is PostgreSQL's (INSERT IGNORE is the MySQL shape)",
	"predicted always fails": "MySQL predicts an omitted NOT NULL column as a 1364 violation, not as a certain failure",
}

// TestFailureProbe judges the MySQL analyzer's predicted failure modes against a running
// mysqld: x/stmtprobe generates writes against a schema carrying every constraint kind,
// runs them, and requires every constraint error the server raises to be one of the
// predicted violations under a key mysql.Violates matches. Skipped without a mysqld on
// PATH (nix-shell -p mysql84).
func TestFailureProbe(t *testing.T) {
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
	rep, err := stmtprobe.RunFailures(ctx, stmtprobe.FailureOptions{
		Options: stmtprobe.Options{
			Dialect:        stmtprobe.Dialect{Header: "-- sqlshape: mysql 8.4", IntType: "INT", StrType: "VARCHAR(20)", Positional: true},
			Load:           load,
			DB:             db.Conn(),
			Seed:           *failSeed,
			N:              *failN,
			KnownUnreached: failureKnownUnreached,
			Log:            t.Logf,
		},
		Wrap:     mysqlrt.WrapError,
		Violates: mysqlrt.Violates[string],
	})
	if err != nil {
		t.Fatal(err)
	}
	text := rep.String("failure probe (mysql)")
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
