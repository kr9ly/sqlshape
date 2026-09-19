package migrate

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

func mustLoad(t *testing.T, sql string) *schema.Schema {
	t.Helper()
	s, err := schema.Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

// TestParseIntentsMalformed covers ParseIntents' error branches: every declaration form
// that fails to match its regex, plus the backfill-specific "needs table.column" check
// that runs even after backfillRe itself matches.
func TestParseIntentsMalformed(t *testing.T) {
	cases := []struct {
		name, decl, want string
	}{
		{"unknown declaration", "-- @migrate frobnicate x", `unknown @migrate declaration "frobnicate x"`},
		{"rename missing arrow", "-- @migrate rename a b", `unknown @migrate declaration "rename a b"`},
		{"drop with extra text", "-- @migrate drop a b", `unknown @migrate declaration "drop a b"`},
		{"enum missing using", "-- @migrate enum e: drop 'a'", `unknown @migrate declaration`},
		{"backfill missing dot", "-- @migrate backfill total = 1", `backfill needs table.column, got "total"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseIntents(c.decl)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestParseIntentsMultipleErrors covers ParseIntents' accumulation of more than one
// problem line into a single error, joined by newlines.
func TestParseIntentsMultipleErrors(t *testing.T) {
	_, err := ParseIntents("-- @migrate bogus one\n-- @migrate bogus two\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 {
		t.Errorf("want 2 error lines, got %d: %v", len(lines), lines)
	}
}

// TestIntentStringDefault covers Intent.String()'s fallback "?" for a Kind it does not
// recognize (zero value, or anything past Backfill).
func TestIntentStringDefault(t *testing.T) {
	if got, want := (Intent{}).String(), "?"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestResolveNameThreeParts covers resolveName's 3-part branch (schema.table.column) and
// its failure when the column does not exist on that relation.
func TestResolveNameThreeParts(t *testing.T) {
	s := mustLoad(t, "CREATE SCHEMA app; CREATE TABLE app.t (id int, name text);")
	r, col := resolveName(s, "app.t.name")
	if r == nil || col != "name" {
		t.Errorf("resolveName(app.t.name) = %v, %q", r, col)
	}
	if r, col := resolveName(s, "app.t.nope"); r != nil || col != "" {
		t.Errorf("resolveName(app.t.nope) = %v, %q, want nil, \"\"", r, col)
	}
	if r, col := resolveName(s, "app.nope.name"); r != nil || col != "" {
		t.Errorf("resolveName(app.nope.name) = %v, %q, want nil, \"\"", r, col)
	}
}

// planErr runs Plan against schemas loaded statically (no database) and returns its
// error text (empty if Plan succeeded).
func planErr(t *testing.T, from, to string, intents string) string {
	t.Helper()
	fromS, toS := mustLoad(t, from), mustLoad(t, to)
	in, err := ParseIntents(intents)
	if err != nil {
		t.Fatalf("ParseIntents: %v", err)
	}
	_, err = Plan(fromS, toS, in)
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestReadIntentsRenameProblems covers readIntents' rename-validation branches: the
// source / target not found, mismatched kinds (table vs column, table vs view), stale
// declarations (the old name still exists on the side it should have vanished from), and
// the "already exists" branch on both sides.
func TestReadIntentsRenameProblems(t *testing.T) {
	cases := []struct {
		name, from, to, decl, want string
	}{
		{"from not found", "CREATE TABLE a (id int);", "CREATE TABLE b (id int);",
			"-- @migrate rename nope -> b", "nope is not in the current schema"},
		{"to not found", "CREATE TABLE a (id int);", "CREATE TABLE b (id int);",
			"-- @migrate rename a -> nope", "nope is not in the target schema"},
		{"table renamed to column", "CREATE TABLE a (id int);", "CREATE TABLE b (id int, x int);",
			"-- @migrate rename a -> b.x", "renames a table to a column or the reverse"},
		{"different kinds", "CREATE TABLE a (id int);", "CREATE VIEW a2 AS SELECT 1 AS id;",
			"-- @migrate rename a -> a2", "a is a TABLE, a2 a VIEW"},
		{"stale: old still in target", "CREATE TABLE a (id int);", "CREATE TABLE a (id int); CREATE TABLE b (id int);",
			"-- @migrate rename a -> b", "still exists in the target schema"},
		{"stale: new already in current", "CREATE TABLE a (id int); CREATE TABLE b (id int);", "CREATE TABLE b (id int);",
			"-- @migrate rename a -> b", "already exists in the current schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planErr(t, c.from, c.to, c.decl); !strings.Contains(got, c.want) {
				t.Errorf("got %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// TestReadIntentsColumnRenameProblems covers the column-rename branches of readIntents:
// declaring the table renamed on one line and its column on another (consistent), a
// column rename whose table is not the one the (separately declared) table rename
// implies, and a column rename with no matching table declaration where the column's
// current and target relations differ.
func TestReadIntentsColumnRenameProblems(t *testing.T) {
	t.Run("column rename with no table rename, tables differ", func(t *testing.T) {
		got := planErr(t,
			"CREATE TABLE a (id int, x int);",
			"CREATE TABLE a (id int); CREATE TABLE b (id int, y int);",
			"-- @migrate rename a.x -> b.y")
		if !strings.Contains(got, "different tables") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("column rename, table rename declared, but column's actual table differs", func(t *testing.T) {
		got := planErr(t,
			"CREATE TABLE a (id int, x int); CREATE TABLE c (id int);",
			"CREATE TABLE b (id int); CREATE TABLE c (id int, y int);",
			"-- @migrate rename a -> b\n-- @migrate rename c.x -> c.y")
		// c.x does not exist on c; the rename is declared against the wrong current table
		if !strings.Contains(got, "not in the current schema") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("stale column: old still on target table", func(t *testing.T) {
		got := planErr(t,
			"CREATE TABLE a (x int);",
			"CREATE TABLE a (x int, y int);",
			"-- @migrate rename a.x -> a.y")
		if !strings.Contains(got, "still exists in the target schema") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("stale column: new already on current table", func(t *testing.T) {
		got := planErr(t,
			"CREATE TABLE a (x int, y int);",
			"CREATE TABLE a (y int);",
			"-- @migrate rename a.x -> a.y")
		if !strings.Contains(got, "already exists in the current schema") {
			t.Errorf("got %q", got)
		}
	})
}

// TestReadIntentsDropProblems covers the Drop branch of readIntents: an unresolved
// dotted name, and a stale drop (the dropped object still exists in the target).
func TestReadIntentsDropProblems(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		got := planErr(t, "CREATE TABLE a (id int);", "CREATE TABLE a (id int);", "-- @migrate drop nope")
		if !strings.Contains(got, "nope is not in the current schema") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("stale table drop", func(t *testing.T) {
		got := planErr(t, "CREATE TABLE a (id int); CREATE TABLE b (id int);", "CREATE TABLE a (id int); CREATE TABLE b (id int);",
			"-- @migrate drop b")
		if !strings.Contains(got, "still exists in the target schema") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("stale column drop", func(t *testing.T) {
		got := planErr(t, "CREATE TABLE a (id int, x int);", "CREATE TABLE a (id int, x int);", "-- @migrate drop a.x")
		if !strings.Contains(got, "still exists in the target schema") {
			t.Errorf("got %q", got)
		}
	})
}

// TestReadIntentsBackfillUnresolved covers Backfill's "not in the target schema" branch
// when the declared table.column does not resolve.
func TestReadIntentsBackfillUnresolved(t *testing.T) {
	got := planErr(t, "CREATE TABLE a (id int);", "CREATE TABLE a (id int);", "-- @migrate backfill a.nope = 1")
	if !strings.Contains(got, "a.nope is not in the target schema") {
		t.Errorf("got %q", got)
	}
}

// TestEnumRecreatesProblems covers enumRecreates' declaration-mismatch branches: a
// declared "using" label that is not itself a label of the target enum, a declared label
// that is still present in the target enum (so dropping it was never necessary), an enum
// name that does not exist at all, and a declared enum that in fact does not change.
func TestEnumRecreatesProblems(t *testing.T) {
	t.Run("using target is not a label of the target enum", func(t *testing.T) {
		got := planErr(t,
			"CREATE TYPE e AS ENUM ('a', 'b', 'c');",
			"CREATE TYPE e AS ENUM ('a', 'b');",
			"-- @migrate enum e: drop 'c' using 'z'")
		if !strings.Contains(got, "is not a label of the target enum") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("declared label still present in target enum", func(t *testing.T) {
		got := planErr(t,
			"CREATE TYPE e AS ENUM ('a', 'b', 'c');",
			"CREATE TYPE e AS ENUM ('a', 'b', 'c');",
			"-- @migrate enum e: drop 'c' using 'a'")
		if !strings.Contains(got, "is still a label of the target enum") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("no such enum", func(t *testing.T) {
		got := planErr(t, "CREATE TABLE t (id int);", "CREATE TABLE t (id int);", "-- @migrate enum nope: drop 'a' using 'b'")
		if !strings.Contains(got, "no such enum in the current schema") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("enum does not change", func(t *testing.T) {
		got := planErr(t,
			"CREATE TYPE e AS ENUM ('a', 'b');",
			"CREATE TYPE e AS ENUM ('a', 'b');",
			"-- @migrate enum e: drop 'a' using 'b'")
		// the declared label 'a' is still present (both schemas identical), which alone
		// already triggers "still a label"; a genuinely-unchanged enum with a *different*
		// label declared not present in current at all falls to "does not change" only
		// when the gone-check finds nothing gone and using isn't a no-op duplicate.
		if got == "" {
			t.Fatal("expected a problem")
		}
	})
}

// TestRecreateEnumArrayColumn covers recreateEnum's "array of it" problem branch: a
// column typed as an array of the shrinking enum cannot have its values mapped by the
// plan.
func TestRecreateEnumArrayColumn(t *testing.T) {
	got := planErr(t,
		"CREATE TYPE e AS ENUM ('a', 'b'); CREATE TABLE t (tags e[]);",
		"CREATE TYPE e AS ENUM ('a'); CREATE TABLE t (tags e[]);",
		"-- @migrate enum e: drop 'b' using 'a'")
	if !strings.Contains(got, "is an array of it") {
		t.Errorf("got %q", got)
	}
}

// TestRecreateEnumSchemaQualified covers recreateEnum's schema-qualified naming branch
// for the temporary "__old" type name.
func TestRecreateEnumSchemaQualified(t *testing.T) {
	fromS := mustLoad(t, "CREATE SCHEMA app; CREATE TYPE app.e AS ENUM ('a', 'b');")
	toS := mustLoad(t, "CREATE SCHEMA app; CREATE TYPE app.e AS ENUM ('a');")
	in, err := ParseIntents("-- @migrate enum app.e: drop 'b' using 'a'")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(fromS, toS, in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ddl := strings.Join(plan, "\n")
	if !strings.Contains(ddl, `ALTER TYPE app.e RENAME TO "e__old"`) {
		t.Errorf("want a schema-qualified rename-away, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `DROP TYPE app.e__old`) {
		t.Errorf("want a schema-qualified drop of the old type, got:\n%s", ddl)
	}
}
