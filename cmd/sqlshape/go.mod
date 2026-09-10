module github.com/kr9ly/sqlshape/cmd/sqlshape

go 1.26.1

// The binary is its own module so that it can be GPLv2 (it carries the MySQL parser) while
// the runtime and pgtest stay Apache 2.0 at their import paths. The three modules of the
// repository release in lockstep under one version: scripts/release.sh vX.Y.Z sets the
// requires below to X.Y.Z and tags vX.Y.Z, mysql/vX.Y.Z and cmd/sqlshape/vX.Y.Z on one
// commit.
require (
	github.com/kr9ly/sqlshape v1.3.0-rc.1
	github.com/kr9ly/sqlshape/mysql v1.3.0-rc.1
)

require golang.org/x/tools v0.49.0

require (
	github.com/fergusstrange/embedded-postgres v1.34.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
