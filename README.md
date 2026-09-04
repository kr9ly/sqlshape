# sqlshape

Static checker for SQL templates in Go code, backed by a real PostgreSQL.

`sqlshape` is a `go/analysis` analyzer. It finds `sqlshape.Query[R, P](template)`
call sites, expands every branch of the template (`if` / `switch` / `range`),
prepares each expansion against a PostgreSQL loaded from `schema.sql`, and checks
that the declared row type `R` and parameter type `P` match what the database
reports.

No code generation. No DSL. Plain SQL, plain structs, and a linter that proves
they fit — hence *shape*.

See [design.md](design.md) for the rationale and architecture.

## Status

Design stage. Nothing runs yet.
