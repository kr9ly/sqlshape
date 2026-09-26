# MySQL: statement errors the checker predicts

[日本語](mysql-errors.ja.md)

What the MySQL analyzer predicts from a statement's own form, numbered as the server numbers the
error: a value a column cannot store, a constant expression the server cannot compute, a function
argument it refuses, a trigger or routine body it refuses at `CREATE`, a name it cannot resolve, a
view it cannot write through. When a diagnostic carries a MySQL error number, look the number up
in the [index](#error-index). The Go types, constraint names, runtime and server settings are in
[mysql.md](mysql.md).

Each entry is one of two kinds. An error is certain: every execution fails the same way, vet
reports it, and the statement has to change. A failure mode may happen at run time, for some rows
or some values: it goes on the statement's expect line, and `mysql.Violates(err, "<number>")`
matches it. Where an entry can be either, it says which position makes it which.

Every behavior on this page was measured against `mysqld` 8.4
([what the checker embeds](mysql.md#what-the-checker-embeds)). On a server of another version
the program still runs, but where that server behaves differently the verdicts are not
guaranteed to match it.

## Error index

The constraint violations (1062, 1451, 1452, 3819, 1048, 1369, 1423) are in
[mysql.md](mysql.md#constraint-names-and-failure-modes), and a trigger's or routine's own
`SIGNAL` numbers in [mysql.md](mysql.md#errors-a-trigger-or-routine-raises).

| error | where |
|---|---|
| 1048 | [Statements without a result set](#statements-without-a-result-set) |
| 1052 | [Name resolution](#name-resolution) |
| 1054 | [Triggers, routines and events](#triggers-routines-and-events), [Name resolution](#name-resolution), [Views](#views) |
| 1062 | [Views](#views), [Statements without a result set](#statements-without-a-result-set) |
| 1066 | [Name resolution](#name-resolution), [Statements without a result set](#statements-without-a-result-set) |
| 1093 | [Name resolution](#name-resolution), [Views](#views) |
| 1136 | [Views](#views), [Statements without a result set](#statements-without-a-result-set) |
| 1146 | [Triggers, routines and events](#triggers-routines-and-events), [Views](#views), [Statements without a result set](#statements-without-a-result-set) |
| 1193 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads), [Triggers, routines and events](#triggers-routines-and-events) |
| 1210 | [Constant function arguments](#constant-function-arguments), [Spatial types](#spatial-types) |
| 1222 | [Triggers, routines and events](#triggers-routines-and-events), [Statements without a result set](#statements-without-a-result-set) |
| 1231 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1238 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads) |
| 1247 | [Name resolution](#name-resolution) |
| 1248 | [Name resolution](#name-resolution) |
| 1263 | [Statements without a result set](#statements-without-a-result-set) |
| 1264 | [Values stored into a column](#values-stored-into-a-column) |
| 1265 | [Values stored into a column](#values-stored-into-a-column) |
| 1288 | [Views](#views), [Statements without a result set](#statements-without-a-result-set) |
| 1292 | [Values stored into a column](#values-stored-into-a-column), [Constant conversions](#constant-conversions), [Constant function arguments](#constant-function-arguments) |
| 1304 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1305 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1308 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1313 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1314 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1318 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1320 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1324 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1327 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1328 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1331 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1332 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1333 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1336 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1339 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1347 | [Views](#views) |
| 1348 | [Views](#views) |
| 1353 | [Name resolution](#name-resolution) |
| 1359 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1360 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1362 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1363 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1364 | [Views](#views), [Statements without a result set](#statements-without-a-result-set) |
| 1366 | [Values stored into a column](#values-stored-into-a-column) |
| 1368 | [Views](#views) |
| 1382 | [Constant function arguments](#constant-function-arguments) |
| 1393 | [Views](#views) |
| 1394 | [Views](#views) |
| 1395 | [Views](#views) |
| 1406 | [Values stored into a column](#values-stored-into-a-column) |
| 1407 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1411 | [Constant function arguments](#constant-function-arguments) |
| 1413 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1414 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1415 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1416 | [Values stored into a column](#values-stored-into-a-column), [Spatial types](#spatial-types) |
| 1422 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1423 | [Views](#views) |
| 1424 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1442 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1443 | [Name resolution](#name-resolution), [Views](#views) |
| 1452 | [Statements without a result set](#statements-without-a-result-set) |
| 1456 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1471 | [Views](#views) |
| 1525 | [Constant conversions](#constant-conversions) |
| 1582 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads) |
| 1583 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads) |
| 1584 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads) |
| 1645 | [Triggers, routines and events](#triggers-routines-and-events) |
| 1690 | [Constant arithmetic](#constant-arithmetic), [Constant conversions](#constant-conversions), [Spatial types](#spatial-types) |
| 3011 | [Triggers, routines and events](#triggers-routines-and-events) |
| 3037 | [Spatial types](#spatial-types) |
| 3516 | [Spatial types](#spatial-types) |
| 3548 | [Spatial types](#spatial-types) |
| 3566 | [Function call arguments and system variable reads](#function-call-arguments-and-system-variable-reads) |
| 3594 | [Name resolution](#name-resolution) |
| 3643 | [Spatial types](#spatial-types) |
| 3819 | [Statements without a result set](#statements-without-a-result-set) |
| 6037 | [Name resolution](#name-resolution) |

## Values stored into a column

A literal the column can never store is not a failure mode but the statement's own error,
since every execution fails the same way in strict mode (the default). The checker applies the
rules the server uses when it stores a value into a column to a literal written straight into a
column by `INSERT ... VALUES`, `UPDATE ... SET`, `ON DUPLICATE KEY UPDATE` or `REPLACE`:

| into | rejected | error |
|---|---|---|
| an integer column | a value outside its range, a real or decimal included once rounded | 1264 |
| | a string holding no number (`'abc'`, `''`) | 1366 |
| | a number followed by other text (`'12a'`) | 1265 |
| `YEAR` | anything but 0, 1-99 and 1901-2155 | 1264 |
| `DECIMAL(M,D)` | more than M-D integer digits once rounded to D places; a negative value into `UNSIGNED` | 1264 |
| `FLOAT` | a value beyond the single-precision range | 1264 |
| `DATE` / `DATETIME` / `TIMESTAMP` | a string the server cannot read as a date and time or finds out of range, a zero month, day or date the `sql_mode` forbids (`NO_ZERO_IN_DATE`, `NO_ZERO_DATE`), a day the month lacks unless `ALLOW_INVALID_DATES`; a number the server does not accept as a date and time; a `TIMESTAMP` outside 1970-01-01 00:00:01 to 2038-01-19 03:14:07 UTC (judged only where the session time zone cannot change the verdict) | 1292 |
| `TIME` | a string the server cannot read as a time, minutes or seconds of 60 or more, more than 838 hours | 1292 |
| `CHAR(n)` / `VARCHAR(n)` / `BINARY(n)` / `VARBINARY(n)` | a string longer than n characters (bytes when binary) once trailing spaces are dropped | 1406 |
| `ENUM` | a string that names no member (compared as the collation does) and is not a member's index; a number outside 1 to the member count | 1265 |
| `SET` | a list naming a member the set lacks | 1265 |
| any spatial type | a number or a string: neither can hold a well-formed geometry | 1416 |

Not read: `INSERT IGNORE` / `UPDATE IGNORE` and a schema whose `sql_mode` is not strict (the
value is stored adjusted, with a warning; under `STRICT_TRANS_TABLES` alone a nontransactional
table is strict for the first row only), a value inside a routine or trigger body, hex and bit
literals, `JSON` and `BIT` columns, and an expression the server would fold (`100 + 28`).

## Constant arithmetic

A constant expression the server cannot compute is the statement's own 1690, judged before any
row is read, as the server computes it:

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

## Constant conversions

A strict `INSERT` / `REPLACE` / `UPDATE` / `DELETE` (outside `IGNORE`) escalates a constant
conversion's warning to the error 1292 `ER_TRUNCATED_WRONG_VALUE`; a `SELECT`, a `SET`, a `DO`
or a non-strict write runs the same expression with a warning, rows present or not:

| expression | fails when |
|---|---|
| `CAST` / `CONVERT` of a constant to `DATE` / `DATETIME` | the value is no datetime under the session's zero-date flags (`CAST('2004-10-0' AS DATE)`, `CAST(65 AS DATETIME)`) |
| ... to `TIME` | the server does not read it as a time, or it passes 838 hours |
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

## Constant function arguments

A function that decides a run-time failure by an argument's value is judged when the
argument is a constant, where the constant-arithmetic rules above place the failure (a
folded term fails the statement, a per-row term is the violation keyed by its own number,
and a query over no rows does not fail):

| function | fails when | error |
|---|---|---|
| `INET_ATON` | the value is not digit groups of at most 255 separated by up to three dots (`'122.256'`, a trailing dot; `'1.2.3'` runs) | 1411, in a strict write outside `IGNORE` |
| `INET6_ATON` | the value is neither a full dotted IPv4 (four groups, no leading `0x`) nor a valid IPv6 text (one `::` gap at most, four hex digits per group) | 1411, the same gate |
| `UNHEX` | the value holds a non-hex character | 1411, the same gate |
| `STR_TO_DATE` | the value does not parse under the format (the server's format specifiers, the en_US month and day names, `%V`/`%v` weeks with their `%X`/`%x` years), or the date fails the session's zero-date flags (under `NO_ZERO_DATE` any zero year, month or day of a date result) | 1411; a parsed value with a non-space tail is 1292, spelt with the format's own result type |
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

## Function call arguments and system variable reads

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
with no rows. The scope table is generated from the server source, so it
covers every stock variable; a plugin's or component's variable (`@@x.y` included) and an
unqualified `@@x` are never judged, and an unknown name is left to the server's own 1193.

## Spatial types

A geometry value has one of seven types (`POINT` ... `GEOMETRYCOLLECTION`); a column is declared
with one of them or with `GEOMETRY` (any). The checker knows a value's type when a constructor
(`POINT(1, 1)`), a typed reader (`ST_PointFromText`) or a constant text or WKB fixes it, and
judges with it:

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

## Triggers, routines and events

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
time (a `DELETE` from a table that does not exist is accepted and fails at every run), the checker reports it as a schema problem, and the body's own statements are
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
  AFTER one: the server's own NOT NULL check runs between the two;
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
| a trigger that writes its own table | 1442, on every one of the 18 timing x event x write combinations: always a failure, reported on the trigger's own definition rather than on a statement that fires it |
| two `DECLARE`s of the same variable name in one block | 1331 |
| two `DECLARE ... CONDITION`s or two `DECLARE ... CURSOR`s of one name in one block (a variable and a cursor of one name are different namespaces) | 1332 / 1333 |
| two `HANDLER`s of one block naming the same condition value (handlers that merely overlap, `SQLEXCEPTION` next to `SQLSTATE '45000'`, are accepted) | 1413 |
| dynamic SQL (`PREPARE` / `EXECUTE` / `DEALLOCATE PREPARE`) or `FLUSH` in a trigger or FUNCTION (a PROCEDURE is exempt) | 1336 |
| `LOCK TABLES` / `UNLOCK TABLES`, `LOAD DATA` or `ALTER VIEW` in any body, a PROCEDURE's included (`SELECT ... INTO OUTFILE` is allowed, even in a trigger or function: it returns no result set) | 1314 |
| a SQLSTATE literal that is not five characters (`SIGNAL SQLSTATE '4500'`, a `CONDITION` or `HANDLER` naming one) | 1407 at CREATE; five characters of any kind are accepted |
| a `SIGNAL` or `RESIGNAL` setting `MYSQL_ERRNO = 0` | 1231, certain every time |
| a bare `RESIGNAL` reached outside any `HANDLER` | 1645, certain every time |
| a PROCEDURE that `CALL`s itself | 1456 on every recursive invocation while `max_sp_recursion_depth` is 0 (the default; declare it above 0 and nothing is predicted); direct self-recursion only, not a routine reaching itself through another |
| a FUNCTION that calls itself | 1424 on every invocation (the `CREATE` goes through; the setting does not apply to functions) |
| a trigger chain writing back into a table already in use further up (`INSERT INTO x` fires `x`'s trigger writing `y`, whose trigger writes `x`), or reading it under a lock (`SELECT ... FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`; a plain read does not collide) | 1442, though neither trigger writes its own table; a chain that a `CALL`ed routine continues counts the same, and a certain failure found anywhere along the chain (1442, 1456) is reported on the firing statement |


Among the body's failure modes, the ones that may happen at run time, declared on the expect
line like a `SIGNAL`'s ([mysql.md](mysql.md#errors-a-trigger-or-routine-raises)):

| construct | failure mode |
|---|---|
| a `CASE` (simple or searched) with no `ELSE` | 1339, "Case not found for CASE statement", when no `WHEN` matches |
| a bare `RESIGNAL` with its own `SET MYSQL_ERRNO` | re-raises with that number, not the caught `SIGNAL`'s |

A call to a FUNCTION the catalog does not know resolves to the schema's own (an unqualified
name that is also a native function resolves to the native one, as on the server; `db.f`
names the routine): a wrong argument count is 1318, no such routine 1305, the result types as
the `RETURNS` declaration and is always nullable (a stored function's `RETURN` can produce
NULL regardless of the declared type; there is no static proof otherwise), and the body's own
failure modes (its SIGNALs, its writes' violations, what they fire) reach the calling
statement. A function that writes a table the calling statement itself reads or writes is
1442 on every execution (reading the table is enough; so is naming it in `FROM`
without reading a column). The writes count through nested calls (a function that `CALL`s
a procedure writing t, or `RETURN`s another function that does, collides the same way), and
inside a body each statement is the invoking one: `UPDATE t SET v = f(v)` with f writing t is
1442 when the routine is `CALL`ed, `SELECT COUNT(*) INTO n FROM t; SET @x = f(1)` as two
statements is not, and a trigger's own table collides with every statement of its body (a
trigger on `other` running `SET @y = g(1)` with g writing `other` is 1442; a table the
firing statement merely reads is not the trigger's). A `CALL` inside a body is
resolved the way a top-level one is (1305 / 1318 / 1414), and the callee's failure modes and
writes become the body's. `-- sqlshape: not null`
above a `CREATE FUNCTION` declares the function never returns NULL, the same directive
[checks.md](checks.md#a-column-that-may-be-null-needs-a-field-that-can-hold-null) documents
for PostgreSQL; a call then types as NOT NULL instead. A PROCEDURE or a TRIGGER still refuses
the directive: neither returns a value for it to describe.

`CALL p(...)` types an `IN` / `INOUT` argument by its parameter and requires an `OUT` /
`INOUT` argument to be a variable (1414). What counts as a
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

## Name resolution

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
visible there. The row alias (`INSERT ... VALUES (...) AS new [(names)]`, 8.0.19) exposes the
inserted columns -- the insert's own fields, renamed positionally by the name list -- to
`ON DUPLICATE KEY UPDATE` beside the target: an unqualified name both carry is 1052, a name
list of the wrong count 1353, an alias colliding with the target 1066, and `VALUES(c)` stays
usable beside it. A derived table needs an alias (1248); `QUALIFY` is rejected as 8.4 rejects it
without the hypergraph optimizer (6037). A `USING` or `NATURAL` join
coalesces its common columns (an unqualified name resolves to the left side, `SELECT *` lists it
once). Table and view names compare as `lower_case_table_names` says; column and key names never
mind case. An `UPDATE` or `DELETE` whose subquery reads the table it writes is refused as the
server refuses it: 1093 for the table itself (in `WHERE`, `EXISTS` or `IN` alike), 1443 for a
view over it; a derived table over it is materialized and accepted, as is `INSERT ... SELECT`
from the same table.

## Views

A view is merged into the query that reads it unless it says `ALGORITHM=TEMPTABLE` or its
query cannot be merged (`GROUP BY`, `HAVING`, `DISTINCT`, `LIMIT`, a set operation, a window
function or a subquery in the select list), as the server decides it. A write through a
merged view lands on its base table; through any other view it is 1288 (an `INSERT`'s spelling
of the same refusal is 1471). A merged view carries the server's own two write flags, computed
over the `FROM` leaves of its query: it is updatable when any leaf is (a base table, or an
updatable view), insertable when every leaf is, and neither when any leaf sits on the nullable
side of an outer join -- so a join with a derived table or a `TEMPTABLE` view stays updatable
but is never insertable, and a view over a `TEMPTABLE` view is neither. Each write's own
rules:

- `INSERT` needs the insertable flag (else 1471). The fields -- the column list, or without
  one every view column -- resolve against the view (1054, 1136); a derived column among them
  is that column's 1348, one outside them, or the same base column behind two view columns,
  is 1471. A `COLLATE` wrapper is transparent, as it is to the server: such a
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

## Statements without a result set

Besides `SELECT` / `INSERT` / `UPDATE` / `DELETE` / `CALL`, the checker reads these, run with
`Exec`:

- `LOAD DATA [LOCAL] INFILE ... INTO TABLE t` is an INSERT of the file's rows: the target must be a
  base table (a view is 1288), the column list resolves against it (a `@var` takes its field and
  assigns nothing), a `SET` assignment types its expression against the column, and a
  placeholder in one takes the column's type. Its failure modes are the INSERT's -- the keys,
  foreign keys and checks of the columns it fills (1062 / 1452 / 3819), `IGNORE` turning them
  into warnings, `REPLACE` deleting the colliding row first -- with two differences the server
  makes: a field for a `NOT NULL` column may be NULL, which is 1263 rather than 1048
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
