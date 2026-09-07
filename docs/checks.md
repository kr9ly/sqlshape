# What the checker verifies

Every `sqlshape.Query[R, P](template)`, `sqlshape.One[R, P](template)`, `sqlshape.Copy[R](...)`
and `sqlshape.MatView(...)` in a package is found by the `go/analysis` analyzer, its template is
expanded into every branch combination ([templates.md](templates.md)), each expansion is analyzed
against `schema.sql`, and the conclusions are compared with the Go types. This page lists what is
compared, grouped by the question it answers.

## Shapes: does the Go code fit the statement?

Result columns ↔ `R`. Every result column of every expansion must bind to exactly one field
of `R`, by name: the `col:"..."` tag, then the `db:"..."` tag, then the field name in snake_case.
A column no field receives and a field no column feeds are both reported. An unnamed column
(`SELECT 1 + 1`) or two columns with the same name (`o.id, u.id`) cannot bind: the checker says
which to alias. When `R` is a scalar (`int64`, `string`, ...) the statement must produce one column.

Nullability. A column that may be NULL needs a nullable field: a pointer, a slice, a map,
`sql.Null*`, `pgtype.*` or any type implementing `sql.Scanner`. The analyzer derives nullability
from the catalog (`NOT NULL`, primary keys, `GENERATED`), from the predicate (`WHERE col IS NOT
NULL`, `col = ...`), from functions (strict functions with non-null input, `coalesce` with a
non-null default) and through joins (an outer join's inner side becomes nullable) and views (a
view's own WHERE refines its columns). The assertion can be overridden from either side: the
field tag `col:",notnull"`, or the template line `-- sqlshape: not null col, col` (the SQL-side
twin), or `-- sqlshape: not null` above a `CREATE FUNCTION` in schema.sql for its result.

Optional projections. A column only some branches select may be received by a nullable
field, which those branches leave nil; a field no branch selects is still an error.

Parameters ↔ `P`. Every `{{.X}}` becomes a `$n` parameter, and the analyzer infers the
PostgreSQL type each `$n` must have from where it is used (`WHERE id = $1` → `bigint`,
`= ANY($1)` → an array). The path is resolved on `P` (`.Filter.Name` walks into a nested struct,
a `range` variable into a slice's element) and the Go type is checked against the type table below:
a mismatch, a lossy conversion (`int64` into `integer`) and a non-nullable Go type where the SQL
side may need NULL are reported. Fields of `P` no expansion reads are reported with `-strict`.

Embedded structs flatten: `type Row struct { Base; Note *string }` receives Base's columns as
its own (two fields binding one column is an error), and `{{.ID}}` reaches a promoted field of
`P`. A named struct field, or an embedded one carrying a `col:"..."` tag, is a nested row instead.

Nested rows. `array_agg(row(o.id, o.total))`, `array_agg(o)`, `row(...)` and composite-typed
columns are received by a struct or a slice of structs. For an anonymous record the struct's
fields are matched positionally; for a named composite by name and order. The runtime scans them
field by field ([runtime.md](runtime.md#nested-rows-and-user-types)).

Composite parameters. `{{.Price}}` where SQL expects `money_amount` takes a struct,
`{{.Items}}` where it expects `order_items[]` a slice of structs; the fields are lined up with the
type's columns exactly like nested rows.

The Go type table is what pgx actually scans and encodes, verified against a running
PostgreSQL:

| PostgreSQL | Go |
|---|---|
| `bool` | `bool` |
| `smallint` / `integer` / `bigint` | `int16` / `int32` / `int64` / `int` (a narrower Go type is accepted with a note: `bigint into int32`) |
| `real` / `double precision` | `float32` / `float64` (`double precision into float32` is noted) |
| `numeric` | `string` (keeps every digit), `pgtype.Numeric`, `big.Rat`, `shopspring/decimal.Decimal`, `apd.Decimal`; a float or integer is accepted with a precision note |
| `text` / `varchar` / `char` / `name` / `citext` and other text-like extension types | `string` (a `string` also encodes as any parameter type) |
| `bytea` | `[]byte` |
| `uuid` | `uuid.UUID` (any package), `[16]byte`, `string` |
| `timestamptz` / `timestamp` / `date` | `time.Time` (`-strict` notes that `timestamp` and `date` lose the zone / the time) |
| `time` | `time.Time`, `string` |
| `interval` | `time.Duration`, `pgtype.Interval` |
| `json` / `jsonb` | `[]byte`, `json.RawMessage`, `string`, or any struct / slice / map pgx unmarshals into |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)` |
| ranges | `pgtype.Range[T]` with `T` checked against the subtype (user-defined ranges too) |
| multiranges | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | the `pgtype` value |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enums, seeded lookup keys, CHECK value sets | a Go named string type ([below](#meaning-what-does-the-value-stand-for)) |
| domains | the base type's Go type, or a named type bound to the domain |
| composites, records | a struct |

Declared bindings. A Go type names the PostgreSQL type it carries in its doc comment:

```go
// sqlshape: type money_amount
type Money struct{ ... }
```

The checker accepts `Money` exactly where SQL has `money_amount` (its arrays and domains over it
included) and reports it anywhere else, and leaves the wire format to the type's own
`sql.Scanner` / `driver.Valuer` (a Scanner receives the text form). This is how a composite
becomes a decimal wrapper, or an extension type a named type. The binding travels with the type
across packages.

COPY. `sqlshape.Copy[R]("order_items", "order_id", "line_no", ...)` is checked like an
INSERT: the table and columns must exist, each column's type must fit the field feeding it, and
every column left out must have a default or be generated.

## Meaning: what does the value stand for?

A Go named type that meets a column with meaning is bound to it by use, without
registration, and the binding is checked everywhere the type appears afterwards (across packages
through `go/analysis` facts).

Enums, lookup tables, CHECK value sets. A Go named string type that meets an enum column, the
key of a seeded lookup table, or a column with `CHECK (col IN (...))` is bound to that value set.
Its typed constants are diffed against the labels both ways: a label with no constant and a
constant with no label are reported, so are `T("typo")` conversions and a `switch` on the type
that does not cover every label. The type may implement `Known() bool`; the row mapper then
rejects labels this build does not know with `*UnknownLabelError`.

A seeded lookup table is a table whose rows are written in schema.sql with an ordinary
`INSERT ... VALUES`. The rows are part of the schema: the checker uses the key column's values as
the value set wherever the key or a column referencing it is used, and the migration keeps the
table's content in step ([migrations.md](migrations.md#seeded-tables)). It is the recommended
home for a value set — rows can carry a label and a sort order, a row in use is protected by the
foreign key, a value can be retired by deleting its row, and a JOIN gives analysts the names.
`-strict` says so on every enum column.

Key identity. A Go named type that meets a key column (a primary key, or a column derived
from one through a foreign key) is bound to that identity: `UserID` passed where `orders.id` is
expected is reported even though both are `bigint`. Composite keys bind positionally.

Domains. A Go named type that meets a domain is bound to it; mixing domains, or passing the
base type where the domain is expected, is reported. Inside SQL a domain is an opaque unit:
`price_yen + weight_g` or `balance > total` is reported even though PostgreSQL accepts it.
Literals and parameters adopt the unit; `yen + yen`, `yen * n`, `abs(yen)`, `coalesce(yen, 0)`
stay yen; an explicit cast to the base type drops it.

Fidelity (`-strict`). An enum, domain or key column carried by an unnamed Go type cannot be
checked and is reported; `timestamp` / `date` received as `time.Time`, and a non-pointer enum
parameter (its zero value `""` is no label and fails at run time) are reported.

Default ownership (`-strict`). A non-nullable parameter that always writes a column with a
`DEFAULT` means the database's default never applies: decide which side owns it (an `{{if}}`
around the column uses the database's).

## Failure modes: what can this write fail on?

For each INSERT / UPDATE / DELETE / MERGE expansion the analyzer lists the constraints it may
violate — unique keys and primary keys, foreign keys in both directions (the row inserted
references a missing parent; the row deleted is still referenced), CHECKs, domain CHECKs, and
NOT NULL when the value written can be NULL — and the template must declare them:

```sql
-- sqlshape: expect order_items_pkey, order_items_order_id_fkey, order_items_qty_check
INSERT INTO order_items (order_id, line_no, sku, qty, price) VALUES (...)
```

A possible violation the line does not name and a named one no expansion can cause are both
reported, so the line stays an exact contract. At run time a class-23 error is wrapped as a
`ConstraintError` under the same key, and `sqlshape.Violates(err, "order_items_qty_check")` reads
it back ([runtime.md](runtime.md#errors)).

Keys. A named constraint is its name. An unnamed one gets the name PostgreSQL gives it, so the
key in the diagnostic, in the expect line and in the error at run time are the same string:

| constraint | key | example |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| `REFERENCES` on `(a)` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| table `CHECK` on `(a)` | `<table>_<a>_check` (a CHECK on several columns, or on none, is `<table>_check`) | `orders_total_check` |
| domain `CHECK` | `<domain>_check` | `yen_check` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| a trigger's error | the SQLSTATE, or the name given with `-- sqlshape: error` | `P0401`, `OrderTooLarge` |

A second constraint that would get the same name is numbered, as PostgreSQL does
(`orders_total_check1`). The diagnostic spells the origin out:
`may violate customers_email_key (UNIQUE (email) on customers, SQLSTATE 23505)`.

NOT NULL from a parameter. When the value that may be NULL is a `{{.X}}`, the violation is
dropped if the Go type of `X` cannot be nil (a `string` never sends NULL); a pointer, slice or map
keeps it.

Triggers. A trigger function that raises is annotated in schema.sql:

```sql
-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger ...
```

The SQLSTATE (under the given name) joins the failure modes of every INSERT / UPDATE / DELETE on
the tables the trigger is attached to, for the events it fires on.

Through functions. A call to a user function carries the failure modes of its body (and of the
functions it calls, and the errors it declares), so `SELECT place_order({{.CustomerID}}, {{.Note}})`
declares the foreign key, the domain CHECK and the trigger's code the way the INSERT inside would;
the diagnostic says `through place_order()`. A NOT NULL the body blames on a parameter is traced to
the call's argument; a `STRICT` function is not even called with a NULL, so that argument drops
the violation.

## Cardinality: can `One` return two rows?

`sqlshape.One[R, P]` asserts at most one row, and the checker proves it for every expansion
separately. A SELECT is single when every FROM item has a unique key — primary key, `UNIQUE`,
unique index, or a partial unique index whose predicate the statement repeats — fixed by equality
to a literal, a parameter, an outer reference or an uncorrelated scalar subquery, following
equalities through joins (an outer join's ON only fixes its nullable side), views, subqueries and
CTEs. An aggregate without `GROUP BY`, a constant `LIMIT 0` / `LIMIT 1`, a SELECT with no FROM, a
one-row `VALUES` and a single-row `INSERT ... RETURNING` are single too; a `FULL JOIN` never is. `{{if .ID}} AND id = {{.ID}} {{end}}` fails on
its else branch, which is the point. At run time `Get` returns `ErrNoRows` and `Find` reports
presence; both return `ErrManyRows` if the database ever contradicts the proof.

## Boundaries: what may this code see?

Visibility policies. A table annotated in schema.sql

```sql
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (...)
```

must be read through that predicate everywhere, views included; a view that repeats the
predicate satisfies it for its readers. A template opts out explicitly with
`-- sqlshape: unfiltered memos`. A `RETURNING` list is not checked (its rows were just written).

Row-level security is part of the schema. `CREATE POLICY` predicates are type-checked against
their table like PostgreSQL does (boolean, no aggregates or window functions, domains respected);
policies and `ENABLE / FORCE ROW LEVEL SECURITY` travel with the table through diff and apply.
A policy on a table whose row security is off is a schema problem. With `-strict`: row security
on with no policy (nobody but the owner sees rows), a `SECURITY DEFINER` function that reaches
the table (the owner's privileges skip the policies unless the table forces them), and a policy
reading `current_setting(name, true)` (a session that never set it sees no rows, silently).
The checker does not ask statements to repeat a policy's predicate: the database filters.

Row ownership. `-require-columns=tenant_id` makes every statement pin that column by
equality on each table that has it; INSERTs must assign it. A row-level security policy that
fixes the column satisfies the requirement (with `-strict`, a note when the table does not
`FORCE` row security, since the pin does not hold for the owner).

Table access. `-no-table-reads` forbids reading tables: SELECTs, and the reading parts of
writes, go through views; a table may still be the target of INSERT / UPDATE / DELETE / MERGE.
`-no-tables` forbids direct table references altogether: application code reads views and calls
functions, tables are the database's private side. `-schemas=a_api,b_private` restricts the
schemas a package may reference (a service boundary over one database).

Raw driver calls. A `Query` / `Exec` on pgx or `database/sql` with a string built at run
time is the hole the template guarantee does not cover. `-raw-sql=constant` (the default)
requires their SQL argument to be a constant; `-raw-sql=forbid` rejects every statement that does
not go through sqlshape (`-raw-sql-allow=pkg/...` exempts packages); `-raw-sql=allow` turns it
off.

## Schema problems

`schema.sql` is itself analyzed when loaded, once per run: the body of every `LANGUAGE sql`
function (parameters in scope, `RETURNS` shape checked), every view and every policy is
type-checked like PostgreSQL does at CREATE time, and a seeded INSERT is checked for
idempotency and types. Findings are reported at the first `Query` of the package as
`sqlshape: schema ...`. The analyzer's non-advisory notes (a domain mismatch, a predicate that
always fails, a collation conflict) are diagnostics too.

With `-strict` the checker adds advisory findings about the schema and the statements, listed in
[flags.md](flags.md#-strict).
