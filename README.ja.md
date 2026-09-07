# sqlshape

sqlshapeは、Goのコードにそのまま書いたSQLを`go vet`で検査するツールである。ORMやクエリビルダを挟まず、SQLと、その結果やパラメータを受け渡すGoの構造体とが食い違っていないかを、コンパイル時に確かめる。

[English](README.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := ByEmail.Get(ctx, db, struct{ Email string }{Email: email})
```

- 生成ではなく検査。列と構造体のフィールドの対応、パラメータの型、NULLの扱い、`One`と宣言した文が本当に1行しか返さないこと、INSERTやUPDATEが違反しうる制約を宣言していることを、`schema.sql`と照らして確かめる。`{{if}}`や`{{range}}`で分岐するSQLは、分岐の全組み合わせが検査される。違反は`go vet`の診断として、コードを実行する前に出る。
- SQLインジェクションは起きない。`{{.X}}`は必ず`$n`のプレースホルダになり、値がSQLの文字列に埋め込まれることはない。実行時も、検査済みのSQLのみが実行できる。
- スキーマの定義は`schema.sql`の1ファイルだけ。静的検査もマイグレーションもこのファイルから導かれるので、モデル定義やマイグレーションファイルを別に書く必要はない。ビュー・関数・ドメイン・複合型・行レベルセキュリティ・seed済みのlookupテーブルもテーブルと同じ厳しさで検査されるので、ロジックをデータベース側に置いても検査の抜け穴にはならない。

## Quickstart

```sql
-- schema.sql
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    deleted_at timestamptz
);
```

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
$ go run github.com/kr9ly/sqlshape/cmd/sqlshape ./...
users.go:20:13: sqlshape: field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
users.go:25:46: sqlshape: may violate users_email_key (UNIQUE (email) on users, SQLSTATE 23505); add `-- sqlshape: expect users_email_key` to the template or make it impossible
```

1つ目は`deleted_at`がNULLになりうるのに`time.Time`で受けている、2つ目はこのINSERTはemailの一意制約に違反しうるのにそれを宣言していない、という指摘である。`DeletedAt`を`*time.Time`にして、INSERTの1行目に`-- sqlshape: expect users_email_key`を書けば通る。実行時はこう使う:

```go
users, err := Users.Collect(ctx, pool, struct{ Name *string }{})          // []User
id, err := Create.First(ctx, pool, struct{ Email, Name string }{e, n})     // int64
if sqlshape.Violates(err, "users_email_key") { /* expect行で宣言した失敗 */ }
```

`db`にはpgxの`*pgxpool.Pool`、`*pgx.Conn`、`pgx.Tx`のどれでも渡せる。

構造体は自分で書かなくてもよい。`type Row struct{}`と`type Params struct{}`を空のまま宣言してSQLだけ書くと、列やパラメータに対応するフィールドが無いという診断が出て、それぞれにSQLから構造体を書き起こすquick fixが付く。`sqlshape -fix ./...`で一括適用できる。

保存のたびに検査するには、ビルドしたバイナリを`go vet`の`-vettool`に指定する。エディタのGo統合が保存時に`go vet`を走らせる設定になっていれば、診断はそこに出る:

```
$ go build -o "$(go env GOPATH)/bin/sqlshape" github.com/kr9ly/sqlshape/cmd/sqlshape
$ go vet -vettool="$(go env GOPATH)/bin/sqlshape" ./...
```

詳細は[docs/flags.ja.md](docs/flags.ja.md#エディタで使う)。

## Examples

同じ受注台帳を、データベースにどこまで任せるかの段階ごとに4つ用意してある:

| | 参考になるポイント |
|---|---|
| [`examples/1-tables`](examples/1-tables) | 基本形。テーブルに対して`Query` / `One`を書き、構造体と突き合わせ、失敗しうる制約をexpect行で宣言する、標準的な使い方 |
| [`examples/2-views`](examples/2-views) | 読み取りをビューにまとめる。JOINや列名の決定をビューに閉じ込めて、アプリケーション側のSQLを薄くする段階 |
| [`examples/3-database-api`](examples/3-database-api) | 書き込みを関数に、値の意味をドメインや複合型に移す。ロジックをデータベース側に置いたとき、検査がどう働くか |
| [`examples/4-everything`](examples/4-everything) | 全機能を使った例。特定の機能の使い方を探すときの索引 |

## 何を検査するか

全リストは[docs/checks.ja.md](docs/checks.ja.md)にある。大きく分けると:

- 形 — SQLが返す列とGoの構造体、`{{.X}}`とパラメータの構造体が、名前も型もNULLの扱いも合っていること。ネストした行や複合型も含む
- 意味 — enumやlookupテーブルの値、主キー、ドメインに対応するGoの型は、使われた箇所からその意味に結びつけられる。別のテーブルのIDを渡す、単位の違うドメインを足す、定数とラベルがずれている、といった型が同じでも意味の違う誤りが見つかる
- 失敗モード — 書き込みが違反しうる制約はexpect行に宣言しなければならない。宣言漏れも、起こりえない宣言も報告されるので、どの制約違反を処理すべきかがコードに書かれた状態が保たれる
- カーディナリティ — `One`と宣言した文は、本当に1行以下しか返さないことがスキーマから証明される
- 境界 — 論理削除の条件を必ず付ける、テナント列で必ず絞る、テーブルを直接読まずビューを通す、といったチームの規約を検査器に強制させられる
- スキーマ自体 — `schema.sql`の関数・ビュー・ポリシーも型検査される。`-strict`を付けると、インデックスが効かない条件やlookupテーブルで済むenumなどの助言も出る

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

| | |
|---|---|
| [docs/checks.ja.md](docs/checks.ja.md) | 検査器が確かめること全部: 形、意味、失敗モード（PostgreSQLの制約命名規則の表つき）、カーディナリティ、境界 |
| [docs/templates.ja.md](docs/templates.ja.md) | テンプレートで使える構文、ディレクティブ、共有フラグメント、危険な書き方、疎検査 |
| [docs/runtime.ja.md](docs/runtime.ja.md) | `Run` / `Collect` / `First` / `Exec`、`One`、`Batch`、`Copy`、`MatView`、Go型の表、型の登録、エラー、本物のPostgreSQLでのテスト |
| [docs/migrations.ja.md](docs/migrations.ja.md) | `diff` / `apply` / `verify-schema`、`-- @migrate`宣言、seed済みテーブル、必要な環境 |
| [docs/flags.ja.md](docs/flags.ja.md) | 全フラグ、`-strict`の助言一覧、エディタ設定 |
| [design.md](design.md) | 動機とアーキテクチャ |

## 現状

アナライザーはPostgreSQL自身のカタログから組み上げたpure GoのPostgreSQL 17アナライザーで、本物のPostgreSQLはテストで答え合わせの相手（オラクル）としてだけ使う。152本のgolden文と、PostgreSQL自身の回帰テストコーパス（`src/test/regress`の22,000文、リリース17.5）でオラクルと一致している。`go test ./...`はこのコーパスをアナライザーと本物のPostgreSQLの両方に流して突き合わせ、新しい不一致が出たら失敗する。既知の不一致19件（行レベルセキュリティの再帰、権限、サーバー内部のエラー、意図的な相違3件）は`internal/analyze/testdata/regress_baseline.txt`に列挙してある。検査器とランタイムは4つのexampleが使う範囲をカバーし、マイグレーション側はテストシナリオを埋め込みPostgreSQLで往復させて確認している。

テストスイート全体は約30秒。コーパスは`internal/analyze/testdata/tools/fetch-regress.sh`で一度取得する（無ければそのテストはSkip）。埋め込みPostgreSQLは`~/.cache/sqlshape`にキャッシュされる。マイグレーションのテストには`pg_dump` 17以上が`PATH`に必要で、無ければSkipする。`.github/workflows/test.yml`がこれらをキャッシュ込みで走らせている。

## ライセンス

Apache License 2.0（[LICENSE](LICENSE)）。埋め込んでいる`pg_catalog`のデータと、PostgreSQLから移植した検証規則はPostgreSQL Licenseに従う（[NOTICE](NOTICE)）。
