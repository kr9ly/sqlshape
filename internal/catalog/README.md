# catalog

Static `pg_catalog` (types, functions, operators, casts, aggregates) embedded as TSV.
This is what the analyzer resolves against; user schema is layered on top elsewhere.

- Regenerate: `go run ./internal/catalog/gen` (boots the oracle PG, `COPY ... TO STDOUT`, writes `data/*.tsv` + `data/VERSION`)
- Load: `catalog.Load()` parses once per process (~10 ms); lookups by OID / name / (source,target) pair
- Column order in the TSVs is the contract between `gen/main.go` and `catalog.go`
