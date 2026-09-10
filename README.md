# sqlshape

[![Go Reference](https://pkg.go.dev/badge/github.com/kr9ly/sqlshape.svg)](https://pkg.go.dev/github.com/kr9ly/sqlshape)
[![release](https://img.shields.io/github/v/release/kr9ly/sqlshape)](https://github.com/kr9ly/sqlshape/releases)
[![test](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml/badge.svg)](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml)
![coverage](.github/badges/coverage.svg)

Write SQL as SQL, and let a `go vet` checker prove the Go code around it fits.

[日本語](README.ja.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := postgres.Get(ctx, db, ByEmail, struct{ Email string }{Email: email})
```

- Checked, not generated. Every statement is analyzed against `schema.sql` by a pure-Go
  PostgreSQL analyzer: the result columns must fit the row type, the `{{.X}}` parameters must
  fit the parameter type, nullability is enforced, `One` has to be provably single-row, and a
  write must declare the constraints it can violate. Every finding is a `go vet` diagnostic,
  reported before the code ever runs.
- Plain SQL, no injection. Templates are Go `text/template`: `{{.X}}` always becomes a
  `$n` parameter, never text, and every `{{if}}` / `{{range}}` combination is expanded and
  checked. The runtime refuses any rendering the checker never saw.
- `schema.sql` is the only definition. The checker reads it, `pgtest` runs your tests against
  a real PostgreSQL with it applied, and `sqlshape diff` derives the migration from it. Views, functions,
  domains, composite types, row-level security and seeded lookup tables are all part of the
  checked surface, so the database can expose a typed API instead of raw tables.

## Install

The checker and the migration commands are one binary. With Go 1.26 or newer:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape@latest
$ sqlshape version
```

Or take a prebuilt binary from the [releases page](https://github.com/kr9ly/sqlshape/releases):
Linux, macOS and Windows, amd64 and arm64 each, as `sqlshape_<version>_<os>_<arch>.tar.gz` (`.zip`
on Windows) with `checksums.txt` beside them. Unpack and put `sqlshape` on your `PATH`.

Nothing else is needed to check code. PostgreSQL's parser (libpg_query, one per supported major
version) is embedded as WebAssembly and runs on wazero, so there is no C compiler to install and no
library to link; the first run compiles the module (about a second) and caches the result under the
user cache directory (`~/.cache/sqlshape` on Linux). `pgtest` and the migration commands, which run
your schema on a real PostgreSQL, download the declared version's server binaries into the same
cache on first use (see [migrations.md](docs/migrations.md#requirements)).

The declarations (`sqlshape.Query`, `sqlshape.One`) are a Go module with no dependencies, and
the runtime for each database is a module of its own, so an application pulls in only its own
driver:

```
$ go get github.com/kr9ly/sqlshape            # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/postgres   # running them on pgx
```

Versions follow semantic versioning; every module of the repository is tagged together
(`vX.Y.Z`, `postgres/vX.Y.Z`, ...). Within a major version, the exported API of these modules and
of `pgtest`, the template syntax, the directives and the checker's flags stay compatible; what
the checker reports may grow with minor versions.

## Quickstart

```sql
-- schema.sql
-- sqlshape: postgres 17
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

The first line of `schema.sql` names the PostgreSQL version the schema is written for; every
statement is judged with that version's grammar and catalog (17 and 18 are supported).

```go
// users.go
type User struct {
	ID        int64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.Query[int64, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

```
$ sqlshape ./...
users.go:20:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:25:46: sqlshape: may violate users_email_key (UNIQUE (email) on users, SQLSTATE 23505); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

Make `DeletedAt` a `*time.Time`, put `-- sqlshape: expect users_email_key` on the INSERT's first
line, and the package is clean. At run time:

```go
users, err := postgres.Collect(ctx, pool, Users, struct{ Name *string }{})          // []User
id, err := postgres.First(ctx, pool, Create, struct{ Email, Name string }{e, n})     // int64
if postgres.Violates(err, "users_email_key") { /* the declared failure mode */ }
```

`pool` is anything pgx gives you: a `*pgxpool.Pool`, `*pgx.Conn` or `pgx.Tx`. The checker does
not care how a statement is run: the declaration is what it reads, and a program may run its
statements through a runtime of its own.

You do not have to write the structs. Declare `type Row struct{}` and `type Params struct{}`
empty, write the SQL, and every column and parameter without a field is reported with a quick fix
that writes the struct from the SQL; `sqlshape -fix ./...` applies them all.

To check on every save, pass the binary to `go vet` as the `-vettool`. Any editor whose Go
integration runs `go vet` on save then shows the diagnostics:

```
$ go vet -vettool="$(which sqlshape)" ./...
```

Details in [docs/flags.md](docs/flags.md#in-the-editor).

## Examples

Four stages of the same order book, one per level of trust in the database:

- [`examples/1-tables`](examples/1-tables) — The basic form: `Query` / `One` against tables, structs matched to the SQL, the constraints a write can violate declared on its expect line
- [`examples/2-views`](examples/2-views) — Reading through views: joins and column names decided once in the schema, so the application's SQL gets thinner
- [`examples/3-database-api`](examples/3-database-api) — Writes as functions, meaning as domains and composite types: how the checks work once logic lives in the database
- [`examples/4-everything`](examples/4-everything) — Every feature in one place; the index to look up how a particular feature is used

## What it checks

The full list is in [docs/checks.md](docs/checks.md). Broadly:

- Shapes: the columns a statement returns and the Go struct that receives them, and the
  `{{.X}}` parameters and the struct that supplies them, agree in name, type and NULL handling.
  Nested rows and composite types included.
- Meaning: a Go type used for an enum or lookup-table value, a primary key or a domain is bound
  to that meaning from where it is used. Passing another table's ID, adding domains of different
  units, or constants that drift from the labels are reported even though the underlying types
  agree.
- Failure modes: a write must declare the constraints it can violate on its expect line. A missing
  declaration and an impossible one are both reported, so the code always states which violations
  it has to handle. No ORM offers this: an ORM's model is a copy of the constraints, so nothing can
  be derived from it about what will fail, and the exception arrives at run time with a name to
  match by hand. Here the list is computed from `schema.sql` (triggers and called functions
  included), the names are PostgreSQL's own, and the runtime error carries the same name, so the
  declaration, the check and the handler are one string.
- Cardinality: a statement declared with `One` is proved from the schema to return at most one
  row.
- Boundaries: team rules that show up in the shape of the SQL, such as always filtering soft-deleted
  rows, always pinning the tenant column, or reading tables only through views, are enforced by the
  checker.
- The schema itself: the functions (SQL and PL/pgSQL bodies), views and policies in `schema.sql` are type-checked too, and
  `-strict` adds advice such as a predicate no index serves or an enum a lookup table would serve
  better.

## SQL outside Go

The rules `schema.sql` declares are not limited to Go code. `sqlshape check` judges any SQL against
them -- an operator's UPDATE before it runs in production, a backfill mixed into a migration, a
query an LLM agent is about to execute -- and prints every judgment with the path that discharged
it, so the output is also the record of what a script was allowed to do:

```
$ sqlshape check ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

A context (`-context ops`) selects the rules that apply to that caller. Details in
[docs/checks.md](docs/checks.md#the-same-rules-for-sql-outside-go-sqlshape-check).

## Migrations

There are no migration files. Edit `schema.sql`, and `sqlshape` derives the DDL from the
difference between it and the database:

```
$ sqlshape diff -db "$DSN" > up.sql         # DDL from the database's state to schema.sql
$ $EDITOR up.sql                            # reorder, split, add USING, interleave a backfill
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # drift: where a database differs from schema.sql
```

The generated DDL may be edited by hand. `apply` checks, before running anything, that applying the
DDL really leads to `schema.sql`, and refuses otherwise. With `-packages` it also refuses while Go
code still uses a column the DDL drops or retypes. Changes whose intent a diff cannot infer, such as a
rename or the removal of an enum label, are declared in `schema.sql` with `-- @migrate` lines. See
[docs/migrations.md](docs/migrations.md).

## Documentation

- [docs/checks.md](docs/checks.md) — everything the checker verifies: shapes, meaning, failure modes (with PostgreSQL's constraint naming rules), cardinality, the rules a schema declares (`require`, aggregates, `sqlshape check`)
- [docs/templates.md](docs/templates.md) — the template subset, directives, shared fragments, hazards, sparse checking
- [docs/runtime.md](docs/runtime.md) — `postgres.Run` / `Collect` / `First` / `Exec`, `One`, `Batch`, `Copy`, `MatView`, the Go type table, type registration, errors, tests on a real PostgreSQL
- [docs/migrations.md](docs/migrations.md) — `diff` / `apply` / `verify-schema`, `-- @migrate` declarations, seeded tables, requirements
- [docs/flags.md](docs/flags.md) — every flag, the `-strict` advisories, editor setup
- [docs/design.md](docs/design.md) — design decisions: what was decided, why, and what was rejected (Japanese)

## Compatibility

PostgreSQL 17 and 18; `schema.sql` declares which. The syntax is PostgreSQL's own: the parser is
libpg_query of that version, so everything the server parses, the checker parses the same way —
SELECT and DML, MERGE, CTEs, window functions, GROUPING SETS, SQL/JSON, ranges, `RETURNING old` /
`new` and temporal keys on 18, extensions such as citext and hstore, and DDL including views,
functions (SQL and PL/pgSQL bodies), triggers and policies. The analyzer is a pure-Go implementation
built from that version's catalog; checking never connects to a PostgreSQL.

The judgment is backed by PostgreSQL's own regression suite: the statements of `src/test/regress`
are run through the analyzer and a real PostgreSQL of the same version side by side, and parameter
types, result columns and errors must agree. On 17 they disagree on 19 of 22,103 statements, on 18
on 31 of 23,384; every one is listed (`check/postgres/analyze/testdata/regress_baseline_<version>.txt`),
and each is either something static analysis cannot decide (row-level security recursion,
permissions, server internals) or a case where the checker is right and the server's Describe
cannot say (the NULLs of `RETURNING old` after an INSERT). The comparison is part of
`go test ./...`, so a new disagreement fails the build.

## License

Everything a checked program links is Apache License 2.0, see [LICENSE](LICENSE): the
declarations (the root module), the runtimes (`postgres`, `mysql`) and `pgtest`. The PostgreSQL
side of the checker (`check/postgres`) embeds `pg_catalog` data and validation rules ported from
PostgreSQL under the PostgreSQL License, see [check/postgres/NOTICE](check/postgres/NOTICE). The
`sqlshape` binary (`cmd/sqlshape`) and the MySQL side of the checker (`check/mysql`, which carries
MySQL's own parser) are modules under the GNU General Public License v2, see
[cmd/sqlshape/LICENSE](cmd/sqlshape/LICENSE); the binary is a development tool, and nothing under
it is linked into your program.
