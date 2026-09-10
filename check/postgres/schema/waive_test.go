package schema

import (
	"reflect"
	"testing"
)

func TestWaivers(t *testing.T) {
	got := Waivers("orders pinned(tenant_id), audit, notes coalesce(a, b) = 1")
	want := map[string][]string{"orders": {"pinned(tenant_id)"}, "audit": {"*"}, "notes": {"coalesce(a, b) = 1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// a view's directives: unfiltered and waive both land on Waived
	s, err := Load(`
CREATE TABLE t (id int PRIMARY KEY, tenant_id int);
-- sqlshape: unfiltered t
-- sqlshape: waive t pinned(tenant_id)
-- sqlshape: require pinned(id)
-- sqlshape: context ops: waive pinned(id)
CREATE VIEW v AS SELECT id FROM t;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	v := s.Relation("", "v")
	if !reflect.DeepEqual(v.Waived["t"], []string{"unfiltered", "pinned(tenant_id)"}) {
		t.Errorf("waived: %v", v.Waived)
	}
	if len(v.Directives) != 4 {
		t.Errorf("directives: %v", v.Directives)
	}
}
