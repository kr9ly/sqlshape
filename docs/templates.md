# Templates

A statement's SQL is a Go `text/template` over the parameter type `P`. The checker expands it
into every SQL text it can produce and checks each one; the runtime renders it and refuses any
text the checker did not see. The template must be a string constant (a literal, or constants
concatenated), because the expansion happens at lint time.

## The subset

Value actions. `{{.Field}}`, `{{.Outer.Inner}}`, `{{.}}` and `{{$x}}` inside a `range` are plain
field references. Each becomes a `$n` parameter in the SQL, never text: the value cannot change the
statement, which is the whole injection guarantee. Function calls, method calls and pipelines are
rejected in value position.

Branches. `{{if}}` / `{{else if}}` / `{{else}}` / `{{end}}` and `{{with}}` are expanded both
ways; `{{range}}` is expanded for 0, 1 and 2 iterations, which covers the empty case, the
single-element case and the separator between elements. A range over a slice of structs gives
each iteration its own `$n` parameters. There is no `{{switch}}`: write
`{{if eq .Sort "a"}} … {{else if eq .Sort "b"}} … {{end}}`.

Conditions may use the builtins `not`, `and`, `or`, `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `len`,
`index`, string and number constants, and field references. A nil pointer, an empty slice or
map, a zero number and an empty string are false, like `text/template`.

Not supported. `{{define}}` / `{{template}}` (share SQL through Go constants instead, see below),
custom functions, variables other than the range element.

## Parameters and `P`

`{{.Filter.Name}}` resolves to the field `Name` of the field `Filter` of `P`; `{{range .Items}}
… {{.Sku}} … {{end}}` resolves to the element type of the slice `Items`; `{{.}}` inside a range of
scalars is the element itself. Embedded structs are promoted. The analyzer infers what
PostgreSQL type each `$n` needs from its position in the SQL, and the Go type at that path must
fit ([checks.md](checks.md#shapes-does-the-go-code-fit-the-statement)). The same field used in two
places must fit both. A field only an `{{if}}` tests must exist on `P` but may be any type.

## Directives

Directives are SQL comments the checker reads. In a template:

| directive | meaning |
|---|---|
| `-- sqlshape: expect users_email_key, orders.total, P0401` | the constraints, NOT NULL columns and SQLSTATEs this write may violate; the checker keeps the list exact ([checks.md](checks.md#failure-modes-what-can-this-write-fail-on)) |
| `-- sqlshape: not null total, note` | these result columns are never NULL, whatever the analyzer derives (the twin of the `col:",notnull"` tag) |
| `-- sqlshape: unfiltered memos` | this statement reads `memos` without its `visible where` predicate on purpose |

In `schema.sql`:

| directive | above | meaning |
|---|---|---|
| `-- sqlshape: visible where deleted_at IS NULL` | `CREATE TABLE` | every read of the table must carry this predicate |
| `-- sqlshape: not null` | `CREATE FUNCTION` | the function's result is never NULL |
| `-- sqlshape: error P0401 = OrderTooLarge` | a trigger's `CREATE FUNCTION` | the trigger raises this SQLSTATE; statements on its tables must expect it under that name |
| `-- sqlshape: seed` | `INSERT ... VALUES` | the seed is additive: rows the declaration does not list stay in the table ([migrations.md](migrations.md#seeded-tables)) |
| `-- @migrate ...` | anywhere | a migration intent ([migrations.md](migrations.md#declaring-what-a-diff-cannot-see)) |

In Go, as a doc comment: `// sqlshape: type money_amount` above a type declaration binds the type
to that PostgreSQL type ([checks.md](checks.md#shapes-does-the-go-code-fit-the-statement)).

## Shared fragments

A template must be a constant, and Go constants concatenate:

```go
const tenantFilter = " AND tenant_id = {{.TenantID}}"

var ListOrders = sqlshape.Query[Order, ListParams](base + tenantFilter)
var ListItems  = sqlshape.Query[Item, ItemParams](itemsBase + tenantFilter)
```

The whole is still a compile-time constant, so the checker expands it as one template; it
requires `P` to have every field the fragment reads, and a diagnostic about the fragment is
reported on the fragment's own line. Values never become SQL text, so this is the one sharing
mechanism that keeps the injection guarantee. A template built at run time (`fmt.Sprintf`, a
variable) is reported: `query template must be a string constant`.

## Hazards

Some things the template syntax allows do not do what they look like in SQL, and are reported:

- an action inside a string literal: `LIKE '%{{.Q}}%'` is text, not a parameter (write
  `'%' || {{.Q}} || '%'`)
- an action inside a SQL comment has no effect
- a bare parameter as an `ORDER BY` / `GROUP BY` item: `ORDER BY {{.Sort}}` sorts by a
  constant, not by the column the value names (branch on it instead:
  `{{if eq .Sort "total"}} total {{else}} id {{end}}`)

## Many branches

Every branch combination is checked while there are at most 256 of them (eight independent
`{{if}}`s). Beyond that the template is checked sparsely: every branch off, every branch on, and
each branch on alone. That still covers independent `AND` predicates in full, since each one is
seen with and without the others. Two things change for a sparse template: `-strict` reports
that it is sparse, and the runtime cannot compare a rendering with the checked set, so it trusts
the branch shape ([runtime.md](runtime.md#the-runtime-refuses-sql-the-checker-never-saw)).
Splitting a large template into two statements chosen in Go restores the full check.

## Diagnostics name the branch

A finding that holds only in some expansions carries the branch signature it was seen in,
`[if@64:then]` or `[range@120:x2]`: the action's byte offset in the template and the way it went.
A problem shared by every expansion is reported once, without the suffix.
