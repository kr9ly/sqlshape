# sqlshape

Make SQL a first-class citizen of your Go codebase.

SQL that lives in your repo gets what your Go code already has: version control,
type checking, refactoring, tests, dependency graphs, and editor diagnostics.
Not by hiding it behind a DSL or an ORM, but by checking it as SQL.

`sqlshape` is a `go/analysis` analyzer. It finds `sqlshape.Query[R, P](template)`
call sites, expands every branch of the template (`if` / `switch` / `range`),
prepares each expansion against a PostgreSQL loaded from `schema.sql`, and checks
that the declared row type `R` and parameter type `P` match what the database
reports.

No code generation. No DSL. Plain SQL, plain structs, and a linter that proves
they fit — hence *shape*. Views, functions, and composite types in `schema.sql`
are part of the same checked surface, so the database can expose a typed public
API instead of raw tables.

See [design.md](design.md) for the rationale and architecture.

## Status

Design stage. Nothing runs yet.
