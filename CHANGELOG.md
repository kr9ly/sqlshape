# Changelog

Notable changes to sqlshape, newest first. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/). Behaviour of the checker that makes a
statement pass or fail is listed under Changed even when the old behaviour was a bug, since a
passing program may start to fail. A release candidate (`v1.1.0-rc.1`) carries the section of the
release it is a candidate for.

## [Unreleased]

## [2.0.0] - 2026-09-14

### Added

- MySQL 8.4. A `schema.sql` that declares `-- sqlshape: mysql 8.4` is checked by a MySQL
  analyzer: the parser and lexer are the server's own, the function catalog is read from the
  server's source, and the verdicts (types, nullability, errors, the `ONLY_FULL_GROUP_BY` check)
  are measured against a running `mysqld`. `One`, the obligations and the failure modes work
  through the same contract PostgreSQL uses, with the constraints named as MySQL reports them
  (`PRIMARY`, `orders_ibfk_1`, `t_chk_1`) and `INSERT IGNORE` / `ON DUPLICATE KEY UPDATE` /
  `REPLACE` judged as the server runs them. What has no MySQL counterpart is listed in
  [docs/mysql.md](docs/mysql.md).
- MySQL triggers, stored procedures and functions, and events: their bodies are analyzed
  (`NEW` / `OLD`, variables, control flow, cursors, `SIGNAL` / `RESIGNAL` / `HANDLER`, `CALL`),
  a statement inherits the failure modes of the triggers it fires and the routines it calls,
  and the server's own refusals (a trigger writing its own table, 1442, among them) are
  reported. `-- sqlshape: error <code> = <Name>` names a raised error, as on PostgreSQL;
  `-- sqlshape: not null` above a `CREATE FUNCTION` declares it never returns NULL.
- On MySQL the checker also reads `LOAD DATA` (an INSERT of the file's rows, with the server's
  own 1263 for a NULL field in a `NOT NULL` column), `LOCK TABLES` / `UNLOCK TABLES`,
  `SELECT ... INTO OUTFILE` / `DUMPFILE` (no result set) and a `SELECT` with a trailing
  `FOR UPDATE` / `FOR SHARE`; the loader applies `ALTER VIEW`. Inside a routine or trigger body
  the first three are the server's own 1314.
- The statement facts the `One` proof and the obligations are judged on are tested against a
  running server the way the checker's types are: `x/stmtprobe` generates schemas with rows
  and statements over them (joins, views, derived tables, CTEs, `EXISTS` / `IN` subqueries,
  `GROUP BY`, `UNION ALL`, writes), and refutes every claim of the facts -- a predicate holding
  on every row, a fixed column, an at-most-one-row proof, the value a write stores -- with the
  rows the server returns, on PostgreSQL 17 and MySQL 8.4. It found, and this release fixes:
  MySQL accepted an `UPDATE` / `DELETE` whose subquery reads the table it writes (the server's
  1093, or 1443 through a view), and PostgreSQL recorded a subquery's reference to a joined
  table's column, when the subquery sits in the join's `ON` clause, as a value known before
  the statement runs rather than as the row's own column. The same probe judges the predicted
  failure modes: writes against a schema carrying every constraint kind (named and unnamed
  keys, foreign keys with each `ON DELETE` action, checks, `NOT NULL`; `IGNORE` / `REPLACE` /
  `ON DUPLICATE KEY UPDATE` and `ON CONFLICT`), with every constraint error the server raises
  required to be a predicted violation under a key `Violates` matches.
- MySQL trigger, procedure and function bodies are tested against the server's own
  CREATE-time verdict the same way (the body probe in `check/mysql/internal/analyze`): every
  construct the server refuses when the body is created, mixed into generated bodies, must
  be refused by the checker too, and nothing the server accepts may be. It found, and this
  release fixes: `SET OLD.col` in an INSERT trigger is 1362, not 1363 (the server checks the
  write before the event), `SET NEW.col` / `SET OLD.col` outside a trigger is 1193, and a
  FUNCTION with no `RETURN` is a schema problem (1320) even when the checker stops at an
  earlier run-time certainty of the same body.
- MySQL's own test corpus (mysql-test/t: 1,281 files, 137,000 statements) replays against a
  running mysqld and the analyzer side by side (`check/mysql/internal/analyze`'s corpus probe,
  the counterpart of PostgreSQL's regress probe): each file on a server of its own, the
  analyzer's schema rebuilt from `SHOW CREATE` after every DDL under the session's `sql_mode`,
  a SELECT's columns compared by name, type family and nullability, an error by number. The
  first run agrees on 44,342 statements and lists 4,605 disagreements as the baseline the next
  rounds work down; two analyzer crashes it found (a routine variable shadowing the column an
  INSERT or UPDATE in the body assigns) are fixed. The second round fixes what its largest
  classes were: a window function's integer widens through the window's temporary table
  (`INT` / `BIGINT` to `BIGINT`, narrower to `INT`, `YEAR` to `INT UNSIGNED`), `USER()` /
  `CURRENT_USER()` / `DATABASE()` / `SCHEMA()` / `CURRENT_ROLE()` are character strings,
  `DATE'...'` / `TIME'...'` / `TIMESTAMP'...'` literals are typed and never NULL, `<=>` is never
  NULL, a `ROLLUP` constant stays `NOT NULL`, `SELECT ... INTO @var` returns no columns in
  either position (a body's trailing `INTO` now counts as an INTO too), and `INSERT INTO t
  VALUES ()` inserts a row of defaults instead of 1136; the probe itself stops judging a file in
  a legacy encoding, a statement after a DDL under `LOCK TABLES`, a `CALL` whose body reads a
  system schema or a temporary table, a `db.routine()` call, and a column name the
  connection's character set rewrote. The third round adds the literal store rules: a
  literal a column can never keep -- an integer out of range, a string that is no number, a
  date `str_to_datetime` rejects under the `sql_mode`, a `TIME` past 838 hours, a string
  longer than the column, an `ENUM` / `SET` member that does not exist, a number or a string
  into a spatial column -- is the statement's own error with the server's number and message
  (1264 / 1265 / 1292 / 1366 / 1406 / 1416), in strict mode and outside `IGNORE`; 140 such
  stores are pinned against mysqld; the corpus baseline falls to 3,619.
- `mysqltest.StartOwn` boots a server of its own even under `mysqltest.Main`, for a test that
  changes accounts, global variables or other databases.
- `postgres.WrapError(err)` and `mysql.WrapError(err)` give an error from a statement run
  outside `Run` / `Exec` the wrapping `Violates` judges.
- `sqlshape.Error(code)` declares a schema's named error in Go (`var OrderTooLarge =
  sqlshape.Error("30001")`); `go vet` checks the declaration against the schema both ways, an
  expect line may use the Name, and `Violates` accepts the value.
- `-- sqlshape: server <var> = <value>` declares the server settings the checker's judgments
  depend on (MySQL: `sql_mode`, `lower_case_table_names`); the checker follows them, and
  `mysqltest.Start` starts the server with them.
- The MySQL runtime, `github.com/kr9ly/sqlshape/mysql/v2` (`Run`, `Collect`, `First`, `Exec`,
  `Get` / `Find` / `ExecOne`, `ConstraintError`, `Violates`) over `database/sql`, and
  `github.com/kr9ly/sqlshape/mysqltest/v2`, which boots the `mysqld` on `PATH` with the
  application's schema; a package whose `TestMain` runs through `mysqltest.Main` boots one
  server per set of declared settings and hands it from test to test instead of booting one
  per `Start`. `examples/5-mysql` shows the whole path.
- `sqlshape diff`, `apply` and `verify-schema` on MySQL: both sides are read as the server's own
  `SHOW CREATE` output, the target canonicalized in a scratch database on the same server, and
  the plan written in MySQL's own DDL, statement by statement. Tables with their options,
  columns with their position, keys, foreign keys, checks, views, triggers, routines, events
  and partitions are compared; seeded rows are not.
- Partitions are migrated on both dialects: a partitioned table is created, a partition added,
  dropped (under a `-- @migrate drop` declaration, since it takes its rows), attached, detached,
  its bound moved with the rows that no longer fit moved along, and a populated table can be
  partitioned after the fact, unpartitioned, or repartitioned, the rows redistributed by the
  server. MySQL covers `RANGE`, `LIST`, `COLUMNS`, `HASH`, `KEY`, `LINEAR` and subpartitioning,
  with `-- @migrate drop partition <table>.<partition>` for a partition that goes.
- The migration planner is tested against a real server the way the checker is: generated
  schema pairs, every kind of change the diff can report, applied with rows in the tables on
  PostgreSQL 17 and 18 and on MySQL 8.4, with the plan required to land on the declared schema
  and leave nothing to plan afterwards ([docs/migrations.md](docs/migrations.md#how-the-plan-is-tested)).
  The orderings and omissions this found are fixed and pinned as regression tests.
- Row-level security, composite types, rules, extensions, schema moves, standalone sequences,
  procedures, aggregates, `EXCLUDE`, partial and expression indexes, `NULLS NOT DISTINCT` and
  `DEFERRABLE` are handled by the PostgreSQL migration plan; `FULLTEXT` / `SPATIAL` / functional
  / `INVISIBLE` keys, `ROW_FORMAT`, table charset changes and column comments by MySQL's.
- `x/dialect.Type.ElemNotNull`: an array whose elements are provably not NULL
  (`array_agg((a, b)::t)`, an inner-joined `NOT NULL` column) binds to `[]T` without the "may
  contain a NULL element" note; `col:",notnull"` on an array field asserts the elements too.
- `-query=pkg.Func,pkg.Other:one` registers a program's own marker functions, read like
  `Query` / `One`.
- Every example in `docs/` runs as a test: the checker must produce exactly the diagnostics the
  docs claim.

### Changed

- The checker is the product; the runtime is a library per database. The root module
  `github.com/kr9ly/sqlshape/v2` holds only the declarations and has no dependencies. Running
  a statement on pgx is `github.com/kr9ly/sqlshape/postgres/v2` (`postgres.Run(ctx, db, stmt,
  p)`; the methods `stmt.Run(ctx, db, p)` are gone), on MySQL `.../mysql/v2`. The analyzers are
  `check/postgres` (Apache 2.0) and `check/mysql` (GPLv2, MySQL's own parser), the binary
  `cmd/sqlshape` (GPLv2, `go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest`);
  nothing under them is linked into your program. Every module path carries `/v2`, and all
  modules release together under one version.
- The `One` proof is written once, over the statement's facts, for every dialect. Two verdicts
  change on PostgreSQL: `GROUP BY 1` (an ordinal or an alias) is no longer a pinned constant, so
  such a statement is not proved single; a `LIMIT 1` over a `UNION` and an aggregate over a
  `FULL JOIN` are proved.
- A template's `if` / `with` / `range` over a path an earlier control already decided follows
  that decision, so impossible expansions are no longer checked; a pointer parameter inside its
  own `{{if .X}}` branch is not NULL there; `-strict` advice about a table or column is reported
  only in packages that touch it.
- PostgreSQL verdicts after a third adversarial round: `TRUNCATE` of a still-referenced table
  is certain to fail; `ON CONFLICT` absorbs a unique constraint only when it can be the
  arbiter (a `DEFERRABLE` key, or a partial index whose predicate is not repeated, keeps its
  23505); a domain's `NOT NULL` is keyed by the domain's name; `unnest(arr)` in the select list
  is nullable; `SET col = DEFAULT` is a transition to the literal default; `-strict` rejects
  `[]T` for an array whose Go element cannot be NULL; `interval` into `time.Duration` carries a
  Lossy note; `paired` requires the write, not matching values; `RETURNING` a `sensitive` column
  is reading it.
- MySQL verdicts after three adversarial rounds: `USING` / `NATURAL` joins coalesce their common
  columns; `WITH ROLLUP` makes non-aggregated result columns nullable; `ON DUPLICATE KEY UPDATE`
  and `REPLACE` count as the writes they are; a `HAVING` conjunct over `GROUP BY` columns is a
  fact; a string column compared to a numeric literal fixes nothing; an expression the server
  rejects in an INSERT's or UPDATE's value is that error, not an unknown type; two `DECLARE`s
  of one condition, cursor or handler condition in a block are the server's own 1332 / 1333 /
  1413, a SQLSTATE literal of other than five characters 1407, `FLUSH` in a trigger or
  FUNCTION 1336, `SIGNAL ... MYSQL_ERRNO = 0` 1231, a FUNCTION calling itself 1424; a
  trigger chain is followed through the routines it `CALL`s and through locking reads, and
  a certain failure anywhere along it is reported on the firing statement; a string column
  compared to `TRUE` or to a `CAST` to a number fixes nothing either, `(a, b) IN ((?, ?))`
  is two equalities, and an unqualified column of a `USING` join pins both sides; a view's
  `OR REPLACE` and `ALGORITHM` are read (a `TEMPTABLE` view is not written through), and
  `WITH CHECK OPTION` on a view the server would not merge is refused as the server's 1368;
  a `SIGNAL` impersonating a builtin number keeps that number as its key. `schema.sql` may
  declare `max_sp_recursion_depth`; above 0 a PROCEDURE's self-recursion is not predicted.
- Obligations on both databases: `require single` on an UPDATE or DELETE asks about the
  target table's rows alone, so a join that only filters no longer breaks the proof;
  `transitions` reads MySQL's constants (both producers now spell a constant the same way).
- Obligations on both databases: `pinned` propagates across a composite foreign key only into a
  `NOT NULL` column (a NULL in a foreign-key column exempts the row from the constraint, so
  such a row joins the parent while agreeing on nothing); `WITH CHECK OPTION` follows every
  view of a chain the servers enforce -- an underlying view behind a join, and an underlying
  view with its own check option under a `LOCAL` one.
- Migrations refuse what they cannot do losslessly instead of noting it: a change of `INHERITS`,
  `OF type`, a domain's base type, a range's subtype, or a composite type's attribute type while
  a column uses it stops `apply` as a problem. `pgtest` and `mysqltest` are sqlshape's own test
  tooling with no compatibility promise; the docs are reorganized per database
  ([docs/postgres.md](docs/postgres.md), [docs/mysql.md](docs/mysql.md)).

## [1.2.0] - 2026-09-09

### Added

- PostgreSQL 18: `schema.sql` declares its version with `-- sqlshape: postgres 18`, and the schema
  and every statement are judged with 18's grammar and catalog (`RETURNING old` / `new`,
  `WITHOUT OVERLAPS` keys and `PERIOD` foreign keys, `NOT ENFORCED` constraints, named `NOT NULL`
  constraints, `VIRTUAL` generated columns). `pgtest` and the migration commands run the declared
  version.
- `One` proves a single row through a temporal key (`valid_at @> {{.Day}}::date`).

### Fixed

- `SELECT DISTINCT ON` expressions are matched to the leading `ORDER BY` expressions by
  expression, not by position.
- An UPDATE that assigns a generated column reports PostgreSQL's own message.

### Changed

- `schema.sql` must declare its PostgreSQL version (`-- sqlshape: postgres 17`); add the line to
  move from 1.1.
- The parser is compiled to WebAssembly and run by wazero instead of linked through cgo:
  `go install` needs no C compiler, and prebuilt binaries cover Linux, macOS and Windows on
  amd64 and arm64.

## [1.1.0] - 2026-09-08

### Added

- Obligations: `schema.sql` declares, above a `CREATE TABLE` or `CREATE VIEW`, what every
  statement touching the relation must do, and vet judges each statement against it
  ([checks.md, Part 2](docs/checks.md#part-2--rules-the-schema-declares)): `require <predicate |
  pinned(col) | immutable(col) | via view | never | paired(table) | single> [on <kinds>]`,
  `aggregate root (children) [lock version]`, `transitions col: a -> b`, `sensitive label: cols`
  with `context name: may read label`, per-caller `context name: require ...; waive ...`, and
  `-- sqlshape: waive` as the opt-out.
- `sqlshape check`: the same judgments for SQL outside Go, one audit line per judgment.

### Changed

- `visible where`, `-require-columns`, `-no-table-reads` and `-no-tables` are obligations now and
  are judged per occurrence of a table (a self join owes it again). Writes are counted by what
  the statement does: each MERGE branch, both halves of `INSERT ... ON CONFLICT DO UPDATE`,
  `TRUNCATE` as a delete, a write through an updatable view as a write to the base table.
  `pinned` on an UPDATE is satisfied by the WHERE only.
- A `-- sqlshape:` directive above a statement that takes none is a schema problem.
- The docs are split: the README is an overview; [docs/](docs/) has checks, templates, runtime,
  migrations and flags, in English and Japanese.

### Fixed

- A vet failure shared by every branch of a template is reported once, without the branch tag.
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
