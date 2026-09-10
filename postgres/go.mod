module github.com/kr9ly/sqlshape/postgres/v2

go 1.26.1

// The PostgreSQL runtime over pgx: runs a sqlshape.Stmt (Run / Collect / First / Exec,
// Get / Find for One), maps rows, loads user types, Batch, Copy, MatView. Its shape is
// pgx's; the MySQL runtime has its own. The root module is required by version;
// scripts/release.sh keeps it at the release's version.

require github.com/jackc/pgx/v5 v5.10.0
