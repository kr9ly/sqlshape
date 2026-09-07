package migrate

import (
	"testing"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// TestRelWord covers relWord's four branches: the default (plain table) is not directly
// tagged by any RelKind case, so it is what falls through for anything but view /
// matview / sequence.
func TestRelWord(t *testing.T) {
	cases := []struct {
		kind schema.RelKind
		want string
	}{
		{schema.Table, "TABLE"},
		{schema.View, "VIEW"},
		{schema.MatView, "MATERIALIZED VIEW"},
		{schema.Sequence, "SEQUENCE"},
	}
	for _, c := range cases {
		if got := relWord(&schema.Relation{Kind: c.kind}); got != c.want {
			t.Errorf("relWord(%v) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestFnWord(t *testing.T) {
	if got, want := fnWord(&schema.Function{}), "FUNCTION"; got != want {
		t.Errorf("fnWord(plain) = %q, want %q", got, want)
	}
	if got, want := fnWord(&schema.Function{IsProc: true}), "PROCEDURE"; got != want {
		t.Errorf("fnWord(proc) = %q, want %q", got, want)
	}
	if got, want := fnWord(&schema.Function{IsAgg: true}), "AGGREGATE"; got != want {
		t.Errorf("fnWord(agg) = %q, want %q", got, want)
	}
}

func TestTypeWord(t *testing.T) {
	if got, want := typeWord("domain"), "DOMAIN"; got != want {
		t.Errorf("typeWord(domain) = %q, want %q", got, want)
	}
	if got, want := typeWord("enum"), "TYPE"; got != want {
		t.Errorf("typeWord(enum) = %q, want %q", got, want)
	}
}

func TestLabelsHelper(t *testing.T) {
	if got := labels(""); got != nil {
		t.Errorf("labels(\"\") = %v, want nil", got)
	}
	if got, want := labels("a, b, c"), []string{"a", "b", "c"}; len(got) != len(want) || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("labels(a,b,c) = %v, want %v", got, want)
	}
}

func TestTypeTextCollation(t *testing.T) {
	s, err := schema.Load(`CREATE TABLE t (name text COLLATE "C");`)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Relation("", "t")
	c := r.Column("name")
	got := typeText(s, c)
	if got != `text COLLATE C` {
		t.Errorf("typeText = %q, want %q", got, `text COLLATE C`)
	}
}

// TestColumnTextNotNullSuppressed covers columnText's notNull=false path: a NOT NULL
// column whose caller asks to leave the constraint off (for a column being backfilled
// before it turns NOT NULL).
func TestColumnTextNotNullSuppressed(t *testing.T) {
	s, err := schema.Load("CREATE TABLE t (n integer NOT NULL);")
	if err != nil {
		t.Fatal(err)
	}
	r := s.Relation("", "t")
	c := r.Column("n")
	if got := columnText(s, c, false); got == `"n" integer NOT NULL` {
		t.Errorf("columnText(notNull=false) should omit NOT NULL, got %q", got)
	}
	if got := columnText(s, c, true); got != `"n" integer NOT NULL` {
		t.Errorf("columnText(notNull=true) = %q, want %q", got, `"n" integer NOT NULL`)
	}
}
