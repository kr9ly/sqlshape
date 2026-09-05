# analyze

Pure-Go PostgreSQL semantic analyzer. `Analyze(schema, sql)` → parameter types, result columns
(type, nullability, provenance) or a PG-style `*Error` (SQLSTATE + position). No PostgreSQL at lint time.

Implements manual chapter 10: operator resolution (§10.2), function resolution (§10.3) incl.
variadic / defaults / polymorphic consistency, implicit / assignment / explicit coercion via
`pg_cast` + array / domain / record rules, `select_common_type` for UNION / CASE / COALESCE /
ARRAY / IN / VALUES (§10.5), `$n` inference from context with `text` fallback, parse-time literal
validation (22P02). Scopes: JOIN (USING / NATURAL / LATERAL), subqueries, CTEs (incl. recursive),
set operations, VALUES, functions in FROM, views (analyzed once), whole-row refs, INSERT / UPDATE /
DELETE with RETURNING and ON CONFLICT.

- `testdata/queries/*.sql` + `.golden`: goldens come from the real PG (`go test ./internal/analyze -update`);
  the default run compares the analyzer to them without starting PG
- Error fixtures agree on SQLSTATE; the message text is informative only
- Not yet: GROUP BY validity (42803), collation, range types' subtypes, ROWS FROM, data-modifying CTEs
