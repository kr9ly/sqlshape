module github.com/kr9ly/sqlshape/mysql/v2

go 1.26.1

// The MySQL runtime over database/sql with go-sql-driver/mysql: runs a sqlshape.Stmt
// (Run / Collect / First / Exec, Get / Find / ExecOne for One) and maps rows. Its shape is
// database/sql's; the PostgreSQL runtime has its own. The root module is required by
// version; scripts/release.sh keeps it at the release's version.

require github.com/go-sql-driver/mysql v1.10.1
