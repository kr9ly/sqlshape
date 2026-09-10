module github.com/kr9ly/sqlshape/examples/v2

go 1.26.1

// The examples are a module of their own so that the root module stays free of database
// dependencies. They are not released: the workspace (go.work) resolves the sqlshape
// modules they import, and the smoke test vets them in it.

require (
	github.com/jackc/pgx/v5 v5.10.0
)
