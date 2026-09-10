package catalog

import (
	"strconv"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, major := range Majors() {
		c, err := Load(major)
		if err != nil {
			t.Fatal(err)
		}
		if c.Major != major || !strings.HasPrefix(c.Version, strconv.Itoa(major)+".") {
			t.Errorf("Load(%d): major %d, version %q", major, c.Major, c.Version)
		}
		if len(Available(major)) < 10 {
			t.Errorf("PostgreSQL %d: %d extension dumps", major, len(Available(major)))
		}
		t.Logf("pg %s: %d types, %d funcs, %d operators, %d casts, %d aggregates",
			c.Version, len(c.Types), len(c.Funcs), len(c.Operators), len(c.Casts), len(c.Aggregates))
	}
	if got := Majors(); len(got) != 2 || got[0] != 17 || got[1] != 18 {
		t.Errorf("Majors() = %v", got)
	}
	if _, err := Load(16); err == nil || !strings.Contains(err.Error(), "no embedded catalog") {
		t.Errorf("Load(16): %v", err)
	}
	c, err := Load(17)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pg %s: %d types, %d funcs, %d operators, %d casts, %d aggregates",
		c.Version, len(c.Types), len(c.Funcs), len(c.Operators), len(c.Casts), len(c.Aggregates))

	int4 := c.TypeByName("int4")
	if int4 == nil || int4.OID != Int4 || int4.Category != 'N' || int4.Array == 0 {
		t.Fatalf("int4 = %+v", int4)
	}
	arr := c.TypeByOID(int4.Array)
	if arr == nil || arr.Name != "_int4" || arr.Elem != Int4 || !arr.IsArray() {
		t.Errorf("_int4 = %+v", arr)
	}
	if !c.TypeByOID(AnyElement).IsPolymorphic() || c.TypeByOID(Text).IsPolymorphic() {
		t.Error("IsPolymorphic")
	}

	// int4 = int4 → bool, implemented by int4eq
	var eq *Operator
	for _, op := range c.OperatorsByName("=") {
		if op.Left == Int4 && op.Right == Int4 {
			eq = op
		}
	}
	if eq == nil || eq.Result != Bool || c.FuncByOID(eq.Code).Name != "int4eq" {
		t.Errorf("int4 = int4: %+v", eq)
	}

	// int4 → int8 is an implicit cast; int8 → int4 is assignment; text → int4 is not in pg_cast (I/O rule)
	if cs := c.CastBetween(Int4, Int8); cs == nil || cs.Context != 'i' {
		t.Errorf("int4->int8: %+v", cs)
	}
	if cs := c.CastBetween(Int8, Int4); cs == nil || cs.Context != 'a' {
		t.Errorf("int8->int4: %+v", cs)
	}
	if cs := c.CastBetween(Text, Int4); cs != nil {
		t.Errorf("text->int4 should not be declared: %+v", cs)
	}

	// sum(int4) → int8, is an aggregate; count(*) has zero args
	var sumInt4 *Func
	for _, f := range c.FuncsByName("sum") {
		if len(f.ArgTypes) == 1 && f.ArgTypes[0] == Int4 {
			sumInt4 = f
		}
	}
	if sumInt4 == nil || sumInt4.Kind != 'a' || sumInt4.RetType != Int8 || c.AggregateByFn(sumInt4.OID) == nil {
		t.Errorf("sum(int4) = %+v", sumInt4)
	}
	// array_agg(anynonarray) → anyarray, polymorphic
	var aa *Func
	for _, f := range c.FuncsByName("array_agg") {
		if len(f.ArgTypes) == 1 && f.ArgTypes[0] == AnyNonArray {
			aa = f
		}
	}
	if aa == nil || aa.RetType != AnyArray {
		t.Errorf("array_agg = %+v", aa)
	}
	// a function with OUT params keeps AllArgTypes/ArgModes
	var found bool
	for _, f := range c.Funcs {
		if len(f.ArgModes) > 0 && len(f.AllArgTypes) == len(f.ArgModes) {
			found = true
			break
		}
	}
	if !found {
		t.Error("no function with arg modes parsed")
	}
}

func TestWithExtensions(t *testing.T) {
	base, err := Load(17)
	if err != nil {
		t.Fatal(err)
	}
	if base.TypeByName("citext") != nil {
		t.Fatal("bootstrap catalog must not contain extension types")
	}
	c, err := base.WithExtensions([]string{"citext", "earthdistance", "cube"})
	if err != nil {
		t.Fatal(err)
	}
	// cube is bundled by earthdistance and loads once
	if len(c.Extensions) != 2 {
		t.Fatalf("extensions: got %d, want 2 (cube bundled by earthdistance)", len(c.Extensions))
	}
	ct := c.TypeByName("citext")
	if ct == nil || ct.Schema != "public" || ct.OID < extBase(0) {
		t.Fatalf("citext: %+v", ct)
	}
	if arr := c.TypeByOID(ct.Array); arr == nil || arr.Elem != ct.OID {
		t.Fatalf("citext array type not renumbered consistently: %+v", arr)
	}
	if cube := c.TypeByName("cube"); cube == nil || cube.OID < extBase(1) {
		t.Fatalf("cube: %+v", cube)
	}
	found := false
	for _, fn := range c.FuncsByName("earth_distance") {
		if fn.Schema == "public" && len(fn.ArgTypes) == 2 && fn.ArgTypes[0] == c.TypeByName("earth").OID {
			found = true
		}
	}
	if !found {
		t.Fatal("earth_distance(earth, earth) not found with renumbered argument types")
	}
	if base.TypeByName("citext") != nil || len(base.Extensions) != 0 {
		t.Fatal("WithExtensions must not modify the base catalog")
	}
	if _, err := base.WithExtensions([]string{"nope"}); err == nil {
		t.Fatal("unknown extension must fail")
	}
	if avail := Available(17); len(avail) < 10 {
		t.Fatalf("Available: %v", avail)
	}
}
