// Package dialect is the seam between the checker (internal/vet) and a database's
// analyzer. A schema.sql names its dialect on its first directive line, `-- sqlshape:
// postgres 18` or `-- sqlshape: mysql 8.4`; the checker reads that line, asks the dialect
// registered under the name for an Analyzer over the schema text, and judges every
// statement through the dialect-neutral Result the Analyzer returns.
//
// PostgreSQL is built in and keeps its own, richer path in internal/vet for now; every
// other dialect registers here (a MySQL analyzer lives in the mysql module, which the
// sqlshape binary imports for its side effect). A dialect's package is linked into the
// binary, never loaded at run time.
//
// Placeholders: the template expander numbers every parameter `$n` whatever the
// dialect. An Analyzer receives SQL in that form and, where its database spells
// parameters differently (`?`), rewrites before parsing; Result.Params is indexed by n.
package dialect

import (
	"fmt"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Declaration is the schema's dialect line.
type Declaration struct {
	Name    string // "postgres", "mysql", ...; "" when the schema declares none
	Version string // as written: "18", "8.4"
}

// Postgres is the name of the built-in dialect.
const Postgres = "postgres"

// directiveLine is any `-- sqlshape: word rest` line; only a registered dialect's name
// (or postgres) in the word position makes it a declaration, the rest are statement
// directives (`not null`, `require`, `unfiltered`, ...).
var directiveLine = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*([a-z][a-z0-9_]*)[ \t]+(\S+)[ \t]*$`)

// Declared reads the schema's dialect declaration. Two declarations that disagree are an
// error; a schema without one declares nothing (the caller decides the default).
func Declared(schemaSQL string) (Declaration, error) {
	var d Declaration
	schemaSQL = strings.TrimPrefix(schemaSQL, "\ufeff") // a byte order mark is not part of the SQL
	for _, m := range directiveLine.FindAllStringSubmatch(schemaSQL, -1) {
		name := m[1]
		if name != Postgres {
			if _, ok := Lookup(name); !ok {
				continue
			}
		}
		if d.Name != "" && (d.Name != name || d.Version != m[2]) {
			return Declaration{}, fmt.Errorf("schema: the dialect is declared twice, as `%s %s` and `%s %s`", d.Name, d.Version, name, m[2])
		}
		d = Declaration{Name: name, Version: m[2]}
	}
	return d, nil
}

// Analyzer judges statements against one loaded schema.
type Analyzer interface {
	// Analyze types one statement, with its parameters as `$n`. A statement the database
	// would reject comes back as an *Error; any other error is the analyzer's own limit.
	Analyze(sql string) (*Result, error)
	// Problems are what the loader could not apply of the schema text, each with its
	// position, reported once per package by the checker.
	Problems() []string
}

// Loader builds an Analyzer over a schema text (its dialect line included).
type Loader func(schemaSQL string) (Analyzer, error)

// Result is the dialect-neutral analysis of one statement: what it returns and what it
// takes.
type Result struct {
	// Params are the placeholder types, indexed by n - 1. A parameter the analyzer could
	// not type has a Type with no Go mapping.
	Params  []Type
	Columns []Column
	// Facts is the statement's record for the contracts written on facts (x/facts): the
	// One proof (x/cardinality) and the obligations are judged on it, whatever the
	// dialect. Nil when the analyzer does not record the statement's shape.
	Facts *facts.Facts
}

// Column is one result column.
type Column struct {
	Name     string
	Type     Type
	Nullable bool
}

// Type is a value type as the dialect names it, with the Go types that carry it.
type Type struct {
	// Name is the dialect's spelling, for messages: "bigint unsigned", "varchar(20)".
	Name string
	// Go lists the Go types a value of this type scans into (result) or encodes from
	// (parameter), as types.TypeString spells them with the package path: "int64",
	// "string", "time.Time", "[]byte". Nil means the dialect has no mapping: the checker
	// accepts the field and says so.
	Go []string
}

// Error is a statement the database itself would reject.
type Error struct {
	Message  string
	Code     string // the database's own code, "" when it has none to give
	Position int    // 0-based byte offset into the SQL; -1 when unknown
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Code)
	}
	return e.Message
}

var (
	mu       sync.RWMutex
	registry = map[string]Loader{}
)

// Register makes a dialect available under name; a dialect package calls it from init.
func Register(name string, load Loader) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic("dialect: " + name + " registered twice")
	}
	registry[name] = load
}

// Lookup finds a registered dialect.
func Lookup(name string) (Loader, bool) {
	mu.RLock()
	defer mu.RUnlock()
	l, ok := registry[name]
	return l, ok
}

// Names lists the registered dialects, postgres first.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry)+1)
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return append([]string{Postgres}, names...)
}
