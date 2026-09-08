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

u, err := ByEmail.Get(ctx, db, struct{ Email string }{Email: email})
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

The checker and the migration commands are one binary:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape@latest
$ sqlshape version
```

It links PostgreSQL's parser (libpg_query) through cgo, so `go install` needs a C compiler (gcc or
clang) on the machine. Prebuilt binaries for Linux (amd64, arm64) and macOS (Apple Silicon) are on
the [releases page](https://github.com/kr9ly/sqlshape/releases); an Intel Mac builds it with
`go install`.

The runtime is an ordinary Go module:

```
$ go get github.com/kr9ly/sqlshape
```

Versions follow semantic versioning and are tagged `vX.Y.Z`. Within a major version, the exported
API of `sqlshape` and `pgtest`, the template syntax, the directives and the checker's flags stay
compatible; what the checker reports may grow with minor versions.

## Quickstart

```sql
-- schema.sql
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

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
users, err := Users.Collect(ctx, pool, struct{ Name *string }{})          // []User
id, err := Create.First(ctx, pool, struct{ Email, Name string }{e, n})     // int64
if sqlshape.Violates(err, "users_email_key") { /* the declared failure mode */ }
```

`db` is anything pgx gives you: a `*pgxpool.Pool`, `*pgx.Conn` or `pgx.Tx`.

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
  it has to handle.
- Cardinality: a statement declared with `One` is proved from the schema to return at most one
  row.
- Boundaries: team rules that show up in the shape of the SQL, such as always filtering soft-deleted
  rows, always pinning the tenant column, or reading tables only through views, are enforced by the
  checker.
- The schema itself: the functions (SQL and PL/pgSQL bodies), views and policies in `schema.sql` are type-checked too, and
  `-strict` adds advice such as a predicate no index serves or an enum a lookup table would serve
  better.

## Migrations

There are no migration files. Edit `schema.sql`, and `sqlshape` derives the DDL from the
difference between it and the database:

```
$ sqlshape diff -db "$DSN" > up.sql         # DDL from the database's state to schema.sql
$ $EDITOR up.sql                            # reorder, split, add USING, interleave a backfill
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # drift: where a database differs from schema.sql
$ sqlshape check ops.sql                    # SQL outside Go, judged against schema.sql's obligations
```

The generated DDL may be edited by hand. `apply` checks, before running anything, that applying the
DDL really leads to `schema.sql`, and refuses otherwise. With `-packages` it also refuses while Go
code still uses a column the DDL drops or retypes. Changes whose intent a diff cannot infer, such as a
rename or the removal of an enum label, are declared in `schema.sql` with `-- @migrate` lines. See
[docs/migrations.md](docs/migrations.md).

## Documentation

- [docs/checks.md](docs/checks.md) — everything the checker verifies: shapes, meaning, failure modes (with PostgreSQL's constraint naming rules), cardinality, the rules a schema declares (`require`, aggregates, `sqlshape check`)
- [docs/templates.md](docs/templates.md) — the template subset, directives, shared fragments, hazards, sparse checking
- [docs/runtime.md](docs/runtime.md) — `Run` / `Collect` / `First` / `Exec`, `One`, `Batch`, `Copy`, `MatView`, the Go type table, type registration, errors, tests on a real PostgreSQL
- [docs/migrations.md](docs/migrations.md) — `diff` / `apply` / `verify-schema`, `-- @migrate` declarations, seeded tables, requirements
- [docs/flags.md](docs/flags.md) — every flag, the `-strict` advisories, editor setup
- [docs/design.md](docs/design.md) — design decisions: what was decided, why, and what was rejected (Japanese)

## Compatibility

All of PostgreSQL 17's syntax is understood: SELECT and DML, MERGE, CTEs, window functions,
GROUPING SETS, SQL/JSON, ranges, extensions such as citext and hstore, and DDL including views,
functions (SQL and PL/pgSQL bodies), triggers and policies. The analyzer is a pure-Go implementation built from PostgreSQL's
own catalog; checking never connects to a PostgreSQL.

"All" is backed by PostgreSQL's own regression suite: the 22,000 statements of `src/test/regress`
are run through the analyzer and a real PostgreSQL 17 side by side, and parameter types, result
columns and errors must agree. They disagree on 19, all of them things static analysis cannot
decide (row-level security recursion, permissions, server internals). The comparison is part of
`go test ./...`, so a new disagreement fails the build.

## License

Apache License 2.0, see [LICENSE](LICENSE). The embedded `pg_catalog` data and the validation
rules ported from PostgreSQL are used under the PostgreSQL License, see [NOTICE](NOTICE).
