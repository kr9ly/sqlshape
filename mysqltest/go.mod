module github.com/kr9ly/sqlshape/mysqltest/v2

go 1.26.1

// mysqltest boots a real mysqld (the one on PATH) loaded with the application's schema.sql
// for its tests. Its own module: nothing an application's binary should depend on. The
// sqlshape modules are required by version; scripts/release.sh keeps them at the release's
// version.

require github.com/go-sql-driver/mysql v1.10.1
