package analyze

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysql/internal/catalog"
	"github.com/kr9ly/sqlshape/mysql/internal/oracle"
	"github.com/kr9ly/sqlshape/mysql/internal/schema"
)

var probeUpdate = flag.Bool("probe-update", false, "rewrite testdata/probe.golden from the server's answers")

// The probe schema: one column per representative type, so that every registry function
// can be called over every kind of argument.
const probeSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  i INT NOT NULL,
  u BIGINT UNSIGNED NOT NULL,
  d DECIMAL(10,2) NOT NULL,
  f DOUBLE NOT NULL,
  s VARCHAR(20) NOT NULL,
  b VARBINARY(20) NOT NULL,
  dt DATETIME(6) NOT NULL,
  da DATE NOT NULL,
  tm TIME NOT NULL,
  j JSON,
  n INT
);
`

// probeArgs are the argument shapes tried for each arity: the same column of each type in
// every position, then a string first with integers after (SUBSTRING, LEFT, REPEAT ...),
// an integer first with strings after, and a NULL in the last position.
var probeArgs = [][]string{
	{"i"}, {"u"}, {"d"}, {"f"}, {"s"}, {"b"}, {"dt"}, {"da"}, {"tm"}, {"j"}, {"n"},
	{"s", "i", "i", "i"}, {"i", "s", "s", "s"}, {"s", "s", "NULL", "NULL"}, {"dt", "i", "i", "i"},
}

// TestProbe generates `SELECT f(args) FROM t` for every function of the native registry
// over the representative argument types, asks a real mysqld and the analyzer, and compares
// them against testdata/probe.golden: one line per statement, `=` when the analyzer agrees
// with the server on the type and nullability, `?` when it leaves the column untyped, `!`
// when it disagrees, `x` when the server rejects the call (the analyzer does not check
// argument types) and `X` when the analyzer rejects what the server accepts. The golden
// file is the record of what the rules cover; a change to it is a change to review.
// Skipped without a mysqld on PATH (nix-shell -p mysql84); -probe-update rewrites it.
func TestProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, probeSchema)
	if errors.Is(err, oracle.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	s, err := schema.Load(probeSchema)
	if err != nil {
		t.Fatal(err)
	}

	var lines []string
	counts := map[byte]int{}
	for i := range catalog.Functions {
		f := &catalog.Functions[i]
		if f.Internal {
			continue
		}
		seen := map[string]bool{}
		for _, shape := range probeArgs {
			for n := max(f.Min, 0); n <= maxArity(f); n++ {
				if !f.Accepts(n) {
					continue
				}
				args := make([]string, n)
				for i := range args {
					args[i] = shape[min(i, len(shape)-1)]
				}
				sql := fmt.Sprintf("SELECT %s(%s) FROM t", f.Name, strings.Join(args, ", "))
				if seen[sql] {
					continue
				}
				seen[sql] = true
				mark, line := probeOne(ctx, o, s, sql)
				counts[mark]++
				lines = append(lines, line)
			}
		}
	}
	sort.Strings(lines)
	summary := fmt.Sprintf("# server %s: %d statements; agree %d, untyped %d, disagree %d, server rejects %d, analyzer rejects %d",
		o.Version, len(lines), counts['='], counts['?'], counts['!'], counts['x'], counts['X'])
	t.Log(summary)
	got := summary + "\n" + strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "probe.golden")
	if *probeUpdate {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -probe-update to create it)", err)
	}
	if string(want) != got {
		wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		shown := 0
		for i := 0; i < max(len(wl), len(gl)) && shown < 40; i++ {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Errorf("golden: %s\n   now: %s", w, g)
				shown++
			}
		}
		t.Errorf("testdata/probe.golden differs (rerun with -probe-update after reviewing)")
	}
}

func maxArity(f *catalog.Function) int {
	if f.Max < 0 {
		return max(f.Min, 1) + 1
	}
	return min(f.Max, max(f.Min, 0)+2)
}

// probeOne is one statement's line: the mark, the statement, what the analyzer says and
// what the server says.
func probeOne(ctx context.Context, o *oracle.Oracle, s *schema.Schema, sql string) (byte, string) {
	d, oerr := o.Describe(ctx, sql)
	r, aerr := Analyze(s, sql)
	var oe *oracle.Error
	switch {
	case oerr != nil && errors.As(oerr, &oe):
		if aerr != nil {
			return 'x', fmt.Sprintf("x %s\t%v\t%d %s", sql, aerr, oe.Number, oe.Message)
		}
		return 'x', fmt.Sprintf("x %s\t%s\t%d %s", sql, ours(r), oe.Number, oe.Message)
	case oerr != nil:
		return 'x', fmt.Sprintf("x %s\t\t%v", sql, oerr)
	case aerr != nil:
		return 'X', fmt.Sprintf("X %s\t%v\t%s", sql, aerr, theirs(d))
	}
	if len(r.Columns) != 1 || len(d.Columns) != 1 {
		return '!', fmt.Sprintf("! %s\t%s\t%s", sql, ours(r), theirs(d))
	}
	c, oc := r.Columns[0], d.Columns[0]
	if !c.Known {
		return '?', fmt.Sprintf("? %s\t?\t%s", sql, theirs(d))
	}
	if typeKey(c.Type) != oracleKey(oc) || c.Nullable != oc.Nullable {
		return '!', fmt.Sprintf("! %s\t%s\t%s", sql, ours(r), theirs(d))
	}
	return '=', fmt.Sprintf("= %s\t%s", sql, ours(r))
}

func ours(r *Result) string {
	var parts []string
	for _, c := range r.Columns {
		if !c.Known {
			parts = append(parts, "?")
			continue
		}
		k := typeKey(c.Type)
		if c.Nullable {
			k += " null"
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, ", ")
}

func theirs(d *oracle.Description) string {
	var parts []string
	for _, c := range d.Columns {
		k := oracleKey(c)
		if c.Nullable {
			k += " null"
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, ", ")
}
