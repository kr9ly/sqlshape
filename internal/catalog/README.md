# catalog

Static `pg_catalog` (types, functions, operators, casts, aggregates, ranges) embedded as TSV.
This is what the analyzer resolves against; user schema is layered on top elsewhere.

Extensions: `data/ext/<name>/` holds the same six TSVs plus `META` (version, schema, requires) for
one extension, dumped from a fresh database right after `CREATE EXTENSION ... CASCADE` (so a dump
bundles what it requires). `catalog.WithExtensions(names)` merges them on load, renumbering their
OIDs into a per-extension range (`extBase`) so they never meet user objects; `Type.Schema` /
`Func.Schema` / `Operator.Schema` keep the namespace they were created in ("" = pg_catalog).
`schema.LoadWith` calls it for every `CREATE EXTENSION` in schema.sql; one without a dump is a
schema problem. Dumped so far: btree_gin, btree_gist, citext, cube, earthdistance, fuzzystrmatch,
hstore, intarray, isn, ltree, pg_trgm, pgcrypto, seg, tablefunc, unaccent, uuid-ossp.

- Regenerate: `go run ./internal/catalog/gen` (boots the oracle PG, `COPY ... TO STDOUT`, writes `data/*.tsv` + `data/VERSION`);
  `-ext <name>` (repeatable) dumps an extension, `-list` shows what the oracle binary ships
- Load: `catalog.Load()` parses once per process (~10 ms); lookups by OID / name / (source,target) pair
- Column order in the TSVs is the contract between `gen/main.go` and `catalog.go`
