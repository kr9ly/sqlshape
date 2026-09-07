package schema

import (
	"testing"

	"github.com/kr9ly/sqlshape/internal/catalog"
)

// TestTypesUnknownOID exercises the defensive t == nil guards in renameUser / moveUser /
// removeUser: called with an OID that names no user type, they are no-ops rather than
// panicking. These paths are not reachable through any DDL the loader accepts (every
// caller already has a *catalog.Type in hand), so they are only reachable white-box.
func TestTypesUnknownOID(t *testing.T) {
	ts := newTypes(nil)
	const bogus = 999999
	ts.renameUser(bogus, "x")   // no panic, no effect
	ts.moveUser(bogus, "other") // no panic, no effect
	ts.removeUser(bogus)        // no panic, no effect
	if len(ts.user) != 0 {
		t.Errorf("no user types should have been created: %v", ts.user)
	}
}

// TestSeedIndexMissing exercises Seed.index's defensive -1 return: looking up a column
// name the seed's row shape does not carry. Not reachable through DDL (every caller
// passes a column already known to be in Seed.Columns), so only white-box.
func TestSeedIndexMissing(t *testing.T) {
	sd := &Seed{Columns: []string{"a", "b"}}
	if sd.index("nope") != -1 {
		t.Error("index of an absent column should be -1")
	}
}

func TestTypesUser(t *testing.T) {
	s := load(t)
	if len(s.Types.User()) == 0 {
		t.Error("User() should list the schema's declared types")
	}
}

// An unqualified type name finds pg_catalog first, as PostgreSQL does: a user type called
// like a built-in does not shadow it unless qualified.
func TestBuiltinNotShadowedByUserType(t *testing.T) {
	s, err := Load(`CREATE TYPE int4 AS (x integer); CREATE TABLE t (a int4, b public.int4);`)
	if err != nil {
		t.Fatal(err)
	}
	rel := s.Relation("", "t")
	if got := rel.Column("a").Type.OID; got != catalog.Int4 {
		t.Errorf("a int4 resolved to OID %d, want the built-in %d", got, catalog.Int4)
	}
	if got := rel.Column("b").Type.OID; got < FirstUserOID {
		t.Errorf("b public.int4 resolved to OID %d, want the user type", got)
	}
}
