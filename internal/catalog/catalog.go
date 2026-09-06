// Package catalog is the static PostgreSQL system catalog (pg_catalog objects)
// the analyzer resolves types, functions, operators and casts against.
//
// The data is dumped from the oracle's PostgreSQL by ./gen and embedded as TSV.
// User schema (tables, enums, domains, views, functions) is layered on top at
// analysis time by a separate package.
package catalog

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"
)

//go:embed data/VERSION data/*.tsv data/ext
var data embed.FS

// OID is a PostgreSQL object identifier.
type OID uint32

// Well-known type OIDs (pg_type.h). Only the ones the analyzer needs by name.
const (
	Bool                    OID = 16
	Bytea                   OID = 17
	Char                    OID = 18
	Name                    OID = 19
	Int8                    OID = 20
	Int2                    OID = 21
	Int4                    OID = 23
	Text                    OID = 25
	OIDType                 OID = 26
	RegProc                 OID = 24
	Tid                     OID = 27
	MacAddr8                OID = 774
	Money                   OID = 790
	RegProcedure            OID = 2202
	RegClass                OID = 2205
	RegType                 OID = 2206
	TxidSnapshot            OID = 2970
	PgSnapshot              OID = 5038
	Xid8                    OID = 5069
	Xid                     OID = 28
	Cid                     OID = 29
	JSON                    OID = 114
	XML                     OID = 142
	Float4                  OID = 700
	Float8                  OID = 701
	Unknown                 OID = 705
	BPChar                  OID = 1042
	Varchar                 OID = 1043
	Date                    OID = 1082
	Time                    OID = 1083
	Timestamp               OID = 1114
	TimestampTZ             OID = 1184
	Interval                OID = 1186
	TimeTZ                  OID = 1266
	Numeric                 OID = 1700
	Record                  OID = 2249
	Cstring                 OID = 2275
	Any                     OID = 2276
	AnyArray                OID = 2277
	Void                    OID = 2278
	Trigger                 OID = 2279
	AnyElement              OID = 2283
	AnyNonArray             OID = 2776
	AnyEnum                 OID = 3500
	UUID                    OID = 2950
	JSONB                   OID = 3802
	JSONPath                OID = 4072
	AnyRange                OID = 3831
	AnyCompatible           OID = 5077
	AnyCompatibleArray      OID = 5078
	AnyCompatibleNonArray   OID = 5079
	AnyCompatibleRange      OID = 5080
	AnyMultirange           OID = 4537
	AnyCompatibleMultirange OID = 4538
)

// Type is a row of pg_type.
type Type struct {
	OID         OID
	Name        string
	Kind        byte // typtype: b base, c composite, d domain, e enum, p pseudo, r range, m multirange
	Category    byte // typcategory (§10.1 categories: N numeric, S string, D datetime, ...)
	IsPreferred bool
	Len         int16
	ByVal       bool
	Elem        OID // element type for arrays, 0 otherwise
	Array       OID // the array type over this type, 0 if none
	RelID       OID // composite: pg_class oid
	BaseType    OID // domain: underlying type
	Typmod      int32
	// Schema is the namespace of an extension's type ("public" usually); "" for pg_catalog.
	Schema string
}

// IsArray reports whether t is a true array type: it has an element type and is a
// varlena (get_element_type). Fixed-length types with typelem (point, line, ...) are not.
func (t *Type) IsArray() bool {
	return t.Elem != 0 && t.Len == -1 && (t.Kind != 'p' || t.Elem == Record)
}

// Relation is a system table or view (pg_class + pg_attribute) the analyzer can resolve
// like a user table: pg_catalog's, information_schema's, or an extension's.
type Relation struct {
	OID     OID
	Name    string
	Kind    byte // relkind: r table, v view
	Schema  string
	Columns []RelColumn
}

// RelColumn is one attribute of a system relation.
type RelColumn struct {
	Name    string
	Type    OID
	Typmod  int32
	NotNull bool
}

// RelationByName returns the system relation schema.name, or nil.
func (c *Catalog) RelationByName(schema, name string) *Relation { return c.relByName[schema+"."+name] }

// Well-known OIDs of the vector types PG never coerces arrays into (find_coercion_pathway).
const (
	OIDVector  OID = 30
	Int2Vector OID = 22
)

// IsPolymorphic reports whether t is one of the any* pseudo types (§38.2.5).
func (t *Type) IsPolymorphic() bool {
	switch t.OID {
	case Any, AnyArray, AnyElement, AnyNonArray, AnyEnum, AnyRange, AnyMultirange,
		AnyCompatible, AnyCompatibleArray, AnyCompatibleNonArray, AnyCompatibleRange, AnyCompatibleMultirange:
		return true
	}
	return false
}

// Func is a row of pg_proc.
type Func struct {
	OID         OID
	Name        string
	Kind        byte // prokind: f function, a aggregate, w window, p procedure
	RetType     OID
	RetSet      bool
	Variadic    OID // element type of the variadic parameter, 0 if none
	NArgs       int16
	NArgDefault int16
	IsStrict    bool
	Volatile    byte     // i immutable, s stable, v volatile
	ArgTypes    []OID    // input args (proargtypes)
	AllArgTypes []OID    // all args incl. OUT/TABLE, nil when same as ArgTypes
	ArgModes    []byte   // i in, o out, b inout, v variadic, t table; nil when all in
	ArgNames    []string // nil when unnamed
	// Schema is the namespace of an extension's function; "" for pg_catalog.
	Schema string
}

// Operator is a row of pg_operator.
type Operator struct {
	OID      OID
	Name     string
	Kind     byte // b binary, l prefix
	Left     OID  // 0 for prefix operators
	Right    OID
	Result   OID
	Code     OID // implementing pg_proc oid
	Com      OID
	Negate   OID
	CanMerge bool
	CanHash  bool
	// Schema is the namespace of an extension's operator; "" for pg_catalog.
	Schema string
}

// Cast is a row of pg_cast.
type Cast struct {
	OID     OID
	Source  OID
	Target  OID
	Func    OID  // 0 for binary-coercible / I/O casts
	Context byte // e explicit only, a assignment, i implicit
	Method  byte // f function, b binary coercible, i I/O conversion
}

// Aggregate is a row of pg_aggregate.
type Aggregate struct {
	FnOID      OID
	Kind       byte // n normal, o ordered-set, h hypothetical-set
	NDirectArg int16
	TransType  OID
}

// Range is a range type with its subtype and multirange type (pg_range).
type Range struct {
	OID     OID
	Subtype OID
	Multi   OID
}

// Catalog is the loaded bootstrap catalog with lookup indexes.
type Catalog struct {
	Version string
	// Extensions are the extensions merged in (WithExtensions), in load order.
	Extensions []*Extension

	Types      []Type
	Funcs      []Func
	Operators  []Operator
	Casts      []Cast
	Aggregates []Aggregate
	Ranges     []Range
	// Relations are the system tables and views (pg_catalog, information_schema, an
	// extension's), with their columns.
	Relations []Relation

	relByName  map[string]*Relation // "schema.name"
	typeByOID  map[OID]*Type
	typeByName map[string]*Type
	funcByOID  map[OID]*Func
	funcByName map[string][]*Func
	opByOID    map[OID]*Operator
	opByName   map[string][]*Operator
	castByPair map[[2]OID]*Cast
	aggByFn    map[OID]*Aggregate
}

var (
	loadOnce sync.Once
	loaded   *Catalog
	loadErr  error
)

// Load returns the embedded bootstrap catalog. Parsing happens once per process.
func Load() (*Catalog, error) {
	loadOnce.Do(func() { loaded, loadErr = parse() })
	return loaded, loadErr
}

// TypeByOID returns the type with the given oid, or nil.
func (c *Catalog) TypeByOID(oid OID) *Type { return c.typeByOID[oid] }

// TypeByName returns the pg_catalog type with the given typname (e.g. "int4", "_text"), or nil.
func (c *Catalog) TypeByName(name string) *Type { return c.typeByName[name] }

// FuncByOID returns the function with the given oid, or nil.
func (c *Catalog) FuncByOID(oid OID) *Func { return c.funcByOID[oid] }

// FuncsByName returns all pg_catalog functions named name (overloads), in oid order.
func (c *Catalog) FuncsByName(name string) []*Func { return c.funcByName[name] }

// OperatorByOID returns the operator with the given oid, or nil.
func (c *Catalog) OperatorByOID(oid OID) *Operator { return c.opByOID[oid] }

// OperatorsByName returns all pg_catalog operators with the given symbol (e.g. "="), in oid order.
func (c *Catalog) OperatorsByName(name string) []*Operator { return c.opByName[name] }

// CastBetween returns the pg_cast entry from source to target, or nil if none is declared.
// Callers must also handle the implicit cases pg_cast does not list (same type, domains, arrays, unknown).
func (c *Catalog) CastBetween(source, target OID) *Cast { return c.castByPair[[2]OID{source, target}] }

// AggregateByFn returns the pg_aggregate row for an aggregate function oid, or nil.
func (c *Catalog) AggregateByFn(fn OID) *Aggregate { return c.aggByFn[fn] }

func parse() (*Catalog, error) {
	c := newCatalog()
	v, err := data.ReadFile("data/VERSION")
	if err != nil {
		return nil, err
	}
	c.Version = strings.TrimSpace(string(v))
	if err := c.load("data", "", func(o OID) OID { return o }); err != nil {
		return nil, err
	}
	c.index()
	return c, nil
}

func newCatalog() *Catalog {
	return &Catalog{
		relByName:  map[string]*Relation{},
		typeByOID:  map[OID]*Type{},
		typeByName: map[string]*Type{},
		funcByOID:  map[OID]*Func{},
		funcByName: map[string][]*Func{},
		opByOID:    map[OID]*Operator{},
		opByName:   map[string][]*Operator{},
		castByPair: map[[2]OID]*Cast{},
		aggByFn:    map[OID]*Aggregate{},
	}
}

// load appends the TSVs under dir. Every OID passes through remap (extension dumps carry
// the oracle's transient OIDs); ns overrides the namespace column when non-empty.
func (c *Catalog) load(dir, ns string, remap func(OID) OID) error {
	r := func(s string) OID { return remap(oid(s)) }
	rs := func(s, sep string) []OID {
		out := oids(s, sep)
		for i := range out {
			out[i] = remap(out[i])
		}
		return out
	}
	schema := func(f string) string {
		if ns != "" {
			return ns
		}
		if f == "pg_catalog" {
			return ""
		}
		return f
	}
	if err := rows(dir, "pg_type", 13, func(f []string) error {
		c.Types = append(c.Types, Type{
			OID: r(f[0]), Name: f[1], Kind: f[2][0], Category: f[3][0], IsPreferred: f[4] == "t",
			Len: int16(num(f[5])), ByVal: f[6] == "t", Elem: r(f[7]), Array: r(f[8]),
			RelID: r(f[9]), BaseType: r(f[10]), Typmod: int32(num(f[11])), Schema: schema(f[12]),
		})
		return nil
	}); err != nil {
		return err
	}
	if err := rows(dir, "pg_proc", 15, func(f []string) error {
		fn := Func{
			OID: r(f[0]), Name: f[1], Kind: f[2][0], RetType: r(f[3]), RetSet: f[4] == "t",
			Variadic: r(f[5]), NArgs: int16(num(f[6])), NArgDefault: int16(num(f[7])),
			IsStrict: f[8] == "t", Volatile: f[9][0],
			ArgTypes: rs(f[10], " "), AllArgTypes: rs(f[11], " "), Schema: schema(f[14]),
		}
		if f[12] != "" {
			fn.ArgModes = []byte(f[12])
		}
		if f[13] != "" {
			fn.ArgNames = strings.Split(f[13], ",")
		}
		c.Funcs = append(c.Funcs, fn)
		return nil
	}); err != nil {
		return err
	}
	if err := rows(dir, "pg_operator", 12, func(f []string) error {
		c.Operators = append(c.Operators, Operator{
			OID: r(f[0]), Name: f[1], Kind: f[2][0], Left: r(f[3]), Right: r(f[4]), Result: r(f[5]),
			Code: r(f[6]), Com: r(f[7]), Negate: r(f[8]), CanMerge: f[9] == "t", CanHash: f[10] == "t", Schema: schema(f[11]),
		})
		return nil
	}); err != nil {
		return err
	}
	if err := rows(dir, "pg_cast", 6, func(f []string) error {
		c.Casts = append(c.Casts, Cast{
			OID: r(f[0]), Source: r(f[1]), Target: r(f[2]), Func: r(f[3]), Context: f[4][0], Method: f[5][0],
		})
		return nil
	}); err != nil {
		return err
	}
	if err := rows(dir, "pg_aggregate", 4, func(f []string) error {
		c.Aggregates = append(c.Aggregates, Aggregate{
			FnOID: r(f[0]), Kind: f[1][0], NDirectArg: int16(num(f[2])), TransType: r(f[3]),
		})
		return nil
	}); err != nil {
		return err
	}
	if err := rows(dir, "pg_class", 9, func(f []string) error {
		oid := r(f[0])
		if n := len(c.Relations); n == 0 || c.Relations[n-1].OID != oid {
			c.Relations = append(c.Relations, Relation{OID: oid, Name: f[1], Kind: f[2][0], Schema: f[3]})
		}
		rel := &c.Relations[len(c.Relations)-1]
		rel.Columns = append(rel.Columns, RelColumn{Name: f[5], Type: r(f[6]), Typmod: int32(num(f[7])), NotNull: f[8] == "t"})
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) { // older extension dumps have no pg_class.tsv
		return err
	}
	return rows(dir, "pg_range", 3, func(f []string) error {
		c.Ranges = append(c.Ranges, Range{OID: r(f[0]), Subtype: r(f[1]), Multi: r(f[2])})
		return nil
	})
}

// index rebuilds the lookup maps over the slices.
func (c *Catalog) index() {
	for i := range c.Relations {
		rel := &c.Relations[i]
		c.relByName[rel.Schema+"."+rel.Name] = rel
	}
	for i := range c.Types {
		t := &c.Types[i]
		c.typeByOID[t.OID] = t
		c.typeByName[t.Name] = t
	}
	for i := range c.Funcs {
		fn := &c.Funcs[i]
		c.funcByOID[fn.OID] = fn
		c.funcByName[fn.Name] = append(c.funcByName[fn.Name], fn)
	}
	for i := range c.Operators {
		op := &c.Operators[i]
		c.opByOID[op.OID] = op
		c.opByName[op.Name] = append(c.opByName[op.Name], op)
	}
	for i := range c.Casts {
		cs := &c.Casts[i]
		c.castByPair[[2]OID{cs.Source, cs.Target}] = cs
	}
	for i := range c.Aggregates {
		a := &c.Aggregates[i]
		c.aggByFn[a.FnOID] = a
	}
}

// RangeOf returns the pg_range entry of a range type, or nil.
func (c *Catalog) RangeOf(rng OID) *Range {
	for i := range c.Ranges {
		if c.Ranges[i].OID == rng {
			return &c.Ranges[i]
		}
	}
	return nil
}

// RangeOfMulti returns the pg_range entry whose multirange type is multi, or nil.
func (c *Catalog) RangeOfMulti(multi OID) *Range {
	for i := range c.Ranges {
		if c.Ranges[i].Multi == multi {
			return &c.Ranges[i]
		}
	}
	return nil
}

// RangeForSubtype returns the (first) range type over subtype, or nil.
func (c *Catalog) RangeForSubtype(sub OID) *Range {
	for i := range c.Ranges {
		if c.Ranges[i].Subtype == sub {
			return &c.Ranges[i]
		}
	}
	return nil
}

// rows streams one embedded TSV (COPY text format) and calls fn per row.
func rows(dir, name string, ncol int, fn func([]string) error) error {
	b, err := data.ReadFile(dir + "/" + name + ".tsv")
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		f := strings.Split(sc.Text(), "\t")
		if len(f) != ncol {
			return fmt.Errorf("%s:%d: %d columns, want %d", name, line, len(f), ncol)
		}
		for i := range f {
			if f[i] == `\N` {
				f[i] = ""
			}
		}
		if err := fn(f); err != nil {
			return fmt.Errorf("%s:%d: %w", name, line, err)
		}
	}
	return sc.Err()
}

func num(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("catalog: bad number %q", s))
	}
	return n
}

func oid(s string) OID { return OID(num(s)) }

func oids(s, sep string) []OID {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]OID, len(parts))
	for i, p := range parts {
		out[i] = oid(p)
	}
	return out
}
