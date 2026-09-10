# oracle

Real PostgreSQL (embedded-postgres, one release per supported major version: `binaries` in
oracle.go) as the ground truth for the pure-Go analyzer.

- `Start(ctx, schemaSQL)` boots a fresh instance of the default version in a temp dir;
  `StartVersion(ctx, v, schemaSQL)` one of another major version (binaries and an initialized
  template data directory are cached per release under `~/.cache/sqlshape/pg-<release>`, ~0.3 s
  start after the first download)
- `Describe(ctx, sql)` = anonymous PREPARE. Returns parameter types, result column types (as `format_type` prints them), and table-column provenance with `attnotnull`. PG rejections come back as `*PgError` with SQLSTATE and position
- `testdata/queries/*.sql` + `.golden`: the contract the analyzer must reproduce. Regenerate with `go test ./check/postgres/oracle -update`

Not used at lint time.
