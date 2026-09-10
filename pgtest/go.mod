module github.com/kr9ly/sqlshape/pgtest/v2

go 1.26.1

// pgtest boots a real PostgreSQL (embedded-postgres) loaded with the application's
// schema.sql for its tests, and verifies the statements against it. Its own module: the
// analyzer and the embedded server it needs are nothing an application's binary should
// depend on. The sqlshape modules are required by version; scripts/release.sh keeps them
// at the release's version.

require (
	github.com/jackc/pgx/v5 v5.10.0
)
