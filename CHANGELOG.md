# Changelog

Notable changes to sqlshape, newest first. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/). Behaviour of the checker that makes a
statement pass or fail is listed under Changed even when the old behaviour was a bug, since a
passing program may start to fail. A release candidate (`v1.1.0-rc.1`) carries the section of the
release it is a candidate for.

## [Unreleased]

## [2.0.0] - 2026-09-10

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

### Added

- The MySQL runtime, `github.com/kr9ly/sqlshape/mysql/v2`: `Run` / `Collect` / `First` /
  `Exec`, `Get` / `Find` / `ExecOne`, over `database/sql` with go-sql-driver/mysql; `{{.X}}`
  becomes `?` on the wire with the arguments in placeholder order, rows map by the shared
  binding rules, a constraint violation is a `ConstraintError` under the schema's name for it
  (`Violates(err, "users_email_key")`). `github.com/kr9ly/sqlshape/mysqltest/v2` boots the
  `mysqld` on PATH with the application's schema for its tests (the analyzer's oracle runs on
  it too). `examples/5-mysql` shows the whole path.
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
