# analyze

Pure-Go PostgreSQL semantic analyzer. `Analyze(schema, sql)` → parameter types, result columns
(type, nullability, provenance) or a PG-style `*Error` (SQLSTATE + position). No PostgreSQL at lint time.

Load the schema with `analyze.Load` (not `schema.Load`): it installs the loader's `ViewHook`, which
analyzes each view body as the view is created and freezes the output columns on
`schema.Relation.Frozen` — PG fixes a view's columns at CREATE VIEW (a later RENAME COLUMN / ADD COLUMN
on a base table does not reach `SELECT *`, and a matview keeps its names), so readers take the frozen
list and re-analyze the body only for the cardinality proof. Frozen columns keep the base column they
reference (`SrcRel` / `Src`) so writes through the view still land on the base table.

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

- Aggregate levels (`aggFrame`, check_agg_arguments): an aggregate belongs to the nearest query level
  its aggregated arguments' columns resolve to; that level becomes grouped, an aggregate of the same
  level inside the arguments is nested (42803), an aggregate may not sit in a FROM item of its own
  level, an ordered-set aggregate's direct arguments may not reach below its level and are checked
  as per-group expressions, GROUPING's arguments must belong to a grouped level
- Name spaces: a LATERAL item sees the earlier FROM items of its level; the left side of a RIGHT / FULL join and an
  UPDATE / DELETE target are in scope for a LATERAL item but illegal to reference (42P10); two items of one name at
  one level are ambiguous (42P09) as soon as a LATERAL item names them, `excluded` against a table named excluded too
- Recursive CTEs: forward references, a WITH nested on the recursive union, a nested WITH of the same name hiding the
  outer one, mutual recursion between items (0A000), SEARCH / CYCLE columns and their name rules (the recursive
  reference then has to sit in the recursive term's own FROM), and checkWellFormedRecursion (`recursive.go`: the query name once, not in a subquery / the nullable
  side of an outer join / EXCEPT / INTERSECT / the non-recursive term, no aggregates, no data-modifying body)
- Windows: named windows (duplicates, unknown references), RANGE offset frames need one ORDER BY column, GROUPS
  needs ORDER BY, no window functions inside window definitions / GROUP BY / RETURNING / JOIN conditions
- Writes through views (`viewdml.go`): INSERT / UPDATE / DELETE / MERGE on an automatically updatable view
  (one table in FROM, no set operation / DISTINCT / GROUP BY / HAVING / LIMIT / OFFSET / WITH / window / aggregate /
  set-returning target) are typed and their violations computed against the base table under the view's column
  names; a computed column is not writable (0A000, also when the view's own default on it fills an INSERT), any other
  view is 55000, a view with INSTEAD OF triggers takes any write. MERGE is checked down the stack of auto-updatable
  views: a rule for one of its actions refuses it (0A000, disabled rules do not count), triggers must cover all actions
  or none, a non-updatable view fails for the first uncovered action (55000); a materialized view is 0A000. System columns (ctid, xmin, xmax, cmin, cmax, tableoid) resolve on tables; system relations
  (pg_catalog, information_schema) come from the catalog (see catalog/README)
- Function calls: named arguments (`f(x => 1)`) map onto parameter names with defaults filling the rest; a
  variadic and a non-variadic candidate with the same effective argument list are one (the non-variadic wins),
  the same signature in two schemas goes to the earlier on the search path, one reached through defaults against
  one that is not is ambiguous (42725); a VARIADIC parameter with a default takes zero arguments; `f(x)` on a
  composite value with a field `f` is the field. type-name(x) is a cast only where the coercion is a relabeling or
  goes through I/O (func_get_detail), and an unqualified name never means a pg_temp type; ROW(..) op ROW(..) needs
  a btree comparison operator (0A000 otherwise). Result column
  names follow FigureColname (a cast names the column only when what it casts has no name of its own; SQL/JSON
  and XML constructors, merge_action(); a function in FROM names its one column by its single OUT parameter, then
  the alias, then the function)
- `testdata/queries/*.sql` + `.golden`: goldens come from the real PG (`go test ./internal/analyze -update`);
  the default run compares the analyzer to them without starting PG
- `regress_test.go`: `-regress /path/to/postgres/src/test/regress` replays PG's own regression corpus against a
  live PG and the analyzer side by side and writes every disagreement to `-regress-report`, grouped by kind
  (DIFF column name / type / nullability, STRICT = analyzer rejects what PG takes, LENIENT = the reverse, CODE =
  different SQLSTATE). A discovery tool, not a gate: `-regress-tests select,join` limits it to some files.
  A full run takes about half a minute: the schedule's first 8 lines build the shared database in order
  (`schema.Apply` extends the analyzer's schema statement by statement; each file's session ends with DISCARD ALL
  on the oracle and the matching temp-table drops / RESETs in the replay, as pg_regress gives every file its own
  session), every later file runs in its own copy of it, `-regress-jobs` (default NumCPU, at most 8) of them at a
  time on a server started with fsync off. psql's `\set` variables mean what they were set to at that point
  (`\set filename` before each COPY), `\c` is a DISCARD ALL, and DISCARD / DEALLOCATE ALL reset pgx's statement
  cache so the oracle's helper queries survive them.
  `SQLSHAPE_ORACLE_LOG=/path` keeps the server log (one MERGE in merge.sql segfaults PG 17's Prepare and is
  skipped by `reCrash`). `testdata/tools/bucket.sh` / `hits.py` slice a report by bucket. The oracle is PG's Describe, so errors PG
  only raises at execution (assignment length coercion of a literal into varchar(n) / bit(n) / numeric(p,s),
  view updatability decided by view-column defaults) count as STRICT there even though the analyzer is right
- `probe_test.go` runs one statement against one schema (`PROBE_SCHEMA=f PROBE_SQL=f [PROBE_ORACLE=1]`) for
  one-off checks while chasing a hit
- `literal_oracle_test.go`: with `-regress`, every `'literal'::type` in the corpus goes through the real input
  function and the analyzer (`-literal-types` narrows it); seconds, not minutes. The input functions are ported
  from PG's C: `datetime.go` (ParseDateTime / DecodeDateTime / DecodeTimeOnly / DecodeInterval /
  DecodeISO8601Interval with the token tables and the default time zone abbreviations; `SET datestyle` /
  `intervalstyle` / `timezone` in schema.sql are honoured), `literal_compound.go` (array_in with dimensions and
  element input by type, range / multirange, record, geo_ops, pg_lsn, bytea, inet / cidr, macaddr, bit),
  `jsonpath.go` (the jsonpath scanner / grammar acceptance rules), `literal.go` (numbers, money, macaddr8,
  tid, xid, snapshots, xml, json surrogates, regtype / regproc / regclass names)
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
