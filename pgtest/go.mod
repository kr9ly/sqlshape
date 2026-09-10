module github.com/kr9ly/sqlshape/pgtest/v2

go 1.26.1

// pgtest boots a real PostgreSQL (embedded-postgres) loaded with the application's
// schema.sql for its tests, and verifies the statements against it. Its own module: the
// analyzer and the embedded server it needs are nothing an application's binary should
// depend on. The sqlshape modules are required by version; scripts/release.sh keeps them
// at the release's version.

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/kr9ly/sqlshape/check/postgres/v2 v2.0.0-rc.1
	github.com/kr9ly/sqlshape/v2 v2.0.0-rc.1
)

require (
	github.com/fergusstrange/embedded-postgres v1.34.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
