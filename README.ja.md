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

| | 使うもの | 読むとき |
|---|---|---|
| [`examples/1-tables`](examples/1-tables) | 素のテーブル、`Query` / `One`、テンプレート、値集合としてのseed済みlookupテーブル、expect行、`pgtest.Start` + `Verify` | ORMから来て、手元のテーブルに対するSQLを検査したい |
| [`examples/2-views`](examples/2-views) | 読みはビュー経由（JOIN・列名・集約・論理削除の条件をビューで一度だけ決める）、書きは引き続きテーブルへのINSERT / UPDATE、`-no-table-reads` | ロジックはまだ移さずに、名前の決定権だけデータベースに持たせたい |
| [`examples/3-database-api`](examples/3-database-api) | 書き込みは関数、ドメイン、enumとCHECKによる値集合、複合型、トリガーのSQLSTATE、`-no-tables` | 意味はスキーマに持たせ、アプリケーションにはAPIだけを見せたい |
| [`examples/4-everything`](examples/4-everything) | 境界としてのスキーマ、拡張、範囲型、ネストした行、宣言型バインディング、複合型配列のパラメータ、Batch、Copy、論理削除ポリシー、行レベルセキュリティ、テナント固定、全フラグ | 全機能を一度に見たい |

## 何を検査するか

全リストは[docs/checks.ja.md](docs/checks.ja.md)にある。要点だけ挙げる:

- 形 — 結果列と`R`のフィールド、`{{.X}}`のパスと`P`のフィールドの対応、nullability、ネストした行（`array_agg(row(...))`や複合型）と構造体の対応、pgxが実際に扱えるGo型の表、自前の型をPostgreSQLの型に結びつける`// sqlshape: type money_amount`
- 意味 — enum、seed済みlookupテーブルのキー、CHECKによる値集合、キー列、ドメインに使われたGoのnamed typeは、その使用箇所からそれぞれに結びつけられ、定数とラベルの過不足が報告される。別のテーブルのIDや別のドメインを混ぜると報告され、SQLの中でもドメインは異なる単位として扱われる
- 失敗モード — INSERT / UPDATE / DELETEは違反しうる制約を`-- sqlshape: expect users_email_key, orders.total`のように宣言しなければならない。宣言漏れも、起こりえない宣言も報告される。実行時には同じ名前で`ConstraintError`が返る
- カーディナリティ — `One[R, P]`は、JOIN・ビュー・サブクエリ・CTEを辿って、すべての展開で1行以下になることが証明される
- 境界 — `-- sqlshape: visible where deleted_at IS NULL`、行レベルセキュリティのポリシー、`-require-columns=tenant_id`、`-no-table-reads` / `-no-tables` / `-schemas`、検査器を迂回する生のドライバ呼び出し
- 危険な書き方 — 文字列リテラルやコメントの中の`{{.X}}`、`ORDER BY`の項目に直接置いたパラメータ、定数でない共有フラグメント
- スキーマ自体の問題 — `schema.sql`のSQL関数・ビュー・ポリシーの本体は読み込み時に一度型検査される。`-strict`では助言も出る（`ORDER BY`の無い`LIMIT`、インデックスが効かない条件、lookupテーブルで済むenumなど）

## マイグレーション

マイグレーションファイルは書かない。`sqlshape`がデータベースと`schema.sql`を比較し、その差分から動く:

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

`apply`は、データベースにDDLを当てた結果が`schema.sql`と一致することを確認してから実行する。`-packages`を付けると、DDLが落とす列にまだ依存しているGoの文があれば拒否する。リネーム、enumラベルの削除、backfillはdiffだけでは決められないので、`schema.sql`に`-- @migrate`行で宣言する。seed済みlookupテーブルは行単位で比較され、`MERGE` 1文で揃えられる。詳細は[docs/migrations.ja.md](docs/migrations.ja.md)。

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
