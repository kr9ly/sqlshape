// Command parsegen regenerates the MySQL CST parser from the server source.
//
//	go run ./internal/parsegen/cmd/parsegen -src ~/.cache/sqlshape/mysql-server -out /tmp/mysqlparse [-wasm]
//	go run ./internal/parsegen/cmd/parsegen -src ... -corpus corpus.sql     # split mysql-test/t for the driver
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/parsegen"
)

func main() {
	home, _ := os.UserHomeDir()
	src := flag.String("src", filepath.Join(home, ".cache", "sqlshape", "mysql-server"), "MySQL server source tree")
	out := flag.String("out", "", "output directory (default ~/.cache/sqlshape/mysqlparse/<version>)")
	wasm := flag.Bool("wasm", false, "also build the wasm module with em++")
	genOnly := flag.Bool("generate-only", false, "write the sources and stop before the toolchain")
	corpus := flag.String("corpus", "", "write mysql-test/t split into statements to this file and exit")
	actions := flag.Bool("actions", false, "print how far the grammar's semantic actions are read, and exit")
	show := flag.String("show", "", "with -actions: comma-separated rule/alt pairs whose symbols and action to print")
	roots := flag.String("roots", "", "with -actions: comma-separated start rules; report the unread actions reachable from them")
	pkg := flag.String("pkg", "", "directory of package mysqlparse: write kinds.go there and, with -wasm, install the module")
	flag.Parse()

	if *actions {
		yacc, err := os.ReadFile(filepath.Join(*src, "sql", "sql_yacc.yy"))
		if err != nil {
			fatal(err)
		}
		alts, err := parsegen.ReadActions(string(yacc))
		if err != nil {
			fatal(err)
		}
		if *show != "" {
			want := map[string]bool{}
			for _, k := range strings.Split(*show, ",") {
				want[k] = true
			}
			for _, a := range alts {
				if want[fmt.Sprintf("%s/%d", a.Rule, a.Index)] {
					fmt.Printf("%s/%d: %s\n    %s\n", a.Rule, a.Index, strings.Join(a.Syms, " "), a.Action)
				}
			}
		} else if *roots != "" {
			fmt.Print(parsegen.ReachReport(alts, strings.Split(*roots, ",")))
		} else {
			fmt.Print(parsegen.ActionReport(alts))
		}
		return
	}
	if *corpus != "" {
		stmts, total, err := parsegen.SplitTestDir(filepath.Join(*src, "mysql-test", "t"))
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*corpus, []byte(parsegen.JoinForDriver(stmts)), 0o644); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "statements=%d kept=%d (dropped those with $vars)\n", total, len(stmts))
		return
	}
	if *out == "" {
		v, err := parsegen.ReadVersion(*src)
		if err != nil {
			fatal(err)
		}
		*out = filepath.Join(home, ".cache", "sqlshape", "mysqlparse", v.String())
	}
	b := &parsegen.Build{Src: *src, Out: *out, Wasm: *wasm, Pkg: *pkg, Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	if _, err := b.Generate(); err != nil {
		fatal(err)
	}
	if *genOnly {
		return
	}
	if err := b.Compile(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "parsegen:", err)
	os.Exit(1)
}
