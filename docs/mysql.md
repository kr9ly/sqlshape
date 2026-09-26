# MySQL

[日本語](mysql.ja.md)

Everything about sqlshape that is MySQL's: the version and server settings the schema declares,
the Go types and constraint names the rules use, the runtime on `database/sql`, the migration
commands, and what the checker embeds and how its verdicts are verified. The rules themselves are in
[checks.md](checks.md) and are the same for every database; this page is where the names,
numbers and types in those rules come from when the schema declares `mysql`.

Read it when you start on MySQL, and when you need the Go type a column takes, how to connect,
or the name a failure mode goes by. What a MySQL error number in a diagnostic means is in
[mysql-errors.md](mysql-errors.md).

- [The schema declares the version and the server's settings](#the-schema-declares-the-version-and-the-servers-settings)
- [The Go type table](#the-go-type-table)
- [The runtime: database/sql](#the-runtime-databasesql): [connecting](#connecting), [running statements](#running-statements), [checking the server's settings](#checking-the-servers-settings-at-start-up)
- [Constraint names and failure modes](#constraint-names-and-failure-modes), including [what a trigger or routine raises](#errors-a-trigger-or-routine-raises)
- [What the rules use](#what-the-rules-use): the `One` proof, [grouping](#grouping)
- [Not on MySQL](#not-on-mysql)
- [Migrations](#migrations)
- [Errors the checker predicts](#errors-the-checker-predicts)
- [What the checker embeds](#what-the-checker-embeds)

## The schema declares the version and the server's settings

```sql
-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'
-- sqlshape: server lower_case_table_names = 1
CREATE TABLE ...
```

`schema.sql` names the MySQL version it is written for; 8.4 is the one embedded. MySQL's own
grammar then parses the schema and every statement, and MySQL's rules type the expressions. Two
declarations that disagree, or one that also names `postgres`, are rejected.

The declaration chooses the rules statements are judged by; it does not restrict the server. The
program runs on whatever server go-sql-driver/mysql connects to, another MySQL version, MariaDB
or another MySQL-compatible database included, but the verdicts still follow 8.4's rules, and
where that server behaves differently they are not guaranteed to match it
([README: Limitations](../README.md#limitations)).

A server variable that changes how a statement is judged is declared next to the version, one
per line, so the checker and the production connection agree on it
([checks.md](checks.md#the-schema-names-the-servers-settings-server)). MySQL reads three:

- `sql_mode`: the names in any case, comma-separated, the empty string, and the combination modes
  `ANSI` and `TRADITIONAL`, expanded as the server expands them. The modes that change a judgment:
  - the lexer bits (`ANSI_QUOTES`, `PIPES_AS_CONCAT`, `IGNORE_SPACE`, `NO_BACKSLASH_ESCAPES`,
    `HIGH_NOT_PRECEDENCE`, `REAL_AS_FLOAT`): how the parser reads the text;
  - `ONLY_FULL_GROUP_BY`: the group check on or off;
  - strict mode (`STRICT_TRANS_TABLES` or `STRICT_ALL_TABLES`): whether a string function is
    nullable, and whether a `NULL` into a `NOT NULL` column is a failure mode;
  - `NO_UNSIGNED_SUBTRACTION`: the difference of unsigned operands is signed.

  The rest act at run time only and are accepted as written.
- `lower_case_table_names`: 0 compares table and view names case-sensitively (the Linux
  default), 1 stores them lower-cased, 2 keeps the spelling and compares without case.
  2 is only for a case-insensitive filesystem (macOS / Windows); on a case-sensitive one
  `mysqld` warns and starts at 0 instead, out of step with a schema that declares 2
  (`mysql.Verify` returns that drift).
- `max_sp_recursion_depth`: 0 to 255. At 0 (the default) a PROCEDURE that `CALL`s itself is
  1456 on every invocation; above 0 the depth a call reaches is not static, so the checker
  predicts nothing for it.

Without a declaration the checker assumes a freshly initialized 8.4 server: the default
`sql_mode` (`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`),
`lower_case_table_names = 0` and `max_sp_recursion_depth = 0`.

## The Go type table

What go-sql-driver/mysql delivers with `parseTime=true`, verified against a running server. An
integer arrives as `int64` (`uint64` for `BIGINT UNSIGNED`), a `DECIMAL` as its text, temporal
types as `time.Time` or a string, binary strings and JSON as `[]byte`. A comparison or a logical
operator is a `bigint(1)`, which `bool` may receive; so may a `TINYINT(1)`.

| MySQL | Go |
|---|---|
| `TINYINT` / `SMALLINT` / `MEDIUMINT` / `INT` / `YEAR` | `int64` / `int32` / `int` (`uint64` / `uint32` / `uint` too when `UNSIGNED`); `TINYINT(1)` also `bool` |
| `BIGINT` | `int64` / `int` (`uint64` / `int64` when `UNSIGNED`); a `bigint(1)` also `bool` |
| `DECIMAL` | `string` |
| `FLOAT` | `float32` / `float64` |
| `DOUBLE` | `float64` |
| `BIT` | `[]byte` |
| `CHAR` / `VARCHAR` / `TEXT` / `ENUM` / `SET` | `string` / `[]byte` |
| `BINARY` / `VARBINARY` / `BLOB` | `[]byte` |
| `JSON` | `[]byte` / `string` |
| `DATE` / `DATETIME` / `TIMESTAMP` | `time.Time` / `string` |
| `TIME` | `string` |
| `ENUM` columns, key identities | a Go named type ([Giving types a meaning](checks.md#giving-types-a-meaning)); an `ENUM` is a value set like a `CHECK (col IN (...))` |

There is no `// sqlshape: type` binding on MySQL: it has no named types to bind a Go type to.

### Result types that differ from the column

A few results are not the type they are written over:

- a window function's integer (`MIN(id) OVER ()`, `FIRST_VALUE`, `NTH_VALUE`, `LAG`, `LEAD`
  ...) reaches the client through the window's temporary table, which widens it: `INT` and
  `BIGINT` become `BIGINT`, `TINYINT` / `SMALLINT` / `MEDIUMINT` become `INT`, the signedness
  kept, and `YEAR` becomes `INT UNSIGNED`. A plain aggregate keeps the column's type
  (`MIN(small)` over a `SMALLINT` is a `SMALLINT`), and so does a `ROLLUP` group column
  unless the plan happens to materialize the grouping, which the checker does not predict;
- under `ROLLUP`, the columns that read the grouped columns are nullable (the super-aggregate
  rows hold NULL there); a constant in the select list is not;
- `DATE'...'` / `TIME'...'` / `TIMESTAMP'...'` literals carry their type (the fractional digits
  written) and are never NULL; one the server cannot read as exactly that type (a `DATE` with
  a time part, a `TIMESTAMP` without one, a zero month under `NO_ZERO_IN_DATE`, a
  displacement outside `-14:00` to `+14:00`) is the statement's error 1525; `<=>` is never
  NULL, whatever its operands;
- `USER()`, `CURRENT_USER()`, `DATABASE()`, `SCHEMA()`, `VERSION()` and `CURRENT_ROLE()` are
  character strings (utf8mb3), not binary strings.

## The runtime: database/sql

```
$ go get github.com/kr9ly/sqlshape/v2         # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/mysql/v2   # running them on database/sql with go-sql-driver/mysql
```

`github.com/kr9ly/sqlshape/mysql/v2` runs a declared statement through `database/sql`, with
go-sql-driver/mysql as the driver. `mysql.DB` is `*sql.DB`, `*sql.Tx` or `*sql.Conn`. The
template's `{{.X}}` become MySQL's positional `?` on the wire, the arguments lined up in the
order the placeholders appear (a parameter used twice is sent twice).

### Connecting

The DSN must set `parseTime=true`. The Go type table above assumes it: without it the driver
returns `DATE`, `DATETIME` and `TIMESTAMP` columns as bytes, and a `time.Time` field cannot
receive them.

```go
db, err := sql.Open("mysql", "app:secret@tcp(localhost:3306)/app?parseTime=true")
```

Set `clientFoundRows=true` as well if an `ExecOne` UPDATE that writes the values a row already
has should count that row ([below](#running-statements)).

### Running statements

```go
for o, err := range mysql.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := mysql.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := mysql.First(ctx, db, ListOrders, p)              // ErrNoRows (sql.ErrNoRows) when none
res, err    := mysql.Exec(ctx, db, MarkPaid, p)                 // sql.Result

u, err     := mysql.Get(ctx, db, UserByEmail, p)                // One: ErrNoRows when absent
u, ok, err := mysql.Find(ctx, db, UserByEmail, p)               // One: ok reports presence
res, err   := mysql.ExecOne(ctx, db, MarkOrderPaid, p)          // One: ErrNoRows when no row was touched
```

What every runtime does the same way (row mapping, `One`, the guarantee that only checked SQL
runs, errors under the expect line's names) is in [runtime.md](runtime.md). What is MySQL's:

- The receive types are the driver's, as in the table above; a `DECIMAL` arrives as its text, so
  a money type of your own can wrap it.
- A constraint violation comes back as a `*mysql.ConstraintError` whose `Key` is the schema's
  name for it, as [below](#constraint-names-and-failure-modes); `mysql.Violates(err, key)` tests
  for it. A trigger's or a routine's own SIGNAL comes back the same way, keyed the way
  [below](#errors-a-trigger-or-routine-raises) describes.
- `ExecOne` judges `RowsAffected`, which MySQL counts as changed rows: an `UPDATE` to the values
  a row already has reports `ErrNoRows` unless the DSN sets `clientFoundRows=true`. For an
  `INSERT ... ON DUPLICATE KEY UPDATE` or a `REPLACE` the 0, 1 or 2 rows MySQL reports for the one
  row are all one row.

### Checking the server's settings at start-up

`mysql.Verify(ctx, db, schemaSQL)` asks the connection for its session `@@sql_mode` and the
server's `lower_case_table_names` and returns an error when they differ from what the schema
declares (a freshly initialized 8.4's defaults when it declares none): a DSN's `sql_mode=...`, a
pool's session setup or a server configured otherwise would run the statements under rules the
checker did not judge them by. A mode name 8.4 does not define (another MySQL version or
MariaDB may run with some) is left out of the comparison rather than reported; only a
difference in the modes 8.4 knows is an error, and its message names the modes it left out.
Call it once at start-up, after opening the pool.

```go
if err := mysql.Verify(ctx, db, schemaSQL); err != nil { ... }
```

## Constraint names and failure modes

A failure mode is named as MySQL names the constraint, and numbered as MySQL numbers the error:

| constraint | name | error |
|---|---|---|
| primary key | `PRIMARY` | 1062 |
| `UNIQUE` key | the key's name | 1062 (a key the server numbers itself, or one a NULL leaves alone, cannot be violated; a prefix key `UNIQUE (c(10))` or an expression key `UNIQUE ((n * 2))` is violated through the columns it reads) |
| foreign key | the `CONSTRAINT` name, or `<table>_ibfk_<n>` when it has none | 1452 on the child side, 1451 on the parent side (following `ON DELETE` / `ON UPDATE CASCADE`) |
| `CHECK` | the `CONSTRAINT` name, or `<table>_chk_<n>` | 3819 |
| `NOT NULL` | `<table>.<column>` | 1048 |
| a view's `WITH CHECK OPTION` | the view's own name (MySQL's own message names it, unlike PostgreSQL's unnamed 44000) | 1369 |
| a base column with no default an INSERT through a view leaves unassigned | the view's own name (the server's message names the view, not the column -- whether or not the view exposes it) | 1423 |

What a statement's own form does to the list:

- `INSERT IGNORE` violates nothing;
- `ON DUPLICATE KEY UPDATE` absorbs the insert's key violations;
- `REPLACE` violates no key and may violate a referencing foreign key (1451);
- `UPDATE IGNORE` absorbs a `WITH CHECK OPTION` view's 1369 the same way it absorbs a key
  or `NOT NULL` violation, unlike a trigger's own `SIGNAL`, which no `IGNORE`
  absorbs;
- without strict mode only a single-row `INSERT` or `REPLACE` (its `ON DUPLICATE KEY UPDATE`
  included) rejects a `NULL` for a `NOT NULL` column; more rows, `INSERT ... SELECT` and `UPDATE`
  store the type's implicit default with a warning, so no 1048 is listed for them.

`mysql.Violates(err, key)` tests the run-time error by the same names; `mysql.WrapError(err)`
gives an error from a statement run outside `Run` / `Exec` the same wrapping first.

A literal the column can never store is not a failure mode but the statement's own error
([mysql-errors.md](mysql-errors.md#values-stored-into-a-column)).

Not a failure mode here: a length, range or `ENUM` value truncation (1265 / 1406 / 1366 /
1264). It is a property of the type (a parameter's Go type, a literal's own value set), caught
on that side.

### Errors a trigger or routine raises

A trigger or a stored routine brings its failure modes to the statements that fire or call it.
A statement on a table takes the failure modes of that table's triggers for its event:
`INSERT` / `UPDATE` / `DELETE`; `REPLACE` fires the INSERT and DELETE triggers, `ON DUPLICATE
KEY UPDATE` the INSERT and UPDATE ones. A trigger's or routine's own writes bring their own
failure modes into the body's: the schema's constraints, and what those writes' own triggers
raise in turn (a cycle is cut).

A `SIGNAL` is keyed the way the [runtime](#running-statements) reads the error back:

- a `SIGNAL` that sets `MYSQL_ERRNO` is keyed by that number as decimal text, a builtin's
  number too: `SET MYSQL_ERRNO = 1062` under SQLSTATE `'23000'` is keyed `1062`, not by a key
  name the message never carried;
- a `SIGNAL` that sets no `MYSQL_ERRNO` is keyed by its SQLSTATE;
- SQLSTATE class `01` is a warning and no failure mode; an unhandled class `02` is 1643, and
  anything else unhandled is 1644.

A named `CONDITION` resolves to its value; a bare `RESIGNAL` re-raises whatever the innermost
`HANDLER` is itself handling. A `DECLARE ... HANDLER FOR` absorbs the matching failure modes of
its own block (`SQLEXCEPTION` everything but classes `01` and `02`, `SQLWARNING` / `NOT FOUND`
their class, a SQLSTATE or number itself); `INSERT` / `UPDATE IGNORE` absorbs none of a
trigger's SIGNALs (the statement still fails). `SELECT ... INTO` carries 1172 unless the query
is provably at most one row, the same proof `One` uses (a query with no row is NOT FOUND, a
warning, never a failure).

`-- sqlshape: error <key> = <Name>` above a `CREATE TRIGGER` / `FUNCTION` / `PROCEDURE`, the
same annotation [checks.md](checks.md#name-the-errors-a-trigger-raises) documents for
PostgreSQL, gives `<key>` a Name that a program mirrors with `sqlshape.Error(<key>)`; vet checks
the two against the schema both ways. On the expect line the Name and the key are
interchangeable. At run time `mysql.Violates` compares the code (`30001`), since a
`sqlshape.Failure` from `sqlshape.Error("30001")` carries the code and nothing else.

What the server refuses in a body at `CREATE`, and the numbers a call to a routine can carry,
are in [mysql-errors.md](mysql-errors.md#triggers-routines-and-events).

### Writes through a view WITH CHECK OPTION

A write through a view declared `WITH CHECK OPTION` discharges `require pinned(<col>)` on the
base table the way [checks.md](checks.md#how-a-declaration-works) describes (`sqlshape check`
prints it as `ok(view)`), and the 1369 above is what makes the pin genuine: a view without the
clause never discharges it (a write through it moves the row out of the view's `WHERE`
silently). What is MySQL's about it:

- a plain `WITH CHECK OPTION` is CASCADED, so every underlying view's `WHERE` counts, behind a
  join as well; `WITH LOCAL CHECK OPTION` stops at the view itself except for an underlying view
  that declares a check option of its own, which the server keeps enforcing;
- it holds for `UPDATE`, `INSERT` and `REPLACE` alike; a `DELETE` never checks the option;
- an underlying view's own check option is enforced whatever the view written through says, and
  the 1369 still names the view written through.

## What the rules use

Every rule in [checks.md](checks.md) applies to a MySQL schema the same way, judged by the MySQL
analyzer: result columns and parameters against the Go types, the NULL handling, the meaning of
types, the failure modes, the `One` proof, and every declaration of Part 2 (`visible where`,
`pinned`, `via view`, `EXISTS`, `aggregate`, `transitions`, `never`, `paired`, `single`,
`sensitive`, `context`). Diagnostics carry MySQL's error numbers and message texts
(`Unknown column 'nope' in 'field list' (MySQL error 1054)`). `{{.X}}` becomes a `?` on the wire.

### The `One` proof

Proved from `PRIMARY KEY` and `UNIQUE` keys over whole columns, `LIMIT 1`, and an aggregate
without `GROUP BY` (wherever it sits: `COALESCE(SUM(total), 0)` is one row as much as `SUM(total)`;
an aggregate a subquery owns is the subquery's); MySQL has no partial indexes.

### Grouping

MySQL runs the checks of `sql_mode` `ONLY_FULL_GROUP_BY`, and so does the checker, with the
server's numbers: in a grouped or aggregated query every select-list, `HAVING`, `ORDER BY` and
window `PARTITION BY` / `ORDER BY` expression is a `GROUP BY` expression, an aggregate, or made of
columns functionally dependent on the group columns (1055; 1140 without `GROUP BY`). The
dependencies the server recognizes are the ones the checker recognizes:

- a table's columns once its `PRIMARY` or `UNIQUE` key is known (a nullable key column only
  where a conjunct rejects its NULL);
- `col = col` and `col = literal` in `WHERE` and inner joins;
- an outer join's `ON`, into its nullable side;
- a derived table's or view's body, through its outputs;
- under `ROLLUP`, the group expressions only.

The neighbouring checks come with it:

| rule | error |
|---|---|
| a column named outside an aggregate in `HAVING` must be a select-list column or alias, or a `GROUP BY` column | 1054 |
| with `DISTINCT`, an `ORDER BY` expression not in the select list may read only select-list columns | 3065 |
| an aggregate in the `ORDER BY` of a query that aggregates nowhere else | 3029 |
| an aggregate in the `ORDER BY` of a set operation | 3028 |

```sql
SELECT email, count(*) FROM users GROUP BY name
-- Expression #1 of SELECT list is not in GROUP BY clause and contains nonaggregated column
-- 'users.email' which is not functionally dependent on columns in GROUP BY clause; this is
-- incompatible with sql_mode=only_full_group_by (MySQL error 1055)

SELECT name, count(*) FROM users GROUP BY id            -- OK: id is the primary key
```

### Two writes in one statement

Two statement forms are two writes, not one, when the schema's rules (`require`, `visible
where`) are judged:

| statement | writes recorded | why |
|---|---|---|
| `INSERT ... ON DUPLICATE KEY UPDATE` | an INSERT, and an UPDATE of the columns the branch assigns | the branch moves an existing row |
| `REPLACE` | an INSERT and a DELETE | a colliding key deletes the old row first (its `AFTER DELETE` trigger fires) |

## Not on MySQL

What does not exist on MySQL is not checked there: `Batch`, `Copy` and `MatView`, PL/pgSQL,
domains, composite types and arrays, `-schemas` (a MySQL schema is one database),
`// sqlshape: type`, and seeded tables in migrations.

## Migrations

`sqlshape diff`, `apply` and `verify-schema` work on a MySQL schema the way they do on
PostgreSQL: the difference between the database and `schema.sql` becomes DDL, the DDL is checked
by its end state, then run. Both sides are read as the server's own `SHOW CREATE TABLE` /
`SHOW CREATE VIEW`, the target's in a scratch database on the `-db` server, and the DDL runs
statement by statement since MySQL's DDL commits implicitly. What is compared, the `enum`
declaration's form and the requirements are in [migrations.md](migrations.md#mysql).

## Errors the checker predicts

A diagnostic that carries a MySQL error number is one of two kinds. An error is certain: every
execution of the statement fails the same way, vet reports it, and the statement has to change.
A failure mode may happen at run time, for some rows or some values: it goes on the
statement's expect line, and `mysql.Violates` matches it when it comes back. The constraint
names above are failure modes. [mysql-errors.md](mysql-errors.md) lists, with an index by error
number, the rest of what the checker predicts from a statement's own form: values stored into columns,
constant expressions, conversions and function arguments, spatial values, trigger and routine
bodies, name resolution, views, and statements without a result set.

## What the checker embeds

The parser and lexer are MySQL 8.4's own, extracted from the server source (the grammar with its
actions stripped, the lexer as it is) and built to WebAssembly; the function catalog is read from
the same source. The analyzer is a pure-Go implementation over that tree; checking never connects
to a MySQL. Because it carries MySQL's parser, the MySQL side of the checker (`check/mysql`) is
under the GNU General Public License v2; it is linked into the `sqlshape` binary only, never into
your program.

The verdicts are checked against a running `mysqld`: 5,033 typed statements (the result types of
every built-in function in every argument combination), 65 error statements, the 376 statements
of the `ONLY_FULL_GROUP_BY` check, and 22 statements and 6 writes under non-default `sql_mode`
values agree with 8.4. Every server behavior this page and [mysql-errors.md](mysql-errors.md)
describe was measured against `mysqld` 8.4.

MySQL's own test corpus is replayed as well: the 1,281 files of `mysql-test/t` (some 137,000
statements) run against a `mysqld` and the analyzer side by side, a SELECT's columns compared by
name, type family and nullability, an error by its number. The statements on which the two
knowingly disagree -- 2,525 -- are pinned one by one
(`check/mysql/internal/analyze/testdata/corpus_baseline.txt`, each with the class of the
disagreement: an error the analyzer does not predict or predicts under another number, a column
typed differently, a schema construct the loader does not model), and a new disagreement fails
the build. That file is the honest list of what the MySQL analyzer does not yet decide the way
the server does.

## License

The MySQL side of the checker (`check/mysql`) carries MySQL's own parser and is under the GNU
General Public License v2, see [cmd/sqlshape/LICENSE](../cmd/sqlshape/LICENSE); it is linked into
the `sqlshape` binary, a development tool, and nothing under it is linked into your program. The
runtime (`mysql`) is Apache License 2.0, like the declarations.
