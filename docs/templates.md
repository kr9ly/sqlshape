# Templates

[日本語](templates.ja.md)

SQL is written as a subset of Go's `text/template`. The input is a value of the parameter type `P`;
each `{{.X}}` becomes a `$n` placeholder, and `{{if}}` and `{{range}}` switch the shape of the
statement. The checker expands every combination of branches and checks each one; the runtime
executes only SQL the checker has seen. Expansion happens at lint time, so the template must be a
string constant.

## What can be written

Values. `{{.Field}}`, `{{.Outer.Inner}}`, `{{.}}`, and `{{$x}}` inside a `range`. Each becomes a
`$n` parameter in the SQL; a value is never spliced in as text. Function calls, method calls and
pipelines are not allowed in value position.

Branches. `{{if}}` / `{{else if}}` / `{{else}}` / `{{end}}`, `{{with}}`, `{{range}}`. There is no
`{{switch}}`; write `{{if eq .Sort "a"}} … {{else if eq .Sort "b"}} … {{end}}`.

Conditions. The builtins `not`, `and`, `or`, `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `len`, `index`,
string and number constants, and field references. A nil pointer, an empty slice or map, zero and
the empty string are false, as in `text/template`.

Not available. `{{define}}` / `{{template}}` (concatenate Go constants instead, see below), custom
functions, variables other than the range element.

```sql
SELECT o.id, o.total, o.created_at
  FROM orders o
 WHERE true
   {{if .CustomerID}} AND o.customer_id = {{.CustomerID}} {{end}}
   {{if .Statuses}}   AND o.status = ANY({{.Statuses}})   {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total DESC {{else}} o.created_at DESC {{end}}, o.id
 {{with .Limit}} LIMIT {{.}} {{end}}
```

This statement has three `{{if}}`s and one `{{with}}`, so it expands to 16 statements, and all 16
are checked.

## Parameters and `P`

`{{.Filter.Name}}` is the field `Name` of the field `Filter` of `P`. Inside
`{{range .Items}} … {{.Sku}} … {{end}}`, `{{.Sku}}` is a field of an element of the slice `Items`;
inside a range over scalars, `{{.}}` is the element itself. Fields of embedded structs are promoted.

The type of each `{{.X}}` is decided by where it is used in the SQL (`WHERE id = {{.ID}}` needs a
`bigint`); a mismatch is reported. Details in [checks.md](checks.md#passing-parameters). A field
that only an `{{if .Flag}}` tests, and no value uses, must exist on `P` but may be of any type.

`{{range}}` is expanded three ways: empty, one element, two elements. Where elements need a
separator, as inside `IN`, write `{{if $i}}, {{end}}`.

```sql
SELECT id FROM products
 WHERE sku IN ({{range $i, $it := .Items}}{{if $i}}, {{end}}{{$it.Sku}}{{end}})
```

## Directives

Directives are SQL comments the checker reads. In a template:

| directive | meaning |
|---|---|
| `-- sqlshape: expect users_email_key, orders.total, P0401` | the constraints, NOT NULL columns and SQLSTATEs this write can violate; the checker keeps the list exact ([checks.md](checks.md#preparing-for-a-write-to-fail)) |
| `-- sqlshape: not null total, note` | these result columns are never NULL, overriding the checker's verdict (the SQL-side twin of the `col:",notnull"` tag) |
| `-- sqlshape: unfiltered memos` | this statement deliberately reads `memos` without its `visible where` predicate |

In `schema.sql`:

| directive | placed above | meaning |
|---|---|---|
| `-- sqlshape: visible where deleted_at IS NULL` | `CREATE TABLE` | every statement reading the table must carry this predicate |
| `-- sqlshape: not null` | `CREATE FUNCTION` | the function's result is never NULL |
| `-- sqlshape: error P0401 = OrderTooLarge` | a function's `CREATE FUNCTION` | names a SQLSTATE the function raises, so expect lines and `Violates` can use the name; a PL/pgSQL body's `RAISE` statements are found without it, under their code |
| `-- sqlshape: seed` | `INSERT ... VALUES` | the seed is additive: rows the declaration does not list stay ([migrations.md](migrations.md#seeded-tables)) |
| `-- @migrate ...` | anywhere | a migration intent ([migrations.md](migrations.md#declaring-what-a-diff-cannot-see)) |

In Go, `// sqlshape: type money_amount` in a type's doc comment binds the type to that PostgreSQL
type ([checks.md](checks.md#the-go-type-table)).

## Sharing SQL

A template must be a constant, and Go constants concatenate. Shared conditions and SELECT lists are
Go constants joined with `+`.

Passes

```go
const tenantFilter = " AND tenant_id = {{.TenantID}}"

var ListOrders = sqlshape.Query[Order, ListParams](base + tenantFilter)
var ListItems  = sqlshape.Query[Item, ItemParams](itemsBase + tenantFilter)
```

The concatenation is expanded as one template. The fields a fragment reads must exist on `P`, and a
diagnostic about the fragment is reported on the line where the fragment is defined.

Rejected: a template built at run time cannot be checked.

```go
var ListOrders = sqlshape.Query[Order, ListParams](fmt.Sprintf(base, table))
// sqlshape: query template must be a string constant
```

## Hazards

Things the template syntax allows but SQL does not do the way they look are reported.

### Do not put `{{.X}}` inside a string literal

Rejected

```sql
SELECT id FROM products WHERE name LIKE '%{{.Q}}%'
--                                        ^ {{.Q}} is inside a string literal: it becomes text, not a parameter (write '%' || {{.Q}} || '%' to concatenate)
```

Passes

```sql
SELECT id FROM products WHERE name LIKE '%' || {{.Q}} || '%'
```

### Do not put a parameter directly in `ORDER BY`

Rejected: it sorts by a constant, not by the column the value names.

```sql
SELECT id, total FROM orders ORDER BY {{.Sort}}
--                                    ^ ORDER BY {{.Sort}} sorts by a constant, not by the column the value names: branch on it instead ({{if eq .Sort "total"}} total {{else}} id {{end}})
```

Passes

```sql
SELECT id, total FROM orders ORDER BY {{if eq .Sort "total"}} total {{else}} id {{end}}
```

### `{{.X}}` inside a comment does nothing

```sql
SELECT id FROM orders -- {{.Note}}
--                       ^ {{.Note}} is inside a comment and has no effect
```

## Many branches

When the branch combinations exceed 256 (eight independent `{{if}}`s), not every combination is
checked; a representative set is. With `-strict` you are told:

```
sqlshape: 512 branch combinations exceed 256: checked sparsely (all branches off, all on, each on alone); the runtime cannot compare renderings with the checked set
```

For a run of independent `AND` conditions the representative set still covers each condition with
and without the others. If branches interact (one nested inside another, an `ORDER BY` tied to a
`WHERE`), something can slip through, so split the template into two statements chosen in Go. The
full check comes back.

## Reading a diagnostic

A problem that occurs only in some expansions ends with the branch it occurred in.

```
field Order.Total is not selected in every branch [if@11:else]: make it a pointer so those branches leave it nil
```

`if@11:else` means the expansion in which the `{{if}}` at byte 11 of the template was false;
`range@120:x2` the one in which the `{{range}}` at byte 120 had two elements. A problem shared by
every expansion is reported once, without the suffix.
