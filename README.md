# sqlshape

Make SQL a first-class citizen of your codebase.

SQL that lives in your repo gets what your application code already has: version control,
type checking, refactoring, tests, dependency graphs, and editor diagnostics.
Not by hiding it behind a DSL or an ORM, but by checking it as SQL.

The core is language-agnostic: a pure-Go PostgreSQL analyzer built from a catalog
generated out of PostgreSQL's own source (`pg_proc.dat`, `pg_type.dat`, ...) plus
your `schema.sql`. A real PostgreSQL is used only as the oracle in differential
tests. The first frontend is a `go/analysis` analyzer that finds
`sqlshape.Query[R, P](template)` call sites, expands every branch of the template
(`if` / `switch` / `range`), analyzes each expansion, and checks that the declared
row type `R` and parameter type `P` match what the SQL actually produces.

No code generation. No DSL. Plain SQL, plain structs, and a linter that proves
they fit — hence *shape*. Views, functions, and composite types in `schema.sql`
are part of the same checked surface, so the database can expose a typed public
API instead of raw tables.

See [design.md](design.md) for the rationale and architecture.

## Status

Design stage. Nothing runs yet.
