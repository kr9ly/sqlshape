# oracle

Real PostgreSQL (embedded-postgres, PG 17) as the ground truth for the pure-Go analyzer.

- `Start(ctx, schemaSQL)` boots a fresh instance in a temp dir (binaries cached in `~/.embedded-postgres-go`, ~1 s start after first download)
- `Describe(ctx, sql)` = anonymous PREPARE. Returns parameter types, result column types (as `format_type` prints them), and table-column provenance with `attnotnull`. PG rejections come back as `*PgError` with SQLSTATE and position
- `testdata/queries/*.sql` + `.golden`: the contract the analyzer must reproduce. Regenerate with `go test ./internal/oracle -update`

Not used at lint time.
