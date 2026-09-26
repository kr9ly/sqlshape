# sqlshape

[![Go Reference](https://pkg.go.dev/badge/github.com/kr9ly/sqlshape/v2.svg)](https://pkg.go.dev/github.com/kr9ly/sqlshape/v2)
[![release](https://img.shields.io/github/v/release/kr9ly/sqlshape)](https://github.com/kr9ly/sqlshape/releases)
[![test](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml/badge.svg)](https://github.com/kr9ly/sqlshape/actions/workflows/test.yml)
![coverage](.github/badges/coverage.svg)

sqlshapeは、Goのコードにそのまま書いたSQLを`go vet`で検査するツールである。ORMやクエリビルダを挟まず、SQLが返す列と受け取る構造体、パラメータ、NULLの扱い、1行しか返さないはずの文、そして書き込みごとに処理すべき制約違反を、コードを実行する前に`schema.sql`と照らして確かめる。

対応するのはPostgreSQL 17 / 18とMySQL 8.4、Go 1.26以上。検査にデータベースもCコンパイラも要らず、アプリケーションがリンクするsqlshapeのコードはApache-2.0だけである。

[English](README.md)

```go
// 行の型、パラメータの型の順。検査器は両方がSQLと合うこと、そして1行以下しか返さないことを証明する
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := postgres.Get(ctx, db, ByEmail, struct{ Email string }{Email: email})   // MySQLなら mysql.Get
```

- 食い違いは実行時の事故ではなくvetの診断になる。構造体が受け取れない列、型の違うパラメータ、NULLになりうる列をポインタでないフィールドで受けること、2行返しうるのに`One`と宣言した文は、どれも`go vet`の診断として出る。直し方が決まるものにはquick fixが付く。
- 失敗モードを宣言し、検査器が証明する。書き込みは違反しうる制約をexpect行に並べる（`-- sqlshape: expect users_email_key`）。検査器は本当の一覧を`schema.sql`から計算し（トリガや呼び出す関数の中まで）、宣言漏れも起こりえない宣言も報告する。実行時は`postgres.Violates(err, "users_email_key")`が同じ名前で突き合わせる。宣言・検査・ハンドラが一つの文字列で結ばれる。
- SQLは素のSQLのままで、SQLインジェクションは起きない。テンプレートはGoの`text/template`。`{{.X}}`は必ずプレースホルダになり、`{{if}}`や`{{range}}`で分岐するSQLは分岐の全組み合わせが検査される。実行時も、検査済みのSQLのみが実行できる（[制約事項](#制約事項)に書いた範囲で）。
- スキーマの定義は`schema.sql`の1ファイルだけ。ビュー・関数・ドメイン・行レベルセキュリティ・seed済みのlookupテーブルもテーブルと同じ厳しさで検査されるので、ロジックをデータベース側に置いても検査の抜け穴にはならない。マイグレーションも`sqlshape diff`が同じファイルから導くので、同期を保つべきマイグレーションファイルは無い。
- 判定はデータベース自身の規則に従う。文を読むのはPostgreSQLとMySQLそれぞれ自身のパーサで、判定は各データベースの回帰テストとテストコーパスで裏付けている（[互換性と検証](#互換性と検証)）。

### 他の道具との違い

sqlcと違い、コードは生成しない。構造体は自分で書き（`sqlshape -fix`でSQLから書き起こしてもよい）、スキーマが変わっても、食い違えば検査器が報告する。ORMと違い、スキーマの写しとしてのモデルが無い。制約名もNULLになりうるかも何行返るかも`schema.sql`そのものから読むので、書き込みがどのエラーを起こしうるかを検査器が言える。

## インストール

検査器とマイグレーションコマンドは1つのバイナリになっている:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@latest
$ sqlshape version
```

ビルド済みのバイナリは[releasesページ](https://github.com/kr9ly/sqlshape/releases)にある。Linux・macOS・Windowsそれぞれamd64とarm64で、`sqlshape_<version>_<os>_<arch>.tar.gz`（Windowsは`.zip`）と`checksums.txt`。展開して`sqlshape`を`PATH`に置く。

次に、宣言と、使うデータベースのランタイムを自分のモジュールに加える。宣言（`sqlshape.Query`、`sqlshape.One`）は依存の無いGoモジュールで、DBごとのランタイムは別のモジュールになっている。アプリケーションが取り込むのは自分のドライバだけである:

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: 検査器が読む宣言
$ go get github.com/kr9ly/sqlshape/postgres/v2   # pgx v5の上で実行する
$ go get github.com/kr9ly/sqlshape/mysql/v2      # またはMySQLの上で、database/sqlとgo-sql-driver/mysqlを通して
```

どのモジュールもGo 1.26以上が要る。検査に他の準備は要らない。データベースもCコンパイラも無しで動く。初回だけ埋め込みのパーサの準備に1秒ほどかかり、結果はキャッシュされる（[検査器の動き方](#検査器の動き方)）。

## Quickstart

データベースを選ぶ: [PostgreSQL](#quickstart-postgresql) · [MySQL](#quickstart-mysql)

### Quickstart: PostgreSQL

`schema.sql`をモジュールのルートに置く。検査器は各パッケージから上にたどって最も近い`schema.sql`（または`schema/`ディレクトリ）を使い、`-schema PATH`で上書きできる。

```sql
-- sqlshape: postgres 17
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

最初の行が、どのバージョンのPostgreSQL向けのスキーマかを名乗る。すべての文はそのバージョンの文法とカタログで判定される（`postgres 17`か`postgres 18`）。

続いて文を宣言する。ここでは`users.go`に書く:

```go
package app

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID        int64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.One[int64, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

`Query[R, P]`と`One[R, P]`は行の型とパラメータの型を取る。`WHERE true`は、省略できる`AND`句を後ろに続けるための書き方で、検査器はその句がある場合と無い場合の両方を検査する（[docs/templates.ja.md](docs/templates.ja.md)）。

```
$ sqlshape ./...
users.go:16:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:20:65: sqlshape: may violate users_email_key (UNIQUE (email) on users, SQLSTATE 23505); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

パスはここでは短縮してあり、実際には完全なパスで出る。1つ目は`deleted_at`がNULLになりうるのに`time.Time`で受けている、2つ目はこのINSERTはemailの一意制約に違反しうるのにそれを宣言していない、という指摘である。両方を直せば通る:

```go
type User struct {
	// ...
	DeletedAt *time.Time
}

var Create = sqlshape.One[int64, struct{ Email, Name string }](`
-- sqlshape: expect users_email_key
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id`)
```

実行時は`github.com/kr9ly/sqlshape/postgres/v2`でこう使う:

```go
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))

users, err := postgres.Collect(ctx, pool, Users, struct{ Name *string }{})          // []User
id, err := postgres.Get(ctx, pool, Create, struct{ Email, Name string }{e, n})       // int64
if postgres.Violates(err, "users_email_key") { /* expect行で宣言した失敗 */ }
```

`Collect`は全行を集め、`Get`は`One`の文を実行してその行を返す。`Run`は行を順に流し、`First`は最初の行を取り、`Exec`は文を実行して行を捨て、`ExecOne`は行を返さない`One`の文を実行する（[docs/runtime.ja.md](docs/runtime.ja.md#文の実行)）。`pool`にはpgxの`*pgxpool.Pool`、`*pgx.Conn`、`pgx.Tx`のどれでも渡せる（[docs/postgres.ja.md](docs/postgres.ja.md)）。

### Quickstart: MySQL

`schema.sql`をモジュールのルートに置く。検査器は各パッケージから上にたどって最も近い`schema.sql`（または`schema/`ディレクトリ）を使い、`-schema PATH`で上書きできる。

```sql
-- sqlshape: mysql 8.4
CREATE TABLE users (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    name       VARCHAR(100) NOT NULL,
    deleted_at DATETIME(6),
    UNIQUE KEY users_email_key (email)
);
```

最初の行がMySQLのバージョンを名乗る。MySQL自身の文法がスキーマと全部の文を読み、MySQLの規則が型を付ける（`mysql 8.4`）。既定と違う`sql_mode`や`lower_case_table_names`でサーバを動かすなら、`-- sqlshape: server`の行で宣言する。

続いて文を宣言する。ここでは`users.go`に書く:

```go
package app

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
)

type User struct {
	ID        uint64
	Email     string
	Name      string
	DeletedAt time.Time
}

var Users = sqlshape.Query[User, struct{ Name *string }](`
SELECT id, email, name, deleted_at FROM users
 WHERE true {{if .Name}} AND name = {{.Name}} {{end}}`)

var Create = sqlshape.Query[struct{}, struct{ Email, Name string }](`
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}})`)
```

`Query[R, P]`は行の型とパラメータの型を取る。INSERTは行を返さないので、行の型は`struct{}`にする。`WHERE true`は、省略できる`AND`句を後ろに続けるための書き方で、検査器はその句がある場合と無い場合の両方を検査する（[docs/templates.ja.md](docs/templates.ja.md)）。

```
$ sqlshape ./...
users.go:16:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:20:70: sqlshape: may violate users_email_key (UNIQUE users_email_key (email) on users, MySQL error 1062); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

パスはここでは短縮してあり、実際には完全なパスで出る。1つ目は`deleted_at`がNULLになりうるのに`time.Time`で受けている、2つ目はこのINSERTは`users_email_key`に違反しうるのにそれを宣言していない、という指摘である。両方を直せば通る:

```go
type User struct {
	// ...
	DeletedAt *time.Time
}

var Create = sqlshape.Query[struct{}, struct{ Email, Name string }](`
-- sqlshape: expect users_email_key
INSERT INTO users (email, name) VALUES ({{.Email}}, {{.Name}})`)
```

実行時は`github.com/kr9ly/sqlshape/mysql/v2`と、ドライバのimport（`_ "github.com/go-sql-driver/mysql"`）でこう使う:

```go
db, err := sql.Open("mysql", "app:secret@tcp(localhost:3306)/app?parseTime=true")

users, err := mysql.Collect(ctx, db, Users, struct{ Name *string }{})          // []User
res, err := mysql.Exec(ctx, db, Create, struct{ Email, Name string }{e, n})     // sql.Result
if mysql.Violates(err, "users_email_key") { /* expect行で宣言した失敗 */ }
```

`Collect`は全行を集め、`Exec`は書き込みを実行してドライバの`sql.Result`を返す。`Run`は行を順に流し、`First`は最初の行を取り、`Get` / `ExecOne`は`One`の文を実行する（[docs/runtime.ja.md](docs/runtime.ja.md#文の実行)）。`db`はgo-sql-driver/mysqlで開いた`*sql.DB`、`*sql.Tx`、`*sql.Conn`のどれでもよく、DSNには`parseTime=true`が要る（[docs/mysql.ja.md](docs/mysql.ja.md#接続)）。

## 構造体の生成、エディタとCI

構造体は自分で書かなくてもよい。`type Row struct{}`と`type Params struct{}`を空のまま宣言してSQLだけ書くと、列やパラメータに対応するフィールドが無いという診断が出て、それぞれにSQLから構造体を書き起こすquick fixが付く。`sqlshape -fix ./...`で一括適用できる。

保存のたびに検査するには、バイナリを`go vet`の`-vettool`に指定する。エディタのGo統合が保存時に`go vet`を走らせる設定になっていれば、診断はそこに出る:

```
$ go vet -vettool="$(which sqlshape)" ./...
```

CIでは`sqlshape ./...`を走らせる。何か報告すれば0以外で終了する。マイナーリリースで診断が増えることがあるので、インストールするバージョンは固定しておく（[バージョニング](#バージョニング)）:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape/v2@v2.0.0
$ sqlshape ./...
```

詳細は[docs/flags.ja.md](docs/flags.ja.md#エディタで使う)。

## 何を検査するか

全リストは[docs/checks.ja.md](docs/checks.ja.md)にある。大きく分けると:

- 形 — SQLが返す列とGoの構造体、`{{.X}}`とパラメータの構造体が、名前も型もNULLの扱いも合っていること。ネストした行や複合型も含む
- 意味 — `OrderID`と`UserID`がどちらも`int64`でも、注文IDの場所にユーザーIDを渡せば報告される。enumやlookupテーブルの値、主キー、ドメインに対応するGoの型は、使われた箇所からその列の意味に結びつけられるので、単位の違うドメインを足す、定数とラベルがずれている、といった型が同じでも意味の違う誤りも見つかる
- 失敗モード — 書き込みが違反しうる制約はexpect行に宣言しなければならず、宣言漏れも、起こりえない宣言も報告される。一覧は`schema.sql`から計算し（トリガや呼び出す関数の中まで）、名前はデータベース自身の命名（PostgreSQLの制約名とSQLSTATE、MySQLのキー名とエラー番号）である
- カーディナリティ — `One`と宣言した文は、本当に1行以下しか返さないことがスキーマから証明される
- 境界 — 論理削除の条件を必ず付ける、テナント列で必ず絞る、テーブルを直接読まずビューを通す、といったチームの規約を`schema.sql`に宣言し、検査器に強制させられる:

  ```sql
  -- sqlshape: require pinned(tenant_id)
  -- sqlshape: visible where deleted_at IS NULL
  CREATE TABLE orders (...);
  ```

- スキーマ自体 — `schema.sql`の関数（SQLもPL/pgSQLも本体まで）・ビュー・ポリシーも型検査される。`-strict`を付けると、インデックスが使われない条件やlookupテーブルで済むenumなどの助言も出る

## Goのコードの外で

### Goの外のSQL

`schema.sql`に宣言した規約はGoコードの中だけのものではない。`sqlshape check`はどんなSQLでもその規約と照合する。本番で流す前の運用のUPDATE、マイグレーションに混ぜたbackfill、LLMエージェントがこれから実行するクエリ。判定は全部、各規約が通ったか・落ちたか・免除されたかとその理由つきで出るので、出力はそのスクリプトに何が許されたかの記録にもなる。

```
$ sqlshape check ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

文は自分の`-- sqlshape: waive`行で規約を免除する。規約は呼び出し元ごとに変えることもできる。`-context ops`を付けると、`schema.sql`が運用向けに宣言した規約（`-- sqlshape: context ops: ...`）が適用される。詳細は[docs/checks.ja.md](docs/checks.ja.md#goの外のsqlにも同じ規約を適用するsqlshape-check)。

### マイグレーション

マイグレーションファイルは書かない。`schema.sql`を直すと、`sqlshape`がデータベースとの差分からDDLを生成する:

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

生成されたDDLは手で直してよい。`apply`は、そのDDLを当てた結果が本当に`schema.sql`と一致するかを実行前に確認し、一致しなければ実行しない。`-packages`を付けると、DDLで消える列や型が変わる列をまだ使っているGoのコードがあれば、それも実行前に止まる。リネームやenumのラベル削除のように差分だけでは意図が決められない変更は、`schema.sql`に`-- @migrate`行で書き添える。

本物のデータベースを動かすのはマイグレーションコマンドだけである。PostgreSQLでは初回に宣言したバージョンのサーババイナリをユーザーのキャッシュにダウンロードするので、その初回はネットワークが要る。MySQLでは対象サーバ上の一時データベースを使う。詳細は[docs/migrations.ja.md](docs/migrations.ja.md)。

## Examples

同じ受注台帳を、データベースにどこまで任せるかの段階ごとに4つ用意してある:

- [`examples/1-tables`](examples/1-tables) — 基本形。テーブルに対して`Query` / `One`を書き、構造体と突き合わせ、失敗しうる制約をexpect行で宣言する、標準的な使い方
- [`examples/2-views`](examples/2-views) — 読み取りをビューにまとめる。JOINや列名の決定をビューに閉じ込めて、アプリケーション側のSQLを薄くする段階
- [`examples/3-database-api`](examples/3-database-api) — 書き込みを関数に、値の意味をドメインや複合型に移す。ロジックをデータベース側に置いたとき、検査がどう働くか
- [`examples/4-everything`](examples/4-everything) — 全機能を使った例。特定の機能の使い方を探すときの索引

MySQL版の1つ目の段階:

- [`examples/5-mysql`](examples/5-mysql) — 同じ宣言をMySQLに対して。`schema.sql`が`mysql 8.4`を名乗り、文は`sqlshape/mysql`（database/sql）で実行する

## 制約事項

- 判定を実測で裏付けているのはPostgreSQL 17と18、MySQL 8.4で、`schema.sql`はそのどれかを宣言する（`-- sqlshape: postgres 17`、`postgres 18`、`mysql 8.4`）。`postgres 16`や`mysql 8.0`のような他の番号は受け付けない。
- 他のバージョンのサーバでもsqlshapeは使える。検査器は走り、アプリケーションもドライバが接続できるサーバ（古いPostgreSQL、他のバージョンのMySQL、MariaDBやその他のMySQL互換データベースを含む）の上で動く。その場合、文は宣言したバージョンの規則で判定されるので、そのサーバの挙動が違う箇所では、判定がサーバの実際の動きと一致することは保証しない。
- プロジェクトが使うデータベースは1つ。どちらかは`schema.sql`が名乗り、文法、型、診断に出る制約の名前、importするランタイムモジュールはすべてその宣言から決まる。
- テンプレートはGoの文字列定数でなければならない。実行時に組み立てたSQLは宣言できず、変わる部分は`{{if}}` / `{{range}}`の分岐で書く。検査していない展開は実行時に拒否される。`{{range}}`は要素2つまでを検査し、分岐の組み合わせが256を超えるテンプレートは代表的な組だけを検査する。その場合、実行時には分岐の形だけを確かめてから実行する（[docs/runtime.ja.md](docs/runtime.ja.md#検査済みのsqlのみが実行できる)）。
- PostgreSQLの機能のうちMySQLに対応物の無いもの（`Copy`、`MatView`、PL/pgSQL、ドメイン、複合型と配列など）は、MySQLでは検査されない（[docs/mysql.ja.md](docs/mysql.ja.md#mysqlに無いもの)）。

## 互換性と検証

| データベース | バージョン | 文法 | 照合先 |
|---|---|---|---|
| PostgreSQL | 17、18 | 宣言したバージョンのlibpg_query | PostgreSQL自身の回帰テスト（本物のサーバと並走） |
| MySQL | 8.4 | サーバのソースから切り出したMySQL自身のパーサと字句解析器 | 動いている`mysqld`とMySQL自身のテストコーパス |

PostgreSQL。アナライザーは宣言したバージョンのカタログから組み上げてある。判定はPostgreSQL自身の回帰テストで裏付けている。アナライザーと本物のサーバを並走させ、17では22,103文のうち19件、18では23,384文のうち31件だけが一致せず、全件を列挙して理由を付けてある（[docs/postgres.ja.md](docs/postgres.ja.md#検査器が埋め込んでいるもの)）。

MySQL。関数の表もパーサと同じサーバのソースから読む。判定は動いている`mysqld`と照合している。組み込み関数全部の結果型、エラーになる文、`ONLY_FULL_GROUP_BY`の検査、`sql_mode`を変えた場合が8.4と一致し、MySQL自身のテストコーパス（`mysql-test/t`、約137,000文）をアナライザーとサーバの両方に流して突き合わせている——一致しないと分かっている2,525文は1件ずつベースラインに固定してあり、新しい不一致が出るとビルドが落ちる（[docs/mysql.ja.md](docs/mysql.ja.md#検査器が埋め込んでいるもの)）。

マイグレーションも両データベースで同じやり方で判定している。生成したスキーマの組を、diffが報告し得る変更の全種にわたって、行の入った本物のサーバにDDLとして流す（[docs/migrations.ja.md](docs/migrations.ja.md#計画がどう検証されているか)）。

### 検査器の動き方

パーサ（PostgreSQLはlibpg_queryを対応するメジャーバージョンごとに、MySQLは8.4自身の文法と字句解析器）はWebAssemblyとして埋め込まれwazeroで動くので、Cコンパイラもリンクするライブラリも無い。初回だけモジュールのコンパイルに1秒ほどかかり、結果はユーザーのキャッシュディレクトリ（Linuxでは`~/.cache/sqlshape`）に置かれる。

### バージョニング

バージョンはsemantic versioningに従い、リポジトリの全モジュールに同じタグを打つ（`vX.Y.Z`、`postgres/vX.Y.Z`、…）。同じメジャーバージョンの中では、これらのモジュールの公開API、テンプレート構文、ディレクティブ、検査器のフラグは互換を保つ。ただしマイナーリリースで診断が増えることがあり、以前は通ったコードがアップグレード後に報告されうる。CIでは`sqlshape`のバージョンを固定し、上げるときは意図して上げる。

## ドキュメント

片方のデータベースにしか関係しないことは[docs/postgres.ja.md](docs/postgres.ja.md)と[docs/mysql.ja.md](docs/mysql.ja.md)にまとめてあり、他のドキュメントは両方の読者に向けて書いてある。

- [docs/postgres.ja.md](docs/postgres.ja.md) — PostgreSQLに属するもの全部: バージョンの宣言、検査器が埋め込むものとその検証、pgxの上のランタイム（`Batch`、`Copy`、`MatView`、型の登録）、PostgreSQLのマイグレーション
- [docs/mysql.ja.md](docs/mysql.ja.md) — MySQLに属するもの全部: バージョンと`server`の宣言、Go型の表、制約名とエラー番号、`ONLY_FULL_GROUP_BY`の検査、`database/sql`の上のランタイム、MySQLのマイグレーション
- [docs/mysql-errors.ja.md](docs/mysql-errors.ja.md) — 検査器が文の形から予測するMySQLのエラー番号（番号の索引つき）: 格納する値、定数式、トリガとルーチンの本体、名前解決、ビュー
- [docs/checks.ja.md](docs/checks.ja.md) — 検査器が確かめること全部（両データベース共通）: 形、意味、失敗モード、カーディナリティ、スキーマが宣言する規約（`schema.sql`のディレクティブ一覧、`require`、集約、`sqlshape check`）
- [docs/templates.ja.md](docs/templates.ja.md) — テンプレートで使える構文、テンプレートに書くディレクティブ、共有フラグメント、危険な書き方、疎検査
- [docs/runtime.ja.md](docs/runtime.ja.md) — どのランタイムでも同じこと: `Run` / `Collect` / `First` / `Exec`、`One`、行のマッピング、エラー、検査済みのSQLだけが走る保証、自前のランタイムでは得られないもの
- [docs/migrations.ja.md](docs/migrations.ja.md) — 必要な環境、`diff` / `apply` / `verify-schema`、`-- @migrate`宣言、複数の環境とロールバック、seed済みテーブル、MySQLで違うところ
- [docs/flags.ja.md](docs/flags.ja.md) — 全フラグ、`-strict`の助言一覧、エディタ設定
- [docs/design.md](docs/design.md) — 設計上の裁定。何を決めたか、なぜか、何を棄てたか

## License

アプリケーションがリンクするsqlshapeのコードはApache License 2.0だけである（宣言のルートモジュールと、ランタイムの`postgres`・`mysql`）。ランタイムが取り込むドライバはそれぞれのライセンスに従う（pgxはMIT、go-sql-driver/mysqlはMPL-2.0）。GPLv2なのは開発ツールである`sqlshape`バイナリとMySQL側の検査器で、アプリケーションにはリンクされない。正確な内訳は以下（原文）。

The only sqlshape code your program links is Apache License 2.0 ([LICENSE](LICENSE)): the
declarations (the root module) and the runtimes (`postgres`, `mysql`). The drivers the runtimes
pull in keep their own licenses (pgx: MIT, go-sql-driver/mysql: MPL-2.0). The PostgreSQL side of
the checker (`check/postgres`) embeds `pg_catalog` data and validation rules ported from
PostgreSQL under the PostgreSQL License, see [check/postgres/NOTICE](check/postgres/NOTICE). The
`sqlshape` binary (`cmd/sqlshape`) and the MySQL side of the checker (`check/mysql`, which carries
MySQL's own parser) are modules under the GNU General Public License v2, see
[cmd/sqlshape/LICENSE](cmd/sqlshape/LICENSE); the binary is a development tool, and nothing under
it is linked into your program.
