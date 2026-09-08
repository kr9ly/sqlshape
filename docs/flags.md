# Flags and editor setup

[日本語](flags.ja.md)

`cmd/sqlshape` is a `go vet -vettool`-compatible checker. Run it as `sqlshape ./...`,
`sqlshape vet ./...` or `go vet -vettool=$(which sqlshape) ./...`; the flags below are passed the
same way in each case.
`sqlshape check` takes `-schema`, `-quiet` and the three boundary flags below
([checks.md](checks.md#declaring-the-rules-in-schemasql-require)). The migration subcommands (`diff`, `apply`, `verify-schema`) have their own flags, listed in
[migrations.md](migrations.md).

## Checker flags

| flag | default | meaning |
|---|---|---|
| `-schema PATH` | nearest `schema.sql` or `schema/` above the package | the schema to check against; a directory's `*.sql` files apply in name order |
| `-strict` | off | also report advisory findings ([below](#-strict)) |
| `-no-table-reads` | off | SELECTs, and the reading parts of writes, must go through views; a table may still be the target of INSERT / UPDATE / DELETE / MERGE |
| `-no-tables` | off | no direct table reference at all: application code reads views and calls functions |
| `-schemas=a_api,b_private` | all | the PostgreSQL schemas this code may reference (a service boundary over one database) |
| `-require-columns=tenant_id` | none | every statement must pin these columns by equality on each table that has them; INSERTs must assign them (a row-level security policy fixing the column also satisfies it) |

`-no-table-reads`, `-no-tables` and `-require-columns` are shorthands for obligations declared per
table in `schema.sql` (`require via view`, `require via view on all`, `require pinned(col)`); the
declaration form also gives `on` kinds, `immutable(col)` and arbitrary predicates
([checks.md](checks.md#declaring-the-rules-in-schemasql-require)).
| `-raw-sql=constant` | `constant` | driver calls outside sqlshape (pgx / `database/sql` `Query`, `Exec`, ...): `constant` requires their SQL to be a constant string, `forbid` rejects them, `allow` ignores them |
| `-raw-sql-allow=pkg/...` | none | packages (or prefixes ending in `/...`) where `-raw-sql=forbid` does not apply |
| `-coverage` | off | report per package how many `Query` / `One` declarations were checked and how many could not be (non-constant templates) |
| `-sync-comments` | off | propose doc comments for result struct fields and types from the schema's `COMMENT ON` (applied with `-fix`) |
| `-fix` | off | apply the suggested fixes (struct rewrites, doc comments) to the source |

## `-strict`

`-strict` adds advisory findings: things that are legal and may be intended, but are worth a
look. In statements:

- a field of `P` no expansion reads
- an enum, domain or key column carried by an unnamed Go type (`string`, `int64`), which the
  binding checks cannot follow
- `timestamp` or `date` received as `time.Time` (the zone, or the time of day, is invented)
- a non-pointer enum parameter: its zero value `""` is not a label and fails at run time
- a non-nullable parameter that always writes a column with a `DEFAULT` or an identity, so the
  database's default never applies
- `LIMIT` without `ORDER BY`: which rows come back is unspecified
- a comparison or `ORDER BY` on an enum, which sorts in declaration order, not alphabetically
- a table predicate no index leads with (a whole-table scan), and a view predicate the planner
  cannot push into the view (the view has `LIMIT` / `OFFSET`, a set operation, a window function)
- a template checked sparsely because it has more than 256 branch combinations
- `MatView.RefreshConcurrently` on a view without a unique index
- `-require-columns` satisfied by a policy on a table without `FORCE ROW LEVEL SECURITY` (the
  pin does not hold for the table's owner)

In the schema:

- an enum column: a seeded lookup table is easier to change and checked the same way
- a materialized view without a unique index, so it cannot be refreshed concurrently
- a table with row-level security enabled and no policy: every role but the owner sees no rows
- a policy reading `current_setting(name, true)`: a session that never set the setting gets
  NULL and sees no rows, silently
- a `SECURITY DEFINER` function that reaches a table with policies the owner is not bound by
- a PL/pgSQL `EXECUTE` of a string built at run time, which cannot be checked

## In the editor

The checker is a `go vet` tool, so it runs wherever `go vet` runs. Build it once and pass it as
the `-vettool`:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape@latest
$ go vet -vettool="$(which sqlshape)" -strict ./...
```

Editors with a Go integration can run `go vet` on save and show its diagnostics inline; give that
integration the same `-vettool` and `-strict` flags. gopls itself does not load third-party
analyzers, which is why the checker goes through `go vet` rather than gopls. In CI, run the same
command. golangci-lint can load `sqlshape` as a module plugin.

### Writing a statement's types from the SQL

Declare `type OrderRow struct{}` and `type OrderParams struct{}` empty, write the query, save.
Every result column is reported as having no field and every `{{.X}}` as having no path, and
each of those diagnostics carries a quick fix that rewrites the struct from the query:

- the columns of every branch, a column only some branches select as a pointer
- nullable columns as pointers
- enums and lookup values as the Go type already bound to them elsewhere in the module
- records and composites as nested structs, arrays of them as slices
- doc comments from `COMMENT ON`
- for `P`: a field per path, typed by what the SQL expects (`.Filter.Name` gives a nested struct,
  a `range` a slice), and a `bool` per `{{if .Flag}}` that only tests the field

The same fix sits on every later mismatch (a column added to the SELECT, a type changed in the
schema); fields that still fit keep their names, doc comments, tags and types, so a type you
chose from the alternatives (`decimal.Decimal` for `numeric`) stays. `numeric` becomes
`shopspring/decimal.Decimal` when the module already imports it, `pgtype.Numeric` otherwise;
`uuid` likewise picks the uuid package in use.

From the command line the fixes are applied with `sqlshape -fix ./...`; in the editor, through the
quick fix attached to each diagnostic.
