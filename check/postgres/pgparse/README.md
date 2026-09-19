# pgparse

The SQL parser: libpg_query compiled to WebAssembly, one module per supported PostgreSQL major
version, run by wazero. `Version.Parse` / `Deparse` / `SplitWithScanner` / `ParsePlPgSqlToJSON`
carry pg_query_go's signatures; every call takes a pooled instance (an instance is not safe for
concurrent use), and a module is compiled once per process, through a cache under the user cache
directory (`sqlshape/wazero`) because `go vet` starts one analyzer process per package.

Node types (`pg_query.pb.go`) are generated from the newest version's `pg_query.proto`. Every
version's tree is read from the parser's JSON output, which names fields and node types (the
protobuf numbers them, and libpg_query renumbers between versions). An older version's renamed
fields are rewritten on the JSON before decoding (`upgrade.go`); a field the table misses fails the
decode on an unknown field rather than vanishing. `Deparse` always runs on the newest module (a
tree of any version is in its types). `Default` is the version schema text without a declaration
gets (code-built schemas, tests); a `schema.sql` file must declare its version.

## Adding a PostgreSQL major version

Each step has a check; the regress probe at the end lists what the analyzer does not yet know
about the version.

1. Parser. `cd check/postgres/pgparse/wasm && nix-shell -p emscripten protobuf protoc-gen-go --run
   "./build.sh <libpg_query tag> pb"` (`pb` regenerates the node types from that tag's proto; the
   tag reads `17-6.2.2` up to 17 and `18.0.0` from 18). Add the `//go:embed` and the `PG<N>`
   constant in `pgparse.go`, set `newest`, and rebuild the older versions' modules without `pb`
   so the shim matches. Compare the new proto with the previous one (field renames / removals)
   and extend `upgrades` in `upgrade.go` for every older version. Check: `go test
   ./check/postgres/pgparse` — `TestCorpus` needs the previous version's regress corpus and fails on a
   field the upgrade missed. Call sites whose generated accessors changed (`ReturningList` →
   `GetReturningClause().GetExprs()` in 18) show up as build errors.
2. Oracle. Add the release to `binaries` in `check/postgres/oracle/oracle.go` (embedded-postgres must
   ship it). Check: `go test ./check/postgres/oracle`.
3. Catalog. `go run ./check/postgres/catalog/gen -pg <N>` then `-pg <N> -all-ext` writes
   `check/postgres/catalog/data/<N>/`. Check: `go test ./check/postgres/catalog`.
4. Regress corpus and baseline. `check/postgres/analyze/testdata/tools/fetch-regress.sh REL_<N>_<x>`
   (the release the oracle runs), then `SQLSHAPE_PG=<N> go test ./check/postgres/analyze -run
   'TestRegress$' -v -regress-report <file>`. Statements the loader drops show as downstream
   42P01s; new checks of the version show as LENIENT hits (the oracle errors, the analyzer does
   not); changed semantics as DIFFs. Port them under a version branch (`a.s.Version.Or() >=
   pgparse.PG<N>`), then write the baseline with `-regress-update` and read what is left: every
   line should be either environment-dependent, a known lenient spot, or a deliberate difference
   worth a note in `docs/design.md`.
5. CI. Add a job like `pg18` in `.github/workflows/test.yml` (corpus fetch, catalog and oracle
   tests, the probe). Mention the version in `schema.DeclaredVersion`'s error (it reads
   `pgparse.Supported()`), `docs/checks.md`, the README and the CHANGELOG.

Dropping a version is the reverse: its embed, constant, catalog directory, baseline and CI job go;
`upgrades` loses its entry.
