# MySQL

[日本語](mysql.ja.md)

Everything about sqlshape that is MySQL's: the version and server settings the schema declares,
what the checker embeds and how its verdicts are verified, the types and constraint names the
rules use, the runtime on `database/sql`, and the migration commands. The rules themselves are in
[checks.md](checks.md) and are the same for every database; this page is where the names,
numbers and types in those rules come from when the schema declares `mysql`.

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

A server variable that changes how a statement is judged is declared next to the version, one
per line, so the checker and the production connection agree on it
([checks.md](checks.md#the-schema-names-the-servers-settings-server)). MySQL reads two:

- `sql_mode`: the names in any case, comma-separated, the empty string, and the combination modes
  `ANSI` and `TRADITIONAL`, expanded as the server expands them. The parser takes the lexer bits
  (`ANSI_QUOTES`, `PIPES_AS_CONCAT`, `IGNORE_SPACE`, `NO_BACKSLASH_ESCAPES`, `HIGH_NOT_PRECEDENCE`,
  `REAL_AS_FLOAT`); `ONLY_FULL_GROUP_BY` turns the group check on and off; strict mode
  (`STRICT_TRANS_TABLES` or `STRICT_ALL_TABLES`) decides whether a string function is nullable
  and whether a `NULL` into a `NOT NULL` column is a failure mode; `NO_UNSIGNED_SUBTRACTION`
  signs a difference. The rest act at run time only and are accepted as written.
- `lower_case_table_names`: 0 compares table and view names case-sensitively (the Linux
  default), 1 stores them lower-cased, 2 keeps the spelling and compares without case.

Without a declaration the checker assumes a freshly initialized 8.4 server: the default
`sql_mode` (`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`)
and `lower_case_table_names = 0`.

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
values agree with 8.4.

## What the rules use

Every rule in [checks.md](checks.md) applies to a MySQL schema the same way, judged by the MySQL
analyzer: result columns and parameters against the Go types, the NULL handling, the meaning of
types, the failure modes, the `One` proof, and every declaration of Part 2 (`visible where`,
`pinned`, `via view`, `EXISTS`, `aggregate`, `transitions`, `never`, `paired`, `single`,
`sensitive`, `context`). Diagnostics carry MySQL's error numbers and message texts
(`Unknown column 'nope' in 'field list' (MySQL error 1054)`). `{{.X}}` becomes a `?` on the wire.

### The Go type table

What go-sql-driver/mysql delivers with `parseTime=true`, verified against a running server. An
integer arrives as `int64` (`uint64` for `BIGINT UNSIGNED`), a `DECIMAL` as its text, temporal
types as `time.Time` or a string, binary strings and JSON as `[]byte`. A comparison or a logical
operator is a `bigint(1)`, which `bool` may receive; so may a `TINYINT(1)`.

| MySQL | Go |
|---|---|
| `TINYINT` / `SMALLINT` / `MEDIUMINT` / `INT` / `YEAR` | `int64` / `int32` / `int` (`uint64` / `uint32` / `uint` too when `UNSIGNED`); `TINYINT(1)` also `bool` |
| `BIGINT` | `int64` / `int` (`uint64` / `int64` when `UNSIGNED`); a `bigint(1)` also `bool` |
| `DECIMAL` | `string` |
| `FLOAT` / `DOUBLE` | `float32` / `float64` / `float64` |
| `BIT` | `[]byte` |
| `CHAR` / `VARCHAR` / `TEXT` / `ENUM` / `SET` | `string` / `[]byte` |
| `BINARY` / `VARBINARY` / `BLOB` | `[]byte` |
| `JSON` | `[]byte` / `string` |
| `DATE` / `DATETIME` / `TIMESTAMP` | `time.Time` / `string` |
| `TIME` | `string` |
| `ENUM` columns, key identities | a Go named type ([Giving types a meaning](checks.md#giving-types-a-meaning)); an `ENUM` is a value set like a `CHECK (col IN (...))` |

There is no `// sqlshape: type` binding on MySQL: it has no named types to bind a Go type to.

### Constraint names and failure modes

A failure mode is named as MySQL names the constraint: `PRIMARY` for the primary key, the key's
name for a `UNIQUE` key, the `CONSTRAINT` name for a foreign key or `<table>_ibfk_<n>` when it
has none, the `CONSTRAINT` name for a `CHECK` or `<table>_chk_<n>`, `<table>.<column>` for
`NOT NULL`. The error numbers are MySQL's: 1062 for a key (a key the server numbers itself, or
one a NULL leaves alone, cannot be violated), 1452 and 1451 for a foreign key (the parent side
following `ON DELETE` / `ON UPDATE CASCADE`), 1048 for `NOT NULL`, 3819 for `CHECK`. `INSERT
IGNORE` violates nothing; `ON DUPLICATE KEY UPDATE` absorbs the insert's key violations; `REPLACE`
violates no key and may violate a referencing foreign key (1451). Without strict mode only a
single-row `INSERT` or `REPLACE` (its `ON DUPLICATE KEY UPDATE` included) rejects a `NULL` for a
`NOT NULL` column; more rows, `INSERT ... SELECT` and `UPDATE` store the type's implicit default
with a warning, so no 1048 is listed for them. `mysql.Violates(err, key)` tests the run-time
error by the same names.

### Triggers and stored routines

The loader reads `CREATE TRIGGER` / `CREATE PROCEDURE` / `CREATE FUNCTION` (`DEFINER`,
`IF NOT EXISTS`, the characteristics) and `DROP` / `ALTER` (characteristics only) the way it
reads a table: no `DELIMITER` is needed, a body's own `;` is read as part of the one compound
statement it belongs to, and a mysql-client `DELIMITER x` line is read too. What the server
itself refuses at CREATE time is a problem the same way an unknown table is: no such table
(1146), the trigger or routine already exists (1359 / 1304), no such trigger or routine to
`DROP` (1360 / 1305), no such trigger for `FOLLOWS` / `PRECEDES` to name (3011). `DROP TABLE`
takes a table's triggers with it; `RENAME TABLE` moves them.

The body is read once per schema, the way a PL/pgSQL function's is on PostgreSQL
([checks.md](checks.md#name-the-errors-a-trigger-raises)):
`NEW.col` / `OLD.col` type as the trigger's own table's columns (an unknown column is 1054;
`OLD` in an INSERT trigger or `NEW` in a DELETE trigger is 1363; writing `OLD`, or writing
`NEW` outside a BEFORE trigger, is 1362 -- a NOT NULL column's `NEW.col` can still be NULL in
a BEFORE trigger, measured); a `DECLARE`d variable or a routine's own parameter shadows a
column of the same name, as on the server. `IF` / `CASE` / `LOOP` / `WHILE` / `REPEAT`,
labelled blocks with `LEAVE` / `ITERATE`, `RETURN`, `SET`, `SELECT ... INTO`, cursors
(`DECLARE` / `OPEN` / `FETCH` / `CLOSE`), `CALL`, `SIGNAL` / `RESIGNAL` and
`DECLARE ... HANDLER FOR` are walked. What the server itself refuses when the body is
created: `LEAVE` / `ITERATE` with no matching label (1308), `RETURN` outside a FUNCTION
(1313), a FUNCTION with no `RETURN` (1320), an undeclared cursor or variable or a `FETCH`
column-count mismatch (1324 / 1327 / 1328), a mismatched `SELECT ... INTO` column count
(1222), a trigger or function that returns a result set (1415), a `COMMIT` / `START
TRANSACTION` / DDL statement inside one (1422). A trigger that writes its own table is 1442
on every one of the 18 timing x event x write combinations (measured): always a failure,
reported on the trigger's own definition rather than on a statement that fires it.

A trigger's or routine's own writes bring their own failure modes into the body's: the
schema's constraints, and what those writes' own triggers raise in turn (a cycle is cut). A
`SIGNAL`'s key is its `MYSQL_ERRNO` as decimal text when it sets one, else its SQLSTATE (the
same rule the [runtime](#the-runtime-databasesql) reads an error back by); SQLSTATE class
`01` is a warning and no failure mode, an unhandled class `02` is 1643, anything else
unhandled is 1644. A named `CONDITION` resolves to its value; a bare `RESIGNAL` re-raises
whatever the innermost `HANDLER` is itself handling. `-- sqlshape: error <key> = <Name>`
above a `CREATE TRIGGER` / `FUNCTION` / `PROCEDURE`, the same annotation
[checks.md](checks.md#name-the-errors-a-trigger-raises) documents for PostgreSQL, gives
`<key>` a Name a program mirrors with `sqlshape.Error(<key>)` and vet checks against the
schema both ways; an expect line and `mysql.Violates` still judge by the key itself
(`30001`, not `<Name>`) -- they may just as well spell it as `<Name>`, since a
`sqlshape.Failure` from `sqlshape.Error("30001")` carries the code and nothing else. A
`DECLARE ... HANDLER FOR` absorbs the matching failure modes of its own block (`SQLEXCEPTION`
everything but classes `01` and `02`, `SQLWARNING` / `NOT FOUND` their class, a SQLSTATE or
number itself); `INSERT` / `UPDATE IGNORE` absorbs none of a trigger's SIGNALs (measured: the
statement still fails). `SELECT ... INTO` carries 1172 unless the query is provably at most
one row, the same proof `One` uses (a query with no row is NOT FOUND, a warning, never a
failure).

A statement on a table takes the failure modes of that table's triggers for its event:
`INSERT` / `UPDATE` / `DELETE`; `REPLACE` fires the INSERT and DELETE triggers, `ON DUPLICATE
KEY UPDATE` the INSERT and UPDATE ones (measured).

A call to a FUNCTION the catalog does not know resolves to the schema's own (an unqualified
name that is also a native function resolves to the native one, as on the server; `db.f`
names the routine): a wrong argument count is 1318, no such routine 1305, the result types as
the `RETURNS` declaration and is always nullable (a stored function's `RETURN` can produce
NULL regardless of the declared type; there is no static proof otherwise), and the body's own
failure modes (its SIGNALs, its writes' violations, what they fire) reach the calling
statement. A function that writes a table the calling statement itself reads or writes is
1442 on every execution (reading the table is enough, measured). `-- sqlshape: not null`
above a `CREATE FUNCTION` declares the function never returns NULL, the same directive
[checks.md](checks.md#a-column-that-may-be-null-needs-a-field-that-can-hold-null) documents
for PostgreSQL; a call then types as NOT NULL instead. A PROCEDURE or a TRIGGER still refuses
the directive: neither returns a value for it to describe.

`CALL p(...)` types an `IN` / `INOUT` argument by its parameter and requires an `OUT` /
`INOUT` argument to be a variable (1414; a `?` counts as one); its own facts are `Kind Call`.
Its result columns come from the body's own INTO-less `SELECT`s: none is no columns, one is
those columns, several of the same shape agree on one list, several of different shapes is
the checker's own error (not something mysqld itself refuses -- it only ever returns
whichever result set the execution path taken produced, at run time). Its failure modes are
the body's own.

### The `One` proof

Proved from `PRIMARY KEY` and `UNIQUE` keys over whole columns, `LIMIT 1`, and an aggregate
without `GROUP BY`; MySQL has no partial indexes.

### Grouping

MySQL runs the checks of `sql_mode` `ONLY_FULL_GROUP_BY`, and so does the checker, with the
server's numbers: in a grouped or aggregated query every select-list, `HAVING`, `ORDER BY` and
window `PARTITION BY` / `ORDER BY` expression is a `GROUP BY` expression, an aggregate, or made of
columns functionally dependent on the group columns (1055; 1140 without `GROUP BY`). The
dependencies the server recognizes are the ones the checker recognizes: a table's columns once
its `PRIMARY` or `UNIQUE` key is known (a nullable key column only where a conjunct rejects its
NULL), `col = col` and `col = literal` in `WHERE` and inner joins, an outer join's `ON` into its
nullable side, and a derived table's or view's body through its outputs; `ROLLUP` allows the group
expressions only. A column named outside an aggregate in `HAVING` must be a select-list column or
alias or a `GROUP BY` column (1054); with `DISTINCT` an `ORDER BY` expression not in the select
list may read only select-list columns (3065); an aggregate in the `ORDER BY` of a query that
aggregates nowhere else (3029) or of a set operation (3028) is rejected.

```sql
SELECT email, count(*) FROM users GROUP BY name
-- Expression #1 of SELECT list is not in GROUP BY clause and contains nonaggregated column
-- 'users.email' which is not functionally dependent on columns in GROUP BY clause; this is
-- incompatible with sql_mode=only_full_group_by (MySQL error 1055)

SELECT name, count(*) FROM users GROUP BY id            -- OK: id is the primary key
```

### Name resolution

`ORDER BY`, `GROUP BY` and `HAVING` see the select list's aliases as the server does (a table
column of the same name wins in `GROUP BY`); a derived table needs an alias (1248); `QUALIFY` is
rejected as 8.4 rejects it without the hypergraph optimizer (6037). A `USING` or `NATURAL` join
coalesces its common columns (an unqualified name resolves to the left side, `SELECT *` lists it
once). Table and view names compare as `lower_case_table_names` says; column and key names never
mind case.

### Not on MySQL

What does not exist on MySQL is not checked there: `Copy` and `MatView`, PL/pgSQL, domains,
composite types and arrays, `-schemas` (a MySQL schema is one database), `// sqlshape: type`, and
seeded tables in migrations.

## The runtime: database/sql

```
$ go get github.com/kr9ly/sqlshape/v2         # Query / One: the declarations the checker reads
$ go get github.com/kr9ly/sqlshape/mysql/v2   # running them on database/sql with go-sql-driver/mysql
```

`github.com/kr9ly/sqlshape/mysql/v2` runs a declared statement through `database/sql`, with
go-sql-driver/mysql as the driver. `mysql.DB` is `*sql.DB`, `*sql.Tx` or `*sql.Conn`. The
template's `{{.X}}` become MySQL's positional `?` on the wire, the arguments lined up in the
order the placeholders appear (a parameter used twice is sent twice).

```go
for o, err := range mysql.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error], streamed
orders, err := mysql.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := mysql.First(ctx, db, ListOrders, p)              // ErrNoRows (sql.ErrNoRows) when none
res, err    := mysql.Exec(ctx, db, MarkPaid, p)                 // sql.Result

u, err     := mysql.Get(ctx, db, UserByEmail, p)                // One: ErrNoRows when absent
u, ok, err := mysql.Find(ctx, db, UserByEmail, p)               // One: ok reports presence
res, err   := mysql.ExecOne(ctx, db, MarkPaid, p)               // One: ErrNoRows when no row was touched
```

What every runtime does the same way (row mapping, `One`, the guarantee that only checked SQL
runs, errors under the expect line's names) is in [runtime.md](runtime.md). What is MySQL's:

- The receive types are the driver's, as in the table above; a `DECIMAL` arrives as its text, so
  a money type of your own can wrap it.
- A constraint violation comes back as a `*mysql.ConstraintError` whose `Key` is the schema's
  name for it, as [above](#constraint-names-and-failure-modes); `mysql.Violates(err, key)` tests
  for it. A trigger's or a routine's own SIGNAL comes back the same way, keyed the way
  [above](#triggers-and-stored-routines) describes.
- `ExecOne` judges `RowsAffected`, which MySQL counts as changed rows: an `UPDATE` to the values
  a row already has reports `ErrNoRows` unless the DSN sets `clientFoundRows=true`. For an
  `INSERT ... ON DUPLICATE KEY UPDATE` or a `REPLACE` the 0, 1 or 2 rows MySQL reports for the one
  row are all one row.
- There is no `Batch`, `Copy` or `MatView`.
- `mysql.Verify(ctx, db, schemaSQL)` asks the connection for its session `@@sql_mode` and the
  server's `lower_case_table_names` and returns an error when they differ from what the schema
  declares (the server's defaults when it declares none): a DSN's `sql_mode=...`, a pool's session
  setup or a server configured otherwise would run the statements under rules the checker did
  not judge them by. Call it once at start-up, after opening the pool.

  ```go
  if err := mysql.Verify(ctx, db, schemaSQL); err != nil { ... }
  ```

## Migrations

`sqlshape diff`, `apply` and `verify-schema` work on a MySQL schema the way they do on
PostgreSQL: the difference between the database and `schema.sql` becomes DDL, the DDL is checked
by its end state, then run. Both sides are read as the server's own `SHOW CREATE TABLE` /
`SHOW CREATE VIEW`, the target's in a scratch database on the `-db` server, and the DDL runs
statement by statement since MySQL's DDL commits implicitly. What is compared, the `enum`
declaration's form and the requirements are in [migrations.md](migrations.md#mysql).

## License

The MySQL side of the checker (`check/mysql`) carries MySQL's own parser and is under the GNU
General Public License v2, see [cmd/sqlshape/LICENSE](../cmd/sqlshape/LICENSE); it is linked into
the `sqlshape` binary, a development tool, and nothing under it is linked into your program. The
runtime (`mysql`) is Apache License 2.0, like the declarations.
