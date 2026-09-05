# sqlshape

Make SQL a first-class citizen of your codebase.

SQL that lives in your repo gets what your application code already has: version control,
type checking, refactoring, tests, dependency graphs, and editor diagnostics.
Not by hiding it behind a DSL or an ORM, but by checking it as SQL.

The core is language-agnostic: a pure-Go PostgreSQL analyzer built from PostgreSQL's
own system catalog (`pg_type`, `pg_proc`, `pg_operator`, `pg_cast`, dumped from PG 17
and embedded) plus your `schema.sql`. A real PostgreSQL is used only as the oracle in differential
tests. The first frontend is a `go/analysis` analyzer that finds
`sqlshape.Query[R, P](template)` call sites, expands every branch of the template
(`if` / `else` / `with` / `range`), analyzes each expansion, and checks that the declared
row type `R` and parameter type `P` match what the SQL actually produces.

No code generation. No DSL. Plain SQL, plain structs, and a linter that proves
they fit — hence *shape*. Views, functions, and composite types in `schema.sql`
are part of the same checked surface, so the database can expose a typed public
API instead of raw tables.

See [design.md](design.md) for the rationale and architecture.

## Try it

```
$ go run ./cmd/sqlshape ./examples/...
```

`cmd/sqlshape` is a `go vet -vettool`-compatible checker. It looks for `schema.sql` above the
package directory (or `-schema path`). See `examples/orders` for the shape of a query.

## Layout

| package | role |
|---|---|
| `sqlshape` | public API: `Query[R, P](template)` (runtime not implemented yet) |
| `internal/expand` | exhaustive template expansion; `{{.X}}` → `$n` with its path on `P` |
| `internal/schema` | `schema.sql` on top of the catalog via libpg_query: tables, views, enums, domains, composites, functions, constraints |
| `internal/catalog` | embedded `pg_catalog` of PG 17 (types, functions, operators, casts, aggregates) |
| `internal/analyze` | the analyzer: chapter-10 type conversion, scopes, DML, `$n` inference, PG-compatible errors |
| `internal/vet` | the `go/analysis` analyzer: result columns ↔ `R`, `$n` ↔ `P`, nullability |
| `internal/oracle` | a real PostgreSQL (embedded-postgres) as the differential-test oracle; never used at lint time |

## Shared interpretation

Meaning that lives in the catalog is checked against the Go side by use, without registration:

- a Go named string type that meets an **enum** column (or a column with `CHECK (col IN (...))`) is bound to it; its typed constants are diffed
  against the labels both ways (across packages via `go/analysis` facts), `T("typo")` conversions and
  non-exhaustive `switch`es are reported
- a Go named type that meets a **key column** (PK, or FK-derived) is bound to that identity;
  `UserID` passed where `orders.id` is expected is reported even though both are `bigint`
- a Go named type that meets a **domain** is bound to it; mixing domains is reported
- inside SQL a domain is an **opaque unit**: `price_yen + weight_g` or `balance > total` is reported even though PG accepts it; literals and parameters adopt the unit, `yen + yen`, `yen * n`, `abs(yen)`, `coalesce(yen, 0)` stay yen, and an explicit cast to the base type drops it
- nullability can be asserted on the SQL side too: `-- sqlshape: not null` before a `CREATE FUNCTION` in schema.sql marks its result, `-- sqlshape: not null col, col` in a template marks result columns (the twin of the `col:",notnull"` tag)
- `LANGUAGE sql` function bodies in schema.sql are analyzed like PG does at CREATE time (parameters in scope, RETURNS shape checked); `sqlshape.MatView("order_stats").Refresh(ctx, db)` is checked against the schema
- a table annotated `-- sqlshape: visible where deleted_at IS NULL` in schema.sql must be read through that predicate everywhere (views included); a template opts out explicitly with `-- sqlshape: unfiltered memos`
- `-require-columns=tenant_id` makes every statement pin that column by equality on each table that has it (row ownership); `pgtest.Start` gives tests a real PostgreSQL with schema.sql applied, and `db.Verify(ctx, stmts...)` prepares every expansion of the given statements on it and fails when PG's parameter / result types or its rejection differ from the checker's conclusion, so the application's tests carry the evidence that the static checks hold for the PostgreSQL it runs on
- **shared fragments** are Go constants concatenated into the template (`const tenantFilter = " AND tenant_id = {{.TenantID}}"`, then `sqlshape.Query[R, P](base + tenantFilter)`): the whole is still a compile-time constant, the checker requires `P` to have every field the fragment reads, and a diagnostic about the fragment lands on the fragment's own line. Values never become SQL text, so this is the one sharing mechanism that keeps the injection guarantee
- **hazards** the template syntax allows but SQL does not honour are reported: an action inside a string literal (`LIKE '%{{.Q}}%'`) or a comment is text, not a parameter (write `'%' || {{.Q}} || '%'`), and a bare parameter as an `ORDER BY` / `GROUP BY` item sorts by a constant (branch on the value instead: `{{if eq .Sort "total"}} total {{else}} id {{end}}`)
- **raw driver calls** are linted too, since a `Query` / `Exec` on pgx or `database/sql` with a string built at run time is the hole the template guarantee does not cover: `-raw-sql=constant` (the default) requires their SQL argument to be a constant, `-raw-sql=forbid` rejects every statement that does not go through sqlshape (`-raw-sql-allow=pkg/...` exempts packages), `-raw-sql=allow` turns it off
- `-no-tables` forbids direct table references (application code reads views and calls functions; tables are the database's private side) and `-schemas=a_api,b_private` enforces a service boundary
- a Go enum type may implement `Known() bool`; the row mapper then rejects labels this build does not know with `*UnknownLabelError`
- an unnamed result column (`SELECT 1 + 1`) or two columns with the same name (`o.id, u.id`) cannot bind to a field: the checker says which column to alias
- `-strict` also reports advisory findings: fields of P the template never reads, such columns carried by unnamed Go types (which cannot be checked), `timestamp` / `date` received as `time.Time`, non-pointer enum parameters (the zero value is no label), parameters that always override a column DEFAULT, `LIMIT` without `ORDER BY`, enum comparison / ORDER BY (declaration order)
- a write's **failure modes** come from the catalog: the checker lists the constraints each INSERT / UPDATE / DELETE expansion may violate (unique keys, foreign keys both ways, CHECKs, domain CHECKs, NOT NULL when the value can be NULL) and requires the template to declare them in a `-- sqlshape: expect users_email_key, orders.total` line; an undeclared possible violation and a declared impossible one are both reported. At runtime a class-23 error becomes a `ConstraintError` keyed by the same names (`sqlshape.Violates(err, "users_email_key")`). Unnamed constraints get PG's own names (`users_pkey`, `orders_total_check`, `yen_check`). Trigger functions annotated `-- sqlshape: error P0401 = OrderTooLarge` add their SQLSTATE to the failure modes of the table's INSERT / UPDATE / DELETE
- the **Go type table** is what pgx actually scans and encodes, verified against a running PG: `inet` → `netip.Addr` / `netip.Prefix`, `cidr` → `netip.Prefix`, `macaddr` → `net.HardwareAddr` / `string`, `interval` → `time.Duration`, `hstore` → `map[string]*string` (the runtime registers pgx's hstore codec), ranges → `pgtype.Range[T]` with `T` checked against the subtype (user-defined ranges too), multiranges → `pgtype.Multirange[pgtype.Range[T]]`, `bit` / `point` / `tsvector` → the `pgtype` value, `xml` / `money` / `tsquery` / `jsonpath` / `timetz` → `string`, `oid` → `uint32`; a `string` encodes as any parameter type. Tables carrying `money` and other types pgx has no codec for still load through `LoadUserTypes`
- **batches and bulk loads**: `sqlshape.NewBatch()` + `sqlshape.Queue(b, stmt, p)` / `QueueOne` send several statements in one round trip (`b.Send(ctx, db)`), each handle giving back its rows or command tag; `sqlshape.Copy[R]("order_items", cols...)` bulk-loads rows of R with COPY FROM, and the checker verifies the table, the columns, each column's type against the field feeding it, and that every column left out has a default. `One(...).Exec` returns `ErrNoRows` when the row did not exist and `ErrManyRows` when the proof failed
- **composite parameters**: `{{.Price}}` where SQL expects `money_amount` takes a struct, `{{.Items}}` where it expects `order_items[]` a slice of structs; the checker lines the struct's fields up with the type's columns (name and order, like nested rows), the runtime encodes them as composite values and loads the type on the connection the first time it meets it (pools: `LoadUserTypes` in AfterConnect)
- **embedded structs** flatten: `type Row struct { Base; Note *string }` receives Base's columns as its own (checker and mapper agree; two fields binding one column is an error), and `{{.ID}}` in a template reaches a promoted field of P. A named struct field, or an embedded one with a `col:"..."` tag, is a nested row instead
- **nested rows**: `array_agg(row(o.id, o.total))`, `array_agg(o)` and composite columns are received by a struct / slice of structs; the checker matches the struct's fields to the row type positionally (by name and order for a named type), the runtime scans them field by field and loads user types (enums, composites, extension types such as `citext` / `hstore` as text) into the connection lazily; `LoadUserTypes` does it wholesale for a pool's AfterConnect
- a column only some branches select may be received by a nullable field (pointer / slice), which those branches leave nil; a field no branch selects is still an error
- `-sync-comments` turns the schema's `COMMENT ON` into doc-comment quick fixes on the receiving Go fields and types; `-coverage` reports how many statements a package checks; `Stmt.Unprepared()` runs a statement without a prepared statement (custom plan every time)
- `-strict` also gives structural performance advice without running PostgreSQL: a table predicate no index leads with, and a view predicate the planner cannot push into the view
- templates with more than 256 branch combinations are checked **sparsely** (all off, all on, each branch alone), which still covers independent AND predicates in full
- the runtime **refuses SQL the checker never saw**: every rendering is compared byte for byte with the static expansion of the same branch signature
- `sqlshape.One[R, P]` asserts **at most one row**, and the checker proves it: every FROM item must have a unique key (PK / UNIQUE / unique index, partial ones when the predicate is repeated) fixed by equality to a literal, parameter, outer reference or uncorrelated scalar subquery, following equalities through joins (an outer join's ON only fixes its nullable side), views, subqueries and CTEs; aggregates without GROUP BY, constant `LIMIT 1` and single-row `INSERT ... RETURNING` are single too. Each expansion is proved separately, so `{{if .ID}} AND id = {{.ID}} {{end}}` fails on its else branch. `Get` returns `ErrNoRows`, `Find` reports presence, and both return `ErrManyRows` if the database ever contradicts the proof

## Status

First vertical slice works: the analyzer agrees with the PostgreSQL oracle on 152 golden
statements (contrib extensions, SQL/JSON, MERGE, GROUPING SETS and a schema full of DDL included) and the checker reports type / column / nullability
findings on real Go code. Not yet: the migration side.
