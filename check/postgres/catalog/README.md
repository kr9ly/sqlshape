# catalog

Static `pg_catalog` (types, functions, operators, casts, aggregates, ranges, and the system
relations) embedded as TSV. This is what the analyzer resolves against; user schema is layered on
top elsewhere.

System relations (`pg_class.tsv`, one row per column): every pg_catalog and information_schema
table / view with its columns' types and NOT NULL, so `SELECT relname FROM pg_class` or a query
over `information_schema.columns` types like any table. information_schema's domains
(`sql_identifier`, `cardinal_number`, ...) ride along in `pg_type.tsv` with their schema; they
resolve only qualified, as in PG. `schema.findRelation` consults these after the user's relations
of the search path (pg_catalog implicitly first, as PG does).

Extensions: `data/ext/<name>/` holds the same six TSVs plus `META` (version, schema, requires) for
one extension, dumped from a fresh database right after `CREATE EXTENSION ... CASCADE` (so a dump
bundles what it requires). `catalog.WithExtensions(names)` merges them on load, renumbering their
OIDs into a per-extension range (`extBase`) so they never meet user objects; `Type.Schema` /
`Func.Schema` / `Operator.Schema` keep the namespace they were created in ("" = pg_catalog).
`schema.LoadWith` calls it for every `CREATE EXTENSION` in schema.sql; one without a dump is a
schema problem. Dumped so far: btree_gin, btree_gist, citext, cube, earthdistance, fuzzystrmatch,
hstore, intarray, isn, ltree, pg_trgm, pgcrypto, seg, tablefunc, unaccent, uuid-ossp.

- One directory per PostgreSQL major version: `data/<major>/` holds `VERSION`, the bootstrap TSVs
  and `ext/`. A schema's `-- sqlshape: postgres <N>` picks the directory (`catalog.Load(major)`).
- Regenerate: `go run ./check/postgres/catalog/gen -pg 18` (boots the oracle PG of that major,
  `COPY ... TO STDOUT`, writes `data/18/*.tsv` + `data/18/VERSION`); `-ext <name>` (repeatable)
  dumps an extension, `-all-ext` every extension the default version has, `-list` shows what the
  oracle binary ships
- Load: `catalog.Load(major)` parses once per process per version (~10 ms); lookups by OID / name /
  (source,target) pair
- Column order in the TSVs is the contract between `gen/main.go` and `catalog.go`
