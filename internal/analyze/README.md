# analyze

Pure-Go PostgreSQL semantic analyzer. `Analyze(schema, sql)` → parameter types, result columns
(type, nullability, provenance) or a PG-style `*Error` (SQLSTATE + position). No PostgreSQL at lint time.

`Result.Notes` carries findings PG itself would accept and so never appear in a golden: domains as
opaque units (`domain.go` — a domain value only meets the same domain or a literal / parameter; mixing
with another domain or the plain base type is a note; unit-preserving operations keep the domain on
their result so the check follows the value). Covered by `TestDomainNotes`; the golden test asserts
that parity fixtures produce no notes.

`Result.AtMostOne` / `ManyRowsWhy` is the cardinality proof (`card.go`): a functional-dependency
fixpoint over the FROM leaves — a leaf is single once a unique key is fixed by equalities to known
values (literals, `$n`, outer refs, uncorrelated scalar subqueries), a single leaf makes its columns
known, outer-join ON clauses only fix their nullable side, views / subqueries / CTEs are proved
recursively with the outer-fixed output columns as seeds. Covered by `TestCardinality`.

`Result.Violations` lists the constraints a write may violate (`violation.go`): every unique key on
INSERT, those touching SET columns on UPDATE, referencing FKs with NO ACTION / RESTRICT on DELETE and
key changes, CHECKs referencing written columns, domain CHECKs, NOT NULL when the value is nullable
(with the parameter number so the checker can drop it for non-nullable Go types); ON CONFLICT absorbs
its arbiter. Unnamed constraints are named by `schema.addConstraint` the way PG names them. Covered by
`TestViolations`.

Implements manual chapter 10: operator resolution (§10.2), function resolution (§10.3) incl.
variadic / defaults / polymorphic consistency, implicit / assignment / explicit coercion via
`pg_cast` + array / domain / record rules, `select_common_type` for UNION / CASE / COALESCE /
ARRAY / IN / VALUES (§10.5), `$n` inference from context with `text` fallback, parse-time literal
validation (22P02). Scopes: JOIN (USING / NATURAL / LATERAL), subqueries, CTEs (incl. recursive),
set operations, VALUES, functions in FROM, views (analyzed once), whole-row refs, ROWS FROM, data-modifying CTEs, CALL, INSERT / UPDATE /
DELETE with RETURNING and ON CONFLICT, MERGE (WHEN MATCHED / NOT MATCHED [BY SOURCE], RETURNING with merge_action()),
SQL/JSON (`json.go`: JSON_OBJECT / JSON_ARRAY / JSON_ARRAYAGG / JSON_OBJECTAGG, JSON_EXISTS / JSON_QUERY / JSON_VALUE,
JSON() / JSON_SCALAR / JSON_SERIALIZE, JSON_TABLE in FROM), SQL/XML expressions, `(expr).*`, functions with OUT
parameters in FROM, WHERE CURRENT OF, and utility statements (TRUNCATE / LOCK / REFRESH MATERIALIZED VIEW / NOTIFY /
SET / SHOW / transaction control / DO / VACUUM / ANALYZE / COPY / DECLARE CURSOR / CREATE TABLE AS). FETCH stays
0A000: a cursor's columns are not known statically.

- Writes through views (`viewdml.go`): INSERT / UPDATE / DELETE / MERGE on an automatically updatable view
  (one table in FROM, no set operation / DISTINCT / GROUP BY / HAVING / LIMIT / OFFSET / WITH / window / aggregate /
  set-returning target) are typed and their violations computed against the base table under the view's column
  names; a computed column is not writable (0A000), any other view is 55000, a view with INSTEAD OF triggers takes
  any write. System columns (ctid, xmin, xmax, cmin, cmax, tableoid) resolve on tables; system relations
  (pg_catalog, information_schema) come from the catalog (see catalog/README)
- Function calls: named arguments (`f(x => 1)`) map onto parameter names with defaults filling the rest; a
  variadic and a non-variadic candidate with the same effective argument list are one (the non-variadic wins,
  then the one using fewer defaults); `f(x)` on a composite value with a field `f` is the field. Result column
  names follow FigureColname (a cast names the column only when what it casts has no name of its own; SQL/JSON
  and XML constructors, merge_action(); a function in FROM names its one column by its single OUT parameter, then
  the alias, then the function)
- `testdata/queries/*.sql` + `.golden`: goldens come from the real PG (`go test ./internal/analyze -update`);
  the default run compares the analyzer to them without starting PG
- `regress_test.go`: `-regress /path/to/postgres/src/test/regress` replays PG's own regression corpus against a
  live PG and the analyzer side by side and writes every disagreement to `-regress-report`, grouped by kind
  (DIFF column name / type / nullability, STRICT = analyzer rejects what PG takes, LENIENT = the reverse, CODE =
  different SQLSTATE). A discovery tool, not a gate: `-regress-tests select,join` limits it to some files
- Error fixtures agree on SQLSTATE; the message text is informative only
- GROUP BY validity (`grouping.go`): grouping expressions matched by deparsed text, aggregate arguments
  exempt, ungrouped columns allowed when their table's primary key is grouped; GROUPING SETS / ROLLUP / CUBE checked against the union of their expressions, grouped columns become nullable
- Nullability is refined by null-rejecting predicates (IS NOT NULL, strict comparisons, inner-join ON), also inside a
  searched CASE branch under its WHEN; non-strict built-ins that never return NULL and nullary functions are not null
- Collations (`collation.go`): explicit / implicit derivation per §24.2.2; conflicting COLLATE clauses and
  UNION / INTERSECT / EXCEPT over conflicting implicit collations are the PG errors (42P21), an indeterminate
  collation reaching a comparison, `lower()` / `max()`, ORDER BY, GROUP BY or DISTINCT is a Note (PG fails at run time).
  Collation names are not validated against pg_collation
- Extensions: `CREATE EXTENSION` in schema.sql merges the extension's dumped catalog (see catalog/README),
  so its types, functions, operators and casts resolve like pg_catalog's, in the schema it was created in
