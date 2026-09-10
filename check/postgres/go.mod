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
	github.com/kr9ly/sqlshape/v2 v2.0.0-rc.1
	github.com/tetratelabs/wazero v1.12.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
