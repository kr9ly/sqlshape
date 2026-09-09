package parsegen

import (
	"fmt"
	"go/format"
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
	Wasm bool   // also build Out/wasm/mysqlparse.wasm with em++
	// Pkg is the directory of Go package mysqlparse; when set, Generate writes kinds.go
	// there and Compile (with Wasm) installs the module as wasm/mysqlparse_<major.minor>.wasm.
	Pkg string
	// ASTPkg is the directory of Go package mysqlast; when set, Generate writes views.go there.
	ASTPkg string
	// CatPkg is the directory of Go package catalog; when set, Generate writes functions.go there.
	CatPkg string
	Log    func(format string, args ...any)
}

// Wasm exports: the parse API (parse.h) and the allocator the host uses for its input.
var wasmExports = []string{"_malloc", "_free", "_mysqlparse_init", "_mysqlparse_parse", "_mysqlparse_error",
	"_mysqlparse_cursor", "_mysqlparse_data", "_mysqlparse_len", "_mysqlparse_free",
	"_mysqlparse_kind_name", "_mysqlparse_kind_count"}

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
		"kinds.h":              g.KindsH(),
		"lexer.cc":             lx.LexerCC,
		"shim/sql/sql_lex.h":   lx.SQLLexH,
		"shim/mysql_version.h": fmt.Sprintf("#pragma once\n#define MYSQL_VERSION_ID %d\n#define MYSQL_SERVER_VERSION %q\n", ver.ID(), ver.String()),
	}
	for name, text := range files {
		if err := writeFile(filepath.Join(b.Out, name), []byte(text)); err != nil {
			return nil, err
		}
	}
	if b.Pkg != "" {
		if err := writeFile(filepath.Join(b.Pkg, "kinds.go"), []byte(g.KindsGo("mysqlparse", ver.String()))); err != nil {
			return nil, err
		}
		alts, err := ReadActions(string(yacc))
		if err != nil {
			return nil, err
		}
		if len(alts) != g.Alternatives {
			return nil, fmt.Errorf("actions: read %d alternatives, the grammar has %d", len(alts), g.Alternatives)
		}
		names, err := ReadNames(b.Src, string(yacc), alts)
		if err != nil {
			return nil, err
		}
		b.log("%s", strings.TrimSpace(strings.SplitN(names.Report(), "\n", 2)[0]))
		if err := writeFile(filepath.Join(b.Pkg, "shapes.go"), []byte(ShapesGo("mysqlparse", ver.String(), g.Kinds, alts, names))); err != nil {
			return nil, err
		}
		if b.ASTPkg != "" {
			if err := writeFile(filepath.Join(b.ASTPkg, "views.go"), []byte(ViewsGo("mysqlast", ver.String(), alts, names))); err != nil {
				return nil, err
			}
		}
		if b.CatPkg != "" {
			cat, err := ReadCatalog(b.Src)
			if err != nil {
				return nil, err
			}
			var grammarClasses []string
			for _, a := range alts {
				if a.Kind == ActNew && strings.HasPrefix(a.Class, "Item_") {
					grammarClasses = append(grammarClasses, a.Class)
				}
			}
			if err := writeFile(filepath.Join(b.CatPkg, "functions.go"), []byte(CatalogGo("catalog", ver.String(), cat, grammarClasses))); err != nil {
				return nil, err
			}
			b.log("%s", strings.SplitN(cat.Report(), "\n", 2)[0])
		}
	}
	b.log("generated for MySQL %s: rules=%d alternatives=%d mid-rule-actions=%d kinds=%d", ver, g.Rules, g.Alternatives, g.MidRuleActions, len(g.Kinds))
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
	ours := []string{"parse.cc", "cst.cc", "lexer.cc", "parser.cc"}

	// the native probe driver
	if err := b.compile("g++", cxxflags, nil, lib, append([]string{"main.cc"}, ours...), filepath.Join(out, "obj"), filepath.Join(out, "mysqlparse")); err != nil {
		return err
	}
	if b.Wasm {
		wasm := filepath.Join(out, "wasm", "mysqlparse.wasm")
		link := []string{"--no-entry", "-sEXPORTED_FUNCTIONS=" + strings.Join(wasmExports, ","),
			"-sALLOW_MEMORY_GROWTH=1", "-sSTACK_SIZE=4194304", "-sERROR_ON_UNDEFINED_SYMBOLS=1"}
		if err := b.compile("em++", cxxflags, link, lib, ours, filepath.Join(out, "wobj"), wasm); err != nil {
			return err
		}
		if b.Pkg != "" {
			ver, err := ReadVersion(src)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(wasm)
			if err != nil {
				return err
			}
			dst := filepath.Join(b.Pkg, "wasm", fmt.Sprintf("mysqlparse_%d.%d.wasm", ver.Major, ver.Minor))
			if err := writeFile(dst, data); err != nil {
				return err
			}
			b.log("installed %s (%d bytes)", dst, len(data))
		}
	}
	return nil
}

// compile builds the library objects (cached in objdir) in parallel and links them with
// ours; linkflags go to the link step only.
func (b *Build) compile(cxx string, flags, linkflags, lib, ours []string, objdir, output string) error {
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
	args := append(append([]string{}, flags...), linkflags...)
	args = append(args, "-o", output)
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
	if strings.HasSuffix(path, ".go") {
		formatted, err := format.Source(data)
		if err != nil {
			return fmt.Errorf("%s: generated Go does not parse: %w", path, err)
		}
		data = formatted
	}
	return os.WriteFile(path, data, 0o644)
}
