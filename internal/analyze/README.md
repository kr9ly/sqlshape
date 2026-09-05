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
