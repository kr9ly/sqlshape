package x

import (
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// The layering the repository promises, checked on the import graph:
//
//	x/*                          the contracts and the shared parts: stdlib and x/* only
//	cmd/sqlshape/internal/vet    the Go frontend: x/* and the binary's own internals only
//	check/<db>/dialect           a dialect's adapter: its own check/<db>/* and x/* only
//
// The frontend still reaches into check/postgres directly (the PostgreSQL path predates
// x/dialect); those edges are listed in debt below and the list is meant to shrink to
// nothing. A new edge outside the rules fails this test.
const module = "github.com/kr9ly/sqlshape/"

// debt are the known violations, package → imports it may still have.
var debt = map[string][]string{
	module + "cmd/sqlshape/v2/internal/vet": {
		module + "check/postgres/v2/analyze",
		module + "check/postgres/v2/catalog",
		module + "check/postgres/v2/obligation",
		module + "check/postgres/v2/pgparse",
		module + "check/postgres/v2/schema",
	},
}

func TestImportBoundary(t *testing.T) {
	pkgs := list(t, "./...", "./cmd/sqlshape/...", "./check/mysql/dialect") // check/postgres has no dialect adapter yet: the frontend reaches into it (debt)
	for _, p := range pkgs {
		rel := strings.TrimPrefix(p.ImportPath, module)
		var allow func(imp string) bool
		switch {
		case strings.HasPrefix(rel, "v2/x/"):
			allow = func(imp string) bool { return strings.HasPrefix(imp, module+"v2/x/") }
		case rel == "cmd/sqlshape/v2/internal/vet":
			allow = func(imp string) bool {
				return strings.HasPrefix(imp, module+"v2/x/") || strings.HasPrefix(imp, module+"cmd/sqlshape/v2/internal/")
			}
		case strings.HasPrefix(rel, "check/") && strings.HasSuffix(rel, "/dialect"):
			own := module + rel[:strings.LastIndex(rel, "/dialect")] + "/"
			allow = func(imp string) bool { return strings.HasPrefix(imp, module+"v2/x/") || strings.HasPrefix(imp, own) }
		default:
			continue
		}
		for _, imp := range p.Imports {
			third := strings.Contains(imp, ".") && !strings.HasPrefix(imp, module)
			if strings.HasPrefix(rel, "v2/x/") && third {
				t.Errorf("%s imports %s: x/ has no dependencies", rel, imp)
				continue
			}
			if !strings.HasPrefix(imp, module) || allow(imp) || owed(p.ImportPath, imp) {
				continue
			}
			t.Errorf("%s imports %s: outside the layering (see debt in x/boundary_test.go)", rel, imp)
		}
	}
	// the debt list names only edges that still exist, so that it cannot hide a repaid one
	for pkg, imps := range debt {
		for _, imp := range imps {
			if !hasEdge(pkgs, pkg, imp) {
				t.Errorf("debt lists %s → %s, which no longer exists: remove it", strings.TrimPrefix(pkg, module), strings.TrimPrefix(imp, module))
			}
		}
	}
}

func owed(pkg, imp string) bool {
	for _, d := range debt[pkg] {
		if d == imp {
			return true
		}
	}
	return false
}

type pkg struct {
	ImportPath string
	Imports    []string
}

func hasEdge(pkgs []pkg, from, to string) bool {
	for _, p := range pkgs {
		if p.ImportPath == from {
			for _, imp := range p.Imports {
				if imp == to {
					return true
				}
			}
		}
	}
	return false
}

// list runs go list over the workspace (the repository root is this package's parent).
func list(t *testing.T, patterns ...string) []pkg {
	cmd := exec.Command("go", append([]string{"list", "-json=ImportPath,Imports"}, patterns...)...)
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ImportPath < pkgs[j].ImportPath })
	return pkgs
}
