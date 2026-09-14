# Changelog

Notable changes to sqlshape, newest first. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/). Behaviour of the checker that makes a
statement pass or fail is listed under Changed even when the old behaviour was a bug, since a
passing program may start to fail. A release candidate (`v1.1.0-rc.1`) carries the section of the
release it is a candidate for.

## [Unreleased]

## [2.0.0] - 2026-09-14

### Added

- `cmd/sqlshape/internal/vet` gained `docs_test.go`: a harness that reads the go/sql fences out of
  `docs/checks.md` and `checks.ja.md` at test time (both the code and the diagnostic wording a
  Rejected fence's trailing comment claims), completes each into a compiling program under
  `testdata/src/docsex_*`, runs it through the checker, and asserts that every diagnostic docs
  claims is a literal substring of one the checker actually produced, with nothing left over. It
  covers "Result columns bind to fields by name", "A column that may be NULL needs a field that can
  hold NULL", "Fix a unique key by equality", "Bulk loading with COPY", and "Name the errors a
  trigger raises" (both dialects) so far, in both languages -- not the full set of fences yet (see
  NOTES.local.md and the package doc for what is not wired up and why).
- `x/dialect.Type` gained `ElemNotNull`: a dialect that can prove an array's elements are
  never NULL sets it (false, the default, means "unknown", not "may be NULL"). PostgreSQL
  sets it for `array_agg(x)` where `x` is itself never NULL (a row constructor such as
  `(a, b)::T`, an inner-joined `NOT NULL` column, a whole-row reference), for a literal
  `ARRAY[...]` whose elements are all provably not NULL, and for `ARRAY(SELECT ...)` over a
  not-null single output column, propagating it through subqueries, CTEs, set operations and
  freshly analyzed views (a frozen view's stored column snapshot has no such field yet and
  stays "unknown"). A plain array column never qualifies, since PostgreSQL declares nothing
  about a column's own elements.

### Changed

- `x/expand`: an `if` / `with` / `range` over a path an earlier control in the same lineage
  already decided (the same `{{if .Status}}` appearing twice, say, once in a column list and
  once in `VALUES`) now follows that decision instead of branching it again, in both the full
  and the sparse expansion; a template no longer yields impossible expansions the runtime's
  evaluator, reading the value once per occurrence, was never actually going to produce.
  `dialect.Advice` now carries the table and column it is about, and `-strict` reports such
  advice only in a package whose statements touch that relation or column (schema-wide advice
  still applies everywhere), so an enum column no longer lectures every package under
  `-strict`. A pointer parameter used inside the then branch of `{{if .X}}` / `{{with .X}}` on
  that very path is recognized as not NULL there (an expansion's `GuardedTrue` covers only the
  exact path the taken guard proved, not paths below it, and the else branch proves nothing).
  PostgreSQL: `array_agg((i.sku, i.qty)::order_item)` into `[]Item` keeps each nested row
  field's own nullability through the cast to a named composite type, instead of discarding it
  (a row constructor is never NULL; `array_agg` of a scalar does pass a NULL element through,
  unaffected). Building on that: the checker's standing "may contain a NULL element" note
  (and its `-strict` rejection) on an array result no longer fires when `dialect.Type.ElemNotNull`
  says the elements are provably not NULL, so `array_agg((i.sku, i.qty)::order_item)` into
  `[]Item` is now accepted with no note at all, while `array_agg` of a plain nullable column,
  a literal `ARRAY[...]` with a `NULL` element, and an ordinary array column keep the note (and
  the `-strict` rejection) unchanged. A parameter compared against a domain column carries the domain (PostgreSQL
  resolves the placeholder to the base type, so `Param.Type` is taken from the compared
  column), so `-strict`'s "carries domain `yen` as a plain `int64`" advice reaches parameters
  too, and a mismatch names the domain (`SQL expects email`).
- MySQL: `-- sqlshape: not null` above a `CREATE FUNCTION` declares the function never returns
  NULL, the same directive PostgreSQL already has; a call then types as NOT NULL. A PROCEDURE
  or TRIGGER still refuses the directive (neither returns a value for it to describe).

- PostgreSQL after a third adversarial round against a running server (8 lanes, 23 findings, each a
  regression test in `adv_*_test.go`), the failure modes first. `TRUNCATE` of a table another table's
  foreign key still references (no `CASCADE`, the referrer not in the list) is reported as certain to
  fail (0A000); `ON CONFLICT (cols)` absorbs a unique constraint only when it can be the arbiter, so a
  `DEFERRABLE` key (55000) or a partial index whose predicate the `ON CONFLICT ... WHERE` does not
  repeat (42P10) keeps its 23505 in the list, and `ON CONFLICT ON CONSTRAINT <exclusion> DO UPDATE`
  is reported as certain to fail (42809); `ON CONFLICT DO NOTHING` without a target absorbs the
  exclusion constraints and partial unique indexes too, and is certain to fail (55000) when any
  key of the table is `DEFERRABLE`. A `NOT NULL` that a domain, not the column, declares is
  keyed by the domain's name (schema-qualified unless public): PostgreSQL reports no table or column
  for it, and `postgres.ConstraintError.Key()` now returns the domain's name for that error, so
  `Violates(err, "email")` matches what the checker predicted. `ORDER BY` / `GROUP BY` resolve an
  output name the way PostgreSQL does: two output columns of one name are ambiguous (42702) unless
  they are the same expression; the ungrouped-column error names its table (`"t1.v"`); an error the
  select list and the `WHERE` share is reported at the select list's position. The result of a
  set-returning function in the select list (`unnest(arr)`) is nullable whatever its argument is:
  an element can be NULL when the array cannot. The `One` proof sees through a cast on the grouped
  column, takes a one-element `= ANY(ARRAY[v])` and a `NOT NULL` column's `IS NOT DISTINCT FROM v`
  for equalities, and matches a partial index's predicate with the literal casts a statement spells
  out; `col = NULL` fixes nothing (it is never true), so it neither pins nor proves single, on MySQL
  too. `SET col = DEFAULT` stores the column's literal default, so a `transitions` declaration
  judges it as a transition to that state (a non-literal default is refused as before, with a
  message that says why). `-strict` rejects `[]T` / `[N]T` for an array whose Go element cannot be
  NULL (`integer[]` into `[]int32`, a composite array into `[]Item`), and both modes note that
  PostgreSQL never promises an array's elements are non-null even when the column is; `interval`
  into `time.Duration` carries a Lossy note (a month flattened to 30 days disagrees with the
  server's calendar arithmetic). vet no longer skips a `sqlshape.Query[R, P]` called through a
  package-level variable (`var q = sqlshape.Query[R, P]; q(...)`), and checks the fields an `{{if}}`
  reads through a nested call (`{{if gt (len .Xs) 0}}`) against `P`. Migrations: rewriting a
  generated column's expression, or its `STORED` / `VIRTUAL` kind (PostgreSQL 18), drops and restores
  the indexes, constraints and referencing foreign keys the `DROP COLUMN` would take with it, instead
  of losing the index or emitting DDL the server refuses (2BP01); a kind-only change is no longer a
  no-op. An `unfiltered` / `waive` written above a `CREATE TABLE` is told where it belongs. Kept as
  they were, and now written down: `paired` requires the write, not matching values; `RETURNING` a
  `sensitive` column is reading it.

- `pgtest` and `mysqltest` are sqlshape's own test tooling, like `check/*`: they boot a real server
  for the examples, the oracles and the conformance tests, and carry no compatibility promise. An
  application tests its database with the server it runs on; the agreement between the checker
  and the server is sqlshape's to keep. The documentation no longer presents them as part of the
  API, and is reorganized for a reader who uses one database: `docs/postgres.md` and
  `docs/mysql.md` gather what is each database's (the declaration, the Go type table, the
  constraint names and error numbers, the runtime, what has no counterpart), the README has a
  quickstart per database, and `checks.md` / `runtime.md` are written for both.

### Added

- `sqlshape.Error(code)` mirrors a schema's `-- sqlshape: error <code> = <Name>` annotation (a
  trigger's or routine's own raised failure mode) in Go: `var OrderTooLarge =
  sqlshape.Error("30001")`. `go vet` checks it both ways -- the declaration's code must be one the
  schema declares under that exact Name, no two declarations in a package may claim the same code,
  and every named failure mode a package's statements can raise must have such a declaration
  somewhere the checker can reach (a Fact carries the binding across packages, the way a declared
  type's does). An expect line may spell the failure by its code or by the Name, whichever reads
  better. `postgres.Violates` / `mysql.Violates` now take any `~string`, so a declared
  `sqlshape.Failure` reads the same as the raw code always did; neither reads the schema, so they
  judge only by the code the value holds, never by Name. The PostgreSQL runtime wraps a custom
  SQLSTATE into `ConstraintError` for any statement with an expect line at all now (it cannot
  resolve a Name back to the code the checker matched statically). `examples/3-database-api`,
  `examples/4-everything` and `examples/5-mysql` declare their triggers' and functions' errors this
  way and test `Violates` against a running server.

- `sqlshape diff`, `apply` and `verify-schema` work on a MySQL schema. Both sides are read as the
  server's own `SHOW CREATE TABLE` / `SHOW CREATE VIEW`, parsed by the loader that reads
  `schema.sql`; the target is canonicalized in a scratch database on the `-db` server (dropped when
  done), or on a `mysqld` from `PATH` with `-from`. Compared: tables, columns (their position
  included, which the plan settles with `MODIFY COLUMN ... AFTER`), keys, foreign keys, checks and
  views; not seeded rows. The plan emits MySQL's own definitions; `-- @migrate`
  is the same grammar, with `enum` naming the ENUM column. `apply` runs statement by statement
  (MySQL's DDL commits implicitly) and reports the statement that failed. The MySQL analyzer now
  rejects an expression the server rejects in an INSERT's or UPDATE's value (`SET total = nope`,
  1054) instead of typing it as unknown.
- MySQL: `CREATE TRIGGER` / `CREATE PROCEDURE` / `CREATE FUNCTION` (`DEFINER`, `IF NOT EXISTS`, the
  characteristics), and `DROP` / `ALTER` of each, are read by the schema loader with no `DELIMITER`
  needed (a compound body's own `;` is read as one statement; a mysql-client `DELIMITER x` line is
  read too), the server's own CREATE-time refusals reported as problems (no such table 1146,
  already exists 1359 / 1304, no such trigger or routine 1360 / 1305, no such `FOLLOWS` /
  `PRECEDES` anchor 3011); `DROP TABLE` takes a table's triggers, `RENAME TABLE` moves them.
  `-- sqlshape: error <key> = <Name>` above a trigger or routine names its failure mode, the way
  it already does for PostgreSQL. Migrations read triggers and routines back with `SHOW CREATE
  TRIGGER` / `PROCEDURE` / `FUNCTION` (`DEFINER` dropped; a trigger's `FOLLOWS` synthesized from
  `information_schema` since `SHOW CREATE TRIGGER` never spells one), diff compares them by
  definition text, and the plan replaces a changed one with `DROP` + `CREATE` (MySQL has no
  `CREATE OR REPLACE TRIGGER`) in dependency order: a trigger's `DROP` before its table's, a
  routine's `CREATE` before the views and tables, a trigger's `CREATE` after the backfills so it
  does not fire on the migration's own writes.
- MySQL: the checker reads trigger and stored routine bodies once per schema (cached, so a
  routine called from several statements or firing several triggers analyzes once). `NEW.col` /
  `OLD.col` type as the trigger's own table's columns (unknown column 1054, the wrong row for the
  event or timing 1362 / 1363 -- a `NOT NULL` column's `NEW.col` can still be NULL in a BEFORE
  trigger, measured); a `DECLARE`d variable or a routine's parameter shadows a column of the same
  name. `IF` / `CASE` / `LOOP` / `WHILE` / `REPEAT`, labelled blocks with `LEAVE` / `ITERATE`,
  `RETURN`, `SET`, `SELECT ... INTO`, cursors, `CALL`, `SIGNAL` / `RESIGNAL` and
  `DECLARE ... HANDLER FOR` are walked, with the server's own CREATE-time refusals (1308, 1313,
  1320, 1324, 1327, 1328, 1222, 1415, 1422) reported on the definition; a trigger writing its own
  table is 1442 on every one of the 18 timing x event x write combinations (measured). A statement
  on a table takes the SIGNALs of its triggers for its event (`REPLACE` fires the INSERT and
  DELETE triggers, `ON DUPLICATE KEY UPDATE` the INSERT and UPDATE ones, measured), the constraints
  the trigger's own writes may violate, and what those writes fire in turn (a cycle is cut). A
  SIGNAL's key is its `MYSQL_ERRNO` in decimal when it sets one, else its SQLSTATE (SQLSTATE class
  `01` a warning, an unhandled `02` 1643, anything else unhandled 1644); a named `CONDITION`
  resolves to its value. A `DECLARE ... HANDLER FOR` absorbs the matching failure modes of its
  block (`SQLEXCEPTION` everything but the `01` and `02` classes, `SQLWARNING` / `NOT FOUND` their
  class, a SQLSTATE or number itself); `INSERT` / `UPDATE IGNORE` absorbs none of a trigger's
  SIGNALs (measured). `SELECT ... INTO` carries 1172 unless the query is provably at most one row.
  A call to a stored FUNCTION the catalog does not know resolves to the schema's (an unqualified
  name that is also a native function is the native one, as on the server; `db.f` names the
  routine), typed from `RETURNS` (always nullable), taking 1318 for a wrong argument count and
  1305 for no such routine, and bringing the body's own failure modes to the calling statement; a
  function that writes a table the statement already references is 1442 on every run (reading the
  table is enough, measured). `CALL p(...)` analyzes `IN` / `INOUT` arguments by the parameters, an
  `OUT` argument must be a variable (1414; a `?` is one), facts `Kind Call`, the result columns
  from the body's own INTO-less `SELECT`s when they agree (none: no columns; several of different
  shapes: the checker's own error), and the body's own failure modes. Cross-checked against a
  running `mysqld`.
- The MySQL runtime: a SIGNAL raised by a trigger or routine comes back as a `ConstraintError`
  whose `Key` is the SQLSTATE when the server numbers it itself (1644, 1643), or the decimal
  `MYSQL_ERRNO` when the SIGNAL set its own error number; `Violates(err, "45000")` /
  `Violates(err, "30001")` read like the checker's own keys. A `01xxx` SIGNAL is a warning and the
  statement succeeds.
- The schema declares the server settings its judgments depend on, one per line next to the
  version: `-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'`,
  `-- sqlshape: server lower_case_table_names = 1`. MySQL reads these two (any other variable, a
  mode 8.4 does not have, or a value out of range is a problem of the schema; PostgreSQL reads no
  variable yet and reports each one). The checker follows them: the parser's bits
  (`ANSI_QUOTES`, `PIPES_AS_CONCAT`, `IGNORE_SPACE`, `NO_BACKSLASH_ESCAPES`,
  `HIGH_NOT_PRECEDENCE`, `REAL_AS_FLOAT`), `ONLY_FULL_GROUP_BY` for the group check, strict mode
  for the nullability of string functions and the 1048 failure mode (without it only a single-row
  `INSERT` / `REPLACE` rejects a `NULL`), `NO_UNSIGNED_SUBTRACTION`, and `lower_case_table_names`
  for how table and view names compare (1 lower-cases them, 2 ignores case). Without a
  declaration the checker assumes the server's defaults, as it did. `mysqltest.Start` passes the
  declared variables to `mysqld`, so the application's tests and the analyzer's conformance
  tests run under the declared mode (22 statements and 6 writes verified against 8.4 under
  `''`, `ANSI` and `NO_UNSIGNED_SUBTRACTION`); `mysql.Verify(ctx, db, schemaSQL)` compares a
  connection's session `@@sql_mode` and the server's `lower_case_table_names` with the
  declaration. `x/sqlmode` holds MySQL's sql_mode names and bits.
- `mysqltest.Start` fails as soon as `mysqld` exits (an option it rejects) instead of waiting a
  minute, and quotes the `[ERROR]` lines of its log.
- MySQL: an aggregate without `GROUP BY` proves one row wherever the aggregate sits in the
  select list or `HAVING` (`COALESCE(SUM(total), 0)`, `COUNT(*) + 1`, `MAX(id)` alike), not only
  when every select item is a bare aggregate call, so a `SELECT COALESCE(SUM(x), 0) INTO v` in a
  routine no longer carries 1172 and the same query passes `One`. Which aggregates are the
  block's own is the group check's answer (a subquery's aggregate over its own columns leaves the
  outer query one row per input row, as before).
- MySQL: `CREATE EVENT` is read (both schedule forms, `STARTS` / `ENDS`, `ON COMPLETION`, the
  status, `COMMENT`), `ALTER EVENT` (each clause replacing its part, `RENAME TO` included) and
  `DROP EVENT` are applied. An event's body is analyzed like a routine's: a table it names that the
  schema does not have is a schema problem (the server accepts it at CREATE time and fails at every
  run, measured), its statements are judged for the obligations, a `RETURN` is 1313. `diff` /
  `apply` / `verify-schema` manage events: read back through `SHOW CREATE EVENT`, compared by
  schedule, bounds, options and body, a changed one a `DROP EVENT` and a `CREATE EVENT`. A time
  `schema.sql` leaves to the server (an omitted or computed `STARTS`, a computed `AT`) is not
  compared, since the server fills it in at creation and reads it back as a literal (measured);
  a literal time is compared as written. Formerly `CREATE EVENT` was refused by the loader.
- MySQL: a qualified function call, `db.f(...)`, names the schema's own FUNCTION even when a
  native function has the same name (an unqualified `abs(-1)` is still the native one, `db.abs(-1)`
  the schema's, `db.nope(1)` 1305; measured), where it used to type as the native function. The
  1442 overlap check now also counts a table the statement names in `FROM` without reading a
  column of it, follows writes through nested calls (a function that `CALL`s a procedure writing
  t, or `RETURN`s a function that does), runs per statement inside a trigger's or routine's body
  (`UPDATE t SET v = f(v)` with f writing t is 1442 when the routine is `CALL`ed; two separate
  statements are not), and counts a trigger's own table against every statement of its body (a
  trigger calling a function that writes the trigger's table is 1442, as a direct write already
  was); a `CALL` inside a body is resolved like a top-level one (1305 / 1318 / 1414) and the
  callee's failure modes and writes become the body's. All measured against mysqld 8.4
  (`TestNestedCallsServer`). An AFTER trigger's `NEW.col` following the column's declared
  nullability is now measured rather than inferred.

### Changed

- The checker is the product; the runtime is a library per database. The root module
  `github.com/kr9ly/sqlshape/v2` now holds only the declarations (`Query`, `One`, `Stmt`, `Render`,
  the row-binding rules) and has no dependencies. Running a statement on pgx is the module
  `github.com/kr9ly/sqlshape/postgres/v2`: `postgres.Run(ctx, db, stmt, p)`, `Collect`, `First`,
  `Exec`, `Get` / `Find` / `ExecOne` for `One`, and `MatView`, `Copy`, `Batch`, `LoadUserTypes`,
  `ConstraintError`, `Violates`, `ErrNoRows` moved there (the methods `stmt.Run(ctx, db, p)` are
  gone: a module cannot add methods to another's type). `Labelled` and `UnknownLabelError` stay in
  the root. `pgtest` is its own module. The PostgreSQL analyzer and migration tools are the module
  `github.com/kr9ly/sqlshape/check/postgres/v2`, the MySQL analyzer `check/mysql` (was `mysql`);
  both are tool-facing, imported by the binary and `pgtest`. An application's go.mod carries the
  root, its runtime and its own driver, nothing of the checker's.
- The markers the checker recognizes are configurable: `-query=pkg.Func,pkg.Other:one` registers
  a program's own generic marker functions, read like `Query` / `One`. A program may run its
  statements through a runtime of its own; what only sqlshape's runtime promises (the rendered SQL
  is the checked SQL, violations come back under the expect line's names, `One` rejects a second
  row) is stated in docs/runtime.md.
- The `One` proof is written once, over the statement's facts, for every dialect: the
  PostgreSQL analyzer no longer proves cardinality on its own tree but records what the proof
  needs (unique keys with a partial index's predicate, temporal keys, a view's or subquery's
  body with its output columns, GROUP BY terms, LIMIT / VALUES / set-operation shape). Two
  verdicts change: a `GROUP BY 1` (an ordinal, or an alias) names an output column and is no
  longer read as a pinned constant, so such a statement is not proved single; a `LIMIT 1`
  over a `UNION` and an aggregate over a `FULL JOIN` are one row and are proved.

### Added

- The MySQL runtime, `github.com/kr9ly/sqlshape/mysql/v2`: `Run` / `Collect` / `First` /
  `Exec`, `Get` / `Find` / `ExecOne`, over `database/sql` with go-sql-driver/mysql; `{{.X}}`
  becomes `?` on the wire with the arguments in placeholder order, rows map by the shared
  binding rules, a constraint violation is a `ConstraintError` under the schema's name for it
  (`Violates(err, "users_email_key")`). `github.com/kr9ly/sqlshape/mysqltest/v2` boots the
  `mysqld` on PATH with the application's schema for its tests (the analyzer's oracle runs on
  it too). `examples/5-mysql` shows the whole path.
- `One` on MySQL, and the proof it rests on written once for every dialect: the analyzer
  records a statement's facts (`x/facts`: leaves with their unique keys, the equalities of
  WHERE and of the joins, what they fix) and `x/cardinality` proves from the facts alone that
  at most one row is touched — a unique key fixed by equality, carried across joins, a
  derived table proved inside, an aggregate without GROUP BY, `LIMIT 1`, a single `VALUES`
  row. The MySQL analyzer also resolves `ORDER BY`, `GROUP BY` and `HAVING` the way the
  server does: positions, select-list aliases, a table column shadowing an alias in
  `GROUP BY`, `HAVING` reaching the select list; unknown names are reported in MySQL's
  words (`in 'order clause'`, `in 'group statement'`, `in 'having clause'`).
- The obligations and the failure modes on MySQL, through the same contract PostgreSQL uses.
  The `-- sqlshape:` directives above a `CREATE TABLE` / `CREATE VIEW` (`require ...`,
  `aggregate`, `context`, `sensitive`, `transitions`, `visible where`; a view's `unfiltered` /
  `waive`) are read by the MySQL schema loader, a declared predicate is typed against the
  table by the MySQL analyzer, and every statement (and every view body) is judged: the
  analyzer now records the columns a statement uses, the nested blocks (derived tables, CTE
  bodies, the subqueries of its conditions, the arms of a set operation) and what a write
  stores. `-- sqlshape: expect` on MySQL: a write may violate a `PRIMARY` / `UNIQUE` key
  (MySQL error 1062; a key the server numbers itself, or one left NULL, cannot), a `FOREIGN
  KEY` (1452 from the child, 1451 from the parent it still refers to, following `ON DELETE /
  UPDATE CASCADE`), a `CHECK` (3819) and NOT NULL (1048, `table.column`, dropped when the Go
  type cannot be nil); `INSERT / UPDATE / DELETE IGNORE` violates nothing and `ON DUPLICATE
  KEY UPDATE` absorbs the insert's unique violations. The constraints are named as the server
  reports them (`PRIMARY`, `orders_ibfk_1`, `t_chk_1`), verified against a running `mysqld`;
  `mysql.Violates(err, "users.name")` matches a NOT NULL violation by that spelling.
- The MySQL server's checks on a grouped, aggregated or `DISTINCT` block, as sql_mode
  `ONLY_FULL_GROUP_BY` runs them (`Group_check`): a select-list, `HAVING`, `ORDER BY` or window
  `PARTITION BY` / `ORDER BY` expression must be a `GROUP BY` expression, an aggregate, or made
  of columns functionally dependent on the group columns -- through a table's `PRIMARY` /
  `UNIQUE` key (a nullable key column only where a conjunct rejects its NULL), the `WHERE` and
  inner-join equalities `col = col` and `col = constant` (a literal or an outer reference; not a
  parameter), an outer join's `ON` into its nullable side (a `WHERE` that rejects the nullable
  side's NULL makes the join inner, a non-deterministic `ON` gives nothing), and a derived
  table's, view's or CTE's body seen through its outputs; `ROLLUP` allows only the group
  expressions themselves. MySQL error 1055 with `GROUP BY`, 1140 for an aggregated query without
  it (whose `ORDER BY` the server drops unchecked). With them the rules the check rests on: a
  column named outside an aggregate in `HAVING` must be a select-list column or alias or a `GROUP
  BY` column (1054, also for a nested query's reference to it), with `DISTINCT` an `ORDER BY`
  expression not in the select list may read only select-list columns (3065), and an aggregate in
  the `ORDER BY` of a query that aggregates nowhere else (3029) or of a set operation (3028) is
  rejected. 376 statements agree with a running `mysqld`. The derived leaves of the facts now
  carry `Outputs` on MySQL too, so `One` looks into a derived table's or view's body.
- MySQL: a parenthesized join as a side of a join, a set operation's `ORDER BY` expression
  (typed against the result columns), `x NOT IN (subquery)`, `QUALIFY` rejected as 8.4 does
  without the hypergraph optimizer (6037), `<=> ALL / ANY` as the syntax error it is (1064).
  The facts record an `EXISTS` or single-column `IN` subquery conjunct as an Exists predicate
  carrying the body (a correlated reference to the enclosing block is an Outer term), so the
  obligations judge the body under its predicate; the conjuncts the server folds away (`WHERE 1
  = 1`) are dropped; a view body's positions are cleared. A MySQL placeholder carries the table
  column it stands for (compared with, or stored into), so vet binds Go named types to key
  identities and diffs `ENUM` labels against typed constants on MySQL too. `REGEXP_LIKE` is a
  bigint(1), `REGEXP_REPLACE` nullable, `QUOTE` of a binary a character string: the probe
  golden has no disagreement left.
- MySQL, after an adversarial round against a running mysqld (7 lanes, 21 findings, every one
  a regression test now): a `USING` or `NATURAL` join coalesces its common columns (an
  unqualified name is not ambiguous, `SELECT *` lists it once); `GROUP BY` on the alias of an
  aggregate is 1056; multi-table `DELETE t1, t2 FROM ...` / `DELETE FROM t1 USING ...` is
  analyzed, its targets judged for the failure modes; the select list is resolved before the
  `WHERE`, as the server does, so its error comes first; a `BIT` operand is unsigned in
  arithmetic; `WITH ROLLUP` makes every non-aggregated result column nullable; `STR_TO_DATE`
  follows a literal format into `DATE` / `TIME` / `DATETIME`; `col -> 'path'` and `->>` parse.
  The facts no longer take an expression bound to the execution (`RAND()`) for a known value,
  so `GROUP BY RAND()` is not one row and `tenant_id = FLOOR(RAND() * 3)` pins nothing. The
  failure modes: `REPLACE` violates no key but may violate a referencing foreign key (1451), a
  multi-table `UPDATE` judges each assignment against its own table, an `UPDATE` through an
  updatable view judges the base table (its parameters carry the base column too). The
  schema loader names unnamed `CHECK` and `FOREIGN KEY` constraints the server's way, one above
  the highest generated number, so `ALTER TABLE ... DROP CHECK t_chk_1` finds its constraint,
  and reads table and view names case-sensitively (lower_case_table_names=0). The runtime's
  `ExecOne` accepts the 0 / 1 / 2 rows `INSERT ... ON DUPLICATE KEY UPDATE` and `REPLACE`
  report for the one row their key names.
- MySQL, a first slice. A `schema.sql` that declares `-- sqlshape: mysql 8.4` is loaded by the
  MySQL schema loader and every `Query` is judged by the MySQL analyzer: the statement's own errors
  (unknown table or column, ambiguity, syntax, with MySQL's message and error number), result
  columns against `R` by name, type and nullability, parameters against `P` where their context
  types them (compared with or assigned to a column, `LIMIT`, the argument positions a function
  declares). Single-block SELECT, INSERT, UPDATE and DELETE over base tables. Expressions are typed
  by the server's own rules, read out of its source: arithmetic promotion (`Item_num_op`), `CASE` /
  `IF` / `COALESCE` / `GREATEST` through `field_type_merge`, casts, aggregates, and every function of
  the native registry through its Item class family and `resolve_type` facts, so a `SUM` is a
  nullable decimal, a comparison a `bigint(1)` a Go `bool` can carry, `LENGTH(?)` types its
  placeholder as a string. The rules are checked against a real mysqld (a local test, not a CI
  dependency): every registry function over representative argument types, 5,267 statements,
  agrees with the server on type and nullability but for 10. Subqueries (scalar, `IN`, `EXISTS`,
  `ANY` / `ALL`, correlated), derived tables (`LATERAL` too), views, common table expressions
  (recursive too), `UNION` / `EXCEPT` / `INTERSECT` and `INSERT ... SELECT` are analyzed with the
  server's rules for what they produce: a scalar subquery is NULL unless its query is guaranteed a
  row, a recursive CTE's columns are always nullable, a derived table the server materializes
  retypes a small integer expression to an int, a merged view is updatable. What has no rule yet
  (user variables, the temporal hybrids such as `ADDTIME`) is accepted with a note; `One`,
  `MatView`, `Copy` and the obligations are not supported yet. Parameters stay `{{.X}}` in the
  template; the analyzer speaks `?` to MySQL.

### Changed

- The binary is its own Go module, `github.com/kr9ly/sqlshape/cmd/sqlshape/v2`, under the GNU
  General Public License v2; install it with `go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest`.
  Everything a checked program imports stays Apache 2.0. Every module's path carries the `/v2`
  major-version suffix Go requires of a 2.x module, so an import of 1.x is
  `github.com/kr9ly/sqlshape` and of 2.x `github.com/kr9ly/sqlshape/v2`. The modules of the
  repository release together under one version, tagged `vX.Y.Z` and `<module>/vX.Y.Z` on the
  same commit.

## [1.2.0] - 2026-09-09

### Added

- PostgreSQL 18. `schema.sql` declares its version with `-- sqlshape: postgres 18`, and the schema
  and every statement are judged with 18's grammar and catalog: `RETURNING old` / `new` (with
  `WITH (OLD AS ..., NEW AS ...)`), `WITHOUT OVERLAPS` keys and `PERIOD` foreign keys, `NOT ENFORCED`
  constraints (which are no failure mode), named `NOT NULL` constraints, `VIRTUAL` generated columns,
  and 18's own checks (a JSON_VALUE DEFAULT whose collation is not the RETURNING type's, a JSON path
  that is not a jsonpath, stricter datetime and aclitem input, an outer-level aggregate over a nested
  CTE). `pgtest` and the migration commands run the declared version's PostgreSQL; `diff`, `apply`
  and `verify-schema` warn when the database runs another major version than the schema declares.
- `One` proves a single row through a temporal key: the scalar columns fixed by equality and the
  range column equal to a known value or containing a known point (`valid_at @> {{.Day}}::date`).

### Fixed

- `SELECT DISTINCT ON` expressions are matched to the leading `ORDER BY` expressions by expression,
  not by where each was written; the same expression in both no longer fails with 42P10.
- An UPDATE that assigns a generated column reports PostgreSQL's message (`column "b" can only be
  updated to DEFAULT`) rather than INSERT's.

### Changed

- `schema.sql` must declare its PostgreSQL version (`-- sqlshape: postgres 17`); a schema without
  the line is not read. Add the line to move from 1.1.
- The parser (libpg_query) is compiled to WebAssembly and run by wazero instead of being linked
  through cgo. `go install` no longer needs a C compiler, and the binary is a plain Go build.
  Parse trees, error messages and positions are unchanged.
- Prebuilt binaries for every platform, cross-compiled by one job: Linux, macOS and Windows, amd64
  and arm64 each (Windows and Intel macOS are new).

## [1.1.0] - 2026-09-08

### Added

- Obligations: `schema.sql` declares, above a `CREATE TABLE` or `CREATE VIEW`, what every statement
  touching the relation must do, and vet judges each statement against it
  ([checks.md, Part 2](docs/checks.md#part-2--rules-the-schema-declares)). The general form is
  `-- sqlshape: require <what> [on <kinds>]`, where `<what>` is an SQL predicate, `pinned(col)`,
  `immutable(col)`, `via view`, `never`, `paired(table)` or `single`.
- `aggregate root (child, ...) [lock version]`: the tables form one aggregate; children are reached
  through the root's key, one statement touches one aggregate, and with `lock` every write names
  the root's version.
- `transitions col: a -> b, b -> c | d`: a status column is a state machine; an UPDATE setting it
  must compare-and-set from a declared predecessor.
- `sensitive label: col, col` and `context name: may read label`: labelled columns are readable
  only in a context that allows the label.
- `context name: require ...; waive ...`: obligations that differ per caller, selected by a
  package's `// sqlshape: context name` comment, vet's `-context`, or `check -context`.
- `-- sqlshape: waive table [obligation]` in a statement or a view definition: an opt-out,
  reported with `-strict`.
- Cross-table predicates: `require EXISTS (SELECT 1 FROM parent p WHERE p.id = parent_id AND ...)`
  is discharged by a join or subquery that witnesses it.
- `sqlshape check`: the same judgments for SQL outside Go (files or stdin), every judgment printed
  as an audit line, exit code 1 on a failure.
- The `internal/facts` package: what a statement provably does, in a form that does not depend on
  the SQL dialect, so that the obligation checker can be shared with other dialects later.

### Changed

- `visible where`, `-require-columns`, `-no-table-reads` and `-no-tables` are now obligations
  (`require <expr> on read`, `require pinned(col)`, `require via view`, `require via view on all`)
  and are judged by the same checker. Judgment is per occurrence of a table: a self join or a
  subquery reading the table again owes the obligation again; `-require-columns` used to accept a
  statement that pinned the column anywhere in it. A row-level security policy or a composite
  foreign key can discharge a pin. MERGE's ON is judged like a WHERE.
- Writes are counted by what the statement does to the table: each MERGE branch is a write of its
  own kind, an `INSERT ... ON CONFLICT DO UPDATE` is an insert and an update, `TRUNCATE` is a
  delete, and a write through an automatically updatable view is a write to the base table.
- `pinned` on an UPDATE is satisfied by the WHERE only; assigning the column in SET is not a pin.
- A `-- sqlshape:` directive above a statement that takes none (`ALTER TABLE`, `COMMENT ON`) is a
  schema problem instead of being ignored.
- The docs are split: the README is an overview; [docs/](docs/) has checks, templates, runtime,
  migrations and flags, in English and Japanese.

### Fixed

- A vet failure shared by every branch of a template is reported once without the branch tag.
- A diagnostic on a column referenced only in GROUP BY had no position.

## [1.0.0] - 2026-09-07

First release: `sqlshape.Query[R, P]` / `One[R, P]` templates checked by `go vet` against
`schema.sql` (a pure Go analyzer of PostgreSQL 17 syntax and types, with PostgreSQL itself as the
test oracle), the runtime on pgx, and `sqlshape diff` / `apply` / `verify-schema` for migrations
from a declared schema.

[Unreleased]: https://github.com/kr9ly/sqlshape/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/kr9ly/sqlshape/compare/v1.2.0...v2.0.0
[1.2.0]: https://github.com/kr9ly/sqlshape/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/kr9ly/sqlshape/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/kr9ly/sqlshape/releases/tag/v1.0.0
