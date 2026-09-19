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

MySQL's own test corpus is replayed as well: the 1,281 files of `mysql-test/t` (some 137,000
statements) run against a `mysqld` and the analyzer side by side, a SELECT's columns compared by
name, type family and nullability, an error by its number. The statements on which the two
knowingly disagree -- 2,534 -- are pinned one by one
(`check/mysql/internal/analyze/testdata/corpus_baseline.txt`, each with the class of the
disagreement: an error the analyzer does not predict or predicts under another number, a column
typed differently, a schema construct the loader does not model), and a new disagreement fails
the build. That file is the honest list of what the MySQL analyzer does not yet decide the way
the server does.

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

A few results are not the type they are written over (all measured):

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

### Constraint names and failure modes

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
  or `NOT NULL` violation (measured), unlike a trigger's own `SIGNAL`, which no `IGNORE`
  absorbs;
- without strict mode only a single-row `INSERT` or `REPLACE` (its `ON DUPLICATE KEY UPDATE`
  included) rejects a `NULL` for a `NOT NULL` column; more rows, `INSERT ... SELECT` and `UPDATE`
  store the type's implicit default with a warning, so no 1048 is listed for them.

`mysql.Violates(err, key)` tests the run-time error by the same names; `mysql.WrapError(err)`
gives an error from a statement run outside `Run` / `Exec` the same wrapping first.

A literal the column can never store is not a failure mode but the statement's own error,
since every execution fails the same way in strict mode (the default). The checker applies the
server's own `Field::store` rules to a literal written straight into a column by `INSERT ...
VALUES`, `UPDATE ... SET`, `ON DUPLICATE KEY UPDATE` or `REPLACE` (all measured):

| into | rejected | error |
|---|---|---|
| an integer column | a value outside its range, a real or decimal included once rounded | 1264 |
| | a string holding no number (`'abc'`, `''`) | 1366 |
| | a number followed by other text (`'12a'`) | 1265 |
| `YEAR` | anything but 0, 1-99 and 1901-2155 | 1264 |
| `DECIMAL(M,D)` | more than M-D integer digits once rounded to D places; a negative value into `UNSIGNED` | 1264 |
| `FLOAT` | a value beyond the single-precision range | 1264 |
| `DATE` / `DATETIME` / `TIMESTAMP` | a string `str_to_datetime` cannot read or finds out of range, a zero month, day or date the `sql_mode` forbids (`NO_ZERO_IN_DATE`, `NO_ZERO_DATE`), a day the month lacks unless `ALLOW_INVALID_DATES`; a number `number_to_datetime` rejects; a `TIMESTAMP` outside 1970-01-01 00:00:01 to 2038-01-19 03:14:07 UTC (judged only where the session time zone cannot change the verdict) | 1292 |
| `TIME` | a string `str_to_time` cannot read, minutes or seconds of 60 or more, more than 838 hours | 1292 |
| `CHAR(n)` / `VARCHAR(n)` / `BINARY(n)` / `VARBINARY(n)` | a string longer than n characters (bytes when binary) once trailing spaces are dropped | 1406 |
| `ENUM` | a string that names no member (compared as the collation does) and is not a member's index; a number outside 1 to the member count | 1265 |
| `SET` | a list naming a member the set lacks | 1265 |
| any spatial type | a number or a string: neither can hold a well-formed geometry | 1416 |

Not read: `INSERT IGNORE` / `UPDATE IGNORE` and a schema whose `sql_mode` is not strict (the
value is stored adjusted, with a warning; under `STRICT_TRANS_TABLES` alone a nontransactional
table is strict for the first row only), a value inside a routine or trigger body, hex and bit
literals, `JSON` and `BIT` columns, and an expression the server would fold (`100 + 28`).

### Spatial types

A geometry value has one of seven types (`POINT` ... `GEOMETRYCOLLECTION`); a column is declared
with one of them or with `GEOMETRY` (any). The checker knows a value's type when a constructor
(`POINT(1, 1)`), a typed reader (`ST_PointFromText`) or a constant text or WKB fixes it, and
judges with it (all measured):

| statement | error |
|---|---|
| `ST_GeomFromText` (and the typed variants) over a constant the server's WKT reader refuses: malformed text, a `LINESTRING` of one point, a polygon ring of fewer than four points or not closed, a `MULTIPOINT` mixing `(x y)` and `x y` or empty; `ST_GeomFromWKB` over a constant WKB it refuses (malformed, trailing bytes); a geometry value given to `ST_GeomFromWKB` | 3037 |
| a typed reader over a constant of another type (`ST_PointFromText('LINESTRING(...)')`; the `GEOMCOLL` variants take the `MULTI*` types) | 3516 |
| a constant SRID outside 0 to 4294967295 | 1690 |
| `LINESTRING(...)` of one argument; a `POLYGON(...)` ring made of `POINT` constants with fewer than four points or not closed | 3037 |
| `LINESTRING` / `POLYGON` / `MULTI*` given an argument known to be another geometry type; an arithmetic or bit operator, a numeric function or `BETWEEN` given a geometry (comparisons are allowed) | 1210 |
| a value stored into a spatial column that is not the internal format of the column's type: a number or a character string, a hex / `UNHEX` constant that is not a 4-byte SRID followed by a well-formed little-endian WKB, a constant of another geometry type | 1416, whatever the `sql_mode`, `IGNORE` included |

A nullable expression of another geometry type stored into a typed spatial column (a `LINESTRING`
column into a `POINT` column) fails on every non-NULL value: it is the failure mode `1416`, which
`mysql.Violates(err, "1416")` matches. Not read: the SRID a column declares (3643), whether a
constant SRID names a spatial reference system (3548), the functions' own run-time checks
(`ST_Centroid` over a degenerate ring), GeoJSON.

### Constant arithmetic

A constant expression the server cannot compute is the statement's own 1690, judged before any
row is read (all measured, as item_func.cc computes):

| expression | error |
|---|---|
| an integer `+` `-` `*` `DIV` whose exact result does not fit `BIGINT`, or `BIGINT UNSIGNED` when an operand is unsigned (`9223372036854775807 * 2`, `CAST(1 AS UNSIGNED) - 2`, `-9223372036854775808 DIV -1`; a string operand makes the operator a `DOUBLE` one, a decimal operand a `DECIMAL` one, neither judged); `ABS(-9223372036854775808)`; `ROUND(n, -k)` over an integer past its range | 1690 `BIGINT [UNSIGNED] value is out of range` |
| a `DOUBLE` result that is infinite: `1e308 + 1e308`, `1e300 / 1e-300`, `EXP(710)`, `POW(2, 1024)`, `COT(0)`, `DEGREES(1e307)`; `CAST(x AS FLOAT)` beyond `FLT_MAX` | 1690 `DOUBLE value is out of range` |
| `CAST(f AS SIGNED / UNSIGNED)` of a `DOUBLE` a function or operator computed, outside the `BIGINT` range (`CAST(POW(2, 63) AS SIGNED)`; a literal is clamped instead) | 1690, naming the inner expression |
| `RANDOM_BYTES(n)` with a constant n outside 1 to 1024 | 1690 `length value is out of range` |

Where the constant sits decides whether the statement fails or a row does. In a `WHERE`,
`HAVING` or `ON` the optimizer folds it before reading rows (`id = 9223372036854775807 + 1`
against a keyed column, `IN (...)`, `LIKE`, a term of the condition): the statement fails. So
does a select item of a query without a `FROM`, a derived table's or CTE's, an `INSERT ...
VALUES` value. A select item of a query with a `FROM`, an `UPDATE`'s `SET`, an `ON DUPLICATE
KEY UPDATE` assignment, a comparison against an unindexed column or against an aggregate in
`HAVING` run per row: the statement carries the failure mode `1690`, which
`mysql.Violates(err, "1690")` matches, and a query over no rows does not fail. What the server never evaluates is left alone: the constants after the
one that decides an `AND` / `OR` (`1 = 0 AND x`), the branch an `IF` / `CASE` / `COALESCE` /
`IFNULL` with a constant condition does not take, the list after the entry a constant `IN`
matches, `x IS NULL` over a never-NULL x, a `GROUP BY` / `ORDER BY` item, an `EXISTS`
subquery's select list, `LIMIT 0`. Not read: a trigger's or routine's body, a hex or bit
literal operand, a user variable, a window's `ORDER BY`, `DECIMAL` overflow (65 digits).

The same two shapes are two writes for the obligation checker (x/obligation), not one:

| statement | writes recorded | why |
|---|---|---|
| `INSERT ... ON DUPLICATE KEY UPDATE` | an INSERT, and an UPDATE of the columns the branch assigns | the branch moves an existing row |
| `REPLACE` | an INSERT and a DELETE | a colliding key deletes the old row first (its `AFTER DELETE` trigger fires, measured) |

A write through a view declared `WITH CHECK OPTION` discharges `require pinned(<col>)` on the
base table (`Discharge.Path` `ByView`):

- when the view's own `WHERE` fixes the column by equality; a plain `WITH CHECK OPTION` is
  CASCADED, so every underlying view's `WHERE` counts too (behind a join as well, measured);
  `WITH LOCAL CHECK OPTION` stops at the view itself except for an underlying view that
  declares a check option of its own, which the server keeps enforcing (measured);
- the 1369 above is what makes the pin genuine: a view without the clause never discharges it
  (a write through it moves the row out of the view's `WHERE` silently);
- for `UPDATE`, `INSERT` and `REPLACE` alike (a `DELETE` never checks the option, measured);
  an underlying view's own check option is enforced whatever the view written through says,
  and the 1369 still names the latter (measured).

Not a failure mode here: a length, range or `ENUM` value truncation (1265 / 1406 / 1366 /
1264). It is a property of the type (a parameter's Go type, a literal's own value set), caught
on that side.

### Constant conversions

A strict `INSERT` / `REPLACE` / `UPDATE` / `DELETE` (outside `IGNORE`) escalates a constant
conversion's warning to the error 1292 `ER_TRUNCATED_WRONG_VALUE`; a `SELECT`, a `SET`, a `DO`
or a non-strict write runs the same expression with a warning, rows present or not (all
measured against mysqld):

| expression | fails when |
|---|---|
| `CAST` / `CONVERT` of a constant to `DATE` / `DATETIME` | the value is no datetime under the session's zero-date flags (`CAST('2004-10-0' AS DATE)`, `CAST(65 AS DATETIME)`) |
| ... to `TIME` | `str_to_time` rejects it or it passes 838 hours |
| ... to `YEAR` | the leading integer of the string is followed by anything (`'2020extra'`, `'2020.5'`), or the value is outside 0-99 and 1901-2155; a string with no digits at all is 0 without a warning |
| ... to `CHAR(n)` | the value's string form is longer than n (`CAST(1000 AS CHAR(3))`) |
| ... to `SIGNED` / `UNSIGNED` | the string is not a whole integer (`'abc'`, `'1.5'`, `'1e2'`) or its magnitude passes `BIGINT UNSIGNED`; a decimal outside the range |
| ... to `DOUBLE` / `FLOAT` / `DECIMAL` | the string holds no number, has a tail, or overflows the double parse (`'1e999'`) |
| `TIMESTAMP(x)` of a constant | x is no datetime (a time-only string, `TIMESTAMP('0000-00-00 10:00:00')` where the mode forbids it) |
| a string operand of `+` `-` `*` `/` | it holds no full number (`10E+0 + 'a'`; `''` is 0 silently) |
| a string operand of a bit operator | it is not a whole integer (`1 >> ''`) |
| a numeric column compared with a constant string | the string holds no number (`WHERE i = '1invalid'`; `''`, `' 1'`, `'1e1'`, `'1.5'` all pass); a `BETWEEN` runs per row instead |

The failure lands where the 1690 above lands — an `INSERT ... VALUES` value or a bare
condition term is the statement's error, an `UPDATE`'s `SET` or a select item over a `FROM`
the failure mode `1292` (`mysql.Violates(err, "1292")`) — except that a comparison's constant
operand always runs per row, so `WHERE d = CAST('2004-10-0' AS DATE)` is the violation, not
the error.

Two conversions fail whatever the statement and the mode:

- a `DATE` / `DATETIME` / `TIMESTAMP` value compared with a constant string that is no
  datetime is the statement's 1525 `Incorrect DATETIME value` wherever the comparison sits —
  a select item, a `JOIN`'s `ON`, a `HAVING`, an `ORDER BY` / `GROUP BY` item, a dead branch —
  and in a strict write the store-shaped 1292 instead (`BETWEEN` and `IN` do not convert
  eagerly; a `TIME` or `YEAR` column is not this rule);
- a datetime string or literal carrying a time zone displacement over a zero month or day
  fails the displacement conversion: 1292 `Truncated incorrect temporal value`.

A `DATE'...'` / `TIMESTAMP'...'` literal stored into a `TIMESTAMP` column must fit 1970-01-01
00:00:01 to 2038-01-19 03:14:07.999999 UTC — exact when the literal writes a displacement (the
range is defined in UTC), otherwise judged only where the session time zone cannot move the
verdict; the fractional seconds round to the column's precision first, and the carry counts
(`TIMESTAMP'1970-01-01 00:00:00.999999+00:00'` into a `TIMESTAMP(0)` is 00:00:01, stored). A
`TIMESTAMP` column ignores `ALLOW_INVALID_DATES` and range-checks a year 0 over a real month
and day, where a `DATETIME` accepts both.

Not read: a function's result stored (`STR_TO_DATE(...)`), a temporal cast's value carried
into the column store, an `UPDATE`'s `ORDER BY` constant, a scalar subquery's constant
compared with a temporal column, a column `DEFAULT` the current mode cannot store, a `TIME`
column's own per-row string conversion.

### Constant function arguments

A function that decides a run-time failure by an argument's value is judged when the
argument is a constant, where the constant-arithmetic rules above place the failure (a
folded term fails the statement, a per-row term is the violation keyed by its own number,
and a query over no rows does not fail):

| function | fails when | error |
|---|---|---|
| `INET_ATON` | the value is not digit groups of at most 255 separated by up to three dots (`'122.256'`, a trailing dot; `'1.2.3'` runs) | 1411, in a strict write outside `IGNORE` |
| `INET6_ATON` | the value is neither a full dotted IPv4 (four groups, no leading `0x`) nor a valid IPv6 text (one `::` gap at most, four hex digits per group) | 1411, the same gate |
| `UNHEX` | the value holds a non-hex character | 1411, the same gate |
| `STR_TO_DATE` | the value does not parse under the format (`extract_date_time`'s specifiers, the en_US month and day names, `%V`/`%v` weeks with their `%X`/`%x` years), or the date fails the session's zero-date flags (under `NO_ZERO_DATE` any zero year, month or day of a date result) | 1411; a parsed value with a non-space tail is 1292, spelt with the format's own result type |
| `UUID_TO_BIN` | the value is not 32 hex digits, the dashed 8-4-4-4-12 form, or that form in braces | 1411, every statement and mode |
| `BIN_TO_UUID` | the value is not exactly 16 bytes | 1411, every statement and mode |
| `PERIOD_ADD` / `PERIOD_DIFF` | a period argument is not a positive `[YY]YYMM` with month 1 to 12 | 1210, every statement and mode |

`mysql.Violates(err, "1411")` / `"1210"` match the per-row violations. A second family is
refused at resolution, wherever the expression sits and rows or none: `NAME_CONST` takes
two literals (one unary minus or a `COLLATE` wrapper is fine, a folded `1+1` or `TRUE` is
not; a NULL name is 1382), `LIKE`'s `ESCAPE` takes a constant of at most one character,
`NTILE` a positive count and `NTH_VALUE` a positive integer position, and `MATCH` columns
of one relation (a select alias or two tables is 1210 `to MATCH`) with a constant
`AGAINST` (a column in it is 1210 `to AGAINST`).

### Function call arguments and system variable reads

A function call argument carrying an alias (`f(x AS a)`, the loadable function syntax) is
refused as the server refuses it, before the argument is even resolved: a native function
checks its argument count first (1582, spelling the name as written), then the alias is 1583
with the name lowercased (`SELECT ABS(3 AS three)` is `... native function 'abs'`); any other
name -- a stored function, even one that does not exist -- is 1584 before the function is
looked up. The data dictionary's own functions (`INTERNAL_TABLE_ROWS` and the rest the
server marks internal) are 3566 whenever a statement names them, before either check.

An explicitly scoped system variable read must match the variable's own scope: `@@session.x`
(`@@local.x` means the same) of a GLOBAL-only variable and `@@global.x` of a SESSION-only one
are the statement's 1238, wherever the read sits -- a dead branch, a subquery, a `SELECT`
with no rows. The scope table is generated from the server source (`sql/sys_vars.cc`), so it
covers every stock variable; a plugin's or component's variable (`@@x.y` included) and an
unqualified `@@x` are never judged, and an unknown name is left to the server's own 1193.

### Triggers and stored routines

The loader reads `CREATE TRIGGER` / `CREATE PROCEDURE` / `CREATE FUNCTION` (`DEFINER`,
`IF NOT EXISTS`, the characteristics) and `DROP` / `ALTER` (characteristics only) the way it
reads a table: no `DELIMITER` is needed, a body's own `;` is read as part of the one compound
statement it belongs to, and a mysql-client `DELIMITER x` line is read too. What the server
itself refuses at CREATE time is a problem the same way an unknown table is: no such table
(1146), the trigger or routine already exists (1359 / 1304), no such trigger or routine to
`DROP` (1360 / 1305), no such trigger for `FOLLOWS` / `PRECEDES` to name (3011). `DROP TABLE`
takes a table's triggers with it; `RENAME TABLE` moves them.

`CREATE EVENT` is read the same way (both schedule forms, `STARTS` / `ENDS`, `ON COMPLETION`,
`ENABLE` / `DISABLE`, `COMMENT`), and `ALTER EVENT` (each clause written replaces that part
of the event, a new schedule the whole schedule, `RENAME TO` the name) and `DROP EVENT` are
applied. Nothing a statement of the program runs reaches an event,
so its body is read for the schema's own sake: the server checks nothing of it at `CREATE`
time (a `DELETE` from a table that does not exist is accepted and fails at every run,
measured), the checker reports it as a schema problem, and the body's own statements are
judged for the obligations like a routine's. A `RETURN` in an event is 1313. The migration
commands manage events ([migrations.md](migrations.md#mysql)).

The body is read once per schema, the way a PL/pgSQL function's is on PostgreSQL
([checks.md](checks.md#name-the-errors-a-trigger-raises)). `IF` / `CASE` / `LOOP` / `WHILE` /
`REPEAT`, labelled blocks with `LEAVE` / `ITERATE`, `RETURN`, `SET`, `SELECT ... INTO`, cursors
(`DECLARE` / `OPEN` / `FETCH` / `CLOSE`), `CALL`, `SIGNAL` / `RESIGNAL` and
`DECLARE ... HANDLER FOR` are walked. Names resolve as on the server:

- `NEW.col` / `OLD.col` type as the trigger's own table's columns; an unknown column is 1054;
- `OLD` in an INSERT trigger, or `NEW` in a DELETE trigger, is 1363;
- writing `OLD` (checked before the event: in an INSERT trigger too), or writing `NEW` outside
  a BEFORE trigger, is 1362; outside a trigger, `SET NEW.col` / `SET OLD.col` is 1193 (a
  read of `NEW.col` there is a column the server resolves only at run time);
- a NOT NULL column's `NEW.col` can still be NULL in a BEFORE trigger and is as declared in an
  AFTER one: the server's own NOT NULL check runs between the two (measured);
- a `DECLARE`d variable or a routine's own parameter shadows a column of the same name.

What the server itself refuses when the body is created is an error here too:

| construct | error |
|---|---|
| `LEAVE` / `ITERATE` with no matching label | 1308 |
| `RETURN` outside a FUNCTION | 1313 |
| a FUNCTION with no `RETURN` | 1320 |
| an undeclared cursor or variable, a `FETCH` column-count mismatch | 1324 / 1327 / 1328 |
| a mismatched `SELECT ... INTO` column count | 1222 |
| a trigger or function that returns a result set (its own INTO-less `SELECT`, or a `CALL`ed PROCEDURE's own) | 1415 |
| `COMMIT` / `START TRANSACTION` / a DDL statement inside a body | 1422 |
| a trigger that writes its own table | 1442, on every one of the 18 timing x event x write combinations (measured): always a failure, reported on the trigger's own definition rather than on a statement that fires it |
| two `DECLARE`s of the same variable name in one block | 1331 |
| two `DECLARE ... CONDITION`s or two `DECLARE ... CURSOR`s of one name in one block (a variable and a cursor of one name are different namespaces) | 1332 / 1333 |
| two `HANDLER`s of one block naming the same condition value (handlers that merely overlap, `SQLEXCEPTION` next to `SQLSTATE '45000'`, are accepted) | 1413 |
| dynamic SQL (`PREPARE` / `EXECUTE` / `DEALLOCATE PREPARE`) or `FLUSH` in a trigger or FUNCTION (a PROCEDURE is exempt) | 1336 |
| `LOCK TABLES` / `UNLOCK TABLES`, `LOAD DATA` or `ALTER VIEW` in any body, a PROCEDURE's included (`SELECT ... INTO OUTFILE` is allowed, even in a trigger or function: it returns no result set) | 1314 |
| a SQLSTATE literal that is not five characters (`SIGNAL SQLSTATE '4500'`, a `CONDITION` or `HANDLER` naming one) | 1407 at CREATE; five characters of any kind are accepted (measured) |
| a `SIGNAL` or `RESIGNAL` setting `MYSQL_ERRNO = 0` | 1231, certain every time |
| a bare `RESIGNAL` reached outside any `HANDLER` | 1645, certain every time |
| a PROCEDURE that `CALL`s itself | 1456 on every recursive invocation while `max_sp_recursion_depth` is 0 (the default; declare it above 0 and nothing is predicted); direct self-recursion only, not a routine reaching itself through another |
| a FUNCTION that calls itself | 1424 on every invocation (the `CREATE` goes through; the setting does not apply to functions) |
| a trigger chain writing back into a table already in use further up (`INSERT INTO x` fires `x`'s trigger writing `y`, whose trigger writes `x`), or reading it under a lock (`SELECT ... FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`; a plain read does not collide, measured) | 1442, though neither trigger writes its own table; a chain that a `CALL`ed routine continues counts the same, and a certain failure found anywhere along the chain (1442, 1456) is reported on the firing statement |

And among the body's failure modes, the "may" shape a `SIGNAL` has:

| construct | failure mode |
|---|---|
| a `CASE` (simple or searched) with no `ELSE` | 1339, "Case not found for CASE statement", when no `WHEN` matches |
| a bare `RESIGNAL` with its own `SET MYSQL_ERRNO` | re-raises with that number, not the caught `SIGNAL`'s |

A trigger's or routine's own writes bring their own failure modes into the body's: the
schema's constraints, and what those writes' own triggers raise in turn (a cycle is cut). A
`SIGNAL`'s key is its `MYSQL_ERRNO` as decimal text when it sets one -- a builtin's number
too, `SET MYSQL_ERRNO = 1062` under SQLSTATE `'23000'` is keyed `1062`, not by a key name the
message never carried (measured) -- else its SQLSTATE (the same rule the
[runtime](#the-runtime-databasesql) reads an error back by); SQLSTATE class
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
1442 on every execution (reading the table is enough, measured; so is naming it in `FROM`
without reading a column). The writes count through nested calls (a function that `CALL`s
a procedure writing t, or `RETURN`s another function that does, collides the same way), and
inside a body each statement is the invoking one: `UPDATE t SET v = f(v)` with f writing t is
1442 when the routine is `CALL`ed, `SELECT COUNT(*) INTO n FROM t; SET @x = f(1)` as two
statements is not, and a trigger's own table collides with every statement of its body (a
trigger on `other` running `SET @y = g(1)` with g writing `other` is 1442; a table the
firing statement merely reads is not the trigger's, measured). A `CALL` inside a body is
resolved the way a top-level one is (1305 / 1318 / 1414), and the callee's failure modes and
writes become the body's. `-- sqlshape: not null`
above a `CREATE FUNCTION` declares the function never returns NULL, the same directive
[checks.md](checks.md#a-column-that-may-be-null-needs-a-field-that-can-hold-null) documents
for PostgreSQL; a call then types as NOT NULL instead. A PROCEDURE or a TRIGGER still refuses
the directive: neither returns a value for it to describe.

`CALL p(...)` types an `IN` / `INOUT` argument by its parameter and requires an `OUT` /
`INOUT` argument to be a variable (1414); its own facts are `Kind Call`. What counts as a
variable there:

| argument | variable? |
|---|---|
| a declared variable, a parameter, a `?` | yes |
| `NEW.col` inside a `BEFORE` trigger's own body | yes (the server's own 1414 message names this exception) |
| `OLD.col`, or `NEW.col` in an `AFTER` trigger | no |
Its result columns come from the body's own INTO-less `SELECT`s: none is no columns, one is
those columns, several of the same shape agree on one list, several of different shapes is
the checker's own error (not something mysqld itself refuses -- it only ever returns
whichever result set the execution path taken produced, at run time). Its failure modes are
the body's own.

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

### Name resolution

`ORDER BY`, `GROUP BY` and `HAVING` see the select list's aliases as the server does, in an
expression as well as bare (`ORDER BY c + 1`, `GROUP BY CONCAT(f1)`; a table column of the same
name wins in `GROUP BY`, an item written with an alias wins over an unaliased column of the
name, two different items of the name are 1052). A nested query placed in the select list,
`GROUP BY`, `HAVING` or `ORDER BY` of an enclosing block sees that block's aliases too, one in
its `WHERE` or `ON` does not (`SELECT name c, (SELECT 1 FROM orders WHERE note = c) FROM users`
runs; the same subquery in the `WHERE` is 1054); an alias declared after the subquery is 1247
(`forward reference in item list`), an alias of an aggregate is 1247 (`reference to group
function`) except from the nested query's own `HAVING`, and always from a `GROUP BY` placement;
an alias of a window function is 3594. `HAVING` never resolves a name against the block's own
tables: a column not in the select list or `GROUP BY` is 1054 at the top level, and an
enclosing block's column inside a subquery (`WHERE EXISTS (SELECT 1 FROM orders HAVING id)`
reads the outer `id`). `_rowid` names a base table's first key when, after the server's
ordering (`PRIMARY`, the unique keys over `NOT NULL` columns, the rest), that key is unique over
one `NOT NULL` integer column (`INT` family, `YEAR`, `BIT`), for a qualified reference or a
level with a single table; a view or a derived table has none. `INSERT ... SELECT ... ON
DUPLICATE KEY UPDATE` resolves its assignments against the target and the `SELECT`'s tables
(an unqualified name both have is 1052) unless the `SELECT` is grouped or aggregated, when the
target alone is in view; `VALUES(c)` is always the target's column; a select alias is never
visible there. A derived table needs an alias (1248); `QUALIFY` is rejected as 8.4 rejects it
without the hypergraph optimizer (6037). A `USING` or `NATURAL` join
coalesces its common columns (an unqualified name resolves to the left side, `SELECT *` lists it
once). Table and view names compare as `lower_case_table_names` says; column and key names never
mind case. An `UPDATE` or `DELETE` whose subquery reads the table it writes is refused as the
server refuses it: 1093 for the table itself (in `WHERE`, `EXISTS` or `IN` alike), 1443 for a
view over it; a derived table over it is materialized and accepted, as is `INSERT ... SELECT`
from the same table (measured).

### Views

A view is merged into the query that reads it unless it says `ALGORITHM=TEMPTABLE` or its
query cannot be merged (`GROUP BY`, `HAVING`, `DISTINCT`, `LIMIT`, a set operation, a window
function or a subquery in the select list) -- the server's own `is_mergeable`. A write through a
merged view lands on its base table; through any other view it is 1288 (an `INSERT`'s spelling
of the same refusal is 1471). A merged view carries the server's own two write flags, computed
over the `FROM` leaves of its query: it is updatable when any leaf is (a base table, or an
updatable view), insertable when every leaf is, and neither when any leaf sits on the nullable
side of an outer join -- so a join with a derived table or a `TEMPTABLE` view stays updatable
but is never insertable, and a view over a `TEMPTABLE` view is neither. Each write's own
rules, measured against the server:

- `INSERT` needs the insertable flag (else 1471). The fields -- the column list, or without
  one every view column -- resolve against the view (1054, 1136); a derived column among them
  is that column's 1348, one outside them, or the same base column behind two view columns,
  is 1471. A `COLLATE` wrapper is transparent (the server's `field_for_view_update`): such a
  column stays plain. A join view additionally needs an explicit column list (1394) naming
  columns of exactly one base table, the `ON DUPLICATE KEY UPDATE` assignments included
  (1393), and `REPLACE` never reaches one (1395: the delete half). The write's failure modes
  are the base table's own (1062 and the rest), and a base column with no default the
  statement leaves unassigned is the view's 1423 rather than the table's 1364.
- `UPDATE` needs the updatable flag (else 1288) and may then assign any plain column: an
  expression of the view's own select list is that column's 1348, a column of a materialized
  leaf inside the view the view's 1288, and the assignments through a join view must keep to
  one base table (1393).
- `DELETE` takes any updatable single-leaf view -- derived columns included, even a view of
  only expressions; a view of more than one leaf is 1395, anything else (a CTE target
  included) 1288.
- a subquery reading the very view the statement writes is the target itself: 1093, not the
  1443 a different view over the same base table gets.

`WITH CHECK OPTION` on
a view the server would not merge is refused when the schema loads, as the server refuses the
`CREATE` (1368). `CREATE OR REPLACE VIEW` replaces the earlier definition, and so does `ALTER
VIEW` (the view must exist, 1146, and be a view, 1347).

### Statements without a result set

Besides `SELECT` / `INSERT` / `UPDATE` / `DELETE` / `CALL`, the checker reads these, run with
`Exec`:

- `LOAD DATA [LOCAL] INFILE ... INTO TABLE t` is an INSERT of the file's rows: the target must be a
  base table (a view is 1288), the column list resolves against it (a `@var` takes its field and
  assigns nothing), a `SET` assignment types its expression against the column, and a
  placeholder in one takes the column's type. Its failure modes are the INSERT's -- the keys,
  foreign keys and checks of the columns it fills (1062 / 1452 / 3819), `IGNORE` turning them
  into warnings, `REPLACE` deleting the colliding row first -- with two differences the server
  makes (measured): a field for a `NOT NULL` column may be NULL, which is 1263 rather than 1048
  (a `SET col = NULL` stays 1048), and a `NOT NULL` column the column list leaves out takes its
  type's implicit default rather than 1364.
- `LOCK TABLES` names tables that must exist (1146, a view may be locked) under distinct aliases
  (1066); `UNLOCK TABLES` resolves nothing. Neither has parameters.
- `SELECT ... INTO OUTFILE` / `INTO DUMPFILE`, in either position of the `INTO`, is typed like the
  `SELECT` it wraps but returns no columns: the rows go to a file on the server. So is `SELECT ...
  INTO @var` / `INTO var` (the row goes into the variables; the count must match, 1222). A
  trailing `FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE` leaves a `SELECT`'s columns as they
  are.
- `INSERT INTO t VALUES ()` (every row empty, with or without a column list) inserts a row of
  defaults: no column is assigned, so there is no 1136, and an omitted `NOT NULL` column
  without a default is the usual 1364.

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
