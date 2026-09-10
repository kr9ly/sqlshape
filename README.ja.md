# sqlshape

[![Go Reference](https://pkg.go.dev/badge/github.com/kr9ly/sqlshape/v2.svg)](https://pkg.go.dev/github.com/kr9ly/sqlshape/v2)
[![release](https://img.shields.io/github/v/release/kr9ly/sqlshape)](https://github.com/kr9ly/sqlshape/releases)
[![test](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml/badge.svg)](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml)
![coverage](.github/badges/coverage.svg)

sqlshapeは、Goのコードにそのまま書いたSQLを`go vet`で検査するツールである。ORMやクエリビルダを挟まず、SQLと、その結果やパラメータを受け渡すGoの構造体とが食い違っていないかを、コンパイル時に確かめる。

[English](README.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := postgres.Get(ctx, db, ByEmail, struct{ Email string }{Email: email})
```

- 生成ではなく検査。列と構造体のフィールドの対応、パラメータの型、NULLの扱い、`One`と宣言した文が本当に1行しか返さないこと、INSERTやUPDATEが違反しうる制約を宣言していることを、`schema.sql`と照らして確かめる。`{{if}}`や`{{range}}`で分岐するSQLは、分岐の全組み合わせが検査される。違反は`go vet`の診断として、コードを実行する前に出る。
- SQLインジェクションは起きない。`{{.X}}`は必ず`$n`のプレースホルダになり、値がSQLの文字列に埋め込まれることはない。実行時も、検査済みのSQLのみが実行できる。
- スキーマの定義は`schema.sql`の1ファイルだけ。静的検査もマイグレーションもこのファイルから導かれるので、モデル定義やマイグレーションファイルを別に書く必要はない。ビュー・関数・ドメイン・複合型・行レベルセキュリティ・seed済みのlookupテーブルもテーブルと同じ厳しさで検査されるので、ロジックをデータベース側に置いても検査の抜け穴にはならない。

## インストール

検査器とマイグレーションコマンドは1つのバイナリになっている。Go 1.26以上で:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest
$ sqlshape version
```

ビルド済みのバイナリは[releasesページ](https://github.com/kr9ly/sqlshape/releases)にある。Linux・macOS・Windowsそれぞれamd64とarm64で、`sqlshape_<version>_<os>_<arch>.tar.gz`（Windowsは`.zip`）と`checksums.txt`。展開して`sqlshape`を`PATH`に置く。

検査に他の準備は要らない。PostgreSQLのパーサ（libpg_query、対応するメジャー版ごとに1つ）はWebAssemblyとして埋め込まれwazeroで動くので、Cコンパイラもリンクするライブラリも無い。初回だけモジュールのコンパイルに1秒ほどかかり、結果はユーザーのキャッシュディレクトリ（Linuxでは`~/.cache/sqlshape`）に置かれる。`pgtest`とマイグレーション系コマンドは本物のPostgreSQLでスキーマを動かすので、初回に宣言した版のサーババイナリを同じキャッシュにダウンロードする（[migrations.ja.md](docs/migrations.ja.md)の要件を参照）。

宣言（`sqlshape.Query`、`sqlshape.One`）は依存の無いGoモジュールで、DBごとのランタイムは別のモジュールになっている。アプリケーションが取り込むのは自分のドライバだけである:

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: 検査器が読む宣言
$ go get github.com/kr9ly/sqlshape/postgres/v2   # pgxの上で実行する
$ go get github.com/kr9ly/sqlshape/mysql/v2      # またはMySQLの上で、database/sqlを通して
```

バージョンはsemantic versioningに従い、リポジトリの全モジュールに同じタグを打つ（`vX.Y.Z`、`postgres/vX.Y.Z`、…）。同じメジャーバージョンの中では、これらのモジュールと`pgtest`の公開API、テンプレート構文、ディレクティブ、検査器のフラグは互換を保つ。検査器が報告する内容はマイナーバージョンで増えることがある。

## Quickstart

```sql
-- schema.sql
-- sqlshape: postgres 17
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

`schema.sql`の最初の行が、どの版のPostgreSQL向けのスキーマかを名乗る。すべての文はその版の文法とカタログで判定される（17と18に対応）。

```go
// users.go
type User struct {
	ID        int64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.Query[int64, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

```
$ sqlshape ./...
users.go:20:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:25:46: sqlshape: may violate users_email_key (UNIQUE (email) on users, SQLSTATE 23505); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

1つ目は`deleted_at`がNULLになりうるのに`time.Time`で受けている、2つ目はこのINSERTはemailの一意制約に違反しうるのにそれを宣言していない、という指摘である。`DeletedAt`を`*time.Time`にして、INSERTの1行目に`-- sqlshape: expect users_email_key`を書けば通る。実行時はこう使う:

```go
users, err := postgres.Collect(ctx, pool, Users, struct{ Name *string }{})          // []User
id, err := postgres.First(ctx, pool, Create, struct{ Email, Name string }{e, n})     // int64
if postgres.Violates(err, "users_email_key") { /* expect行で宣言した失敗 */ }
```

`db`にはpgxの`*pgxpool.Pool`、`*pgx.Conn`、`pgx.Tx`のどれでも渡せる。

構造体は自分で書かなくてもよい。`type Row struct{}`と`type Params struct{}`を空のまま宣言してSQLだけ書くと、列やパラメータに対応するフィールドが無いという診断が出て、それぞれにSQLから構造体を書き起こすquick fixが付く。`sqlshape -fix ./...`で一括適用できる。

保存のたびに検査するには、バイナリを`go vet`の`-vettool`に指定する。エディタのGo統合が保存時に`go vet`を走らせる設定になっていれば、診断はそこに出る:

```
$ go vet -vettool="$(which sqlshape)" ./...
```

詳細は[docs/flags.ja.md](docs/flags.ja.md#エディタで使う)。

## Examples

同じ受注台帳を、データベースにどこまで任せるかの段階ごとに4つ用意してある:

- [`examples/1-tables`](examples/1-tables) — 基本形。テーブルに対して`Query` / `One`を書き、構造体と突き合わせ、失敗しうる制約をexpect行で宣言する、標準的な使い方
- [`examples/2-views`](examples/2-views) — 読み取りをビューにまとめる。JOINや列名の決定をビューに閉じ込めて、アプリケーション側のSQLを薄くする段階
- [`examples/3-database-api`](examples/3-database-api) — 書き込みを関数に、値の意味をドメインや複合型に移す。ロジックをデータベース側に置いたとき、検査がどう働くか
- [`examples/4-everything`](examples/4-everything) — 全機能を使った例。特定の機能の使い方を探すときの索引
- [`examples/5-mysql`](examples/5-mysql) — 同じ宣言をMySQLに対して。`schema.sql`が`mysql 8.4`を名乗り、文は`sqlshape/mysql`（database/sql）で実行し、テストは`mysqltest`で本物のmysqldを起こす

## 何を検査するか

全リストは[docs/checks.ja.md](docs/checks.ja.md)にある。大きく分けると:

- 形 — SQLが返す列とGoの構造体、`{{.X}}`とパラメータの構造体が、名前も型もNULLの扱いも合っていること。ネストした行や複合型も含む
- 意味 — enumやlookupテーブルの値、主キー、ドメインに対応するGoの型は、使われた箇所からその意味に結びつけられる。別のテーブルのIDを渡す、単位の違うドメインを足す、定数とラベルがずれている、といった型が同じでも意味の違う誤りが見つかる
- 失敗モード — 書き込みが違反しうる制約はexpect行に宣言しなければならない。宣言漏れも、起こりえない宣言も報告されるので、どの制約違反を処理すべきかがコードに書かれた状態が保たれる。これはORMには無い。ORMのモデルは制約の写しなので、そこから「何が失敗しうるか」は導けず、例外は実行時に名前を持って届き、それを手で突き合わせることになる。sqlshapeでは一覧を`schema.sql`から計算し（トリガや呼び出す関数の中まで）、名前はPostgreSQL自身の命名で、実行時のエラーも同じ名前を運ぶ。宣言・検査・ハンドラが一つの文字列で結ばれる
- カーディナリティ — `One`と宣言した文は、本当に1行以下しか返さないことがスキーマから証明される
- 境界 — 論理削除の条件を必ず付ける、テナント列で必ず絞る、テーブルを直接読まずビューを通す、といったチームの規約を検査器に強制させられる
- スキーマ自体 — `schema.sql`の関数（SQLもPL/pgSQLも本体まで）・ビュー・ポリシーも型検査される。`-strict`を付けると、インデックスが使われない条件やlookupテーブルで済むenumなどの助言も出る

## Goの外のSQL

`schema.sql`に宣言した規約はGoコードの中だけのものではない。`sqlshape check`はどんなSQLでもその規約と照合する。本番で流す前の運用のUPDATE、マイグレーションに混ぜたbackfill、LLMエージェントがこれから実行するクエリ。判定は全部、履行経路つきで出るので、出力はそのスクリプトに何が許されたかの記録にもなる。

```
$ sqlshape check ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

文脈（`-context ops`）で、その呼び出し元に適用する規約を選ぶ。詳細は[docs/checks.ja.md](docs/checks.ja.md#goの外のsqlにも同じ規約を適用するsqlshape-check)。

## マイグレーション

マイグレーションファイルは書かない。`schema.sql`を直すと、`sqlshape`がデータベースとの差分からDDLを生成する:

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

生成されたDDLは手で直してよい。`apply`は、そのDDLを当てた結果が本当に`schema.sql`と一致するかを実行前に確認し、一致しなければ実行しない。`-packages`を付けると、DDLで消える列や型が変わる列をまだ使っているGoのコードがあれば、それも実行前に止まる。リネームやenumのラベル削除のように差分だけでは意図が決められない変更は、`schema.sql`に`-- @migrate`行で書き添える。詳細は[docs/migrations.ja.md](docs/migrations.ja.md)。

## ドキュメント

- [docs/checks.ja.md](docs/checks.ja.md) — 検査器が確かめること全部: 形、意味、失敗モード（PostgreSQLの制約命名規則の表つき）、カーディナリティ、境界
- [docs/templates.ja.md](docs/templates.ja.md) — テンプレートで使える構文、ディレクティブ、共有フラグメント、危険な書き方、疎検査
- [docs/runtime.ja.md](docs/runtime.ja.md) — `postgres.Run` / `Collect` / `First` / `Exec`、`One`、`Batch`、`Copy`、`MatView`、Go型の表、型の登録、エラー、本物のPostgreSQLでのテスト
- [docs/migrations.ja.md](docs/migrations.ja.md) — `diff` / `apply` / `verify-schema`、`-- @migrate`宣言、seed済みテーブル、必要な環境
- [docs/flags.ja.md](docs/flags.ja.md) — 全フラグ、`-strict`の助言一覧、エディタ設定
- [docs/design.md](docs/design.md) — 設計上の裁定。何を決めたか、なぜか、何を棄てたか

## 互換性

PostgreSQL 17と18に対応し、どちらかは`schema.sql`が宣言する。構文はPostgreSQL自身のものである。パーサはその版のlibpg_queryなので、サーバが読める文は検査器も同じように読む。SELECTとDML、MERGE、CTE、ウィンドウ関数、GROUPING SETS、SQL/JSON、範囲型、18の`RETURNING old` / `new`と時制キー、citextやhstoreなどの拡張、ビュー・関数（SQLとPL/pgSQLの本体まで）・トリガー・ポリシーを含むDDL。アナライザーはその版のカタログから組み上げたpure Goの実装で、検査時にPostgreSQLへ接続することはない。

判定の裏付けはPostgreSQL自身の回帰テストである。`src/test/regress`の文を、アナライザーと同じ版の本物のPostgreSQLに並走させ、パラメータの型・結果列・エラーの判定が一致することを確認している。一致しないのは17で22,103文のうち19件、18で23,384文のうち31件。全件を`check/postgres/analyze/testdata/regress_baseline_<version>.txt`に列挙してあり、どれも静的解析では判定できないもの（行レベルセキュリティの再帰、権限、サーバー内部のエラー）か、検査器が正しくサーバのDescribeには見えないもの（INSERTの`RETURNING old`がNULLであること）である。この突き合わせは`go test ./...`の一部なので、新しい不一致が出ればテストが失敗する。

## License

Everything a checked program links is Apache License 2.0, see [LICENSE](LICENSE): the
declarations (the root module), the runtimes (`postgres`, `mysql`) and `pgtest`. The PostgreSQL
side of the checker (`check/postgres`) embeds `pg_catalog` data and validation rules ported from
PostgreSQL under the PostgreSQL License, see [check/postgres/NOTICE](check/postgres/NOTICE). The
`sqlshape` binary (`cmd/sqlshape`) and the MySQL side of the checker (`check/mysql`, which carries
MySQL's own parser) are modules under the GNU General Public License v2, see
[cmd/sqlshape/LICENSE](cmd/sqlshape/LICENSE); the binary is a development tool, and nothing under
it is linked into your program.
