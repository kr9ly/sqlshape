module github.com/kr9ly/sqlshape/check/postgres/v2

go 1.26.1

// The PostgreSQL side of the checker: the analyzer, the schema loader, the embedded
// catalog, the libpg_query parser (WebAssembly), the oracle (embedded PostgreSQL), the
// migration tools. Tool-facing: cmd/sqlshape and pgtest import it; its packages are
// exported so that they can, and carry no compatibility promise of their own. The root
// module is required by version; scripts/release.sh keeps it at the release's version.

require (
	github.com/fergusstrange/embedded-postgres v1.34.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/tetratelabs/wazero v1.12.0
	google.golang.org/protobuf v1.36.12
)
