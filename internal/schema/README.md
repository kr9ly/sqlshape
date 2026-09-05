# schema

Layers `schema.sql` over the bootstrap catalog. Parsed with libpg_query (pg_query_go).

- `Load(sql)` → `*Schema`: relations (tables / views / matviews / composite types) with columns, constraints (PK / UNIQUE / FK / CHECK, unique indexes incl. partial), user types (enum labels, domain checks, composites + their array and row types with synthetic OIDs from 16384), function signatures, `COMMENT ON`
- Expression bodies (view queries, DEFAULT, CHECK, generated) stay as AST for the analyzer
- `Types.Format` reproduces `format_type()` spelling; `Types.BaseOf` flattens domains the way PG's wire protocol does
- Unsupported DDL lands in `Problems` with a byte offset; loading never aborts on it
- `TestAgainstOracle` boots real PG on the same file and diffs every table's columns
