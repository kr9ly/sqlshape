# sqlshape

[![Go Reference](https://pkg.go.dev/badge/github.com/kr9ly/sqlshape/v2.svg)](https://pkg.go.dev/github.com/kr9ly/sqlshape/v2)
[![release](https://img.shields.io/github/v/release/kr9ly/sqlshape)](https://github.com/kr9ly/sqlshape/releases)
[![test](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml/badge.svg)](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml)
![coverage](.github/badges/coverage.svg)

Keep your SQL as plain SQL in Go code, and have `go vet` prove it matches your schema: result
columns against your structs, parameters, NULL handling, single-row queries, and the constraint
violations each write has to handle, all checked against `schema.sql` before the code runs.

Supports PostgreSQL 17 / 18 and MySQL 8.4 on Go 1.26+. Checking needs no database and no C
compiler, and the sqlshape code your program links is Apache-2.0.

[日本語](README.ja.md)

```go
// Row type, then parameter type. The checker proves both fit the SQL, and that it returns at most one row.
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := postgres.Get(ctx, db, ByEmail, struct{ Email string }{Email: email})   // or mysql.Get
```

- Mismatches are vet errors, not runtime surprises. A column the struct cannot hold, a
  parameter of the wrong type, a nullable column read into a non-pointer, or a `One` query that
  could return two rows is a `go vet` diagnostic, with a quick fix where one exists.
- Failure modes are declared and proved. Every write lists the constraints it can violate
  (`-- sqlshape: expect users_email_key`). The checker computes the real list from `schema.sql`,
  including triggers and called functions, and reports both missing and impossible entries. At
  run time `postgres.Violates(err, "users_email_key")` matches the same name: declaration, check
  and handler are one string.
- The SQL stays plain, and nothing can be injected into it. Templates are Go `text/template`.
  `{{.X}}` always becomes a bound parameter, every `{{if}}` / `{{range}}` combination is checked, and the runtime refuses any
  rendering the checker never saw (within the limits in [Limitations](#limitations)).
- `schema.sql` is the only definition. Views, functions, domains, row-level security and
  seeded lookup tables are all checked. `sqlshape diff` derives the migration from the same file,
  so there are no migration files to keep in sync.
- The rules are the database's own. Statements are parsed by PostgreSQL's and MySQL's own parsers,
  and the verdicts are cross-checked against each database's regression and test suites
  ([Compatibility](#compatibility-and-verification)).

### How it differs

Unlike sqlc, nothing is generated: you write the structs (or let `sqlshape -fix` write them from
the SQL), and the checker keeps them honest as the schema changes. Unlike an ORM, there is no
model that copies the schema. Constraint names, nullability and cardinality come from
`schema.sql` itself, which is how the checker can tell you which errors a write can raise.

## Install

The checker and the migration commands are one binary:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest
$ sqlshape version
```

Or take a prebuilt binary from the [releases page](https://github.com/kr9ly/sqlshape/releases):
Linux, macOS and Windows, amd64 and arm64 each, as `sqlshape_<version>_<os>_<arch>.tar.gz` (`.zip`
on Windows) with `checksums.txt` beside them. Unpack and put `sqlshape` on your `PATH`.

Then add the declarations and the runtime for your database to your module. The declarations
(`sqlshape.Query`, `sqlshape.One`) have no dependencies, and each runtime is a module of its own,
so an application pulls in only its own driver:

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/postgres/v2   # running them on pgx v5
$ go get github.com/kr9ly/sqlshape/mysql/v2      # or on MySQL, through database/sql and go-sql-driver/mysql
```

Every module needs Go 1.26 or newer. Checking needs nothing else: no database, no C compiler. The
first run takes about a second to prepare the embedded parsers and caches the result
([how the checker runs](#how-the-checker-runs)).

## Quickstart

Pick your database: [PostgreSQL](#quickstart-postgresql) · [MySQL](#quickstart-mysql)

### Quickstart: PostgreSQL

Put `schema.sql` at the root of your module. The checker uses the nearest `schema.sql` (or
`schema/` directory) above each package, and `-schema PATH` overrides that.

```sql
-- sqlshape: postgres 17
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

The first line names the PostgreSQL version the schema is written for. Every statement is judged
with that version's grammar and catalog (`postgres 17` or `postgres 18`).

Then declare the statements, here in `users.go`:

```go
package app

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID        int64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.One[int64, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

`Query[R, P]` and `One[R, P]` take the row type and the parameter type. `WHERE true` lets the
optional `AND` clause follow; the checker checks the statement both with and without it
([docs/templates.md](docs/templates.md)).

```
$ sqlshape ./...
users.go:16:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:20:65: sqlshape: may violate users_email_key (UNIQUE (email) on users, SQLSTATE 23505); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

Paths are shortened here; the checker prints them in full. The first diagnostic says `deleted_at` can be NULL but the field cannot hold NULL. The second says
the INSERT can violate the unique constraint on `email` and does not declare it. Fix both, and the
package is clean:

```go
type User struct {
	// ...
	DeletedAt *time.Time
}

var Create = sqlshape.One[int64, struct{ Email, Name string }](`
-- sqlshape: expect users_email_key
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

At run time, with `github.com/kr9ly/sqlshape/postgres/v2`:

```go
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))

users, err := postgres.Collect(ctx, pool, Users, struct{ Name *string }{})          // []User
id, err := postgres.Get(ctx, pool, Create, struct{ Email, Name string }{e, n})       // int64
if postgres.Violates(err, "users_email_key") { /* the declared failure mode */ }
```

`Collect` gathers every row and `Get` runs a `One` statement and returns its row. `Run` streams
rows, `First` takes the first, `Exec` runs a statement and discards its rows, and `ExecOne` runs
a `One` statement that returns no rows
([docs/runtime.md](docs/runtime.md#statements)). `pool` is anything pgx gives you: a
`*pgxpool.Pool`, `*pgx.Conn` or `pgx.Tx` ([docs/postgres.md](docs/postgres.md)).

### Quickstart: MySQL

Put `schema.sql` at the root of your module. The checker uses the nearest `schema.sql` (or
`schema/` directory) above each package, and `-schema PATH` overrides that.

```sql
-- sqlshape: mysql 8.4
CREATE TABLE users (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    name       VARCHAR(100) NOT NULL,
    deleted_at DATETIME(6),
    UNIQUE KEY users_email_key (email)
);
```

The first line names the MySQL version. MySQL's own grammar parses the schema and every statement,
and MySQL's rules type them (`mysql 8.4`). A `-- sqlshape: server` line declares a `sql_mode` or
`lower_case_table_names` the server runs with, when it is not the default.

Then declare the statements, here in `users.go`:

```go
package app

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

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

`Query[R, P]` takes the row type and the parameter type; an INSERT returns no rows, so its row
type is `struct{}`. `WHERE true` lets the optional `AND` clause follow; the checker checks the
statement both with and without it ([docs/templates.md](docs/templates.md)).

```
$ sqlshape ./...
users.go:16:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:20:70: sqlshape: may violate users_email_key (UNIQUE users_email_key (email) on users, MySQL error 1062); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

Paths are shortened here; the checker prints them in full. The first diagnostic says `deleted_at` can be NULL but the field cannot hold NULL. The second says
the INSERT can violate `users_email_key` and does not declare it. Fix both, and the package is
clean:

```go
type User struct {
	// ...
	DeletedAt *time.Time
}

var Create = sqlshape.Query[struct{}, struct{ Email, Name string }](`
-- sqlshape: expect users_email_key
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}})`)
```

At run time, with `github.com/kr9ly/sqlshape/mysql/v2` and the driver imported
(`_ "github.com/go-sql-driver/mysql"`):

```go
db, err := sql.Open("mysql", "app:secret@tcp(localhost:3306)/app?parseTime=true")

users, err := mysql.Collect(ctx, db, Users, struct{ Name *string }{})          // []User
res, err := mysql.Exec(ctx, db, Create, struct{ Email, Name string }{e, n})     // sql.Result
if mysql.Violates(err, "users_email_key") { /* the declared failure mode */ }
```

`Collect` gathers every row and `Exec` runs a write and returns the driver's `sql.Result`. `Run`
streams rows, `First` takes the first, and `Get` / `ExecOne` run `One` statements
([docs/runtime.md](docs/runtime.md#statements)). `db` is a `*sql.DB`, `*sql.Tx` or `*sql.Conn`
opened with go-sql-driver/mysql; the DSN needs `parseTime=true` ([docs/mysql.md](docs/mysql.md)).

## Struct generation, editor and CI

You do not have to write the structs. Declare `type Row struct{}` and `type Params struct{}`
empty, write the SQL, and every column and parameter without a field is reported with a quick fix
that writes the struct from the SQL; `sqlshape -fix ./...` applies them all.

To check on every save, pass the binary to `go vet` as the `-vettool`. Any editor whose Go
integration runs `go vet` on save then shows the diagnostics:

```
$ go vet -vettool="$(which sqlshape)" ./...
```

In CI, run `sqlshape ./...`: it exits non-zero when it reports anything. Pin the version you
install, since a minor release may add diagnostics
([Versioning](#versioning)):

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@v2.0.0
$ sqlshape ./...
```

Details in [docs/flags.md](docs/flags.md#in-the-editor).

## What it checks

The full list is in [docs/checks.md](docs/checks.md). Broadly:

- Shapes: the columns a statement returns and the Go struct that receives them, and the
  `{{.X}}` parameters and the struct that supplies them, agree in name, type and NULL handling.
  Nested rows and composite types included.
- Meaning: if `OrderID` and `UserID` are both `int64`, passing a user ID where an order ID goes
  is still reported. A Go type used for an enum or lookup-table value, a primary key or a domain
  is bound to the column it stands for, so adding domains of different units, or constants that
  drift from the labels, are reported too.
- Failure modes: a write must declare the constraints it can violate on its expect line, and both
  a missing declaration and an impossible one are reported. The list is computed from
  `schema.sql` (triggers and called functions included), and the names are the database's own
  (PostgreSQL's constraint names and SQLSTATEs, MySQL's key names and error numbers).
- Cardinality: a statement declared with `One` is proved from the schema to return at most one
  row.
- Boundaries: team rules that show up in the shape of the SQL, such as always filtering soft-deleted
  rows, always pinning the tenant column, or reading tables only through views, are declared in
  `schema.sql` and enforced by the checker:

  ```sql
  -- sqlshape: require pinned(tenant_id)
  -- sqlshape: visible where deleted_at IS NULL
  CREATE TABLE orders (...);
  ```

- The schema itself: the functions (SQL and PL/pgSQL bodies), views and policies in `schema.sql`
  are type-checked too, and `-strict` adds advice such as a predicate no index serves or an enum a
  lookup table would serve better.

## Beyond Go code

### SQL outside Go

The rules `schema.sql` declares are not limited to Go code. `sqlshape check` judges any SQL against
them -- an operator's UPDATE before it runs in production, a backfill mixed into a migration, a
query an LLM agent is about to execute -- and prints every judgment and why each rule passed,
failed or was waived, so the output is also the record of what a script was allowed to do:

```
$ sqlshape check ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

A statement waives a rule with its own `-- sqlshape: waive` line. Rules can also be scoped to a
caller: `-context ops` applies the rules `schema.sql` declares for operators
(`-- sqlshape: context ops: ...`). Details in
[docs/checks.md](docs/checks.md#the-same-rules-for-sql-outside-go-sqlshape-check).

### Migrations

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
rename or the removal of an enum label, are declared in `schema.sql` with `-- @migrate` lines.

The migration commands are the only ones that run a real database. On PostgreSQL they download
the declared version's server binaries into the user cache on first use, so that first run needs
network access; on MySQL they use a scratch database on the server they migrate. See
[docs/migrations.md](docs/migrations.md).

## Examples

Four stages of the same order-management schema, from plain tables to putting more logic in the
database, plus the first stage on MySQL:

- [`examples/1-tables`](examples/1-tables) — The basic form: `Query` / `One` against tables, structs matched to the SQL, the constraints a write can violate declared on its expect line
- [`examples/2-views`](examples/2-views) — Reading through views: joins and column names decided once in the schema, so the application's SQL gets thinner
- [`examples/3-database-api`](examples/3-database-api) — Writes as functions, meaning as domains and composite types: how the checks work once logic lives in the database
- [`examples/4-everything`](examples/4-everything) — Every feature in one place; the index to look up how a particular feature is used

On MySQL:

- [`examples/5-mysql`](examples/5-mysql) — The same declarations against MySQL: `schema.sql` declares `mysql 8.4`, the statements run through `sqlshape/mysql` on database/sql

## Limitations

- The verdicts are measured against PostgreSQL 17 and 18 and MySQL 8.4, and `schema.sql` declares
  one of them (`-- sqlshape: postgres 17`, `postgres 18` or `mysql 8.4`); any other number, such
  as `postgres 16` or `mysql 8.0`, is refused.
- A server of another version still works with sqlshape: the checker runs, and your program runs
  on any server its driver connects to, including older PostgreSQL releases, other MySQL
  versions, MariaDB and other MySQL-compatible databases. Statements are then judged by the
  declared version's rules, so where that server behaves differently, the verdicts are not
  guaranteed to match what it does.
- One database per project. `schema.sql` declares which, and the grammar, the types, the
  constraint names in the diagnostics and the runtime module all follow from that declaration.
- A template must be a Go string constant. SQL assembled at run time cannot be declared; the
  parts that vary are `{{if}}` / `{{range}}` branches, and the runtime refuses any rendering the
  checker never saw. A `{{range}}` is checked for up to two elements, and a template with more
  than 256 branch combinations only for a representative set; for those, the runtime confirms
  only the shape of the branches before running
  ([docs/runtime.md](docs/runtime.md#only-checked-sql-runs)).
- Some PostgreSQL features have no MySQL counterpart and are not checked there (`Copy`,
  `MatView`, PL/pgSQL, domains, composite types and arrays, and more;
  [docs/mysql.md](docs/mysql.md#not-on-mysql)).

## Compatibility and verification

| Database | Versions | Grammar | Verified against |
|---|---|---|---|
| PostgreSQL | 17, 18 | libpg_query of the declared version | PostgreSQL's regression suite, on a real server |
| MySQL | 8.4 | MySQL's own parser and lexer, from the server source | a running `mysqld` and MySQL's test corpus |

PostgreSQL: the analyzer is built from the declared version's catalog. The verdicts are checked
against PostgreSQL's own regression suite, run through the analyzer and a real server side by
side: on 17 they disagree on 19 of 22,103 statements, on 18 on 31 of 23,384, every one listed and
explained ([docs/postgres.md](docs/postgres.md#what-the-checker-embeds)).

MySQL: the function catalog comes from the same server source as the parser. The verdicts are
checked against a running `mysqld`: the result types of every built-in function, the error
statements, the `ONLY_FULL_GROUP_BY` check and the `sql_mode` variants agree with 8.4, and MySQL's
own test corpus (`mysql-test/t`, some 137,000 statements) replays against the analyzer and the
server side by side -- the 2,525 statements they knowingly disagree on are pinned one by one as a
baseline, and a new disagreement fails the build
([docs/mysql.md](docs/mysql.md#what-the-checker-embeds)).

The migrations are judged the same way on both databases: generated schema pairs, every kind of
change the diff can report, run as DDL against a real server with rows in its tables
([docs/migrations.md](docs/migrations.md#how-the-plan-is-tested)).

### How the checker runs

The parsers (PostgreSQL's libpg_query per supported major version, MySQL 8.4's own grammar and
lexer) are embedded as WebAssembly and run on wazero, so there is no C compiler to install and no
library to link. The first run compiles the module (about a second) and caches the result under
the user cache directory (`~/.cache/sqlshape` on Linux).

### Versioning

Versions follow semantic versioning; every module of the repository is tagged together
(`vX.Y.Z`, `postgres/vX.Y.Z`, ...). Within a major version, the exported API of these modules, the
template syntax, the directives and the checker's flags stay compatible. A minor release may add
diagnostics, so code that passed before can be reported after an upgrade: pin the `sqlshape`
version in CI and upgrade deliberately.

## Documentation

[docs/postgres.md](docs/postgres.md) and [docs/mysql.md](docs/mysql.md) each gather what is
specific to one database; the rest of the documentation is written for both.

- [docs/postgres.md](docs/postgres.md) — everything PostgreSQL's: the version declaration, what the checker embeds and how it is verified, the runtime on pgx (`Batch`, `Copy`, `MatView`, type registration), migrations on PostgreSQL
- [docs/mysql.md](docs/mysql.md) — everything MySQL's: the version and `server` declarations, the Go type table, constraint names and error numbers, the `ONLY_FULL_GROUP_BY` check, the runtime on `database/sql`, migrations on MySQL
- [docs/checks.md](docs/checks.md) — everything the checker verifies, for both databases: shapes, meaning, failure modes, cardinality, the rules a schema declares (`require`, aggregates, `sqlshape check`)
- [docs/templates.md](docs/templates.md) — the template subset, directives, shared fragments, hazards, sparse checking
- [docs/runtime.md](docs/runtime.md) — what every runtime does: `Run` / `Collect` / `First` / `Exec`, `One`, row mapping, errors, the guarantee that only checked SQL runs, and what a runtime of your own does not get
- [docs/migrations.md](docs/migrations.md) — `diff` / `apply` / `verify-schema`, `-- @migrate` declarations, seeded tables, requirements, what differs on MySQL
- [docs/flags.md](docs/flags.md) — every flag, the `-strict` advisories, editor setup
- [docs/design.md](docs/design.md) — design decisions: what was decided, why, and what was rejected (Japanese)

## License

The only sqlshape code your program links is Apache License 2.0 ([LICENSE](LICENSE)): the
declarations (the root module) and the runtimes (`postgres`, `mysql`). The drivers the runtimes
pull in keep their own licenses (pgx: MIT, go-sql-driver/mysql: MPL-2.0). The PostgreSQL side of
the checker (`check/postgres`) embeds `pg_catalog` data and validation rules ported from
PostgreSQL under the PostgreSQL License, see [check/postgres/NOTICE](check/postgres/NOTICE). The
`sqlshape` binary (`cmd/sqlshape`) and the MySQL side of the checker (`check/mysql`, which carries
MySQL's own parser) are modules under the GNU General Public License v2, see
[cmd/sqlshape/LICENSE](cmd/sqlshape/LICENSE); the binary is a development tool, and nothing under
it is linked into your program.
