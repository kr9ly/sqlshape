package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/oracle"
)

// TestAdvSemantic checks name-resolution and error-reporting cases the analyzer's own
// oracle/error-case tables do not cover (USING / NATURAL JOIN column merging, GROUP BY on an
// aggregate's own alias, multi-table DELETE, and which of several problems in one statement
// gets reported). Each subtest states what a real mysqld does and checks that the analyzer
// agrees; skipped without a mysqld on PATH (nix-shell -p mysql84).
func TestAdvSemantic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, testSchema)
	if errors.Is(err, oracle.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	s := load(t)

	// serverOK asserts the server accepts sql (sanity: the test itself must describe real
	// server behavior, not a guess).
	serverOK := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := o.Describe(ctx, sql); err != nil {
			t.Fatalf("%s: the server rejects it (test premise is wrong): %v", sql, err)
		}
	}

	// TestAdvUsingUnqualifiedColumnResolves: MySQL's JOIN ... USING (col) coalesces the
	// named column from both sides into one output column, so an unqualified reference to it
	// is not ambiguous -- the server accepts `SELECT id FROM users u JOIN orders o USING
	// (id)` (both tables have `id`; USING merges them into a single visible `id`). The
	// analyzer instead resolves `id` against both sides independently and reports it
	// ambiguous, rejecting a statement the server runs.
	t.Run("UsingUnqualifiedColumnResolves", func(t *testing.T) {
		sql := "SELECT id FROM users u JOIN orders o USING (id)"
		serverOK(t, sql)
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: analyzer rejects what the server accepts: %v", sql, err)
		}
		if len(r.Columns) != 1 || r.Columns[0].Name != "id" {
			t.Fatalf("%s: got columns %+v, want one column named id", sql, r.Columns)
		}
	})

	// TestAdvNaturalJoinMergesCommonColumns: NATURAL JOIN implicitly USINGs every column
	// name common to both sides (here just `id`, present on both users and orders); the
	// server accepts an unqualified `id` and, for `SELECT *`, emits it once, not once per
	// side. The analyzer treats NATURAL JOIN as an unconditional cross join: it flags
	// unqualified `id` as ambiguous, and `SELECT *` would carry two `id` columns instead of
	// one (checked here via the unqualified-reference case, which is the one that turns a
	// valid statement into a rejection).
	t.Run("NaturalJoinMergesCommonColumns", func(t *testing.T) {
		sql := "SELECT id FROM users NATURAL JOIN orders"
		serverOK(t, sql)
		if _, err := Analyze(s, sql); err != nil {
			t.Fatalf("%s: analyzer rejects what the server accepts: %v", sql, err)
		}
	})

	// TestAdvNaturalJoinStarColumnCount: SELECT * over a NATURAL JOIN lists each common
	// column once (server: 7 columns -- the shared `id` plus the other 6), not once per
	// side. The analyzer does not fold the duplicate and reports 8 columns, which would
	// misalign a caller scanning the result by position.
	t.Run("NaturalJoinStarColumnCount", func(t *testing.T) {
		sql := "SELECT * FROM users u NATURAL JOIN orders o"
		d, err := o.Describe(ctx, sql)
		if err != nil {
			t.Fatalf("%s: the server rejects it (test premise is wrong): %v", sql, err)
		}
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatalf("%s: analyzer rejects what the server accepts: %v", sql, err)
		}
		if len(r.Columns) != len(d.Columns) {
			t.Fatalf("%s: analyzer reports %d columns, the server %d (%v vs server %v)", sql, len(r.Columns), len(d.Columns), columnNames(r), describeColumnNames(d))
		}
	})

	// TestAdvGroupByAggregateAliasRejected: MySQL rejects GROUP BY on a select-list alias
	// whose expression is itself an aggregate (error 1056, "Can't group on '<alias>'") --
	// grouping by the very value being aggregated is circular. The analyzer accepts it
	// silently instead of reporting 1056.
	t.Run("GroupByAggregateAliasRejected", func(t *testing.T) {
		sql := "SELECT COUNT(*) AS c FROM users GROUP BY c"
		_, err := o.Describe(ctx, sql)
		var oe *oracle.Error
		if !errors.As(err, &oe) || oe.Number != 1056 {
			t.Fatalf("%s: test premise is wrong: server error is %v, want 1056", sql, err)
		}
		r, aerr := Analyze(s, sql)
		if aerr == nil {
			t.Fatalf("%s: analyzer accepts what the server rejects with 1056; got columns %v", sql, columnNames(r))
		}
		ae, ok := aerr.(*Error)
		if !ok || ae.Code != 1056 {
			t.Fatalf("%s: analyzer error is %v, want *Error with code 1056", sql, aerr)
		}
	})

	// TestAdvMultiTableDeleteSupported: `DELETE t1 FROM t1 JOIN t2 ON ... WHERE ...` (the
	// multi-table delete form, target list before FROM) is valid MySQL that the server runs.
	// The analyzer's delete() only understands the single-table `DELETE FROM t ...` form; fed
	// the multi-table form it fails target() on the delete's target-list node and returns a
	// bare fmt.Errorf ("analyze: table name not understood: nil") -- not even a *Error, so a
	// caller that type-switches on *Error (as the analyzer's own callers do) cannot tell this
	// apart from a real internal fault.
	t.Run("MultiTableDeleteSupported", func(t *testing.T) {
		sql := "DELETE u FROM users u JOIN orders o ON o.user_id = u.id WHERE o.id = 1"
		serverOK(t, sql)
		if _, err := Analyze(s, sql); err != nil {
			t.Fatalf("%s: analyzer rejects what the server accepts: %v", sql, err)
		}
	})

	// TestAdvSelectListErrorReportedBeforeWhereError: when a statement has two problems --
	// an ambiguous unqualified column in the select list, and an unknown column in WHERE --
	// the server reports the select-list problem first (1052, ambiguous 'id'). The analyzer
	// validates WHERE before it validates the select list's ambiguity, so for this statement
	// it reports a different error entirely (1054, unknown column 'o.userid') than the one
	// the server would give the same client.
	t.Run("SelectListErrorReportedBeforeWhereError", func(t *testing.T) {
		sql := "SELECT id FROM users u JOIN orders o ON TRUE WHERE o.userid = 1"
		_, err := o.Describe(ctx, sql)
		var oe *oracle.Error
		if !errors.As(err, &oe) || oe.Number != 1052 {
			t.Fatalf("%s: test premise is wrong: server error is %v, want 1052", sql, err)
		}
		_, aerr := Analyze(s, sql)
		ae, ok := aerr.(*Error)
		if !ok {
			t.Fatalf("%s: analyzer error is %v, want *Error", sql, aerr)
		}
		if ae.Code != 1052 {
			t.Fatalf("%s: analyzer reports %d %q, server reports 1052 %q", sql, ae.Code, ae.Message, oe.Message)
		}
	})
}

func columnNames(r *Result) []string {
	names := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		names[i] = c.Name
	}
	return names
}

func describeColumnNames(d *oracle.Description) []string {
	names := make([]string, len(d.Columns))
	for i, c := range d.Columns {
		names[i] = c.Name
	}
	return names
}
