# sqlshape

[![Go Reference](https://pkg.go.dev/badge/github.com/kr9ly/sqlshape/v2.svg)](https://pkg.go.dev/github.com/kr9ly/sqlshape/v2)
[![release](https://img.shields.io/github/v/release/kr9ly/sqlshape)](https://github.com/kr9ly/sqlshape/releases)
[![test](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml/badge.svg)](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml)
![coverage](.github/badges/coverage.svg)

Write SQL as SQL, and let a `go vet` checker prove the Go code around it fits. For PostgreSQL and
MySQL.

[日本語](README.ja.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := postgres.Get(ctx, db, ByEmail, struct{ Email string }{Email: email})   // or mysql.Get
```

- Checked, not generated. Every statement is analyzed against `schema.sql` by a pure-Go analyzer
  built from your database's own parser and rules: the result columns must fit the row type, the
  `{{.X}}` parameters must fit the parameter type, nullability is enforced, `One` has to be
  provably single-row, and a write must declare the constraints it can violate. Every finding is a
  `go vet` diagnostic, reported before the code ever runs.
- Plain SQL, no injection. Templates are Go `text/template`: `{{.X}}` always becomes a
  parameter, never text, and every `{{if}}` / `{{range}}` combination is expanded and checked. The
  runtime refuses any rendering the checker never saw.
- `schema.sql` is the only definition. The checker reads it, and `sqlshape diff` derives the
  migration from it. Views,
  functions, domains, composite types, row-level security and seeded lookup tables are all part
  of the checked surface, so the database can expose a typed API instead of raw tables.
- One database per project. `schema.sql` declares which (`-- sqlshape: postgres 17` or
  `-- sqlshape: mysql 8.4`), and everything follows from the declaration: the grammar, the types,
  the constraint names in the diagnostics and the runtime module you import. [docs/postgres.md](docs/postgres.md) and [docs/mysql.md](docs/mysql.md) each
  gather what is specific to one; the rest of the documentation is written for both.

## Install

The checker and the migration commands are one binary. With Go 1.26 or newer:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest
$ sqlshape version
```

Or take a prebuilt binary from the [releases page](https://github.com/kr9ly/sqlshape/releases):
Linux, macOS and Windows, amd64 and arm64 each, as `sqlshape_<version>_<os>_<arch>.tar.gz` (`.zip`
on Windows) with `checksums.txt` beside them. Unpack and put `sqlshape` on your `PATH`.

Nothing else is needed to check code. The parsers (PostgreSQL's libpg_query per supported major
version, MySQL 8.4's own grammar and lexer) are embedded as WebAssembly and run on wazero, so
there is no C compiler to install and no library to link; the first run compiles the module
(about a second) and caches the result under the user cache directory (`~/.cache/sqlshape` on
Linux). Only the migration commands run a real database: on PostgreSQL they download the declared
version's binaries into the same cache on first use, on MySQL they use a scratch database on the
server they migrate.

The declarations (`sqlshape.Query`, `sqlshape.One`) are a Go module with no dependencies, and
the runtime for each database is a module of its own, so an application pulls in only its own
driver:

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/postgres/v2   # running them on pgx
$ go get github.com/kr9ly/sqlshape/mysql/v2      # or on MySQL, through database/sql
```

Versions follow semantic versioning; every module of the repository is tagged together
(`vX.Y.Z`, `postgres/vX.Y.Z`, ...). Within a major version, the exported API of these modules, the
template syntax, the directives and the checker's flags stay compatible; what the checker reports may grow with minor versions.

## Quickstart: PostgreSQL

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

`pool` is anything pgx gives you: a `*pgxpool.Pool`, `*pgx.Conn` or `pgx.Tx`
([docs/postgres.md](docs/postgres.md)).

## Quickstart: MySQL

```sql
-- schema.sql
-- sqlshape: mysql 8.4
CREATE TABLE users (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    name       VARCHAR(100) NOT NULL,
    deleted_at DATETIME(6),
    UNIQUE KEY users_email_key (email)
);
```

The first line of `schema.sql` names the MySQL version; MySQL's own grammar parses the schema and
every statement, and MySQL's rules type them (8.4 is supported). A `-- sqlshape: server` line
declares a `sql_mode` or `lower_case_table_names` the server runs with, when it is not the
default.

```go
// users.go
type User struct {
	ID        uint64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.Query[struct{}, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}})`)
```

```
$ sqlshape ./...
users.go:16:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:20:70: sqlshape: may violate users_email_key (UNIQUE users_email_key (email) on users, MySQL error 1062); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

Make `DeletedAt` a `*time.Time`, put `-- sqlshape: expect users_email_key` on the INSERT's first
line, and the package is clean. At run time:

```go
users, err := mysql.Collect(ctx, db, Users, struct{ Name *string }{})          // []User
res, err := mysql.Exec(ctx, db, Create, struct{ Email, Name string }{e, n})     // sql.Result
if mysql.Violates(err, "users_email_key") { /* the declared failure mode */ }
```

`db` is a `*sql.DB`, `*sql.Tx` or `*sql.Conn` opened with go-sql-driver/mysql (`parseTime=true`)
([docs/mysql.md](docs/mysql.md)).

## Struct generation and editor setup

The same on either database. The checker does not care how a statement is run: the declaration is what it reads, and a program
may run its statements through a runtime of its own.

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
- [`examples/5-mysql`](examples/5-mysql) — The same declarations against MySQL: `schema.sql` declares `mysql 8.4`, the statements run through `sqlshape/mysql` on database/sql

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
  included), the names are the database's own (PostgreSQL's constraint names and SQLSTATEs, MySQL's
  key names and error numbers), and the runtime error carries the same name, so the declaration,
  the check and the handler are one string.
- Cardinality: a statement declared with `One` is proved from the schema to return at most one
  row.
- Boundaries: team rules that show up in the shape of the SQL, such as always filtering soft-deleted
  rows, always pinning the tenant column, or reading tables only through views, are enforced by the
  checker.
- The schema itself: the functions (SQL and PL/pgSQL bodies), views and policies in `schema.sql`
  are type-checked too, and `-strict` adds advice such as a predicate no index serves or an enum a
  lookup table would serve better.

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

- [docs/postgres.md](docs/postgres.md) — everything PostgreSQL's: the version declaration, what the checker embeds and how it is verified, the runtime on pgx (`Batch`, `Copy`, `MatView`, type registration), migrations on PostgreSQL
- [docs/mysql.md](docs/mysql.md) — everything MySQL's: the version and `server` declarations, the Go type table, constraint names and error numbers, the `ONLY_FULL_GROUP_BY` check, the runtime on `database/sql`, migrations on MySQL
- [docs/checks.md](docs/checks.md) — everything the checker verifies, for both databases: shapes, meaning, failure modes, cardinality, the rules a schema declares (`require`, aggregates, `sqlshape check`)
- [docs/templates.md](docs/templates.md) — the template subset, directives, shared fragments, hazards, sparse checking
- [docs/runtime.md](docs/runtime.md) — what every runtime does: `Run` / `Collect` / `First` / `Exec`, `One`, row mapping, errors, the guarantee that only checked SQL runs
- [docs/migrations.md](docs/migrations.md) — `diff` / `apply` / `verify-schema`, `-- @migrate` declarations, seeded tables, requirements, what differs on MySQL
- [docs/flags.md](docs/flags.md) — every flag, the `-strict` advisories, editor setup
- [docs/design.md](docs/design.md) — design decisions: what was decided, why, and what was rejected (Japanese)

## Compatibility

PostgreSQL 17 and 18. The syntax is PostgreSQL's own (libpg_query of the declared version) and
the analyzer is built from that version's catalog. The verdicts are checked against PostgreSQL's
own regression suite, run through the analyzer and a real server side by side: on 17 they
disagree on 19 of 22,103 statements, on 18 on 31 of 23,384, every one listed and explained
([docs/postgres.md](docs/postgres.md#what-the-checker-embeds)). The migrations are judged the same
way: generated schema pairs, every kind of change the diff can report, run as DDL against a real
server with rows in its tables
([docs/migrations.md](docs/migrations.md#how-the-plan-is-tested)).

MySQL 8.4. The parser and lexer are MySQL's own, extracted from the server source, and the
function catalog comes from the same source. The verdicts are checked against a running `mysqld`:
the result types of every built-in function, the error statements, the `ONLY_FULL_GROUP_BY` check
and the `sql_mode` variants agree with 8.4, and MySQL's own test corpus (`mysql-test/t`, some
137,000 statements) replays against the analyzer and the server side by side -- the 2,534
statements they knowingly disagree on are pinned one by one as a baseline, and a new
disagreement fails the build ([docs/mysql.md](docs/mysql.md#what-the-checker-embeds)).
What has no MySQL counterpart (`Copy`, `MatView`, PL/pgSQL, domains, composite types and arrays,
`-schemas`, `// sqlshape: type`, seeded tables in migrations) is listed there. Its migrations are
judged against a running `mysqld` the same way as PostgreSQL's.

## License

Everything a checked program links is Apache License 2.0, see [LICENSE](LICENSE): the
declarations (the root module) and the runtimes (`postgres`, `mysql`). The PostgreSQL
side of the checker (`check/postgres`) embeds `pg_catalog` data and validation rules ported from
PostgreSQL under the PostgreSQL License, see [check/postgres/NOTICE](check/postgres/NOTICE). The
`sqlshape` binary (`cmd/sqlshape`) and the MySQL side of the checker (`check/mysql`, which carries
MySQL's own parser) are modules under the GNU General Public License v2, see
[cmd/sqlshape/LICENSE](cmd/sqlshape/LICENSE); the binary is a development tool, and nothing under
it is linked into your program.
