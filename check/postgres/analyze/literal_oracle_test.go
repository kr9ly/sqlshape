package analyze

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kr9ly/sqlshape/check/postgres/oracle"
)

// TestLiteralOracle runs every 'literal'::type found in PG's regress corpus through the
// real input function and through the analyzer, and reports where they disagree. It
// needs -regress like TestRegress but takes seconds, not minutes; GUC-dependent inputs
// (datestyle / timezone) show up as false hits here and are settled by TestRegress.
func TestLiteralOracle(t *testing.T) {
	if *regressDir == "" {
		t.Skip("-regress not set")
	}
	ctx := context.Background()
	o, err := oracle.Start(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn := o.Conn()
	// -literal-types narrows the run; the default is every type the port validates
	typeSet := map[string]bool{}
	for _, t := range strings.Split(*literalTypes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			typeSet[t] = true
		}
	}
	re := regexp.MustCompile(`(?is)'((?:[^']|'')*)'\s*::\s*([a-z_][a-z_0-9 ]*?)(\[\])?\b(?:[^a-z_\[]|$)|\b(timestamptz|timestamp with time zone|timestamp|date|timetz|time with time zone|time|interval)\s*(?:\(\d\))?\s*'((?:[^']|'')*)'`)
	files, _ := filepath.Glob(filepath.Join(*regressDir, "sql", "*.sql"))
	type lit struct{ typ, val string }
	seen := map[lit]bool{}
	var lits []lit
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			val, typ := m[1], strings.ToLower(strings.TrimSpace(m[2]))+m[3]
			if m[2] == "" {
				typ, val = strings.ToLower(m[4]), m[5]
			}
			switch typ {
			case "timestamp with time zone":
				typ = "timestamptz"
			case "time with time zone":
				typ = "timetz"
			}
			if !typeSet[typ] {
				continue
			}
			l := lit{typ, strings.ReplaceAll(val, "''", "'")}
			if !seen[l] {
				seen[l] = true
				lits = append(lits, l)
			}
		}
	}
	sort.Slice(lits, func(i, j int) bool {
		if lits[i].typ != lits[j].typ {
			return lits[i].typ < lits[j].typ
		}
		return lits[i].val < lits[j].val
	})
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	var mismatches []string
	perType := map[string][2]int{}
	for _, l := range lits {
		var dummy string
		want := "ok"
		if err := conn.QueryRow(ctx, "SELECT $1::text::"+l.typ+"::text", l.val).Scan(&dummy); err != nil {
			var pe *pgconn.PgError
			if !errors.As(err, &pe) {
				t.Fatalf("%q::%s: %v", l.val, l.typ, err)
			}
			want = pe.Code
		}
		got := "ok"
		if _, err := Analyze(s, "SELECT "+quoteLit(l.val)+"::"+l.typ); err != nil {
			var aerr *Error
			if errors.As(err, &aerr) {
				got = aerr.Code
			} else {
				got = "internal"
			}
		}
		c := perType[l.typ]
		c[0]++
		if got != want {
			c[1]++
			mismatches = append(mismatches, fmt.Sprintf("%-14s %-6s %-6s %q", l.typ, want, got, l.val))
		}
		perType[l.typ] = c
	}
	var types []string
	for k := range perType {
		types = append(types, k)
	}
	sort.Strings(types)
	var sb strings.Builder
	for _, k := range types {
		fmt.Fprintf(&sb, "%-14s %4d literals %4d mismatches\n", k, perType[k][0], perType[k][1])
	}
	t.Logf("%d literals, %d mismatches\n%s\n(oracle / analyzer / literal)\n%s", len(lits), len(mismatches), sb.String(), strings.Join(mismatches, "\n"))
}

func quoteLit(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

var literalTypes = flag.String("literal-types", "timestamp,timestamptz,date,time,timetz,interval", "types TestLiteralOracle checks (comma-separated)")
