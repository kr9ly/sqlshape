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
- `sqlshape.Error(code)` declares a schema's named error in Go (`var OrderTooLarge =
  sqlshape.Error("30001")`); `go vet` checks the declaration against the schema both ways, an
  expect line may use the Name, and `Violates` accepts the value.
- `-- sqlshape: server <var> = <value>` declares the server settings the checker's judgments
  depend on (MySQL: `sql_mode`, `lower_case_table_names`); the checker follows them, and
  `mysqltest.Start` starts the server with them.
- The MySQL runtime, `github.com/kr9ly/sqlshape/mysql/v2` (`Run`, `Collect`, `First`, `Exec`,
  `Get` / `Find` / `ExecOne`, `ConstraintError`, `Violates`) over `database/sql`, and
  `github.com/kr9ly/sqlshape/mysqltest/v2`, which boots the `mysqld` on `PATH` with the
  application's schema. `examples/5-mysql` shows the whole path.
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
- MySQL verdicts after two adversarial rounds: `USING` / `NATURAL` joins coalesce their common
  columns; `WITH ROLLUP` makes non-aggregated result columns nullable; `ON DUPLICATE KEY UPDATE`
  and `REPLACE` count as the writes they are; a `HAVING` conjunct over `GROUP BY` columns is a
  fact; a string column compared to a numeric literal fixes nothing; an expression the server
  rejects in an INSERT's or UPDATE's value is that error, not an unknown type.
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
