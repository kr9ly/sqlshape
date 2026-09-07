# sqlshape

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
$ go run github.com/kr9ly/sqlshape/cmd/sqlshape ./...
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

To check on every save, build the binary once and pass it to `go vet` as the `-vettool`. Any
editor whose Go integration runs `go vet` on save then shows the diagnostics:

```
$ go build -o "$(go env GOPATH)/bin/sqlshape" github.com/kr9ly/sqlshape/cmd/sqlshape
$ go vet -vettool="$(go env GOPATH)/bin/sqlshape" ./...
```

Details in [docs/flags.md](docs/flags.md#in-the-editor).

## Examples

Four stages of the same order book, one per level of trust in the database:

| | what it uses | read it when |
|---|---|---|
| [`examples/1-tables`](examples/1-tables) | plain tables, `Query` / `One`, templates, a seeded lookup table as the value set, `expect` lines, `pgtest.Start` + `Verify` | you come from an ORM and want checked SQL on the tables you have |
| [`examples/2-views`](examples/2-views) | views as the read model (joins, names, aggregates and the soft-delete predicate decided once), writes still plain INSERT / UPDATE on tables, `-no-table-reads` | you want the database to own what things are called without moving logic into it yet |
| [`examples/3-database-api`](examples/3-database-api) | functions for writes, domains, an enum and a CHECK value set, a composite, a trigger SQLSTATE, `-no-tables` | you want the schema to carry the meaning and the application to see an API |
| [`examples/4-everything`](examples/4-everything) | schemas as a boundary, extensions, ranges, nested rows, declared type bindings, composite array parameters, Batch, Copy, soft-delete policy, row-level security, tenant pinning, every flag | you want to see the whole surface at once |

## What it checks

The full list is in [docs/checks.md](docs/checks.md). In one line each:

- Shapes — result columns ↔ `R` fields, `{{.X}}` paths ↔ `P` fields, nullability, nested
  rows (`array_agg(row(...))`, composites) into structs, a verified table of pgx Go types, and
  `// sqlshape: type money_amount` to bind your own type to a PostgreSQL type
- Meaning — a Go named type that meets an enum, a seeded lookup table's key, a CHECK value
  set, a key column or a domain is bound to it by use and its constants are diffed with the
  labels; mixing identities or domains is reported, and domains are opaque units inside SQL
- Failure modes — a write must declare the constraints it can violate in
  `-- sqlshape: expect users_email_key, orders.total`; both a missing and an impossible one are
  reported, and the runtime hands them back as `ConstraintError` under the same names
- Cardinality — `One[R, P]` is proved to return at most one row in every expansion, through
  joins, views, subqueries and CTEs
- Boundaries — `-- sqlshape: visible where deleted_at IS NULL`, row-level security policies,
  `-require-columns=tenant_id`, `-no-table-reads` / `-no-tables` / `-schemas`, and raw driver
  calls that bypass the checker
- Hazards — `{{.X}}` inside a string literal or comment, a parameter as an `ORDER BY` item,
  shared fragments that are not constants
- Schema problems — the bodies of SQL functions, views and policies in `schema.sql` are
  type-checked once at load, and with `-strict` the checker gives advisory findings
  (`LIMIT` without `ORDER BY`, a predicate no index leads with, an enum where a lookup table
  would do, ...)

## Migrations

There are no migration files. `sqlshape` compares the database with `schema.sql` and works from
the difference:

```
$ sqlshape diff -db "$DSN" > up.sql         # DDL from the database's state to schema.sql
$ $EDITOR up.sql                            # reorder, split, add USING, interleave a backfill
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # drift: where a database differs from schema.sql
```

`apply` refuses unless the database plus the DDL reads back as `schema.sql`, and with `-packages`
unless no Go statement still depends on a column the DDL drops. Renames, enum label removals and
backfills are declared in `schema.sql` with `-- @migrate` lines; seeded lookup tables are diffed
row by row and kept in step with one `MERGE`. See [docs/migrations.md](docs/migrations.md).

## Documentation

| | |
|---|---|
| [docs/checks.md](docs/checks.md) | everything the checker verifies: shapes, meaning, failure modes (with PostgreSQL's constraint naming rules), cardinality, boundaries |
| [docs/templates.md](docs/templates.md) | the template subset, directives, shared fragments, hazards, sparse checking |
| [docs/runtime.md](docs/runtime.md) | `Run` / `Collect` / `First` / `Exec`, `One`, `Batch`, `Copy`, `MatView`, the Go type table, type registration, errors, tests on a real PostgreSQL |
| [docs/migrations.md](docs/migrations.md) | `diff` / `apply` / `verify-schema`, `-- @migrate` declarations, seeded tables, requirements |
| [docs/flags.md](docs/flags.md) | every flag, the `-strict` advisories, editor setup |
| [design.md](design.md) | rationale and architecture (Japanese) |

## Status

The analyzer is a pure-Go PostgreSQL 17 analyzer built from PostgreSQL's own catalog; a real
PostgreSQL is used only as the oracle in tests. It agrees with that oracle on 152 golden
statements and on PostgreSQL's own regression corpus (22,000 statements of `src/test/regress`,
release 17.5), which `go test ./...` replays side by side and gates: the 19 known disagreements
(row-level security recursion, permissions, server internals, three deliberate differences) are
listed in `internal/analyze/testdata/regress_baseline.txt`. The checker and the runtime cover the
surface the four examples exercise; the migration side round-trips its scenarios through an
embedded PostgreSQL.

The suite takes about half a minute. The corpus is fetched once by
`internal/analyze/testdata/tools/fetch-regress.sh` (the test skips without it), the embedded
PostgreSQL is cached under `~/.cache/sqlshape`, and `pg_dump` 17 or later must be on `PATH` for
the migration tests (they skip without it). `.github/workflows/test.yml` runs it with those caches.

## License

Apache License 2.0, see [LICENSE](LICENSE). The embedded `pg_catalog` data and the validation
rules ported from PostgreSQL are used under the PostgreSQL License, see [NOTICE](NOTICE).
