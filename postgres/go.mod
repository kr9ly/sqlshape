module github.com/kr9ly/sqlshape/postgres/v2

go 1.26.1

// The PostgreSQL runtime over pgx: runs a sqlshape.Stmt (Run / Collect / First / Exec,
// Get / Find for One), maps rows, loads user types, Batch, Copy, MatView. Its shape is
// pgx's; the MySQL runtime has its own. The root module is required by version;
// scripts/release.sh keeps it at the release's version.

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/kr9ly/sqlshape/v2 v2.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	golang.org/x/text v0.29.0 // indirect
)
