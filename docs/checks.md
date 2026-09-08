# What the checker verifies

[日本語](checks.ja.md)

The checker finds every `sqlshape.Query[R, P](template)`, `sqlshape.One[R, P](template)`,
`sqlshape.Copy[R](...)` and `sqlshape.MatView(...)` in a package, expands each template into every
combination of its branches ([templates.md](templates.md)), analyzes each expansion against
`schema.sql`, and compares the result with the Go types. This page goes through what you write,
situation by situation, and shows what is rejected and what passes, together with the diagnostic
you will see.

It has three parts. **Part 1** is what every statement gets, with no declaration: the shape of the
result and the parameters, the meaning of the types, the failure modes, the `One` proof. **Part 2**
is what `schema.sql` can additionally demand of the statements that touch a table: predicates, pinned
columns, aggregates, state machines, labelled columns. **Part 3** is what is checked outside a
statement: the Go code around it, and the schema itself.

## Contents

- [Part 1 — Checks on every statement](#part-1--checks-on-every-statement)
  - [Receiving a SELECT](#receiving-a-select)
  - [Passing parameters](#passing-parameters)
  - [Giving types a meaning](#giving-types-a-meaning)
  - [Preparing for a write to fail](#preparing-for-a-write-to-fail)
  - [Returning one row (`One`)](#returning-one-row-one)
  - [Bulk loading with COPY](#bulk-loading-with-copy)
- [Part 2 — Rules the schema declares](#part-2--rules-the-schema-declares)
  - [How a declaration works](#how-a-declaration-works)
  - [Every read carries the visibility predicate (`visible where`)](#every-read-carries-the-visibility-predicate-visible-where)
  - [A column is pinned on every statement (`pinned`, `-require-columns`)](#a-column-is-pinned-on-every-statement-pinned--require-columns)
  - [Tables are read through views (`via view`, `-no-table-reads` / `-no-tables`)](#tables-are-read-through-views-via-view--no-table-reads---no-tables)
  - [A predicate across tables has a witness (`EXISTS`)](#a-predicate-across-tables-has-a-witness-exists)
  - [An aggregate is reached through its root, one per statement (`aggregate`)](#an-aggregate-is-reached-through-its-root-one-per-statement-aggregate)
  - [A status column moves along its declared transitions (`transitions`)](#a-status-column-moves-along-its-declared-transitions-transitions)
  - [Append-only tables, paired writes, single-row deletes (`never`, `paired`, `single`)](#append-only-tables-paired-writes-single-row-deletes-never-paired-single)
  - [Labelled columns are read only where allowed (`sensitive`, `may read`)](#labelled-columns-are-read-only-where-allowed-sensitive-may-read)
  - [Different callers, different rules (`context`)](#different-callers-different-rules-context)
  - [SQL outside Go is judged the same way (`sqlshape check`)](#sql-outside-go-is-judged-the-same-way-sqlshape-check)
- [Part 3 — Outside the statement](#part-3--outside-the-statement)
  - [Do not run SQL that bypasses sqlshape (`-raw-sql`)](#do-not-run-sql-that-bypasses-sqlshape--raw-sql)
  - [A package references only its schemas (`-schemas`)](#a-package-references-only-its-schemas--schemas)
  - [Problems in the schema itself](#problems-in-the-schema-itself)

## Part 1 — Checks on every statement

### Receiving a SELECT

For the row type `R`, the checker verifies that every result column has a field to receive it,
every field has a column, and types and NULL handling agree. With branches in the template, this
holds for every expansion.

#### Result columns bind to fields by name

The match is looked up by the `col:"..."` tag, then the `db:"..."` tag, then the field name in
snake_case.

Rejected

```go
type Order struct {
	ID           int64
	CustomerName string // field Order.CustomerName has no result column
}
```

```sql
SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id
--           ^ result column "name" has no field in Order
```

Passes: alias the column, or name the column in the tag.

```sql
SELECT o.id, c.name AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id
```

```go
type Order struct {
	ID           int64
	CustomerName string `col:"name"`
}
```

#### Unnamed and duplicate columns need an alias

Rejected

```sql
SELECT id, count(*) FROM orders GROUP BY id
--         ^ result column 2 has no name: give it an alias (... AS name) so it can bind to a field of Order

SELECT o.id, c.id FROM orders o JOIN customers c ON c.id = o.customer_id
--           ^ result columns 1 and 2 are both named "id": alias one of them (... AS other_name)
```

Passes

```sql
SELECT id, count(*) AS n FROM orders GROUP BY id
SELECT o.id, c.id AS customer_id FROM orders o JOIN customers c ON c.id = o.customer_id
```

#### A column that may be NULL needs a field that can hold NULL

Fields that can hold NULL: pointers, slices, maps, `sql.Null*`, `pgtype.*`, and any type that
implements `sql.Scanner`.

Rejected

```go
type User struct {
	ID        int64
	DeletedAt time.Time // field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
}
```

```sql
SELECT id, deleted_at FROM users
```

Passes

```go
type User struct {
	ID        int64
	DeletedAt *time.Time
}
```

Whether a column may be NULL is derived from NOT NULL constraints and primary keys, from the WHERE
clause (`deleted_at IS NOT NULL` or `deleted_at = ...` rules NULL out), from outer joins (the inner
side may be NULL), from functions (a `strict` function of non-NULL arguments is not NULL, nor is
`coalesce(x, 0)`; a handful of strict built-ins and operators, `meta ->> 'key'` among them, can
still return NULL when there is nothing to report), and from a view's own WHERE clause. A view
tracks its underlying column's NOT NULL live, the way PostgreSQL itself does: a later
`ALTER TABLE ... DROP NOT NULL` on the base table reaches it too, even through a chain of views
and past whatever the view's own WHERE clause does. If you know better than the checker, override
it on the Go side with the `col:",notnull"` tag or on the SQL side with a
`-- sqlshape: not null deleted_at` line in the template. For a function's result, put
`-- sqlshape: not null` above its `CREATE FUNCTION` in `schema.sql`.

#### A column only some branches select needs a field that can hold NULL

Rejected

```go
type Order struct {
	ID    int64
	Total string // field Order.Total is not selected in every branch [if@11:else]: make it a pointer so those branches leave it nil
}
```

```sql
SELECT id {{if .WithTotal}}, total{{end}} FROM orders
```

Passes

```go
type Order struct {
	ID    int64
	Total *string
}
```

The branches that do not select the column leave the field nil. A field no branch selects is
`has no result column`.

#### Column and field types follow the table below

Rejected

```go
type Order struct {
	ID    int64
	Total float64 // field Total is float64 but column "total" is numeric(12,2)
}
```

```sql
SELECT id, total FROM orders
```

Passes

```go
type Order struct {
	ID    int64
	Total string // keeps every digit; decimal.Decimal (shopspring/decimal) works too
}
```

#### Nested rows are received by structs

`array_agg(row(...))`, `array_agg(t)`, `row(...)` and composite-typed columns are received by a
struct or a slice of structs. An anonymous `row(...)` matches fields by position; a named composite
type by name and order.

Rejected: the struct's fields are not in the composite type's order.

```sql
-- schema.sql
CREATE TYPE order_item AS (sku text, qty integer);
```

```go
type Item struct {
	Qty int32 // field Items.Qty is at position 1 but the row type's column 1 is "sku" (fields are scanned in order)
	Sku string
}
type Order struct {
	ID    int64
	Items []Item
}
```

```sql
SELECT o.id, array_agg((i.sku, i.qty)::order_item) AS items
  FROM orders o JOIN order_items i ON i.order_id = o.id
 GROUP BY o.id
```

Passes

```go
type Item struct {
	Sku string
	Qty int32
}
```

#### A single-column statement can be received by a scalar

Passes

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM orders`)
```

Rejected

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT id, total FROM orders`)
// R is int64 but the query returns 2 columns
```

#### Embedded structs are flattened

Passes

```go
type Base struct {
	ID        int64
	CreatedAt time.Time
}
type Order struct {
	Base
	Total string
}
```

```sql
SELECT id, created_at, total FROM orders
```

Rejected: two fields would receive the same column.

```go
type Order struct {
	Base
	ID    int64 // Order: fields Base.ID and ID both bind to column "id"
	Total string
}
```

A named struct field, or an embedded field carrying a `col:"..."` tag, is not flattened: it is
treated as a nested row.

#### The Go type table

What pgx actually scans and encodes, verified against a running PostgreSQL. The same table applies
to receiving columns and to passing parameters.

| PostgreSQL | Go |
|---|---|
| `bool` | `bool` |
| `smallint` / `integer` / `bigint` | `int16` / `int32` / `int64` / `int` (a narrower Go type is accepted with a note such as `bigint into int32`) |
| `real` / `double precision` | `float32` / `float64` (`double precision into float32` is noted) |
| `numeric` | `string` (keeps every digit), `pgtype.Numeric`, `big.Rat`, `shopspring/decimal.Decimal`, `apd.Decimal`; a float or an integer is accepted with a precision note |
| `text` / `varchar` / `char` / `name` / `citext` and other text-like extension types | `string` (a `string` also encodes as a parameter of any type) |
| `bytea` | `[]byte` |
| `uuid` | `uuid.UUID` (any package), `[16]byte`, `string` |
| `timestamptz` / `timestamp` / `date` | `time.Time` (`-strict` notes that `timestamp` and `date` lose the zone or the time) |
| `time` | `time.Time`, `string` |
| `interval` | `time.Duration`, `pgtype.Interval` |
| `json` / `jsonb` | `[]byte`, `json.RawMessage`, `string`, or any struct, slice or map pgx unmarshals into |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)` (each element is checked the same way a plain `T` parameter or column is, notes included) |
| ranges | `pgtype.Range[T]`, with `T` checked against the subtype (user-defined ranges too) |
| multiranges | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | the `pgtype` value |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enums, seeded lookup keys, CHECK value sets | a Go named string type ([below](#giving-types-a-meaning)) |
| domains | the base type's Go type, or a named type bound to the domain |
| composites, records | a struct |

A type not in the table, or one you want to receive with your own type, is declared on the Go type
in its doc comment:

```go
// sqlshape: type money_amount
type Money struct{ ... }   // implements sql.Scanner / driver.Valuer
```

The checker then accepts `Money` exactly where the SQL has `money_amount` (its arrays and domains
over it included) and reports it anywhere else. Conversion is left to the type's own
`sql.Scanner` / `driver.Valuer`; the Scanner receives the text form.

### Passing parameters

`{{.X}}` becomes a `$n` parameter in the SQL. The checker infers the PostgreSQL type each `$n` needs
from where it is used (`WHERE id = $1` needs `bigint`, `= ANY($1)` an array) and verifies that the
corresponding field of `P` fits, by the type table above.

#### A parameter's type follows where it is used

Rejected

```go
type Params struct {
	ID int64 // parameter .ID is int64 but SQL expects uuid
}
```

```sql
SELECT id, email FROM users WHERE id = {{.ID}}   -- id is uuid
```

Passes

```go
type Params struct {
	ID uuid.UUID
}
```

A `string` can be passed as a parameter of any type (it is sent in text form and interpreted by
PostgreSQL). A parameter *wider* than the column it feeds gets an overflow note: an `int64` value
going into an `integer` column is `parameter .ID: int64 into integer may overflow` (the same for a
`float64` value going into a `real` column). This applies element-wise to an array parameter too:
`[]int64` into `smallint[]` gets `parameter .Tags: int64 into smallint may overflow`.

#### A parameter that may be NULL is a pointer

A field whose absence is expressed by a branch, `{{if .Name}} AND name = {{.Name}} {{end}}`, is a
pointer or a slice. A plain `string` cannot send NULL, so it assumes there is always a value.

Rejected: an enum parameter that is not a pointer (`-strict`).

```go
type Params struct {
	Status OrderStatus // parameter .Status is a non-pointer OrderStatus: its zero value "" is not a label of enum order_status and fails at runtime (SQLSTATE 22P02) when unset
}
```

Passes

```go
type Params struct {
	Status *OrderStatus
}
```

#### Nested paths and `range`

`{{.Filter.Name}}` is the field `Name` of the field `Filter` of `P`. Inside
`{{range .Items}} … {{.Sku}} … {{end}}`, `{{.Sku}}` is a field of the element type of the slice
`Items`.

Passes

```go
type Params struct {
	Filter struct{ Name *string }
	Items  []struct{ Sku string; Qty int32 }
}
```

```sql
SELECT id FROM products
 WHERE true {{if .Filter.Name}} AND name = {{.Filter.Name}} {{end}}
   AND sku IN ({{range $i, $it := .Items}}{{if $i}}, {{end}}{{$it.Sku}}{{end}})
```

#### Composite parameters are structs

Where the SQL expects a composite type, pass a struct; where it expects an array of one, a slice of
structs. Fields line up with the type's columns as for nested rows.

Passes

```sql
-- schema.sql
CREATE TYPE order_item AS (sku text, qty integer);
CREATE FUNCTION place_order(customer_id bigint, items order_item[]) RETURNS bigint ...
```

```go
type Item struct {
	Sku string
	Qty int32
}
type Params struct {
	CustomerID int64
	Items      []Item
}
```

```sql
SELECT place_order({{.CustomerID}}, {{.Items}})
```

#### Do not leave unused fields in `P` (`-strict`)

```go
type Params struct {
	ID    int64
	Limit int32 // sqlshape: parameter field Limit is never used by the template
}
```

```sql
SELECT id FROM orders WHERE id = {{.ID}}
```

#### Do not always send a value into a column with a DEFAULT (`-strict`)

```go
type NewOrder struct {
	CustomerID int64
	Status     OrderStatus // parameter .Status always sends a value into orders.status, so its DEFAULT never applies: decide which side owns the default (make the column conditional with {{if}} to use the database's)
}
```

```sql
INSERT INTO orders (customer_id, status) VALUES ({{.CustomerID}}, {{.Status}})
```

Passes: to use the database's default, make the column conditional. If the application always
decides, drop the column's `DEFAULT`.

```sql
INSERT INTO orders (customer_id {{if .Status}}, status{{end}})
VALUES ({{.CustomerID}} {{if .Status}}, {{.Status}}{{end}})
```

### Giving types a meaning

Values a `bigint` or `text` cannot tell apart, such as an enum label, a table's ID or an amount in
some unit, are named types in Go. The checker binds a named type to the meaning on the SQL side
(an enum, a lookup table's key, a CHECK value set, a primary key, a domain) and reports every use
that disagrees.

#### Meaning comes from use, not from a registry

There is no place where a Go type is declared to "be" a table's ID or an enum. A plain named type
acquires its meaning the first time a statement passes or receives it against a column that has one,
and keeps it from then on, in every package that uses the type.

```go
type UserID int64 // nothing more: no tag, no comment, no registration
```

```go
// 1. the first meeting binds: UserID now stands for users.id
var User = sqlshape.One[User, struct{ ID UserID }](`SELECT id, email FROM users WHERE id = {{.ID}}`)

// 2. every later meeting is checked against that binding
var Total = sqlshape.One[int64, struct{ ID UserID }](`SELECT total FROM orders WHERE id = {{.ID}}`)
// sqlshape: parameter .ID is UserID, which stands for key users.id elsewhere, but here meets key orders.id
```

The same goes for a string type meeting an enum column or the key of a seeded lookup table (its
constants are then compared with the labels), and for an integer type meeting a domain column (it
then carries the unit). Which statement comes first does not matter: all bindings of a type are
collected and must agree. A type that never meets such a column is never checked. The binding is
exported as an analysis fact, so a type declared in one package and used in another is one type
with one meaning.

Why this way: sqlshape generates nothing and has no registration API, so the Go code stays plain Go;
binding by use means a type is checked exactly where it matters and never has to be announced.

#### Enum and lookup values agree with the named type's constants

For a named string type used with an enum column, the key of a seeded lookup table, or a column
with `CHECK (col IN (...))`, the typed constants are compared with the labels in both directions.

```sql
-- schema.sql
CREATE TABLE order_statuses (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO order_statuses VALUES ('pending', 'Pending'), ('paid', 'Paid'), ('shipped', 'Shipped');
CREATE TABLE orders (..., status text NOT NULL REFERENCES order_statuses(code));
```

Nothing ties `OrderStatus` to `order_statuses` by name. The type is bound to the value set where
it meets the column in a statement: here `{{.Status}}` feeds `orders.status`, whose foreign key
points at `order_statuses(code)`, so `OrderStatus` is now the Go side of that lookup table. The
binding is exported as a fact and holds in every package that uses the type; a type that never
meets a column is never checked.

```go
var ByStatus = sqlshape.Query[Order, struct{ Status OrderStatus }](`
SELECT id, total FROM orders WHERE status = {{.Status}}`)
```

Rejected

```go
type OrderStatus string

const (
	Pending  OrderStatus = "pending"
	Paid     OrderStatus = "paid"
	Canceled OrderStatus = "canceled" // sqlshape: OrderStatus has constant "canceled" which is not a label of value set of order_statuses (lookup table)
)
// sqlshape: value set of order_statuses (lookup table) has label "shipped" but OrderStatus has no constant for it
```

Passes

```go
const (
	Pending OrderStatus = "pending"
	Paid    OrderStatus = "paid"
	Shipped OrderStatus = "shipped"
)
```

A conversion such as `OrderStatus("typo")` is reported as
`sqlshape: OrderStatus("typo") is not a label of ...`, and a `switch` that does not handle every
label as `sqlshape: switch on OrderStatus does not handle ... labels: shipped`. If the type
implements `Known() bool`, the row mapper returns `*UnknownLabelError` at run time for a label this
build does not know. A seeded lookup table is the recommended home for a value set, over an enum
([migrations.md](migrations.md#seeded-tables)).

#### Do not pass another table's ID

A named type used with a primary key column, or a column derived from one through a foreign key,
is treated as that table's ID. The binding comes from use, as with value sets: `UserID` below stands
for `users.id` because some statement passed it where `users.id` (or a foreign key to it) was expected.

```go
type UserID int64
type OrderID int64

var User = sqlshape.One[User, struct{ ID UserID }](`SELECT id, email FROM users WHERE id = {{.ID}}`)
```

Rejected

```go

type Params struct {
	ID UserID // sqlshape: parameter .ID is UserID, which stands for key users.id elsewhere, but here meets key orders.id
}
```

```sql
SELECT total FROM orders WHERE id = {{.ID}}
```

Passes

```go
type Params struct {
	ID OrderID
}
```

#### Do not mix domains of different units

A named type used with a domain column is bound to that domain. Inside SQL too, a domain is a unit
distinct from its base type: PostgreSQL itself falls back to the base type and allows the
operation, the checker reports it.

```sql
-- schema.sql
CREATE DOMAIN yen AS bigint;
CREATE DOMAIN gram AS integer;
CREATE TABLE products (id bigint PRIMARY KEY, price yen NOT NULL, weight gram NOT NULL);
```

Rejected

```sql
SELECT id FROM products WHERE price + weight > 1000
--                            ^ domain mismatch: yen + gram: mixes yen with gram (cast to the base type to drop the domain)
```

```go
type Params struct {
	Max int64 // sqlshape: parameter .Max carries domain yen as a plain int64; declare a named type to have it checked   (-strict)
}
```

Passes

```go
type Yen int64

type Params struct {
	Max Yen
}
```

```sql
SELECT id FROM products WHERE price > {{.Max}}
```

Literals and parameters take the unit of the other side: `price + 100`, `price * 2`, `abs(price)`
and `coalesce(price, 0)` stay yen. To drop the unit, cast to the base type explicitly
(`price::bigint + weight::bigint`).

#### Do not receive with a type that loses information (`-strict`)

```go
type Event struct {
	At  time.Time // field At: timestamp without time zone into time.Time: which zone the value is in becomes the application's implicit choice (prefer timestamptz)
	Day time.Time // field Day: date into time.Time: a zone conversion can move the day (keep it at UTC midnight or use a civil date type)
}
```

`timestamptz` gets no such note. If a `date` is received as `time.Time`, decide in the code that it
is UTC midnight.

### Preparing for a write to fail

INSERT, UPDATE, DELETE and MERGE can fail on a constraint. For each expansion the checker lists the
constraints it can violate and verifies that the template declares them on a
`-- sqlshape: expect` line. At run time the violation comes back as a `ConstraintError` under the
declared name ([runtime.md](runtime.md#errors)).

#### Declare the constraints a write can violate

Rejected

```sql
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id
--                     ^ may violate customers_email_key (UNIQUE (email) on customers, SQLSTATE 23505); add `-- sqlshape: expect customers_email_key` to the template or make it impossible
```

Passes

```sql
-- sqlshape: expect customers_email_key
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id
```

```go
_, err := CreateCustomer.First(ctx, db, p)
if sqlshape.Violates(err, "customers_email_key") { ... }
```

Listed are unique constraints and primary keys, foreign keys in both directions (the inserted row
references a missing parent; the deleted row is still referenced by a child), EXCLUDE constraints,
CHECKs, domain CHECKs, and NOT NULL where the value written may be NULL. A NULL that would come from
a parameter is dropped when the field's Go type cannot be nil (a `string`, for instance).

A DELETE or an UPDATE that changes a referenced key can also fail through what its foreign keys'
`ON DELETE` / `ON UPDATE` actions do to the referencing rows, not just through a foreign key that
rejects the change outright (`NO ACTION` / `RESTRICT`): `SET NULL` can hit a NOT NULL constraint on
the referencing column, `SET DEFAULT` can hit the same foreign key again (its default value need not
exist in the parent) and the same NOT NULL if there is no default, and `CASCADE` deletes (or updates)
the referencing rows, which is checked the same way one level further down -- so a chain of several
`ON DELETE CASCADE` foreign keys can still end in a failure several tables away.

A write through a view with `WITH [LOCAL | CASCADED] CHECK OPTION` can also fail with SQLSTATE
44000 (PostgreSQL's own error names no constraint here, so the SQLSTATE is the expect line's key,
the same way an unannotated trigger SQLSTATE is). `CASCADED` (the default when the option is given
without a qualifier) checks the WHERE clause of every updatable view further down the stack as
well, not just this one.

#### Do not declare a violation that cannot happen

Rejected

```sql
-- sqlshape: expect orders_total_check
--                  ^ expects orders_total_check but no expansion can violate it
UPDATE orders SET note = {{.Note}} WHERE id = {{.ID}}
```

The expect line is kept as the exact list of reasons the statement can fail. Remove declarations
that are no longer needed.

#### Constraint names

A named constraint goes by its name. An unnamed one goes by the name PostgreSQL gives it, so the
diagnostic, the expect line and the run-time error all carry the same string.

| constraint | key | example |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| `REFERENCES` on `(a)` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| table `CHECK` referencing exactly one column `(a)` | `<table>_<a>_check` (a CHECK on several columns, or on none, is `<table>_check`) | `orders_total_check` |
| domain `CHECK` | `<domain>_check` | `yen_check` |
| `EXCLUDE (a, b)` | `<table>_<a>_<b>_excl` | `reservations_room_during_excl` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| an error raised by a trigger | the SQLSTATE, or the name given with `-- sqlshape: error` | `P0401`, `OrderTooLarge` |
| `WITH CHECK OPTION` on a view | the SQLSTATE (PostgreSQL's own 44000 error names no constraint) | `44000` |

A second constraint that would get the same name is numbered, as PostgreSQL does
(`orders_total_check1`). A generated name over PostgreSQL's 63-byte identifier limit is cut down the
same way PostgreSQL cuts it, without splitting a multibyte character.

#### Name the errors a trigger raises

The checker reads a PL/pgSQL trigger body, so a `RAISE EXCEPTION ... USING ERRCODE = 'P0401'`
inside it already adds `P0401` to the failure modes (a `RAISE` without `ERRCODE` is `P0001`).
The annotation gives the code a name to use on expect lines and in `Violates`:

```sql
-- schema.sql
-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total > 1000000 THEN RAISE EXCEPTION 'order too large' USING ERRCODE = 'P0401'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_size BEFORE INSERT OR UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION check_order_size();
```

```sql
-- sqlshape: expect OrderTooLarge, orders_customer_id_fkey
INSERT INTO orders (customer_id, total) VALUES ({{.CustomerID}}, {{.Total}})
```

The SQLSTATE joins the failure modes of the statements on the trigger's table for the events it
fires on (here INSERT and UPDATE).

#### PL/pgSQL statements that fail on their own

A few PL/pgSQL statements can fail without a `RAISE`, and the checker adds their SQLSTATE to the
body's failure modes the same way it does for an explicit one:

- `SELECT ... INTO STRICT` and `EXECUTE ... INTO STRICT` are `P0002` (no_data_found) when the
  query returns no rows, and `P0003` (too_many_rows) when it returns more than one; `P0003` is
  dropped when the query is provably at most one row (the same proof `One` uses).
- A `CASE` statement with no matching `WHEN` and no `ELSE` is `20000` (case_not_found).
- An `ASSERT` is `P0004` (assert_failure) when its condition is false.

`RAISE ... USING ERRCODE = <expr>` resolves statically when `<expr>` is a string literal, a
variable declared with a literal default that is never reassigned, or, inside an `EXCEPTION`
handler, the bare `SQLSTATE` re-raising what the handler caught. Any other expression leaves the
SQLSTATE undetermined, which the checker reports rather than assuming `P0001`.

A `BEGIN ... EXCEPTION WHEN ... END` block catches whatever its `WHEN` conditions cover -- a
condition name, an error class name (matches every code of that class, so
`integrity_constraint_violation` catches any `23xxx`), a literal `SQLSTATE '...'`, or `OTHERS` --
so those failure modes do not reach the caller. Whatever the handler itself goes on to raise does.

#### A statement that calls a function declares the function's failure modes too

A call to a user-defined function inherits the failure modes of its body (`LANGUAGE sql` or
`plpgsql`): the constraints its writes can violate, the triggers those writes fire, the functions
it calls, and the SQLSTATEs it raises.

```sql
SELECT place_order({{.CustomerID}}, {{.Note}})
--     ^ may violate orders_customer_id_fkey (FOREIGN KEY (customer_id) on orders REFERENCES customers, SQLSTATE 23503, through place_order()); add `-- sqlshape: expect orders_customer_id_fkey` to the template or make it impossible
```

A NOT NULL the body blames on a parameter is traced to the call's argument. A `STRICT` function is
not called with a NULL, so that argument's violation is dropped.

### Returning one row (`One`)

`sqlshape.One[R, P]` declares that the statement returns at most one row, and the checker proves it
from the schema for every expansion. If it cannot, that is an error. At run time `Get` returns
`ErrNoRows` when there is no row, `Find` reports presence, and both return `ErrManyRows` if the
database contradicts the proof with a second row.

#### Fix a unique key by equality

Rejected

```go
var ByName = sqlshape.One[User, struct{ Name string }](`
SELECT id, email FROM users WHERE name = {{.Name}}`)
// One: cannot prove at most one row: users: no unique key is fixed by equality (keys: (id), (email))
```

Passes

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email FROM users WHERE email = {{.Email}}`)
```

A statement is single when, for every table in FROM, a unique key (primary key, `UNIQUE`, unique
index, or a partial unique index whose predicate the WHERE clause repeats) is fixed by equality to a
literal, a parameter, an outer reference or an uncorrelated scalar subquery. Equalities are followed
through joins (an outer join's ON fixes only the nullable side), views, subqueries and CTEs. An
aggregate without `GROUP BY`, a constant `LIMIT 0` / `LIMIT 1`, a SELECT without FROM, a one-row
`VALUES` and a one-row `INSERT ... RETURNING` are single too. A `FULL JOIN` never is, and neither is
a key declared `DEFERRABLE`: its uniqueness is not enforced until commit, so a transaction can hold
two rows sharing it for its own lifetime.

#### Every branch must be provable

Rejected

```go
var Find = sqlshape.One[User, struct{ ID *int64 }](`
SELECT id, email FROM users WHERE true {{if .ID}} AND id = {{.ID}} {{end}}`)
// One: cannot prove at most one row: users: no unique key is fixed by equality (keys: (id), (email)) [if@39:else]
```

In the branch where `.ID` is nil the condition disappears and every row comes back. Finding that is
the point of the check: make this a `Query`, or make `.ID` a non-pointer and drop the branch.

### Bulk loading with COPY

`sqlshape.Copy[R]("order_items", "order_id", "line_no", ...)` is checked like an INSERT: the table
and columns exist, each column's type fits the field that feeds it, and every column left out has a
default or is generated.

Rejected

```go
type Item struct {
	OrderID int64
	Sku     string
}
var Load = sqlshape.Copy[Item]("order_items", "order_id", "sku")
// Copy into order_items: column "line_no" is NOT NULL without a default and is not copied
```

Passes

```go
type Item struct {
	OrderID int64
	LineNo  int16
	Sku     string
}
var Load = sqlshape.Copy[Item]("order_items", "order_id", "line_no", "sku")
```

## Part 2 — Rules the schema declares

Rules that show up in the shape of the SQL can be enforced by the checker: always filter
soft-deleted rows, always pin the tenant column, read tables only through views, move a status
column only along its transitions. They are declared in `schema.sql`, above the table they are about,
and every statement that touches the table must satisfy them. None applies until declared.

### How a declaration works

A table (or view) declares an **obligation**; a statement that touches it must **discharge** the
obligation with what it provably does. The general form is a directive above `CREATE TABLE` or
`CREATE VIEW`:

```sql
-- sqlshape: require <what> [on <kinds>]
```

| `<what>` | the statement must | default `on` |
|---|---|---|
| an SQL boolean expression (`deleted_at IS NULL`, `status <> 'closed' AND amount > 0`, `EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)`) | carry it for the table's rows: each conjunct is implied by the WHERE / ON (equalities, IS NULL, IS NOT NULL) or appears verbatim; an `EXISTS` needs a witness ([below](#a-predicate-across-tables-has-a-witness-exists)) | `read` |
| `pinned(tenant_id)` | fix the column to one value (`= {{.X}}`, a literal, an outer reference) when reading, updating or deleting; assign it when inserting | `all` |
| `immutable(tenant_id)` | not assign the column in an UPDATE | `update` |
| `via view` | not reference the table directly (a write target is allowed unless `on` includes writes) | `read` |
| `never` | not exist: an append-only table (`require never on update, delete`) | `write` |
| `paired(outbox)` | write the named table in the same statement too (a data-modifying `WITH`) | `insert` |
| `single` | provably touch at most one row (the `One` proof) | `delete` |

`<kinds>` is a comma-separated list of `select`, `insert`, `update`, `delete`, or the groups `read`
(SELECT, and the target of an UPDATE / DELETE / MERGE, whose WHERE reads it), `write` and `all`. A
`$n` in a declaration stands for any value known before the row is examined -- a parameter, a
literal, an outer reference -- not for that parameter number; a Go template's `{{.X}}` is such a
value.

Three declarations are not `require` lines but expand into obligations: `visible where <expr>` (the
original spelling of `require <expr> on read`), `aggregate` and `transitions`; `sensitive` labels
columns. The vet flags `-require-columns=tenant_id`, `-no-table-reads` and `-no-tables` are
shorthands for `require pinned(tenant_id)` on every table that has the column, `require via view` on
every table, and `require via view on all`.

An obligation is discharged one of five ways, and `-strict` reports the ones that deserve a look:

1. by the statement's own WHERE / ON / SET;
2. by a view: a view's definition is judged on its own when the schema loads, and readers of the
   view are not judged again for the tables inside it (a function body is judged the same way);
3. by a row-level security policy whose USING establishes it, for roles subject to row security
   (`-strict` notes the owner caveat unless the table has `FORCE ROW LEVEL SECURITY`);
4. across a composite foreign key: with `FOREIGN KEY (order_id, tenant_id) REFERENCES orders (id, tenant_id)`,
   a join on `order_id = orders.id` where `orders.tenant_id` is pinned pins `order_items.tenant_id` too;
5. by an opt-out in the statement: `-- sqlshape: unfiltered orders` (the predicate obligations) or
   `-- sqlshape: waive orders pinned(tenant_id)` (one obligation, spelled as declared; `waive orders`
   alone waives every obligation on the table). Opt-outs are reported with `-strict`.

Each occurrence of a table is judged on its own: a self-join or a subquery that reads the table
again owes the obligation again. `RETURNING` lists are not judged. An obligation declared on a view
binds the view's readers; the view's own definition answers for the tables inside it.

### Every read carries the visibility predicate (`visible where`)

```sql
-- schema.sql
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (...);
```

Rejected

```sql
SELECT id, body FROM memos WHERE user_id = {{.UserID}}
-- rows of memos are visible where deleted_at IS NULL: add that predicate for memos, or opt out with `-- sqlshape: unfiltered memos`
```

Passes

```sql
SELECT id, body FROM memos WHERE user_id = {{.UserID}} AND deleted_at IS NULL
```

```sql
-- a statement that deliberately reads every row
-- sqlshape: unfiltered memos
SELECT id, body FROM memos WHERE id = {{.ID}}
```

Views are checked by the same rule; reading through a view that carries the predicate satisfies it
for the view's readers. `RETURNING` columns are not checked: they are rows just written.

### A column is pinned on every statement (`pinned`, `-require-columns`)

```sql
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (...);
```

Rejected

```sql
SELECT id, total FROM orders WHERE id = {{.ID}}
-- orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)
```

Passes

```sql
SELECT id, total FROM orders WHERE id = {{.ID}} AND tenant_id = {{.TenantID}}
```

An INSERT must assign `tenant_id`. A row-level security policy that fixes `tenant_id` satisfies the
rule too, but only for roles subject to row security: unless the table has
`FORCE ROW LEVEL SECURITY`, `-strict` notes
`orders.tenant_id is pinned by policy ... for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner`.

Optimistic locking is the same declaration on a version column, for writes only:

```sql
-- sqlshape: require pinned(version) on update, delete
CREATE TABLE orders (..., version int NOT NULL DEFAULT 1);
CREATE TRIGGER orders_bump_version BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION bump_version();
```

```sql
UPDATE orders SET status = {{.Status}} WHERE id = {{.ID}} AND version = {{.Version}}
-- passes; without `AND version = ...`:
-- orders.version is not pinned: every statement on orders must fix version by equality (or assign it)
```

The database increments `version` (the trigger); the statement must name the version it saw
(`pinned`); a `One` UPDATE that matched nothing returns `ErrNoRows` (someone got there first).

### Tables are read through views (`via view`, `-no-table-reads` / `-no-tables`)

```sql
-- sqlshape: require via view
CREATE TABLE orders (...);
```

Rejected (`-no-table-reads`)

```sql
SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id
-- table orders is read directly; with -no-table-reads application code reads views (tables are written, not read)
```

Passes

```sql
SELECT id, customer_name FROM order_summary
```

`-no-table-reads` still allows a table as the target of INSERT, UPDATE, DELETE and MERGE.
`-no-tables` forbids every table reference, writes included

### A predicate across tables has a witness (`EXISTS`)

A predicate that reaches across tables is written as `EXISTS`, and the statement must have a
**witness** for it: a table of the same level joined so that the body holds (an inner join; an outer
join's ON does not restrict the subject's rows), or an unnegated `EXISTS` / `IN (SELECT ...)` of its
own whose body holds.

```sql
-- sqlshape: require EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)
CREATE TABLE shipments (...);
```

Passes, all three

```sql
SELECT s.carrier FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.tenant_id = {{.T}}
SELECT carrier FROM shipments s WHERE EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = {{.T}})
SELECT carrier FROM shipments WHERE order_id IN (SELECT id FROM orders WHERE tenant_id = {{.T}})
```

Rejected: an outer join, a negated subquery, a join that never fixes the tenant

```sql
SELECT s.carrier FROM shipments s LEFT JOIN orders o ON o.id = s.order_id AND o.tenant_id = {{.T}}
SELECT carrier FROM shipments s WHERE NOT EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = {{.T}})
```

### An aggregate is reached through its root, one per statement (`aggregate`)

Some tables only make sense together. An order has its line items and its notes; nobody adds a line
to an order without going through the order, and an invariant like "the total matches the lines"
holds for the order as a whole, not for a line on its own. Domain-driven design calls such a cluster
an **aggregate**: one table is the root, the others are its children, the outside world holds the
root's ID and nothing else, and one change touches one aggregate. Applications enforce this by
convention (a repository per aggregate, a service layer), which holds until someone writes the SQL
directly. sqlshape can enforce the part of it that shows in the shape of a statement, and it is a
bundle of obligations declared once above the root:

```sql
-- sqlshape: aggregate orders (order_items, order_notes)
CREATE TABLE orders (...);
```

It expands to `require pinned(<foreign key to orders>) on all` on each child (a child is reached
through its root: pin the key, or join on the root's key) and to `alone` on every table of the
aggregate: one statement touches one aggregate. Reading across aggregates is what a view is for;
tables in no aggregate (lookups) are free to join. A child must have a foreign key to the root, or to
another member: a grandchild pins its key to the parent it hangs off, and a join up the chain
discharges it link by link.

Rejected

```sql
SELECT o.status, v.id FROM orders o JOIN invoices v ON v.order_id = o.id WHERE o.id = {{.ID}}
-- orders belongs to aggregate orders and this statement also touches invoices of aggregate invoices: one statement, one aggregate (read across aggregates through a view)
```

`lock <column>` makes the root's version the lock of the whole aggregate:

```sql
-- sqlshape: aggregate orders (order_items, order_item_tags) lock version
```

The root owes `pinned(version) on update, delete` (name the version you saw), and every child owes,
on UPDATE / DELETE, an `EXISTS` witnessing the root row at that version through its foreign key
(through its parent's, for a grandchild):

```sql
UPDATE order_items i SET qty = {{.Q}} FROM orders o WHERE o.id = i.order_id AND o.version = {{.V}} AND i.id = {{.ID}}
```

Incrementing the version stays the database's job (a trigger on the root, fired by the children's
triggers if they must bump it); a `One` write that matched nothing returns `ErrNoRows`.

### A status column moves along its declared transitions (`transitions`)

```sql
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled, paid -> refunded
CREATE TABLE orders (...);
```

A status column is a small state machine: an order goes from draft to submitted, then to paid or
cancelled, never from draft straight to paid and never back. The declaration writes the machine down.
The check is that an UPDATE setting the column to a state fixes the current state in its WHERE to
one of that state's predecessors. This is compare-and-set: if two requests both try to mark the same
order paid, the second one's `WHERE status = 'submitted'` no longer matches and it updates nothing,
instead of silently paying twice. A `One` UPDATE reports that as `ErrNoRows`.

Passes

```sql
UPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND status = 'submitted'
```

Rejected

```sql
UPDATE orders SET status = 'paid' WHERE id = {{.ID}}
-- orders.status: SET status = 'paid' must fix the current state in WHERE (status = 'submitted')
UPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND status = 'draft'
-- orders.status: draft -> paid is not a declared transition (paid comes from submitted)
```

The target must be a literal naming a declared state; `SET status = {{.S}}` is refused (no predecessor
set can be checked for it). An INSERT is not a transition and is not checked. The declaration is the
same shape as a seeded `(from_status, to_status)` lookup table, which a trigger can enforce on the
database side.

### Append-only tables, paired writes, single-row deletes (`never`, `paired`, `single`)

```sql
-- sqlshape: require never on update, delete
CREATE TABLE ledger (...);
-- sqlshape: require paired(outbox) on insert
-- sqlshape: require single on delete
CREATE TABLE orders (...);
```

`never` says the statement must not exist: an UPDATE or DELETE on `ledger` is rejected
(`ledger is declared \`require never on update, delete\`: no statement may do this to it`), inside
a `WITH` as much as on its own.

`paired(outbox)` is for the outbox pattern. When a write must also notify the outside world (a
message queue, a webhook, another service), sending the notification directly risks the two getting
out of step: the row is written but the message is lost, or the message goes out and the write
rolls back. The outbox pattern writes the message into a table in the same transaction as the
change, and a separate process delivers it from there. `paired` makes the pairing a rule: an INSERT
into `orders` must write `outbox` in the same statement, so the two cannot be separated even by a
forgotten line, and no transaction layer is needed:

```sql
WITH o AS (INSERT INTO orders (...) VALUES (...) RETURNING id)
INSERT INTO outbox (id, payload) SELECT id, 'created' FROM o
-- passes; a bare INSERT INTO orders:
-- a write to orders must also write outbox in the same statement (a data-modifying WITH): require paired(outbox) on insert
```

`single` says a DELETE must provably touch at most one row, by the same proof as `One`:

```sql
DELETE FROM orders WHERE id = {{.ID}}              -- passes
DELETE FROM orders WHERE status = 'cancelled'
-- orders requires a single-row DELETE: fix a unique key by equality (the One proof)
```

### Labelled columns are read only where allowed (`sensitive`, `may read`)

```sql
-- sqlshape: sensitive pii: email, phone
CREATE TABLE orders (...);
-- sqlshape: context billing: may read pii
CREATE VIEW order_contacts AS SELECT id, email, left(phone, 3) || '***' AS phone_masked FROM orders;
```

A statement may reference a labelled column -- in its SELECT list or in a WHERE alike -- only in a
context that `may read` the label ([contexts](#different-callers-different-rules-context)); storing a
value into it is not reading it. The label follows a column through a view that passes it through,
and stops at an expression (a masked column).

```sql
SELECT email FROM orders WHERE id = {{.ID}}            -- fails outside billing
-- orders.email is pii: this context may not read it (a context with `may read pii`, or a view that masks it)
SELECT email FROM order_contacts WHERE id = {{.ID}}    -- fails too: the view passes email through
SELECT phone_masked FROM order_contacts                -- passes: an expression carries no label
```

### Different callers, different rules (`context`)

An operator's script may run without a tenant, an analyst may only read views, billing may read
personal data. A **context** declares the difference per table, and a package or a `check` run selects
one:

```sql
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete
-- sqlshape: context analyst: require via view
-- sqlshape: context billing: may read pii
CREATE TABLE orders (...);
```

```go
// Package ops runs the operator's scripts.
//
// sqlshape: context ops
package ops
```

`waive <body>` lifts a base obligation (spelled as declared) inside the context; `require ...` adds
one that holds there only; `may read <label>` permits a label. A package names its context in its
package comment; otherwise vet's `-context` flag applies; `sqlshape check -context ops` selects one
for a file. Without a context, the base obligations alone apply.

### SQL outside Go is judged the same way (`sqlshape check`)

The same judgment is available for SQL that is not in Go code: an operator's UPDATE, a backfill, a
query an agent is about to run.

```
$ sqlshape check -schema schema.sql ops.sql     # or: ... < ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

Every judgment is printed, with the path that discharged it (`ok`, `ok(policy)`, `ok(fk)`,
`waived`), so the output doubles as the audit of what a script was allowed to do; `-quiet` prints
failures only. The exit code is 1 when a statement fails an obligation or does not analyze. The
`-- sqlshape:` lines above a statement belong to it, and `-context`, `-require-columns`,
`-no-tables` and `-no-table-reads` are accepted as in vet.

## Part 3 — Outside the statement

### Do not run SQL that bypasses sqlshape (`-raw-sql`)

template guarantee does not cover.

Rejected (the default, `-raw-sql=constant`)

```go
rows, err := pool.Query(ctx, "SELECT id FROM orders WHERE "+where)
// sqlshape: SQL passed to Query must be a constant: a string built at run time can carry injected SQL; write the dynamic parts as a sqlshape.Query template (or pass -raw-sql=allow)
```

Passes

```go
rows, err := pool.Query(ctx, "SELECT id FROM orders WHERE status = $1", status)
```

`-raw-sql=forbid` rejects every statement that does not go through sqlshape, constant or not
(`pgxpool.Query executes SQL outside sqlshape; with -raw-sql=forbid every statement goes through sqlshape.Query / One / Copy (or list the package in -raw-sql-allow)`).
Packages still migrating are exempted with `-raw-sql-allow=pkg/...`.

### A package references only its schemas (`-schemas`)

`-schemas=a_api,b_private` restricts the PostgreSQL schemas a package may reference: a service
boundary over one database.

```sql
SELECT id FROM c_private.orders
-- c_private.orders is outside the schemas this code may reference (a_api,b_private)
```

### Problems in the schema itself

`schema.sql` is analyzed once when loaded. The bodies of `LANGUAGE sql` and `LANGUAGE plpgsql`
functions, views and policies get the same type check PostgreSQL performs at CREATE time, and seed
INSERTs are checked for idempotency and types. Problems are reported at the first `Query` of the
package as `sqlshape: schema ...`.

A PL/pgSQL body is checked statement by statement with its variables in scope: `DECLARE`d
variables by their type (`%TYPE` and `%ROWTYPE` resolved against the schema), record variables by
the shape of the query that filled them (`FOR r IN SELECT ...`, `SELECT ... INTO r`), `NEW` and
`OLD` in a trigger function by the row type of each table a `CREATE TRIGGER` attaches it to,
function parameters, `FOUND`, `TG_OP` and the other trigger variables, `SQLSTATE` and `SQLERRM`
in an exception handler. Assignments and `RETURN` are checked against the declared types,
`RETURN QUERY` against `RETURNS TABLE`, and a name that is both a variable and a column is
ambiguous, as in PostgreSQL. `EXECUTE` of a constant string is checked like the statement it runs;
a string built at run time cannot be, and `-strict` says so:

```sql
CREATE FUNCTION purge(tbl text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'DELETE FROM ' || quote_ident(tbl);
  -- sqlshape: schema: function purge: line 3: EXECUTE runs SQL built at run time, which is not checked (a constant string would be)
END $$;
```

```sql
-- schema.sql
CREATE VIEW order_summary AS
SELECT o.id, c.nmae AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id;
-- sqlshape: schema schema.sql: view order_summary: column "nmae" does not exist (SQLSTATE 42703)
```

Row-level security policies are checked here too: a `CREATE POLICY` predicate must be boolean,
contain no aggregates or window functions, and respect domain units. A policy on a table whose row
security is off is reported. The checker does not ask statements to repeat a policy's predicate; the
database does the filtering.

`-strict` adds advisory findings about the schema and the statements, listed in
[flags.md](flags.md#-strict).
