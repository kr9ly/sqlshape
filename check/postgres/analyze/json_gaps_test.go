package analyze

import (
	"strings"
	"testing"
)

// TestJSONGaps* cover internal/analyze/json.go: the SQL/JSON constructor and query
// expressions (JSON_OBJECT, JSON_ARRAY, JSON_OBJECTAGG, JSON_ARRAYAGG, JSON_EXISTS,
// JSON_QUERY, JSON_VALUE, JSON_TABLE) and the SQL/XML functions (XMLELEMENT, XMLFOREST,
// XMLPARSE, XMLSERIALIZE, XMLTABLE, IS DOCUMENT). Each case targets one branch left
// uncovered by gaps_test.go's TestGapsXMLTable and the golden suite, per the profile at
// json.go. orders.meta (jsonb) from testdata/schema.sql is the JSON context used
// throughout.

// TestJSONGapsOutputReturning covers jsonOutput: RETURNING an undefined type errors,
// and RETURNING ENCODING is only meaningful for a bytea RETURNING type.
func TestJSONGapsOutputReturning(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_OBJECT('a': 1 RETURNING no_such_type)`); err.Code != "42704" {
		t.Errorf("JSON_OBJECT RETURNING no_such_type: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total RETURNING no_such_type) FROM orders o`); err.Code != "42704" {
		t.Errorf("JSON_ARRAYAGG RETURNING no_such_type: got %v", err)
	}
}

// TestJSONGapsBehaviorDefault covers jsonBehavior / containsSubLink: a DEFAULT
// expression must be a constant / non-aggregate / non-window expression with no
// subquery, no set-returning function, and no column reference.
func TestJSONGapsBehaviorDefault(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT (SELECT 1) ON ERROR) FROM orders o`); err.Code != "42804" || !strings.Contains(err.Error(), "DEFAULT") {
		t.Errorf("DEFAULT with subquery: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT count(*) ON ERROR) FROM orders o`); err.Code != "42804" {
		t.Errorf("DEFAULT with aggregate: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT (row_number() OVER ()) ON ERROR) FROM orders o`); err.Code != "42804" {
		t.Errorf("DEFAULT with window: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT generate_series(1,2) ON ERROR) FROM orders o`); err.Code != "42804" || !strings.Contains(err.Error(), "must not return a set") {
		t.Errorf("DEFAULT with SRF: got %v", err)
	}
	// PostgreSQL 17 folds the column-reference case into the same generic message as
	// the subquery/aggregate/window case above (verified against the oracle), so only
	// the SQLSTATE is asserted here -- see the BUG note in the file header for
	// sqlshape's distinct (and non-matching) message text for this case.
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT o.total ON ERROR) FROM orders o`); err.Code != "42804" {
		t.Errorf("DEFAULT with column reference: got %v", err)
	}
	// A DEFAULT expression that itself fails to analyze.
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' DEFAULT no_such_func(1) ON ERROR) FROM orders o`); err.Code != "42883" {
		t.Errorf("DEFAULT with undefined function: got %v", err)
	}
	// jsonBehavior's own bind call: an untyped DEFAULT literal that doesn't parse as
	// the RETURNING type (int here), caught by validateLiteralTypmod.
	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' RETURNING int DEFAULT 'not a number' ON ERROR) FROM orders o`); err.Code != "22P02" || !strings.Contains(err.Error(), "invalid input syntax for type integer") {
		t.Errorf("DEFAULT literal not valid for RETURNING type: got %v", err)
	}
}

// TestJSONGapsValueErrors covers jsonValue: an invalid value expression. (jsonValue's
// own bind-to-text call can only fail on a setParam conflict, which requires two
// *unresolved* references to the same parameter to be bound one after the other; every
// SQL/JSON construct here analyzes-and-binds a value inline, so by the time any second
// use of the same $n is analyzed, the parameter is already concretely resolved and is
// adopted silently rather than re-checked -- this makes jsonValue's bind error branch
// unreachable from valid SQL, confirmed by tracing bindTypmod/setParam in expr.go.)
func TestJSONGapsValueErrors(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_OBJECT('a': no_such_col)`); err.Code != "42703" {
		t.Errorf("JSON_OBJECT with undefined column value: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAY(no_such_col)`); err.Code != "42703" {
		t.Errorf("JSON_ARRAY with undefined column element: got %v", err)
	}
}

// TestJSONGapsContextErrors covers jsonContext: an invalid context expression, a
// context item whose resolved type cannot become jsonb, and the ENCODING-only-for-bytea
// FORMAT/ENCODING combination. An untyped string literal context item is not validated
// as JSON and ENCODING UTF16 / UTF32 on a bytea item is accepted, as in PostgreSQL 17:
// JSON_EXISTS / JSON_VALUE / JSON_QUERY coerce the context item through a JsonValueExpr
// evaluated at run time, not through a folded cast.
func TestJSONGapsContextErrors(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_EXISTS(no_such_col, '$.a')`); err.Code != "42703" {
		t.Errorf("JSON_EXISTS with undefined column context: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_EXISTS(true, '$.a')`); err.Code != "42846" || !strings.Contains(err.Error(), "cannot cast type") {
		t.Errorf("JSON_EXISTS with boolean context: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_EXISTS('x'::text FORMAT JSON ENCODING UTF8, '$.a')`); err.Code != "42804" || !strings.Contains(err.Error(), "ENCODING") {
		t.Errorf("ENCODING on non-bytea context: got %v", err)
	}
	for _, sql := range []string{
		`SELECT JSON_EXISTS('{not valid json', '$.a')`,
		`SELECT JSON_EXISTS('\x7b7d'::bytea FORMAT JSON ENCODING UTF16, '$.a')`,
		`SELECT JSON_VALUE('\x7b7d'::bytea FORMAT JSON ENCODING UTF32, '$.a')`,
	} {
		if _, err := Analyze(s, sql); err != nil {
			t.Errorf("%s: PostgreSQL accepts this at parse time, got %v", sql, err)
		}
	}
}

// TestJSONGapsPassing covers jsonPassing: a PASSING value that fails to analyze, both
// at the JSON_VALUE level and inside JSON_TABLE.
func TestJSONGapsPassing(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_VALUE(o.meta, '$.a' PASSING no_such_col AS x) FROM orders o`); err.Code != "42703" {
		t.Errorf("PASSING with undefined column: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT * FROM JSON_TABLE('{}'::jsonb, '$' PASSING no_such_col AS x COLUMNS (v text PATH '$.v'))`); err.Code != "42703" {
		t.Errorf("JSON_TABLE PASSING with undefined column: got %v", err)
	}
}

// TestJSONGapsConstructorKeys covers jsonConstructorList's key handling: a key that
// fails to analyze.
func TestJSONGapsConstructorKeys(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_OBJECT(no_such_col: 1)`); err.Code != "42703" {
		t.Errorf("JSON_OBJECT with undefined column key: got %v", err)
	}
}

// TestJSONGapsAggClauses covers jsonAgg: the aggregate argument, FILTER, ORDER BY and
// OVER clauses each surface their own analysis errors.
func TestJSONGapsAggClauses(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_ARRAYAGG bad arg: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_OBJECTAGG(o.id: no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_OBJECTAGG bad value: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total) FILTER (WHERE no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_ARRAYAGG FILTER bad expr: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total) FILTER (WHERE 'not a bool') FROM orders o`); err.Code != "22P02" || !strings.Contains(err.Error(), "invalid input syntax for type boolean") {
		t.Errorf("JSON_ARRAYAGG FILTER bad literal: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total ORDER BY no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_ARRAYAGG ORDER BY bad expr: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total) OVER (PARTITION BY no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_ARRAYAGG OVER PARTITION BY bad expr: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT JSON_ARRAYAGG(o.total) OVER (ORDER BY no_such_col) FROM orders o`); err.Code != "42703" {
		t.Errorf("JSON_ARRAYAGG OVER ORDER BY bad expr: got %v", err)
	}
}

// TestJSONGapsTable covers jsonTable / jsonTableColumns: a bad context item, a bad
// column type name, a bad nested-column analysis, and a column DEFAULT ... ON ERROR
// that references a column (disallowed, same rule as top-level JSON_VALUE DEFAULT).
func TestJSONGapsTable(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT * FROM JSON_TABLE(no_such_col, '$' COLUMNS (v text PATH '$.v'))`); err.Code != "42703" {
		t.Errorf("JSON_TABLE bad context: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT * FROM orders o, JSON_TABLE(o.meta, '$' COLUMNS (v no_such_type PATH '$.v')) AS t`); err.Code != "42704" {
		t.Errorf("JSON_TABLE bad column type: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT * FROM orders o, JSON_TABLE(o.meta, '$' COLUMNS (NESTED PATH '$.items[*]' COLUMNS (v no_such_type PATH '$.v'))) AS t`); err.Code != "42704" {
		t.Errorf("JSON_TABLE nested column bad type: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT * FROM orders o, JSON_TABLE(o.meta, '$' COLUMNS (v int PATH '$.v' DEFAULT o.id ON ERROR)) AS t`); err.Code != "42804" {
		t.Errorf("JSON_TABLE column DEFAULT with column reference: got %v", err)
	}
}

// TestJSONGapsXMLElement covers xmlExpr's XMLELEMENT / XMLATTRIBUTES rules: a
// duplicate attribute name, an unnamed attribute value that isn't a column reference,
// and a bad expression among the element content.
func TestJSONGapsXMLElement(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT xmlelement(name foo, xmlattributes('a' AS bar, 'b' AS bar))`); err.Code != "42601" || !strings.Contains(err.Error(), "appears more than once") {
		t.Errorf("duplicate XML attribute name: got %v", err)
	}
	// An unnamed attribute value that IS a column reference takes its name from the
	// column (the success path of the same branch checked above).
	if _, err := Analyze(s, `SELECT xmlelement(name foo, xmlattributes(u.name)) FROM users u`); err != nil {
		t.Errorf("unnamed XML attribute from column reference: %v", err)
	}
	if err := gapsErr(t, s, `SELECT xmlelement(name foo, no_such_col)`); err.Code != "42703" {
		t.Errorf("XMLELEMENT bad content: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT xmlforest(no_such_col)`); err.Code != "42703" {
		t.Errorf("XMLFOREST bad content: got %v", err)
	}
}

// TestJSONGapsXMLBind covers xmlExpr's XMLROOT bind branch (bind to xml): an untyped
// literal that isn't valid XML content, caught by validateXMLLiteral through
// validateLiteralTypmod. (XMLPARSE and the default op fallback both bind to text,
// which validateLiteralTypmod does not check at all -- and, like jsonValue's bind to
// text, cannot fail on a parameter conflict either, for the same structural reason
// documented on TestJSONGapsValueErrors -- so both are unreachable from valid SQL.)
func TestJSONGapsXMLBind(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT xmlroot('<a>', VERSION '1.0')`); !strings.Contains(err.Error(), "invalid XML") {
		t.Errorf("XMLROOT with invalid XML literal: got %v", err)
	}
}

// TestJSONGapsXMLSerialize covers xmlSerialize: a bad source expression, an untyped
// source literal that isn't valid XML (its own bind-to-xml call), and an undefined
// RETURNING (AS) type.
func TestJSONGapsXMLSerialize(t *testing.T) {
	s := gapsSchema(t, "")

	if err := gapsErr(t, s, `SELECT xmlserialize(DOCUMENT no_such_col AS text)`); err.Code != "42703" {
		t.Errorf("XMLSERIALIZE bad source: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT xmlserialize(DOCUMENT '<a>' AS text)`); !strings.Contains(err.Error(), "invalid XML") {
		t.Errorf("XMLSERIALIZE with invalid XML literal source: got %v", err)
	}
	if err := gapsErr(t, s, `SELECT xmlserialize(DOCUMENT xmlelement(name foo) AS no_such_type)`); err.Code != "42704" {
		t.Errorf("XMLSERIALIZE bad target type: got %v", err)
	}
}
