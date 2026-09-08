# What the checker verifies

[日本語](checks.ja.md)

The checker finds every `sqlshape.Query[R, P](template)`, `sqlshape.One[R, P](template)`,
`sqlshape.Copy[R](...)` and `sqlshape.MatView(...)` in a package, expands each template into every
combination of its branches ([templates.md](templates.md)), analyzes each expansion against
`schema.sql`, and compares the result with the Go types. This page goes through what you write,
situation by situation, and shows what is rejected and what passes, together with the diagnostic
you will see.

## Receiving a SELECT

For the row type `R`, the checker verifies that every result column has a field to receive it,
every field has a column, and types and NULL handling agree. With branches in the template, this
holds for every expansion.

### Result columns bind to fields by name

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

### Unnamed and duplicate columns need an alias

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

### A column that may be NULL needs a field that can hold NULL

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

### A column only some branches select needs a field that can hold NULL

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

### Column and field types follow the table below

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

### Nested rows are received by structs

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

### A single-column statement can be received by a scalar

Passes

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM orders`)
```

Rejected

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT id, total FROM orders`)
// R is int64 but the query returns 2 columns
```

### Embedded structs are flattened

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

### The Go type table

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

## Passing parameters

`{{.X}}` becomes a `$n` parameter in the SQL. The checker infers the PostgreSQL type each `$n` needs
from where it is used (`WHERE id = $1` needs `bigint`, `= ANY($1)` an array) and verifies that the
corresponding field of `P` fits, by the type table above.

### A parameter's type follows where it is used

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

### A parameter that may be NULL is a pointer

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

### Nested paths and `range`

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

### Composite parameters are structs

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

### Do not leave unused fields in `P` (`-strict`)

```go
type Params struct {
	ID    int64
	Limit int32 // sqlshape: parameter field Limit is never used by the template
}
```

```sql
SELECT id FROM orders WHERE id = {{.ID}}
```

### Do not always send a value into a column with a DEFAULT (`-strict`)

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

## Giving types a meaning

Values a `bigint` or `text` cannot tell apart, such as an enum label, a table's ID or an amount in
some unit, are named types in Go. The checker binds a named type to the meaning on the SQL side
(an enum, a lookup table's key, a CHECK value set, a primary key, a domain) from where it is used,
and reports every later use that disagrees. Nothing is registered. The binding crosses packages.

### Enum and lookup values agree with the named type's constants

For a named string type used with an enum column, the key of a seeded lookup table, or a column
with `CHECK (col IN (...))`, the typed constants are compared with the labels in both directions.

```sql
-- schema.sql
CREATE TABLE order_statuses (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO order_statuses VALUES ('pending', 'Pending'), ('paid', 'Paid'), ('shipped', 'Shipped');
CREATE TABLE orders (..., status text NOT NULL REFERENCES order_statuses(code));
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

### Do not pass another table's ID

A named type used with a primary key column, or a column derived from one through a foreign key,
is treated as that table's ID.

Rejected

```go
type UserID int64
type OrderID int64

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

### Do not mix domains of different units

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

### Do not receive with a type that loses information (`-strict`)

```go
type Event struct {
	At  time.Time // field At: timestamp without time zone into time.Time: which zone the value is in becomes the application's implicit choice (prefer timestamptz)
	Day time.Time // field Day: date into time.Time: a zone conversion can move the day (keep it at UTC midnight or use a civil date type)
}
```

`timestamptz` gets no such note. If a `date` is received as `time.Time`, decide in the code that it
is UTC midnight.

## Preparing for a write to fail

INSERT, UPDATE, DELETE and MERGE can fail on a constraint. For each expansion the checker lists the
constraints it can violate and verifies that the template declares them on a
`-- sqlshape: expect` line. At run time the violation comes back as a `ConstraintError` under the
declared name ([runtime.md](runtime.md#errors)).

### Declare the constraints a write can violate

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

### Do not declare a violation that cannot happen

Rejected

```sql
-- sqlshape: expect orders_total_check
--                  ^ expects orders_total_check but no expansion can violate it
UPDATE orders SET note = {{.Note}} WHERE id = {{.ID}}
```

The expect line is kept as the exact list of reasons the statement can fail. Remove declarations
that are no longer needed.

### Constraint names

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

### Name the errors a trigger raises

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

### PL/pgSQL statements that fail on their own

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

### A statement that calls a function declares the function's failure modes too

A call to a user-defined function inherits the failure modes of its body (`LANGUAGE sql` or
`plpgsql`): the constraints its writes can violate, the triggers those writes fire, the functions
it calls, and the SQLSTATEs it raises.

```sql
SELECT place_order({{.CustomerID}}, {{.Note}})
--     ^ may violate orders_customer_id_fkey (FOREIGN KEY (customer_id) on orders REFERENCES customers, SQLSTATE 23503, through place_order()); add `-- sqlshape: expect orders_customer_id_fkey` to the template or make it impossible
```

A NOT NULL the body blames on a parameter is traced to the call's argument. A `STRICT` function is
not called with a NULL, so that argument's violation is dropped.

## Returning one row (`One`)

`sqlshape.One[R, P]` declares that the statement returns at most one row, and the checker proves it
from the schema for every expansion. If it cannot, that is an error. At run time `Get` returns
`ErrNoRows` when there is no row, `Find` reports presence, and both return `ErrManyRows` if the
database contradicts the proof with a second row.

### Fix a unique key by equality

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

### Every branch must be provable

Rejected

```go
var Find = sqlshape.One[User, struct{ ID *int64 }](`
SELECT id, email FROM users WHERE true {{if .ID}} AND id = {{.ID}} {{end}}`)
// One: cannot prove at most one row: users: no unique key is fixed by equality (keys: (id), (email)) [if@39:else]
```

In the branch where `.ID` is nil the condition disappears and every row comes back. Finding that is
the point of the check: make this a `Query`, or make `.ID` a non-pointer and drop the branch.

## Enforcing team rules

Rules that show up in the shape of the SQL can be enforced by the checker: always filter
soft-deleted rows, always pin the tenant column, read tables only through views.

### A predicate every read must carry

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

### Always pin the tenant column (`-require-columns=tenant_id`)

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

### Do not read tables directly (`-no-table-reads` / `-no-tables`)

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
(`table orders is referenced directly; with -no-tables application code reads views and calls functions only`).
`-schemas=a_api,b_private` restricts the schemas a package may reference
(`c_private.orders is outside the schemas this code may reference (a_api,b_private)`).

### Declaring the rules in schema.sql (`require`)

The three rules above are instances of one mechanism: a table declares an obligation, and every
statement that touches it must discharge the obligation with what it provably does. `visible where`
and the flags are shorthands; the general form is a directive above `CREATE TABLE` (or `CREATE VIEW`):

```sql
-- sqlshape: require <what> [on <kinds>]
```

| `<what>` | the statement must | default `on` |
|---|---|---|
| an SQL boolean expression (`deleted_at IS NULL`, `status <> 'closed' AND amount > 0`) | carry it for the table's rows: each conjunct is implied by the WHERE / ON (equalities, IS NULL, IS NOT NULL) or appears verbatim | `read` |
| `pinned(tenant_id)` | fix the column to one value (`= {{.X}}`, a literal, an outer reference) when reading, updating or deleting; assign it when inserting | `all` |
| `immutable(tenant_id)` | not assign the column in an UPDATE | `update` |
| `via view` | not reference the table directly (a write target is allowed unless `on` includes writes) | `read` |

`<kinds>` is a comma-separated list of `select`, `insert`, `update`, `delete`, or the groups `read`
(SELECT, and the target of an UPDATE / DELETE / MERGE, whose WHERE reads it), `write` and `all`.

Optimistic locking is one declaration and no new machinery:

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

An obligation is discharged one of five ways, and `-strict` reports the ones that deserve a look:

1. by the statement's own WHERE / ON / SET;
2. by a view: a view's definition is judged on its own when the schema loads, and readers of the
   view are not judged again for the tables inside it;
3. by a row-level security policy whose USING establishes it, for roles subject to row security
   (`-strict` notes the owner caveat unless the table has `FORCE ROW LEVEL SECURITY`);
4. across a composite foreign key: with `FOREIGN KEY (order_id, tenant_id) REFERENCES orders (id, tenant_id)`,
   a join on `order_id = orders.id` where `orders.tenant_id` is pinned pins `order_items.tenant_id` too;
5. by an opt-out in the statement: `-- sqlshape: unfiltered orders` (the predicate obligations) or
   `-- sqlshape: waive orders pinned(tenant_id)` (one obligation, spelled as declared; `waive orders`
   alone waives every obligation on the table). Opt-outs are reported with `-strict`.

Each occurrence of a table is judged on its own: a self-join or a subquery that reads the table
again owes the obligation again. `RETURNING` lists are not judged.

An aggregate (DDD's consistency unit) is a bundle of these obligations, declared once above its root:

```sql
-- sqlshape: aggregate orders (order_items, order_notes)
CREATE TABLE orders (...);
```

It expands to `require pinned(<foreign key to orders>) on all` on each child (a child is reached
through its root: pin the key, or join on the root's key) and to `alone` on every table of the
aggregate: one statement touches one aggregate. Reading across aggregates is what a view is for;
tables in no aggregate (lookups) are free to join. A child must have a foreign key to the root.

```sql
SELECT o.status, v.id FROM orders o JOIN invoices v ON v.order_id = o.id WHERE o.id = {{.ID}}
-- orders belongs to aggregate orders and this statement also touches invoices of aggregate invoices: one statement, one aggregate (read across aggregates through a view)
```

The flags remain as shorthands: `-require-columns=tenant_id` is `require pinned(tenant_id)` on every
table that has the column, `-no-table-reads` is `require via view` on every table, `-no-tables` is
`require via view on all`.

### Do not run SQL that bypasses sqlshape (`-raw-sql`)

Calling pgx's or `database/sql`'s `Query` / `Exec` with a string built at run time is the hole the
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

## Bulk loading with COPY

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

## Problems in the schema itself

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
