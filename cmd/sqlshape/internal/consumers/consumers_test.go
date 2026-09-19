package consumers

import (
	"go/token"
	"strings"
	"testing"
)

func site(file string, offset int, owner string) Site {
	return Site{Pos: token.Position{Filename: file, Offset: offset, Line: 1, Column: 1}, Owner: owner}
}

func TestSiteString(t *testing.T) {
	s := site("a.go", 1, "pkg.Func")
	if got, want := s.String(), s.Pos.String()+" (pkg.Func)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	s2 := site("a.go", 1, "")
	if got, want := s2.String(), s2.Pos.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIndexBasics(t *testing.T) {
	x := New()
	s1 := site("a.go", 10, "pkg.A")
	s2 := site("a.go", 20, "pkg.B")
	x.AddColumn("orders", "status", s1)
	x.AddColumn("orders", "status", s1) // duplicate, should not double up
	x.AddColumn("orders", "id", s2)
	x.AddRelation("customers", s2)

	if got := x.Column("orders", "status"); len(got) != 1 || got[0] != s1 {
		t.Errorf("Column(orders,status) = %v", got)
	}
	if got := x.Relation("orders"); len(got) != 2 {
		t.Errorf("Relation(orders) = %v, want 2 sites", got)
	}
	if got := x.Relation("customers"); len(got) != 1 || got[0] != s2 {
		t.Errorf("Relation(customers) = %v", got)
	}
	if got, want := x.Columns(), []string{"orders.id", "orders.status"}; !equalStrs(got, want) {
		t.Errorf("Columns() = %v, want %v", got, want)
	}
	if got, want := x.Relations(), []string{"customers", "orders"}; !equalStrs(got, want) {
		t.Errorf("Relations() = %v, want %v", got, want)
	}
	if got, want := x.Len(), 2; got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}
}

func TestIndexMerge(t *testing.T) {
	a := New()
	sA := site("a.go", 1, "pkg.A")
	a.AddColumn("orders", "status", sA)

	b := New()
	sB := site("b.go", 2, "pkg.B")
	b.AddColumn("orders", "status", sB)
	b.AddColumn("orders", "id", sB)
	b.AddRelation("customers", sB)

	a.Merge(b)
	if got := a.Column("orders", "status"); len(got) != 2 {
		t.Errorf("merged Column(orders,status) = %v, want 2 sites", got)
	}
	if got := a.Relation("customers"); len(got) != 1 {
		t.Errorf("merged Relation(customers) = %v, want 1 site", got)
	}
	if got := a.Len(); got != 2 {
		t.Errorf("merged Len() = %d, want 2", got)
	}
	// merging a into itself again should not duplicate sites
	a.Merge(b)
	if got := a.Column("orders", "status"); len(got) != 2 {
		t.Errorf("re-merged Column(orders,status) = %v, want still 2 sites", got)
	}
}

func TestIndexSitesSortedByPosition(t *testing.T) {
	x := New()
	s1 := site("b.go", 5, "pkg.B")
	s2 := site("a.go", 5, "pkg.A")
	s3 := site("a.go", 1, "pkg.A0")
	x.AddColumn("t", "c", s1)
	x.AddColumn("t", "c", s2)
	x.AddColumn("t", "c", s3)
	got := x.Column("t", "c")
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	// a.go sorts before b.go; within a.go, offset 1 before offset 5
	if got[0] != s3 || got[1] != s2 || got[2] != s1 {
		t.Errorf("sort order wrong: %v", got)
	}
}

func TestIndexString(t *testing.T) {
	x := New()
	x.AddColumn("orders", "status", site("a.go", 1, "pkg.A"))
	x.AddColumn("orders", "id", site("a.go", 2, "pkg.A"))
	got := x.String()
	if !strings.HasPrefix(got, "orders.id\n") {
		t.Errorf("String() should list columns sorted, got:\n%s", got)
	}
	if !strings.Contains(got, "orders.status\n") {
		t.Errorf("String() missing orders.status, got:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.HasPrefix(line, "    ") {
			continue
		}
		if line != "orders.id" && line != "orders.status" {
			t.Errorf("unexpected line %q", line)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
