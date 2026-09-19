package schema

// Adversarial pass 3, second half (schema lane): follow-up on 2nd pass's schema.go additions
// (CHECK OPTION reading on views). See scratchpad/adv3/brief-followup-schema-runtime.md
// (face 5) for the questions this file answers. Each finding states the server-measured
// behavior and currently FAILS against the loader as it stands today.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// TestAdv3bCreateOrReplaceViewFalsePositive (severity: medium — a false positive that stops
// a legitimate, everyday statement): mysqld 8.4.11 accepts CREATE OR REPLACE VIEW naming an
// already-existing view without complaint, and the view afterward has the *new* definition
// (measured below via information_schema.views' CHECK_OPTION). The loader's createView
// (check/mysql/internal/schema/schema.go:1377) instead reports "view already exists" as a
// Problem and leaves the *old* view untouched in the model.
//
// The likely cause: createView decides whether OR REPLACE was written by testing
// `n.Arg("replace") == nil` (schema.go:1385), where "replace" is the "create_view_mode"
// field the grammar hook wires in (check/mysql/internal/mysqlast/hooks_dml.go's viewHead,
// `mode, algorithm = st.Fields["create_view_mode"], st.Fields["create_view_algorithm"]`).
// Neither "view_replace_or_algorithm" (mysqlparse/shapes.go rule 902) nor "view_replace"
// (rule 903, "OR_SYM REPLACE_SYM") is ever built as a Struct carrying a "create_view_mode"
// field — both are plain ActDefault pass-throughs — so `mode` is nil whether or not OR
// REPLACE was written, and createView always takes the "already exists" branch for a
// second CREATE (OR REPLACE or not) of the same view name.
func TestAdv3bCreateOrReplaceViewFalsePositive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, tenant_id INT NOT NULL);
CREATE VIEW v AS SELECT id, tenant_id FROM t WHERE tenant_id = 1;
CREATE OR REPLACE VIEW v AS SELECT id, tenant_id FROM t WHERE tenant_id = 1 WITH CASCADED CHECK OPTION;
`
	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatalf("server: test premise wrong -- CREATE OR REPLACE VIEW of an existing view "+
			"must succeed: %v", err)
	}
	defer db.Close()
	var checkOpt string
	if err := db.Conn().QueryRowContext(ctx,
		"SELECT CHECK_OPTION FROM information_schema.views WHERE table_schema='sqlshape' AND table_name='v'",
	).Scan(&checkOpt); err != nil {
		t.Fatal(err)
	}
	if checkOpt != "CASCADED" {
		t.Fatalf("server: test premise wrong -- v's CHECK_OPTION should read back CASCADED "+
			"after the OR REPLACE, got %q", checkOpt)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("loader: unexpected problem for a valid CREATE OR REPLACE VIEW: %s", p)
	}
	v := s.View("v")
	if v == nil {
		t.Fatal("loader: no view v")
	}
	if v.CheckOption != "CASCADED" {
		t.Errorf("loader: v.CheckOption = %q, want %q (the OR REPLACE's own definition, matching the server)", v.CheckOption, "CASCADED")
	}
}

// TestAdv3bCheckOptionOnNonUpdatableViewNotFlagged (severity: high — the loader says a
// schema has no problems, but applying it to a real server fails outright: a false claim of
// "this DDL is fine"): mysqld 8.4.11 refuses CREATE VIEW ... WITH CHECK OPTION on a
// non-updatable view (one with GROUP BY here) at CREATE time, Error 1368 "CHECK OPTION on
// non-updatable view 'sqlshape.v_bad'". The loader's Load (check/mysql/internal/schema)
// builds the same statement with zero Problems and a CheckOption of "CASCADED" on the
// view, as if the CREATE would succeed -- there is no updatability check anywhere in
// createView (schema.go:1377) mirroring the server's own 1368 rule. A migration plan that
// creates this view (check/mysql/migrate) would only discover the failure when actually
// applied against a live server, not from the loader/diff/plan step that is supposed to
// catch schema-level mistakes ahead of time.
func TestAdv3bCheckOptionOnNonUpdatableViewNotFlagged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, tenant_id INT NOT NULL);
CREATE VIEW v_bad AS SELECT tenant_id, COUNT(*) c FROM t GROUP BY tenant_id WITH CHECK OPTION;
`
	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err == nil {
		db.Close()
		t.Fatal("server: test premise wrong -- CREATE VIEW ... WITH CHECK OPTION on a GROUP BY (non-updatable) view must fail with 1368")
	}
	if !strings.Contains(err.Error(), "1368") {
		t.Fatalf("server: test premise wrong -- expected 1368, got: %v", err)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) == 0 {
		t.Errorf("loader: Load reports no problems for a view whose WITH CHECK OPTION the server always refuses at CREATE time (1368); want a Problem flagging the non-updatable view")
	}
}

// TestAdv3bViewAlgorithmNeverRead (severity: high — feeds a false "may be updatable" verdict
// downstream): mysqld 8.4.11 preserves a view's own ALGORITHM (measured below via `SHOW
// CREATE VIEW`, which echoes back "ALGORITHM=TEMPTABLE" verbatim) and reports it
// non-updatable in information_schema.views.is_updatable, because ALGORITHM=TEMPTABLE always
// forces materialization regardless of how simple the query looks. The loader's View.Algorithm
// (check/mysql/internal/schema/schema.go:292/1380, `Algorithm:
// strings.TrimPrefix(str(n.Arg("algorithm")), "VIEW_ALGORITHM_")`) is "" for every view
// tested here -- MERGE, TEMPTABLE, UNDEFINED and the default -- never the declared value.
//
// Root cause, the same as TestAdv3bCreateOrReplaceViewFalsePositive above: viewHead
// (mysqlast/hooks_dml.go) reads `algorithm` from `st.Fields["create_view_algorithm"]`, a
// field name that "view_replace_or_algorithm" / "view_algorithm" (mysqlparse/shapes.go rules
// 902/904) never actually construct (both are plain ActDefault pass-throughs with no Struct
// building that field), so the algorithm is silently dropped no matter what was written.
//
// This is not an isolated cosmetic gap: check/mysql/internal/analyze/analyze.go:1046 reads
// `v.Algorithm == "TEMPTABLE"` as (half of) its own test for whether a view may be written
// through -- with Algorithm always "", that half of the test can never fire, so a view whose
// ALGORITHM=TEMPTABLE was written explicitly (forcing non-updatability even when the query
// shape alone would otherwise look mergeable) is invisible to that check, which then falls
// back to judging updatability from the query shape alone.
func TestAdv3bViewAlgorithmNeverRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE ALGORITHM=TEMPTABLE VIEW v AS SELECT id FROM t;
`
	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, ddl, definer, security string
	if err := db.Conn().QueryRowContext(ctx, "SHOW CREATE VIEW v").Scan(&name, &ddl, &definer, &security); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "ALGORITHM=TEMPTABLE") {
		t.Fatalf("server: test premise wrong -- expected the declared ALGORITHM=TEMPTABLE to read back in SHOW CREATE VIEW, got: %s", ddl)
	}
	var updatable string
	if err := db.Conn().QueryRowContext(ctx,
		"SELECT is_updatable FROM information_schema.views WHERE table_schema='sqlshape' AND table_name='v'",
	).Scan(&updatable); err != nil {
		t.Fatal(err)
	}
	if updatable != "NO" {
		t.Fatalf("server: test premise wrong -- expected is_updatable='NO' for an ALGORITHM=TEMPTABLE view, got %q", updatable)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	v := s.View("v")
	if v == nil {
		t.Fatal("loader: no view v")
	}
	if v.Algorithm != "TEMPTABLE" {
		t.Errorf("loader: v.Algorithm = %q, want %q (the view's own declared ALGORITHM, matching the server's SHOW CREATE VIEW readback)", v.Algorithm, "TEMPTABLE")
	}
}
