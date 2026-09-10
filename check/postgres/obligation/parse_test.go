package obligation

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Obligation
		ok   bool
		err  bool
	}{
		{"visible where deleted_at IS NULL", Obligation{Subject: "t", Kinds: OnRead, Body: Body{Predicate: "deleted_at IS NULL"}}, true, false},
		{"require deleted_at IS NULL", Obligation{Subject: "t", Kinds: OnRead, Body: Body{Predicate: "deleted_at IS NULL"}}, true, false},
		{"require  status = 'open'  on update, delete", Obligation{Subject: "t", Kinds: OnUpdate | OnDelete, Body: Body{Predicate: "status = 'open'"}}, true, false},
		{"require pinned(tenant_id)", Obligation{Subject: "t", Kinds: OnAll, Body: Body{Pinned: "tenant_id"}}, true, false},
		{"require pinned(version) on update, delete", Obligation{Subject: "t", Kinds: OnUpdate | OnDelete, Body: Body{Pinned: "version"}}, true, false},
		{"require immutable(tenant_id)", Obligation{Subject: "t", Kinds: OnUpdate, Body: Body{Immutable: "tenant_id"}}, true, false},
		{"require via view", Obligation{Subject: "t", Kinds: OnRead, Body: Body{ViaView: true}}, true, false},
		{"require via view on all", Obligation{Subject: "t", Kinds: OnAll, Body: Body{ViaView: true, IncludeWrites: true}}, true, false},
		{"require EXISTS (SELECT 1 FROM x WHERE x.k = k) on all", Obligation{Subject: "t", Kinds: OnAll, Body: Body{Predicate: "EXISTS (SELECT 1 FROM x WHERE x.k = k)"}}, true, false},
		{"require coalesce(a, 'on ') = 'on ' on read", Obligation{Subject: "t", Kinds: OnRead, Body: Body{Predicate: "coalesce(a, 'on ') = 'on '"}}, true, false},
		{"unfiltered a, b", Obligation{}, false, false},
		{"require", Obligation{}, false, false},
		{"require ", Obligation{}, false, false},
		{"require x = 1 on sometimes", Obligation{}, true, true},
		{"require pinned()", Obligation{}, true, true},
	}
	for _, c := range cases {
		got, ok, err := Parse("t", c.in)
		if ok != c.ok || (err != nil) != c.err {
			t.Errorf("%q: ok=%v err=%v", c.in, ok, err)
			continue
		}
		if err != nil || !ok {
			continue
		}
		got.Source = ""
		if got != c.want {
			t.Errorf("%q:\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}
