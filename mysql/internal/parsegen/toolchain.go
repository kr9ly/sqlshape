package parsegen

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Build describes one generation run.
type Build struct {
	Src  string // MySQL server source tree (sparse checkout is enough)
	Out  string // output directory; library objects are cached under Out/obj and Out/wobj
	Wasm bool   // also build Out/wasm/mysqlparse.{js,wasm} with em++
	Log  func(format string, args ...any)
}

// Version is the server version read from MYSQL_VERSION.
type Version struct{ Major, Minor, Patch int }

func (v Version) ID() int        { return v.Major*10000 + v.Minor*100 + v.Patch }
func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }
func (b *Build) log(f string, a ...any) {
	if b.Log != nil {
		b.Log(f, a...)
	}
}

// ReadVersion parses MYSQL_VERSION at the root of the server source.
func ReadVersion(src string) (Version, error) {
	b, err := os.ReadFile(filepath.Join(src, "MYSQL_VERSION"))
	if err != nil {
		return Version{}, err
	}
	var v Version
	for line := range strings.SplitSeq(string(b), "\n") {
		k, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(val)
		switch k {
		case "MYSQL_VERSION_MAJOR":
			v.Major = n
		case "MYSQL_VERSION_MINOR":
			v.Minor = n
		case "MYSQL_VERSION_PATCH":
			v.Patch = n
		}
	}
	if v.Major == 0 {
		return Version{}, fmt.Errorf("MYSQL_VERSION: no MYSQL_VERSION_MAJOR")
	}
	return v, nil
}

// Generate writes every generated and hand-written source into Out and returns
// the grammar counts. It runs no compiler.
func (b *Build) Generate() (*Grammar, error) {
	ver, err := ReadVersion(b.Src)
	if err != nil {
		return nil, err
	}
	yacc, err := os.ReadFile(filepath.Join(b.Src, "sql", "sql_yacc.yy"))
	if err != nil {
		return nil, err
	}
	g, err := StripGrammar(string(yacc))
	if err != nil {
		return nil, err
	}
	lexCC, err := os.ReadFile(filepath.Join(b.Src, "sql", "sql_lex.cc"))
	if err != nil {
		return nil, err
	}
	lexH, err := os.ReadFile(filepath.Join(b.Src, "sql", "sql_lex.h"))
	if err != nil {
		return nil, err
	}
	lx, err := CutLexer(string(lexCC), string(lexH))
	if err != nil {
		return nil, err
	}

	// hand-written C++ (everything under cc/ except the head/tail pieces spliced above)
	err = fs.WalkDir(cc, "cc", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := filepath.Base(p)
		switch base {
		case "lexer_head.cc", "lexer_shims.cc", "sql_lex_head.h", "sql_lex_tail.h":
			return nil
		}
		data, err := cc.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFile(filepath.Join(b.Out, strings.TrimPrefix(p, "cc/")), data)
	})
	if err != nil {
		return nil, err
	}
	files := map[string]string{
		"grammar.y":            g.Text,
		"lexer.cc":             lx.LexerCC,
		"shim/sql/sql_lex.h":   lx.SQLLexH,
		"shim/mysql_version.h": fmt.Sprintf("#pragma once\n#define MYSQL_VERSION_ID %d\n#define MYSQL_SERVER_VERSION %q\n", ver.ID(), ver.String()),
	}
	for name, text := range files {
		if err := writeFile(filepath.Join(b.Out, name), []byte(text)); err != nil {
			return nil, err
		}
	}
	b.log("generated for MySQL %s: rules=%d alternatives=%d mid-rule-actions=%d", ver, g.Rules, g.Alternatives, g.MidRuleActions)
	return g, nil
}

// Compile runs the toolchain over Generate's output: bison for the grammar and
// the hint tokens, the server's gen_lex_hash and uca9dump, then g++ (and em++).
func (b *Build) Compile() error {
	out := b.Out
	src := b.Src
	inc := []string{"-I" + out, "-I" + filepath.Join(out, "shim"), "-I" + filepath.Join(out, "gen"),
		"-I" + filepath.Join(src, "include"), "-I" + src, "-I" + filepath.Join(src, "strings")}
	cxxflags := append([]string{"-std=c++20", "-DNDEBUG", "-O2", "-w"}, inc...)
	gen := filepath.Join(out, "gen")
	for _, d := range []string{filepath.Join(gen, "sql"), filepath.Join(gen, "strings"), filepath.Join(out, "wasm")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// 1. grammar → parser.cc / parser.h. bison itself checks %expect.
	if err := b.run(out, "bison", "-Wnone", "--defines=parser.h", "-o", "parser.cc", "grammar.y"); err != nil {
		return err
	}
	// 2. hint tokens header (lex.h includes it), then the keyword hash from the server's generator
	if err := b.run(out, "bison", "-Wnone", "--defines="+filepath.Join(gen, "sql", "sql_hints.yy.h"), "-o", os.DevNull, filepath.Join(src, "sql", "sql_hints.yy")); err != nil {
		return err
	}
	if err := b.run(out, "g++", append(cxxflags, "-o", filepath.Join(gen, "gen_lex_hash"), filepath.Join(src, "sql", "gen_lex_hash.cc"))...); err != nil {
		return err
	}
	if err := b.runTo(filepath.Join(gen, "sql", "lex_hash.h"), out, filepath.Join(gen, "gen_lex_hash")); err != nil {
		return err
	}
	// 3. the two generated UCA han tables
	if err := b.run(out, "g++", append(cxxflags, "-o", filepath.Join(gen, "uca9dump"), filepath.Join(src, "strings", "uca9-dump.cc"))...); err != nil {
		return err
	}
	for _, lang := range []string{"ja", "zh"} {
		dst := filepath.Join(gen, "strings", "uca900_"+lang+"_tbls.cc")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := b.run(out, filepath.Join(gen, "uca9dump"), lang, "--in_file="+filepath.Join(src, "strings", "lang_data", lang+"_hans.txt"), "--out_file="+dst); err != nil {
			return err
		}
	}
	// 4. the charset library: every compiled collation the server has
	lib := []string{filepath.Join(src, "sql", "sql_lex_hash.cc"), filepath.Join(gen, "strings", "uca900_ja_tbls.cc"), filepath.Join(gen, "strings", "uca900_zh_tbls.cc")}
	for _, pat := range []string{"ctype*.cc", "collations*.cc"} {
		m, err := filepath.Glob(filepath.Join(src, "strings", pat))
		if err != nil {
			return err
		}
		sort.Strings(m)
		lib = append(lib, m...)
	}
	for _, f := range []string{"xml.cc", "int2str.cc", "my_strchr.cc", "str_alloc.cc", "sql_chars.cc", "my_uctype.cc", "my_strtoll10.cc", "dtoa.cc"} {
		lib = append(lib, filepath.Join(src, "strings", f))
	}
	ours := []string{"main.cc", "cst.cc", "lexer.cc", "parser.cc"}

	if err := b.compile("g++", cxxflags, lib, ours, filepath.Join(out, "obj"), filepath.Join(out, "mysqlparse")); err != nil {
		return err
	}
	if b.Wasm {
		if err := b.compile("em++", cxxflags, lib, ours, filepath.Join(out, "wobj"), filepath.Join(out, "wasm", "mysqlparse.js")); err != nil {
			return err
		}
	}
	return nil
}

// compile builds the library objects (cached in objdir) in parallel and links them with ours.
func (b *Build) compile(cxx string, flags, lib, ours []string, objdir, output string) error {
	if err := os.MkdirAll(objdir, 0o755); err != nil {
		return err
	}
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var objs []string
	for _, f := range lib {
		o := filepath.Join(objdir, strings.TrimSuffix(filepath.Base(f), ".cc")+".o")
		objs = append(objs, o)
		if _, err := os.Stat(o); err == nil {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := b.run(b.Out, cxx, append(append([]string{}, flags...), "-c", "-o", o, f)...); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	args := append(append([]string{}, flags...), "-o", output)
	args = append(args, ours...)
	args = append(args, objs...)
	if err := b.run(b.Out, cxx, args...); err != nil {
		return err
	}
	b.log("built %s", output)
	return nil
}

func (b *Build) run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	outb, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(shorten(args), " "), err, outb)
	}
	return nil
}

func (b *Build) runTo(path, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	outb, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return writeFile(path, outb)
}

var reLongPath = regexp.MustCompile(`^(/[^/]+){3,}/`)

func shorten(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = reLongPath.ReplaceAllString(a, ".../")
	}
	return out
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
