package dialect

import "testing"

type nopAnalyzer struct{}

func (nopAnalyzer) Analyze(string) (*Result, error) { return &Result{}, nil }
func (nopAnalyzer) Problems() []string              { return nil }
func (nopAnalyzer) Traits() Traits                  { return Traits{} }

func TestDeclared(t *testing.T) {
	Register("testdb", func(string) (Analyzer, error) { return nopAnalyzer{}, nil })
	cases := []struct {
		sql  string
		want Declaration
		err  bool
	}{
		{"-- sqlshape: postgres 18\nCREATE TABLE t (a int);", Declaration{"postgres", "18"}, false},
		{"\ufeff  --  sqlshape:   testdb 1.0  \n", Declaration{"testdb", "1.0"}, false},
		{"CREATE TABLE t (a int);\n-- sqlshape: not null a\n-- sqlshape: require x", Declaration{}, false},
		{"-- sqlshape: unknowndb 3\n", Declaration{}, false},
		{"-- sqlshape: postgres 17\n-- sqlshape: postgres 18\n", Declaration{}, true},
		{"-- sqlshape: postgres 17\n-- sqlshape: testdb 1.0\n", Declaration{}, true},
		{"-- sqlshape: postgres 17\n-- sqlshape: postgres 17\n", Declaration{"postgres", "17"}, false},
	}
	for _, c := range cases {
		got, err := Declared(c.sql)
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v, want error %v", c.sql, err, c.err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.sql, got, c.want)
		}
	}
	if names := Names(); len(names) != 2 || names[0] != Postgres || names[1] != "testdb" {
		t.Errorf("Names() = %v", names)
	}
}
