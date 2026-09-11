package analyze

import "testing"

// Adversarial round 3 (PostgreSQL), "null" lane: result column type / nullability.
// Each test below is reproduced against a real PostgreSQL (via oracle.Start / the
// checkNullability harness in adv_null_test.go) and asserts the correct behaviour, so it
// currently fails against sqlshape's analyzer.

// TestAdvNull3UnnestSelectListElementCanBeNull reproduces against real PG: a NOT NULL
// array column can still hold NULL *elements* (NOT NULL on an array type only forbids
// the array itself being NULL, not its elements). unnest() used as a set-returning
// function in the SELECT list expands each element as its own output row, and any
// element that is NULL comes out as a NULL row -- regardless of the array column's own
// NOT NULL. sqlshape's analyzer declares the unnest() result column NOT NULL here (it
// must be deriving the SRF's nullability from the source array's own nullability,
// without accounting for element-level NULLs), which is the high-severity hole: a vet
// consumer would bind this to a non-pointer Go field and panic (or silently misbehave)
// the first time a row holds a NULL element.
//
// Note FROM-clause unnest (`FROM t, unnest(arr) AS x`) is already handled correctly
// (nullable=true) -- this is specific to unnest() called directly in the SELECT list.
func TestAdvNull3UnnestSelectListElementCanBeNull(t *testing.T) {
	schemaSQL := `
CREATE TABLE t (id int PRIMARY KEY, arr int[] NOT NULL);
`
	inserts := []string{
		`INSERT INTO t (id, arr) VALUES (1, ARRAY[1, NULL, 3])`,
	}
	checkNullability(t, schemaSQL, inserts, `SELECT unnest(arr) FROM t`)
}
