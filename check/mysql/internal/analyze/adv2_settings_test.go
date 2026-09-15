package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// adv2_settings_test.go: adversarial findings for the "settings" lane (see the brief for the
// ground rules): the `-- sqlshape: server ...` declarations and how each sql_mode bit changes
// a judgment. Every case below was reproduced against a real mysqld 8.4.11 (nix-shell -p
// mysql84) before being written down; it states the correct behavior and is expected to fail
// against the current implementation.

// TestAdv2StrictTransTablesIgnoresEngine: STRICT_TRANS_TABLES enforces strict mode only for
// transactional storage engines; the manual is explicit that for a nontransactional table
// (MyISAM) "an error occurs if the invalid data occurs in a single-row statement or the
// first row of a multiple-row statement" but a later row of a multiple-row statement gets
// the column's implicit default with a warning instead, exactly as under no strict mode at
// all. Only STRICT_ALL_TABLES enforces strict mode unconditionally, on every engine and
// every row.
//
// Measured on 8.4.11 with sql_mode='STRICT_TRANS_TABLES' and CREATE TABLE a (id INT NOT NULL
// PRIMARY KEY, s VARCHAR(20) NOT NULL) ENGINE=MyISAM:
//
//	INSERT INTO a (id, s) VALUES (1, 'x'), (2, NULL)   -- succeeds; row 2 stores '' with a warning
//	INSERT INTO a (id, s) VALUES (1, NULL)             -- Error 1048 (single row, still strict)
//
// (with sql_mode='STRICT_ALL_TABLES' on the same table, both statements give Error 1048: the
// engine makes no difference there, which the analyzer already gets right.)
//
// The analyzer does not read the table's engine at all when deciding whether a NULL into a
// NOT NULL column is a violation: `notNull := a.s.Settings.Strict() || w.rows == 1` in
// check/mysql/internal/analyze/violations.go:228 treats STRICT_TRANS_TABLES exactly like
// STRICT_ALL_TABLES (both set the same bit tested by sqlmode.Mode.Strict(),
// x/sqlmode/sqlmode.go), so it lists a NOT NULL violation for row 2 above no matter the
// table's storage engine. schema.Table already carries Engine (set from CREATE TABLE's
// ENGINE=... clause, check/mysql/internal/schema/schema.go:765), so the information needed
// to tell the two modes apart is there and simply unused.
func TestAdv2StrictTransTablesIgnoresEngine(t *testing.T) {
	const schemaSQL = `-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'STRICT_TRANS_TABLES'
CREATE TABLE a (id INT NOT NULL PRIMARY KEY, s VARCHAR(20) NOT NULL) ENGINE=MyISAM;
`
	s, err := schema.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r, err := Analyze(s, "INSERT INTO a (id, s) VALUES (1, 'x'), (2, $1)")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, v := range r.Violations {
		if v.Code == 1048 && v.Key() == "a.s" {
			t.Errorf("STRICT_TRANS_TABLES on a MyISAM table: got a NOT NULL violation for the "+
				"second row of a multi-row INSERT, but a real mysqld 8.4.11 accepts it (stores "+
				"the column's implicit default with a warning): %+v", r.Violations)
		}
	}
}

// TestAdv2StrictTransTablesServer is the server-side confirmation of the case above: a fresh
// mysqld started under sql_mode='STRICT_TRANS_TABLES' with the MyISAM table accepts the
// second row's NULL (no error), while the same statement on a MyISAM table under
// STRICT_ALL_TABLES, and a single-row INSERT under STRICT_TRANS_TABLES, are both rejected
// with 1048 -- so the divergence is specific to STRICT_TRANS_TABLES, a non-transactional
// engine, and a row after the first.
func TestAdv2StrictTransTablesServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const schemaSQL = `-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'STRICT_TRANS_TABLES'
CREATE TABLE a (id INT NOT NULL PRIMARY KEY, s VARCHAR(20) NOT NULL) ENGINE=MyISAM;
`
	db, err := mysqltest.Start(ctx, schemaSQL)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := db.Conn()

	if _, err := conn.ExecContext(ctx, "INSERT INTO a (id, s) VALUES (1, 'x'), (2, NULL)"); err != nil {
		t.Errorf("multi-row INSERT, 2nd row NULL, MyISAM, STRICT_TRANS_TABLES: want success, got %v", err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM a"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO a (id, s) VALUES (1, NULL)"); err == nil || !strings.Contains(err.Error(), "1048") {
		t.Errorf("single-row INSERT, NULL, MyISAM, STRICT_TRANS_TABLES: want Error 1048, got %v", err)
	}
}
