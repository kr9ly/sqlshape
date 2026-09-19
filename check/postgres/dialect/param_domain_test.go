package dialect

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// TestParamOfKeepsDomainOfComparedColumn is docs/checks.md's "Do not mix domains of
// different units (PostgreSQL)": a parameter compared with a domain column
// (`price > {{.Max}}`) should carry that domain the way a domain *result* column already
// does (TypeOf reads the schema column's own declared type), so a plain int64 parameter
// gets the same -strict "carries domain ... as a plain int64" advisory a plain int64
// result column gets.
//
// PostgreSQL itself does not hand this to us on the wire: a parameter compared against a
// domain column is resolved to the domain's own *base* type (select_common_type falls
// back the moment one side is an unknown-typed placeholder -- domains reuse their base
// type's operators), confirmed by TestDomainNotes ("SELECT balance - $1 FROM users"
// keeps typ "yen" only because the *result* is balance's own expression; the parameter's
// own analyze.Result.Params entry is plain bigint, not the domain). ParamOf recovers the
// domain from the parameter's Source (the column it was compared with) instead.
func TestParamOfKeepsDomainOfComparedColumn(t *testing.T) {
	schemaSQL := `
CREATE DOMAIN yen AS bigint;
CREATE DOMAIN gram AS integer;
CREATE TABLE products (id bigint PRIMARY KEY, price yen NOT NULL, weight gram NOT NULL);
`
	s, err := schema.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, aerr := analyze.Analyze(s, `SELECT id FROM products WHERE price > $1`)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	if len(r.Params) != 1 {
		t.Fatalf("want 1 param, got %d", len(r.Params))
	}
	// the raw analyzer type PostgreSQL itself resolved: the domain's base type, not yen
	// -- this is the fact ParamOf has to work around, not a bug in Analyze.
	if got := s.Types.Format(r.Params[0]); got != "bigint" {
		t.Fatalf("analyze.Result.Params[0] = %s, want bigint (PostgreSQL resolves a parameter met against a domain column to the domain's base type)", got)
	}

	out := ResultOf(s, r)
	if len(out.Params) != 1 {
		t.Fatalf("want 1 param in the contract, got %d", len(out.Params))
	}
	p := out.Params[0]
	if p.Type.Kind != dialect.Domain {
		t.Errorf("Params[0].Type.Kind = %v, want Domain", p.Type.Kind)
	}
	if p.Type.Named != "yen" {
		t.Errorf("Params[0].Type.Named = %q, want %q", p.Type.Named, "yen")
	}
	if p.Source == nil || p.Source.Table != "products" || p.Source.Column != "price" {
		t.Errorf("Params[0].Source = %+v, want products.price", p.Source)
	}
}

// TestParamOfLeavesNonDomainParamsAlone is the negative case: a parameter compared with
// a plain (non-domain) column keeps whatever type PostgreSQL resolved, unchanged.
func TestParamOfLeavesNonDomainParamsAlone(t *testing.T) {
	schemaSQL := `CREATE TABLE products (id bigint PRIMARY KEY, qty integer NOT NULL);`
	s, err := schema.Load(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	r, aerr := analyze.Analyze(s, `SELECT id FROM products WHERE qty > $1`)
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}
	out := ResultOf(s, r)
	if len(out.Params) != 1 {
		t.Fatalf("want 1 param, got %d", len(out.Params))
	}
	if out.Params[0].Type.Named != "" {
		t.Errorf("Params[0].Type.Named = %q, want \"\" (qty is not a domain)", out.Params[0].Type.Named)
	}
}
