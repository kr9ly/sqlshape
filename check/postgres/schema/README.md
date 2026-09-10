# schema

Layers `schema.sql` over the bootstrap catalog. Parsed with libpg_query (pg_query_go).

- `Load(sql)` → `*Schema`: relations (tables / views / matviews / composite types) with columns, constraints (PK / UNIQUE / FK / CHECK, unique indexes incl. partial), user types (enum labels, domain checks, composites + their array and row types with synthetic OIDs from 16384), function signatures, `COMMENT ON`
- Expression bodies (view queries, DEFAULT, CHECK, generated) stay as AST for the analyzer
- `Types.Format` reproduces `format_type()` spelling; `Types.BaseOf` flattens domains the way PG's wire protocol does
- `ddl.go`: partitions and INHERITS (parent columns / constraints copied), `LIKE ... INCLUDING`, RENAME (relations,
  columns, constraints, types, functions, triggers), DROP, ALTER TABLE DROP COLUMN / CONSTRAINT and the no-op
  actions, ALTER TYPE ADD / RENAME VALUE, ALTER DOMAIN, CREATE TYPE AS RANGE (+ multirange), CREATE COLLATION
  (COLLATE names are validated against built-ins, declarations and locale-style names), CREATE AGGREGATE /
  OPERATOR / CAST (resolved like catalog ones), `SET search_path` (lookup order and where unqualified CREATE lands),
  interval field / precision typmods
- `seed.go`: `INSERT ... VALUES` into a table records its rows as `Relation.Seed` (a lookup table's fixed content, keyed by the PK or a NOT NULL unique constraint the rows give as constants); non-idempotent INSERTs (no key, volatile values, `DEFAULT`, `ON CONFLICT`, duplicate keys, differing column lists) are Problems, and the analyzer type-checks each INSERT through `CheckStatement`. `-- sqlshape: seed` marks the rows additive
- Unsupported DDL lands in `Problems` with a byte offset; loading never aborts on it
- `TestAgainstOracle` boots real PG on the same file and diffs every table's columns
