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
	"github.com/kr9ly/sqlshape/v2/x/obligation"
	"os"
	"path/filepath"
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
		if _, ok := Lookup(name); !ok && name != Postgres {
			continue
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
	// Traits are the dialect's conventions for Go types.
	Traits() Traits
}

// Loader builds an Analyzer over a schema text (its dialect line included).
type Loader func(schemaSQL string) (Analyzer, error)

// Result is the dialect-neutral analysis of one statement: what it returns, what it
// takes, what it touches, what it may fail on, and its facts for the contracts.
type Result struct {
	// Params are the placeholders, indexed by n - 1. A parameter the analyzer could not
	// type has an Unknown Type.
	Params  []Param
	Columns []Column
	// Notes are the analyzer's findings that are not errors: reported as they are, or only
	// under -strict when Advisory.
	Notes []Note
	// Violations are the constraints the statement may violate, for the expect line.
	Violations []Violation
	// Relations are the schema objects the statement references, for the boundary checks
	// (-schemas, -no-tables) and the consumers index.
	Relations []RelationRef
	// Uses are the relation columns the statement reads or assigns.
	Uses []facts.Use
	// Facts is the statement's record for the contracts written on facts (x/facts): the
	// One proof (x/cardinality) and the obligations are judged on it, whatever the
	// dialect. Nil when the analyzer does not record the statement's shape.
	Facts *facts.Facts
	// ManyRowsWhy is the dialect's own reason a One proof failed, when its analyzer proves
	// cardinality itself; "" leaves the verdict to x/cardinality.
	ManyRowsWhy string
}

// Param is one placeholder: its type, and the column it stands for when the analyzer
// knows it (compared with or assigned to).
type Param struct {
	Type   Type
	Source *Source
}

// Column is one result column, or a field of a composite type.
type Column struct {
	Name     string
	Type     Type
	Nullable bool
	// Source is the table column this column reads unchanged, if any.
	Source *Source
}

// Source is a table column a result column or parameter stands for.
type Source struct {
	Table   string // as the facts spell it: schema-qualified unless in the default schema
	Column  string
	NotNull bool
	// Assigned: the parameter is stored into the column (INSERT / UPDATE), not compared.
	Assigned bool
	// Identity is the key the column carries when it is a key or references one, after
	// following foreign keys to their root ("public.users.id"); "" for other columns. Two
	// columns with the same Identity hold the same kind of value; the checker binds a Go
	// type to it.
	Identity string
	// Values are the values the column may hold when the schema fixes them, and
	// ValuesFrom says how: "check" for a CHECK (col IN (...)), "seed" for the key of a
	// lookup table seeded in the schema (Identity names the key). Nil otherwise.
	Values     []string
	ValuesFrom string
	// HasDefault: the column has a DEFAULT; Generated: the database computes it (an
	// identity or generated column). Both matter to a parameter assigned to the column.
	HasDefault bool
	Generated  bool
	// Comment is the column's COMMENT, "" for none.
	Comment string
}

// Note is a finding about the statement that is not an error.
type Note struct {
	Message  string
	Position int // 0-based byte offset into the SQL; -1 when unknown
	Advisory bool
	// Param is the parameter the note is about (1-based), 0 for none; the message writes
	// it as "$n" and the checker substitutes the parameter's Go path.
	Param int
}

// Violation is a constraint the statement may violate: what the runtime reports and the
// template's expect line names.
type Violation struct {
	Key        string // the name the expect line uses: the constraint's, or table.column for NOT NULL
	Code       string // the database's error code (SQLSTATE 23505, MySQL 1062)
	Table      string
	Columns    []string
	Constraint string
	Detail     string // how the checker describes it ("UNIQUE (email) on users, SQLSTATE 23505")
	// Param is the parameter whose NULL would violate a NOT NULL (1-based), 0 otherwise:
	// the checker drops the violation when the Go type cannot be NULL.
	Param int
}

// RelationRef is one relation the statement references.
type RelationRef struct {
	Name     string // as the facts spell it
	Schema   string // the namespace, for -schemas; "" when the dialect has none
	Kind     facts.RelKind
	Position int  // 0-based byte offset into the SQL; -1 when unknown
	Target   bool // written by the statement
}

// Type is a value type as the dialect names it, with the Go types that carry it. It is a
// tree: an array has its Elem, a range its Elem (the subtype), a domain its Base, a
// composite or record its Fields.
type Type struct {
	// Name is the dialect's spelling, for messages: "bigint unsigned", "varchar(20)".
	Name string
	// Named is the canonical name of a type the schema defines (an enum, a domain, a
	// composite, a range), what a `// sqlshape: type X` declaration names; "" for a
	// built-in type.
	Named string
	Kind  TypeKind
	// Elem is an array's element type or a range's subtype; Base a domain's base type.
	Elem *Type
	Base *Type
	// Fields are a composite type's columns.
	Fields []Column
	// Labels are an enum's labels, in order.
	Labels []string
	// Result and Param list the Go types a value scans into (a result column) and encodes
	// from (a parameter), best first: the first is what a struct written from the SQL
	// uses. Spelled as GoSpelling describes. Both nil: the dialect has no mapping (the
	// checker accepts the field with a note).
	Result []GoFit
	Param  []GoFit
}

// TypeKind is the shape of a Type.
type TypeKind byte

const (
	Scalar TypeKind = iota
	Array
	Composite  // a named row type
	Record     // an anonymous row
	Range      // Elem is the subtype
	Multirange // Elem is the range
	Enum
	Domain // Base is the underlying type
	Void   // a column that carries nothing (a procedure-like function's result)
)

// Unknown reports a type the dialect has no Go mapping for.
func (t Type) Unknown() bool { return len(t.Result) == 0 && len(t.Param) == 0 }

// GoFit is one Go type that carries a value, and what is lost when it does.
type GoFit struct {
	// Go is a spelling of the Go type (GoSpelling).
	Go string
	// Lossy is why the mapping loses information ("numeric into float64 loses precision"),
	// "" when it is faithful; the checker reports a lossy fit as a finding.
	Lossy string
	// Advice is what the mapping leaves to the application even when faithful ("timestamp
	// without time zone into time.Time: the zone is the application's implicit choice");
	// the checker reports it under -strict.
	Advice string
}

// GoSpelling is the grammar of GoFit.Go, what the checker matches a Go type against:
//
//	bool, int, int8 .. int64, uint .. uint64, float32, float64, string   a basic type
//	[]byte, [16]byte                                                    byte slices and arrays
//	time.Time, net/netip.Addr, github.com/google/uuid.UUID              a named type, by package path and name
//	struct                                                              any struct (a row: Fields decide the rest)
//	map[string]string, map[string]*string                               maps
//	json                                                                anything but a number or a bool
//	[]$elem, [N]$elem                                                   a slice / array whose element fits Elem
//	github.com/jackc/pgx/v5/pgtype.Range[$elem]                         a generic named type whose argument fits Elem
//
// A Go type whose underlying type spells the same fits too (a named string carries a
// varchar). The checker also accepts, for every type, a Go type that decodes or encodes
// itself (sql.Scanner for a result, driver.Valuer for a parameter), and the
// NullWrappers of the dialect.
const GoSpelling = "see the documentation of GoSpelling"

// Traits are the dialect's conventions the checker applies to every type.
type Traits struct {
	// TextParams: a Go string encodes as a parameter of any type (the driver sends text).
	TextParams bool
	// NullWrappers are package paths whose types all carry a Valid flag (pgx's pgtype): a
	// field of such a type receives NULL, and its value is not checked further.
	NullWrappers []string
}

// Schema is what the checker asks of a loaded schema beyond analyzing statements: its
// relations, for Copy and MatView and the doc comments it writes; the schema's own
// definitions, judged like statements; its advice. A dialect's Analyzer offers it by
// implementing Schemaed.
type Schema interface {
	// Relation resolves a name the way a program writes it ("orders", "sales.orders");
	// nil when the schema has none.
	Relation(name string) *Relation
	Relations() []*Relation
	// Definitions are the schema's own statements — view bodies, function bodies, policy
	// predicates — each with the facts the obligations are judged on, its notes, and the
	// error that stopped its analysis.
	Definitions() []Definition
	// Advice are the schema-level advisories (-strict): what the schema does less well than
	// it looks.
	Advice() []string
	// Type resolves a type by the name a `// sqlshape: type X` declaration writes.
	Type(name string) (Type, bool)
	// Source describes a column of a relation the way a statement's Source would (its
	// identity, its value set, its default); nil when the column does not exist.
	Source(table, column string) *Source
}

// Schemaed is an Analyzer that also gives the checker its Schema.
type Schemaed interface {
	Schema() Schema
}

// Contracted is an Analyzer whose schema the obligations can be declared on and judged
// against (x/obligation): its Contract, and the lowering of a declared predicate.
type Contracted interface {
	Contract() obligation.Schema
	Lower(expr string, rel obligation.Relation) ([]facts.Pred, error)
}

// Relation is a table or view as the checker sees it.
type Relation struct {
	Name    string // as the facts spell it (schema-qualified unless in the default schema)
	Schema  string
	Kind    facts.RelKind
	Columns []SchemaColumn
	Comment string
	// Unique: the relation has a unique index over whole columns without a predicate
	// (what REFRESH MATERIALIZED VIEW CONCURRENTLY needs).
	Unique bool
}

// Column finds a column by name.
func (r *Relation) Column(name string) *SchemaColumn {
	for i := range r.Columns {
		if r.Columns[i].Name == name {
			return &r.Columns[i]
		}
	}
	return nil
}

// SchemaColumn is a relation's column as declared.
type SchemaColumn struct {
	Name       string
	Type       Type
	NotNull    bool
	HasDefault bool
	Identity   bool // the database numbers it (an identity / serial column); a value may still be given
	Generated  bool // a generated column: computed from other columns, never written
	Comment    string
}

// Definition is one of the schema's own statements as the checker judges it.
type Definition struct {
	What  string // "view order_summary", "function pay: line 3", "policy p on t"
	Facts *facts.Facts
	Notes []Note
	Err   string // what stopped the analysis, "" when it went through
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

// ReadSchema reads the schema at path: a .sql file, or a directory whose *.sql files are
// concatenated in name order (so `schema/010_types.sql`, `schema/020_tables.sql`, ...
// apply like one file), each preceded by a comment naming it so that a problem's position
// can be traced back.
func ReadSchema(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	files, err := filepath.Glob(filepath.Join(path, "*.sql"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s: no *.sql files", path)
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "-- file: %s\n", filepath.Base(f))
		b.Write(src)
		if len(src) > 0 && src[len(src)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
