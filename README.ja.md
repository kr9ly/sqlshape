# sqlshape

SQLはSQLのまま書く。周りのGoコードがSQLに合っていることは`go vet`の検査器が証明する。

[English](README.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := ByEmail.Get(ctx, db, struct{ Email string }{Email: email})
```

- 生成ではなく検査。すべての文をpure GoのPostgreSQLアナライザーが`schema.sql`と照らして解析する。
  結果列は行の型に、`{{.X}}`パラメータはパラメータの型に合っていなければならず、nullabilityは強制され、
  `One`は1行以下と証明できなければならず、書き込みは違反しうる制約を宣言しなければならない。
  結果はエディタのProblemsペインに出る。
- 素のSQL、注入なし。テンプレートはGoの`text/template`で、`{{.X}}`は必ず`$n`パラメータになり、
  テキストにはならない。`{{if}}` / `{{range}}`の組み合わせはすべて展開されて検査される。
  ランタイムは検査器が見ていない描画を拒否する。
- `schema.sql`が唯一の定義。検査器はこれを読み、`pgtest`はこれを適用した実PostgreSQLでテストを走らせ、
  `sqlshape diff`はこれからマイグレーションを導く。ビュー・関数・ドメイン・複合型・行レベルセキュリティ・
  seed済みlookupテーブルはすべて検査対象なので、データベースは生のテーブルではなく型付きのAPIを公開できる。

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

`DeletedAt`を`*time.Time`にし、INSERTの1行目に`-- sqlshape: expect users_email_key`を置けばパッケージは通る。
実行時は:

```go
users, err := Users.Collect(ctx, pool, struct{ Name *string }{})          // []User
id, err := Create.First(ctx, pool, struct{ Email, Name string }{e, n})     // int64
if sqlshape.Violates(err, "users_email_key") { /* 宣言した失敗モード */ }
```

`db`はpgxが返すもの何でもよい: `*pgxpool.Pool`、`*pgx.Conn`、`pgx.Tx`。

保存時に検査器を走らせるには、一度ビルドして`go vet`に渡す。goplsはサードパーティのアナライザーを
読めないが、Go拡張のvet-on-saveは走らせられる:

```
$ go build -o "$(go env GOPATH)/bin/sqlshape" github.com/kr9ly/sqlshape/cmd/sqlshape
$ go vet -vettool="$(go env GOPATH)/bin/sqlshape" ./...
```

VS Code: `"go.vetOnSave": "workspace"`、`"go.vetFlags": ["-vettool=/path/to/sqlshape"]`。
`type Row struct{}`と`type Params struct{}`を空で宣言し、クエリを書いて保存すると、すべての診断に
SQLからstructを書き起こすquick fixが付く（コマンドラインでは`sqlshape -fix ./...`）。
詳細は [docs/flags.ja.md](docs/flags.ja.md#エディタで使う)。

## Examples

同じ受注台帳を、データベースへの信頼の度合いごとに4段階で:

| | 使うもの | 読むとき |
|---|---|---|
| [`examples/1-tables`](examples/1-tables) | 素のテーブル、`Query` / `One`、テンプレート、値集合としてのseed済みlookupテーブル、`expect`行、`pgtest.Start` + `Verify` | ORMから来て、手元のテーブルに対する検査済みSQLが欲しい |
| [`examples/2-views`](examples/2-views) | 読みモデルとしてのビュー（JOIN・名前・集約・論理削除の述語を一度だけ決める）、書きは引き続きテーブルへのINSERT / UPDATE、`-no-table-reads` | ロジックはまだ移さず、物の名前だけデータベースに持たせたい |
| [`examples/3-database-api`](examples/3-database-api) | 書き込みは関数、ドメイン、enumとCHECKの値集合、複合型、トリガーのSQLSTATE、`-no-tables` | 意味はスキーマが持ち、アプリケーションにはAPIを見せたい |
| [`examples/4-everything`](examples/4-everything) | 境界としてのスキーマ、拡張、範囲型、ネストした行、宣言型バインディング、複合配列パラメータ、Batch、Copy、論理削除ポリシー、行レベルセキュリティ、テナント固定、全フラグ | 全部を一度に見たい |

それぞれに`schema.sql`、何を示すかを書いた`doc.go`、全文を実PostgreSQLで検証するテストがある。
`go run ./cmd/sqlshape ./examples/...`で全部検査できる。

## 何を検査するか

全リストは [docs/checks.ja.md](docs/checks.ja.md)。1行ずつ:

- 形 — 結果列 ↔ `R`のフィールド、`{{.X}}`のパス ↔ `P`のフィールド、nullability、ネストした行
  （`array_agg(row(...))`、複合型）からstructへ、検証済みのpgx Go型表、自前の型をPostgreSQLの型に
  結びつける`// sqlshape: type money_amount`
- 意味 — enum・seed済みlookupテーブルのキー・CHECKの値集合・キー列・ドメインに出会ったGoのnamed typeは
  使用箇所からそれに束縛され、定数はラベルとdiffされる。同一性やドメインの混用は報告され、ドメインはSQL内で不透明な単位
- 失敗モード — 書き込みは違反しうる制約を`-- sqlshape: expect users_email_key, orders.total`で宣言する。
  抜けも起こりえないものも報告され、ランタイムは同じ名前で`ConstraintError`を返す
- カーディナリティ — `One[R, P]`はすべての展開で1行以下と証明される。JOIN・ビュー・サブクエリ・CTEを通して
- 境界 — `-- sqlshape: visible where deleted_at IS NULL`、行レベルセキュリティのポリシー、
  `-require-columns=tenant_id`、`-no-table-reads` / `-no-tables` / `-schemas`、検査器を迂回する生のドライバ呼び出し
- 危険 — 文字列リテラルやコメントの中の`{{.X}}`、`ORDER BY`項目としてのパラメータ、定数でない共有フラグメント
- スキーマの問題 — `schema.sql`のSQL関数・ビュー・ポリシーの本体は読み込み時に一度型検査され、`-strict`では
  助言的な指摘も出る（`ORDER BY`の無い`LIMIT`、先頭列にインデックスの無い述語、lookupテーブルで済むenum、…）

## マイグレーション

マイグレーションファイルは無い。`sqlshape`がデータベースと`schema.sql`を比較し、差分から動く:

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql へ至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト: データベースが schema.sql と違う箇所
```

`apply`は、データベース + DDLが`schema.sql`として読み戻せなければ拒否し、`-packages`付きならDDLが落とす列に
依存するGoの文が残っていても拒否する。リネーム・enumラベルの削除・backfillは`schema.sql`の`-- @migrate`行で
宣言する。seed済みlookupテーブルは行単位でdiffされ、`MERGE` 1文で揃えられる。
[docs/migrations.ja.md](docs/migrations.ja.md) を参照。

## ドキュメント

| | |
|---|---|
| [docs/checks.ja.md](docs/checks.ja.md) | 検査器が検証するものすべて: 形、意味、失敗モード（PostgreSQLの制約命名規則つき）、カーディナリティ、境界 |
| [docs/templates.ja.md](docs/templates.ja.md) | テンプレートのサブセット、ディレクティブ、共有フラグメント、危険、疎検査 |
| [docs/runtime.ja.md](docs/runtime.ja.md) | `Run` / `Collect` / `First` / `Exec`、`One`、`Batch`、`Copy`、`MatView`、Go型表、型登録、エラー、実PostgreSQLでのテスト |
| [docs/migrations.ja.md](docs/migrations.ja.md) | `diff` / `apply` / `verify-schema`、`-- @migrate`宣言、seed済みテーブル、要件 |
| [docs/flags.ja.md](docs/flags.ja.md) | 全フラグ、`-strict`の助言、エディタ設定 |
| [design.md](design.md) | 動機とアーキテクチャ |

## 現状

アナライザーはPostgreSQL自身のカタログから組み上げたpure GoのPostgreSQL 17アナライザーで、実PostgreSQLは
テストのオラクルとしてだけ使う。152本のgolden文と、PostgreSQL自身の回帰コーパス（`src/test/regress`の
22,000文、リリース17.5）でオラクルと一致する。`go test ./...`はコーパスを両者に並走させてゲートし、既知の
不一致19件（行レベルセキュリティの再帰、権限、サーバー内部、意図的な相違3件）は
`internal/analyze/testdata/regress_baseline.txt`に列挙してある。検査器とランタイムは4つのexampleが使う表面を
カバーし、マイグレーション側はシナリオを埋め込みPostgreSQLで往復させている。

スイート全体は約30秒。コーパスは`internal/analyze/testdata/tools/fetch-regress.sh`が一度取得する
（無ければテストはSkip）。埋め込みPostgreSQLは`~/.cache/sqlshape`にキャッシュされ、マイグレーションのテストには
`pg_dump` 17以上が`PATH`に要る（無ければSkip）。`.github/workflows/test.yml`がそのキャッシュ込みで走らせる。

## ライセンス

Apache License 2.0（[LICENSE](LICENSE)）。埋め込んだ`pg_catalog`データとPostgreSQLから移植した検証規則は
PostgreSQL Licenseの下で使っている（[NOTICE](NOTICE)）。
