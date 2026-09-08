// Package pgparse is the SQL parser: libpg_query compiled to WebAssembly and run by wazero.
//
// It offers the four operations sqlshape uses (Parse, Deparse, SplitWithScanner,
// ParsePlPgSqlToJSON) with the signatures of pg_query_go, so call sites do not care which
// runs underneath. Running the parser as wasm instead of through cgo keeps the binary a
// plain Go build (cross-compiled, CGO_ENABLED=0) and lets one binary carry one parser
// module per PostgreSQL version, so a schema declaring a version is judged by that
// version's own grammar.
//
// One parser module is embedded per supported PostgreSQL major version. The Go node types
// are generated from the newest version's pg_query.proto; every version's tree is read from
// the parser's JSON output, which names fields and node types (the protobuf numbers them,
// and libpg_query renumbers between versions), and an older version's few renamed fields
// are rewritten to the newest shape on the way in (see upgrade).
//
// A wasm instance is not safe for concurrent use; instances are pooled and one is taken
// per call. A module is compiled once per process on first use.
package pgparse

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/emscripten"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Built by wasm/build.sh from libpg_query tag 17-6.2.2.
//
//go:embed wasm/pg_query_17.wasm
var wasm17 []byte

// Built by wasm/build.sh from libpg_query tag 18.0.0.
//
//go:embed wasm/pg_query_18.wasm
var wasm18 []byte

// Error is what the parser reports: the message as PostgreSQL words it, and the 1-based
// character position in the input the message points at (0 when there is none).
type Error struct {
	Message   string
	Cursorpos int
}

func (e *Error) Error() string { return e.Message }

// module is one compiled parser with a pool of instances.
type module struct {
	wasm     []byte
	once     sync.Once
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	err      error
	pool     sync.Pool // *instance
}

// Version is a PostgreSQL major version with an embedded parser.
type Version int

const (
	PG17 Version = 17
	PG18 Version = 18

	// Default is the version the package-level functions parse with: the newest version
	// sqlshape supports end to end (parser, catalog, oracle, regress corpus). PG18's parser
	// is embedded ahead of the rest.
	Default = PG17

	// newest is the version whose pg_query.proto the Go node types come from, and whose
	// module deparses (a tree of any version is in its types).
	newest = PG18
)

var modules = map[Version]*module{
	PG17: {wasm: wasm17},
	PG18: {wasm: wasm18},
}

// Supported lists the versions with an embedded parser, oldest first.
func Supported() []Version { return []Version{PG17, PG18} }

func (v Version) module() *module {
	m := modules[v]
	if m == nil {
		panic(fmt.Sprintf("pgparse: no parser for PostgreSQL %d", int(v)))
	}
	return m
}

func (m *module) compile() error {
	m.once.Do(func() {
		ctx := context.Background()
		m.runtime = wazero.NewRuntimeWithConfig(ctx, runtimeConfig())
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, m.runtime); err != nil {
			m.err = err
			return
		}
		m.compiled, m.err = m.runtime.CompileModule(ctx, m.wasm)
		if m.err != nil {
			return
		}
		// emscripten's longjmp emulation (invoke_*): PostgreSQL reports a syntax error by
		// longjmp, so this is the path every parse error takes.
		if _, err := emscripten.InstantiateForModule(ctx, m.runtime, m.compiled); err != nil {
			m.err = err
		}
	})
	return m.err
}

// runtimeConfig compiles through an on-disk cache when the user has a cache directory.
// Compiling the module takes most of a second, and `go vet` starts one analyzer process per
// package; from the cache the compiled code loads in a few tens of milliseconds. The cache
// is keyed by wazero on the module's bytes and its own version, so a rebuilt wasm or an
// upgraded wazero never reads a stale entry.
func runtimeConfig() wazero.RuntimeConfig {
	cfg := wazero.NewRuntimeConfig()
	dir, err := os.UserCacheDir()
	if err != nil {
		return cfg
	}
	cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(dir, "sqlshape", "wazero"))
	if err != nil {
		return cfg
	}
	return cfg.WithCompilationCache(cache)
}

// instance is one instantiated module: its own linear memory, in use by one call at a time.
type instance struct {
	mod api.Module
	fns map[string]api.Function
}

func (m *module) acquire() (*instance, error) {
	if err := m.compile(); err != nil {
		return nil, err
	}
	if v := m.pool.Get(); v != nil {
		return v.(*instance), nil
	}
	ctx := context.Background()
	// No name: wazero rejects a second instance of the same module name in one runtime.
	mod, err := m.runtime.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	in := &instance{mod: mod, fns: map[string]api.Function{}}
	if _, err := in.call("pg_query_init"); err != nil {
		mod.Close(ctx)
		return nil, err
	}
	return in, nil
}

// release returns an instance to the pool; an instance whose call trapped is dropped
// instead, since its memory may be in any state.
func (m *module) release(in *instance, trapped bool) {
	if trapped {
		in.mod.Close(context.Background())
		return
	}
	m.pool.Put(in)
}

func (in *instance) call(name string, args ...uint64) (uint64, error) {
	fn := in.fns[name]
	if fn == nil {
		fn = in.mod.ExportedFunction(name)
		if fn == nil {
			return 0, fmt.Errorf("pgparse: wasm export %q missing", name)
		}
		in.fns[name] = fn
	}
	res, err := fn.Call(context.Background(), args...)
	if err != nil {
		return 0, fmt.Errorf("pgparse: %s: %w", name, err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

// put copies s into the instance's memory as a NUL-terminated string.
func (in *instance) put(s string) (uint32, error) {
	p, err := in.call("malloc", uint64(len(s)+1))
	if err != nil {
		return 0, err
	}
	if !in.mod.Memory().Write(uint32(p), append([]byte(s), 0)) {
		return 0, fmt.Errorf("pgparse: memory write of %d bytes failed", len(s)+1)
	}
	return uint32(p), nil
}

// putBytes copies b into the instance's memory.
func (in *instance) putBytes(b []byte) (uint32, error) {
	p, err := in.call("malloc", uint64(max(len(b), 1)))
	if err != nil {
		return 0, err
	}
	if !in.mod.Memory().Write(uint32(p), b) {
		return 0, fmt.Errorf("pgparse: memory write of %d bytes failed", len(b))
	}
	return uint32(p), nil
}

// cstr reads a NUL-terminated string at p.
func (in *instance) cstr(p uint32) string {
	mem := in.mod.Memory()
	var sb strings.Builder
	for i := p; ; i++ {
		b, ok := mem.ReadByte(i)
		if !ok || b == 0 {
			break
		}
		sb.WriteByte(b)
	}
	return sb.String()
}

// bytesAt copies n bytes at p.
func (in *instance) bytesAt(p, n uint32) ([]byte, error) {
	b, ok := in.mod.Memory().Read(p, n)
	if !ok {
		return nil, fmt.Errorf("pgparse: memory read of %d bytes at %d failed", n, p)
	}
	return append([]byte(nil), b...), nil
}

// with runs f on a pooled instance of m.
func (m *module) with(f func(in *instance) error) error {
	in, err := m.acquire()
	if err != nil {
		return err
	}
	err = f(in)
	// A *Error is the parser's own report (a syntax error), the instance is fine; any other
	// error came from the wasm call itself.
	_, parserErr := err.(*Error)
	m.release(in, err != nil && !parserErr)
	return err
}

// parseProtobuf returns the protobuf encoding of the ParseResult for sql.
func (m *module) parseProtobuf(sql string) (out []byte, err error) {
	err = m.with(func(in *instance) error {
		input, err := in.put(sql)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(input))
		res, err := in.call("shim_parse", uint64(input))
		if err != nil {
			return err
		}
		defer in.call("shim_parse_free", res)
		if e, err := in.call("shim_parse_error", res); err != nil {
			return err
		} else if e != 0 {
			cur, _ := in.call("shim_parse_cursor", res)
			return &Error{Message: in.cstr(uint32(e)), Cursorpos: int(int32(cur))}
		}
		n, err := in.call("shim_parse_len", res)
		if err != nil {
			return err
		}
		p, err := in.call("shim_parse_data", res)
		if err != nil {
			return err
		}
		out, err = in.bytesAt(uint32(p), uint32(n))
		return err
	})
	return out, err
}

// parseJSON returns the JSON encoding of the ParseResult for sql. The JSON names fields
// and node types, where the protobuf numbers them, and libpg_query renumbers between
// PostgreSQL versions; so JSON is what a tree from any version's parser is read from.
func (m *module) parseJSON(sql string) (out string, err error) {
	err = m.with(func(in *instance) error {
		input, err := in.put(sql)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(input))
		res, err := in.call("shim_parse_json", uint64(input))
		if err != nil {
			return err
		}
		defer in.call("shim_parse_json_free", res)
		if e, err := in.call("shim_parse_json_error", res); err != nil {
			return err
		} else if e != 0 {
			cur, _ := in.call("shim_parse_json_cursor", res)
			return &Error{Message: in.cstr(uint32(e)), Cursorpos: int(int32(cur))}
		}
		p, err := in.call("shim_parse_json_tree", res)
		if err != nil {
			return err
		}
		out = in.cstr(uint32(p))
		return nil
	})
	return out, err
}

// Parse parses sql with the Default version's parser.
func Parse(sql string) (*ParseResult, error) { return Default.Parse(sql) }

// Parse parses sql with this version's grammar into a parse tree (in the newest version's
// node types).
func (v Version) Parse(sql string) (*ParseResult, error) {
	js, err := v.module().parseJSON(sql)
	if err != nil {
		return nil, err
	}
	if v != newest {
		js, err = upgrade(v, js)
		if err != nil {
			return nil, err
		}
	}
	tree := &ParseResult{}
	if err := protojson.Unmarshal([]byte(js), tree); err != nil {
		return nil, fmt.Errorf("pgparse: decoding parse tree: %w", err)
	}
	return tree, nil
}

// Deparse renders a parse tree back to SQL.
func Deparse(tree *ParseResult) (out string, err error) {
	b, err := proto.Marshal(tree)
	if err != nil {
		return "", fmt.Errorf("pgparse: encoding parse tree: %w", err)
	}
	err = newest.module().with(func(in *instance) error {
		data, err := in.putBytes(b)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(data))
		res, err := in.call("shim_deparse", uint64(data), uint64(len(b)))
		if err != nil {
			return err
		}
		defer in.call("shim_deparse_free", res)
		if e, err := in.call("shim_deparse_error", res); err != nil {
			return err
		} else if e != 0 {
			return &Error{Message: in.cstr(uint32(e))}
		}
		q, err := in.call("shim_deparse_query", res)
		if err != nil {
			return err
		}
		out = in.cstr(uint32(q))
		return nil
	})
	return out, err
}

// ParsePlPgSqlToJSON parses PL/pgSQL with the Default version.
func ParsePlPgSqlToJSON(input string) (string, error) { return Default.ParsePlPgSqlToJSON(input) }

// ParsePlPgSqlToJSON parses the PL/pgSQL function bodies in input (CREATE FUNCTION
// statements) and returns their parse trees as JSON.
func (v Version) ParsePlPgSqlToJSON(input string) (out string, err error) {
	err = v.module().with(func(in *instance) error {
		p, err := in.put(input)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(p))
		res, err := in.call("shim_plpgsql", uint64(p))
		if err != nil {
			return err
		}
		defer in.call("shim_plpgsql_free", res)
		if e, err := in.call("shim_plpgsql_error", res); err != nil {
			return err
		} else if e != 0 {
			return &Error{Message: in.cstr(uint32(e))}
		}
		j, err := in.call("shim_plpgsql_json", res)
		if err != nil {
			return err
		}
		out = in.cstr(uint32(j))
		return nil
	})
	return out, err
}

// SplitWithScanner splits input into statements by the Default version's scanner alone, so
// a text the grammar rejects can still be cut at its semicolons. With trimSpace, each
// statement is trimmed of surrounding whitespace.
func SplitWithScanner(input string, trimSpace bool) ([]string, error) {
	return Default.SplitWithScanner(input, trimSpace)
}

// SplitWithScanner splits input into statements with this version's scanner.
func (v Version) SplitWithScanner(input string, trimSpace bool) ([]string, error) {
	out, err := v.module().splitWithScanner(input)
	if trimSpace {
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
	}
	return out, err
}

func (m *module) splitWithScanner(input string) (out []string, err error) {
	err = m.with(func(in *instance) error {
		p, err := in.put(input)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(p))
		res, err := in.call("shim_split", uint64(p))
		if err != nil {
			return err
		}
		defer in.call("shim_split_free", res)
		if e, err := in.call("shim_split_error", res); err != nil {
			return err
		} else if e != 0 {
			return &Error{Message: in.cstr(uint32(e))}
		}
		n, err := in.call("shim_split_n", res)
		if err != nil {
			return err
		}
		for i := range n {
			loc, err := in.call("shim_split_location", res, i)
			if err != nil {
				return err
			}
			l, err := in.call("shim_split_len", res, i)
			if err != nil {
				return err
			}
			out = append(out, input[int32(loc):int32(loc)+int32(l)])
		}
		return nil
	})
	return out, err
}
