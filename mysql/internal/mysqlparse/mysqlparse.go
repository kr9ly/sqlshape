// Package mysqlparse is the MySQL parser: the server's own grammar and lexer, cut out of
// the server source by parsegen, compiled to WebAssembly and run by wazero.
//
// The grammar is sql_yacc.yy with its semantic actions stripped, so what comes back is a
// concrete syntax tree: one node per grammar rule reduced, one leaf per token, each leaf
// carrying its byte span in the input. Node kinds are the grammar's own rule and token
// names (kinds.go, generated with the module). Turning that into sqlshape's view of a
// statement is the job of the packages above this one; this package only parses.
//
// A wasm instance is not safe for concurrent use; instances are pooled and one is taken
// per call. The module is compiled once per process on first use, through wazero's
// on-disk cache when the user has a cache directory.
package mysqlparse

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/emscripten"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Built by parsegen from the MySQL 8.4 server source.
//
//go:embed wasm/mysqlparse_8.4.wasm
var wasm84 []byte

// Mode carries the sql_mode bits the lexer reads, with MySQL's own values.
type Mode uint32

const (
	PipesAsConcat      Mode = 2
	ANSIQuotes         Mode = 4
	IgnoreSpace        Mode = 8
	NoBackslashEscapes Mode = 0x40000 * 4
	HighNotPrecedence  Mode = 1 << 29
)

// Kind is a CST node kind: a grammar rule for inner nodes, a token for leaves.
type Kind uint16

// String is the grammar's name for the kind: a rule name such as "select_stmt", a token
// name such as "IDENT" or "SELECT_SYM", or a quoted character such as "'('".
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", uint16(k))
}

// IsTerminal reports whether the kind is a token.
func (k Kind) IsTerminal() bool { return k >= firstTerminal }

// KindOf returns the kind named s, or false.
func KindOf(s string) (Kind, bool) {
	kindIndexOnce.Do(func() {
		kindIndex = make(map[string]Kind, len(kindNames))
		for i, n := range kindNames {
			kindIndex[n] = Kind(i)
		}
	})
	k, ok := kindIndex[s]
	return k, ok
}

var (
	kindIndex     map[string]Kind
	kindIndexOnce sync.Once
)

// Node is one CST node. Leaves have Start/End (byte offsets into the parsed text) and no
// Children; inner nodes have Children and their span is that of their leaves.
type Node struct {
	Kind Kind
	// Alt is which alternative of the rule was reduced, 0-based in grammar order; it selects
	// the node's Shape. Leaves have Alt 0.
	Alt      int
	Start    int
	End      int
	Children []*Node
}

// IsLeaf reports whether the node is a token.
func (n *Node) IsLeaf() bool { return n.Kind.IsTerminal() }

// Text is the node's span of sql, the text it was parsed from.
func (n *Node) Text(sql string) string { return sql[n.Start:n.End] }

// Error is what the parser reports: bison's message and the byte offset of the token it
// stopped at.
type Error struct {
	Message string
	Offset  int
}

func (e *Error) Error() string { return fmt.Sprintf("%s at byte %d", e.Message, e.Offset) }

// Parse parses one statement (no trailing ';' is needed; one is accepted) and returns its
// CST. The root is the grammar's start rule. A syntax error is returned as *Error.
func Parse(sql string, mode Mode) (*Node, error) {
	var out *Node
	err := mod84.with(func(in *instance) error {
		p, err := in.put(sql)
		if err != nil {
			return err
		}
		defer in.call("free", uint64(p))
		r, err := in.call("mysqlparse_parse", uint64(p), uint64(len(sql)), uint64(mode))
		if err != nil {
			return err
		}
		defer in.call("mysqlparse_free", r)
		if e, err := in.call("mysqlparse_error", r); err != nil {
			return err
		} else if e != 0 {
			cur, err := in.call("mysqlparse_cursor", r)
			if err != nil {
				return err
			}
			return &Error{Message: in.cstr(uint32(e)), Offset: int(cur)}
		}
		data, err := in.call("mysqlparse_data", r)
		if err != nil {
			return err
		}
		n, err := in.call("mysqlparse_len", r)
		if err != nil {
			return err
		}
		b, err := in.bytesAt(uint32(data), uint32(n))
		if err != nil {
			return err
		}
		out, err = decode(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decode reads the wire format parse.h describes: preorder, u16 kind with bit 15 on leaves,
// then u32 start / u32 end for a leaf or u16 alternative + u16 child count for a rule.
func decode(b []byte) (*Node, error) {
	pos := 0
	var rec func() (*Node, error)
	rec = func() (*Node, error) {
		if pos+2 > len(b) {
			return nil, fmt.Errorf("mysqlparse: truncated tree at %d", pos)
		}
		k := binary.LittleEndian.Uint16(b[pos:])
		pos += 2
		n := &Node{Kind: Kind(k & 0x7fff)}
		if k&0x8000 != 0 {
			if pos+8 > len(b) {
				return nil, fmt.Errorf("mysqlparse: truncated leaf at %d", pos)
			}
			n.Start = int(binary.LittleEndian.Uint32(b[pos:]))
			n.End = int(binary.LittleEndian.Uint32(b[pos+4:]))
			pos += 8
			return n, nil
		}
		if pos+4 > len(b) {
			return nil, fmt.Errorf("mysqlparse: truncated node at %d", pos)
		}
		n.Alt = int(binary.LittleEndian.Uint16(b[pos:]))
		count := int(binary.LittleEndian.Uint16(b[pos+2:]))
		pos += 4
		if count > 0 {
			n.Children = make([]*Node, count)
			first := true
			for i := range n.Children {
				c, err := rec()
				if err != nil {
					return nil, err
				}
				n.Children[i] = c
				// an empty rule has no span of its own; take the span of the children that have one
				if c.End > c.Start || c.IsLeaf() {
					if first || c.Start < n.Start {
						n.Start = c.Start
					}
					if first || c.End > n.End {
						n.End = c.End
					}
					first = false
				}
			}
		}
		return n, nil
	}
	root, err := rec()
	if err != nil {
		return nil, err
	}
	if pos != len(b) {
		return nil, fmt.Errorf("mysqlparse: %d trailing bytes after the tree", len(b)-pos)
	}
	return root, nil
}

// --- wazero plumbing, the shape of internal/pgparse ---------------------------------

type module struct {
	wasm     []byte
	once     sync.Once
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	err      error
	pool     sync.Pool // *instance
}

var mod84 = &module{wasm: wasm84}

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
		if _, err := emscripten.InstantiateForModule(ctx, m.runtime, m.compiled); err != nil {
			m.err = err
		}
	})
	return m.err
}

// runtimeConfig compiles through the same on-disk cache as pgparse; the module is large
// (the whole collation library rides along), so the cache matters more here.
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
	mod, err := m.runtime.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	in := &instance{mod: mod, fns: map[string]api.Function{}}
	if rc, err := in.call("mysqlparse_init"); err != nil || rc != 0 {
		mod.Close(ctx)
		if err == nil {
			err = fmt.Errorf("mysqlparse: the charset registry did not come up")
		}
		return nil, err
	}
	return in, nil
}

func (m *module) release(in *instance, trapped bool) {
	if trapped {
		in.mod.Close(context.Background())
		return
	}
	m.pool.Put(in)
}

func (m *module) with(f func(in *instance) error) error {
	in, err := m.acquire()
	if err != nil {
		return err
	}
	err = f(in)
	_, parserErr := err.(*Error)
	m.release(in, err != nil && !parserErr)
	return err
}

func (in *instance) call(name string, args ...uint64) (uint64, error) {
	fn := in.fns[name]
	if fn == nil {
		fn = in.mod.ExportedFunction(name)
		if fn == nil {
			return 0, fmt.Errorf("mysqlparse: wasm export %q missing", name)
		}
		in.fns[name] = fn
	}
	res, err := fn.Call(context.Background(), args...)
	if err != nil {
		return 0, fmt.Errorf("mysqlparse: %s: %w", name, err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

func (in *instance) put(s string) (uint32, error) {
	p, err := in.call("malloc", uint64(len(s)+1))
	if err != nil {
		return 0, err
	}
	if !in.mod.Memory().Write(uint32(p), append([]byte(s), 0)) {
		return 0, fmt.Errorf("mysqlparse: memory write of %d bytes failed", len(s)+1)
	}
	return uint32(p), nil
}

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

func (in *instance) bytesAt(p, n uint32) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	b, ok := in.mod.Memory().Read(p, n)
	if !ok {
		return nil, fmt.Errorf("mysqlparse: memory read of %d bytes at %d failed", n, p)
	}
	return append([]byte(nil), b...), nil
}
