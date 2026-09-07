# sqlshape

SQL は SQL のまま書く。周りの Go コードが SQL に合っていることは `go vet` の検査器が証明する。

[English](README.md)

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email, name, deleted_at FROM users WHERE email = {{.Email}}`)

u, err := ByEmail.Get(ctx, db, struct{ Email string }{Email: email})
```

- 生成ではなく検査。すべての文を pure Go の PostgreSQL アナライザーが `schema.sql` と照らして解析する。
  結果列は行の型に、`{{.X}}` パラメータはパラメータの型に合っていなければならず、nullability は強制され、
  `One` は 1 行以下と証明できなければならず、書き込みは違反しうる制約を宣言しなければならない。
  結果はエディタの Problems ペインに出る。
- 素の SQL、注入なし。テンプレートは Go の `text/template` で、`{{.X}}` は必ず `$n` パラメータになり、
  テキストにはならない。`{{if}}` / `{{range}}` の組み合わせはすべて展開されて検査される。
  ランタイムは検査器が見ていない描画を拒否する。
- `schema.sql` が唯一の定義。検査器はこれを読み、`pgtest` はこれを適用した実 PostgreSQL でテストを走らせ、
  `sqlshape diff` はこれからマイグレーションを導く。ビュー・関数・ドメイン・複合型・行レベルセキュリティ・
  seed 済み lookup テーブルはすべて検査対象なので、データベースは生のテーブルではなく型付きの API を公開できる。

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

`DeletedAt` を `*time.Time` にし、INSERT の 1 行目に `-- sqlshape: expect users_email_key` を置けばパッケージは通る。
実行時は:

```go
users, err := Users.Collect(ctx, pool, struct{ Name *string }{})          // []User
id, err := Create.First(ctx, pool, struct{ Email, Name string }{e, n})     // int64
if sqlshape.Violates(err, "users_email_key") { /* 宣言した失敗モード */ }
```

`db` は pgx が返すもの何でもよい: `*pgxpool.Pool`、`*pgx.Conn`、`pgx.Tx`。

保存時に検査器を走らせるには、一度ビルドして `go vet` に渡す。gopls はサードパーティのアナライザーを
読めないが、Go 拡張の vet-on-save は走らせられる:

```
$ go build -o "$(go env GOPATH)/bin/sqlshape" github.com/kr9ly/sqlshape/cmd/sqlshape
$ go vet -vettool="$(go env GOPATH)/bin/sqlshape" ./...
```

VS Code: `"go.vetOnSave": "workspace"`、`"go.vetFlags": ["-vettool=/path/to/sqlshape"]`。
`type Row struct{}` と `type Params struct{}` を空で宣言し、クエリを書いて保存すると、すべての診断に
SQL から struct を書き起こす quick fix が付く（コマンドラインでは `sqlshape -fix ./...`）。
詳細は [docs/flags.ja.md](docs/flags.ja.md#エディタで使う)。

## Examples

同じ受注台帳を、データベースへの信頼の度合いごとに 4 段階で:

| | 使うもの | 読むとき |
|---|---|---|
| [`examples/1-tables`](examples/1-tables) | 素のテーブル、`Query` / `One`、テンプレート、値集合としての seed 済み lookup テーブル、`expect` 行、`pgtest.Start` + `Verify` | ORM から来て、手元のテーブルに対する検査済み SQL が欲しい |
| [`examples/2-views`](examples/2-views) | 読みモデルとしてのビュー（JOIN・名前・集約・論理削除の述語を一度だけ決める）、書きは引き続きテーブルへの INSERT / UPDATE、`-no-table-reads` | ロジックはまだ移さず、物の名前だけデータベースに持たせたい |
| [`examples/3-database-api`](examples/3-database-api) | 書き込みは関数、ドメイン、enum と CHECK の値集合、複合型、トリガーの SQLSTATE、`-no-tables` | 意味はスキーマが持ち、アプリケーションには API を見せたい |
| [`examples/4-everything`](examples/4-everything) | 境界としてのスキーマ、拡張、範囲型、ネストした行、宣言型バインディング、複合配列パラメータ、Batch、Copy、論理削除ポリシー、行レベルセキュリティ、テナント固定、全フラグ | 全部を一度に見たい |

それぞれに `schema.sql`、何を示すかを書いた `doc.go`、全文を実 PostgreSQL で検証するテストがある。
`go run ./cmd/sqlshape ./examples/...` で全部検査できる。

## 何を検査するか

全リストは [docs/checks.ja.md](docs/checks.ja.md)。1 行ずつ:

- 形 — 結果列 ↔ `R` のフィールド、`{{.X}}` のパス ↔ `P` のフィールド、nullability、ネストした行
  （`array_agg(row(...))`、複合型）から struct へ、検証済みの pgx Go 型表、自前の型を PostgreSQL の型に
  結びつける `// sqlshape: type money_amount`
- 意味 — enum・seed 済み lookup テーブルのキー・CHECK の値集合・キー列・ドメインに出会った Go の named type は
  使用箇所からそれに束縛され、定数はラベルと diff される。同一性やドメインの混用は報告され、ドメインは SQL 内で不透明な単位
- 失敗モード — 書き込みは違反しうる制約を `-- sqlshape: expect users_email_key, orders.total` で宣言する。
  抜けも起こりえないものも報告され、ランタイムは同じ名前で `ConstraintError` を返す
- カーディナリティ — `One[R, P]` はすべての展開で 1 行以下と証明される。JOIN・ビュー・サブクエリ・CTE を通して
- 境界 — `-- sqlshape: visible where deleted_at IS NULL`、行レベルセキュリティのポリシー、
  `-require-columns=tenant_id`、`-no-table-reads` / `-no-tables` / `-schemas`、検査器を迂回する生のドライバ呼び出し
- 危険 — 文字列リテラルやコメントの中の `{{.X}}`、`ORDER BY` 項目としてのパラメータ、定数でない共有フラグメント
- スキーマの問題 — `schema.sql` の SQL 関数・ビュー・ポリシーの本体は読み込み時に一度型検査され、`-strict` では
  助言的な指摘も出る（`ORDER BY` の無い `LIMIT`、先頭列にインデックスの無い述語、lookup テーブルで済む enum、…）

## マイグレーション

マイグレーションファイルは無い。`sqlshape` がデータベースと `schema.sql` を比較し、差分から動く:

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql へ至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト: データベースが schema.sql と違う箇所
```

`apply` は、データベース + DDL が `schema.sql` として読み戻せなければ拒否し、`-packages` 付きなら DDL が落とす列に
依存する Go の文が残っていても拒否する。リネーム・enum ラベルの削除・backfill は `schema.sql` の `-- @migrate` 行で
宣言する。seed 済み lookup テーブルは行単位で diff され、`MERGE` 1 文で揃えられる。
[docs/migrations.ja.md](docs/migrations.ja.md) を参照。

## ドキュメント

| | |
|---|---|
| [docs/checks.ja.md](docs/checks.ja.md) | 検査器が検証するものすべて: 形、意味、失敗モード（PostgreSQL の制約命名規則つき）、カーディナリティ、境界 |
| [docs/templates.ja.md](docs/templates.ja.md) | テンプレートのサブセット、ディレクティブ、共有フラグメント、危険、疎検査 |
| [docs/runtime.ja.md](docs/runtime.ja.md) | `Run` / `Collect` / `First` / `Exec`、`One`、`Batch`、`Copy`、`MatView`、Go 型表、型登録、エラー、実 PostgreSQL でのテスト |
| [docs/migrations.ja.md](docs/migrations.ja.md) | `diff` / `apply` / `verify-schema`、`-- @migrate` 宣言、seed 済みテーブル、要件 |
| [docs/flags.ja.md](docs/flags.ja.md) | 全フラグ、`-strict` の助言、エディタ設定 |
| [design.md](design.md) | 動機とアーキテクチャ |

## 現状

アナライザーは PostgreSQL 自身のカタログから組み上げた pure Go の PostgreSQL 17 アナライザーで、実 PostgreSQL は
テストのオラクルとしてだけ使う。152 本の golden 文と、PostgreSQL 自身の回帰コーパス（`src/test/regress` の
22,000 文、リリース 17.5）でオラクルと一致する。`go test ./...` はコーパスを両者に並走させてゲートし、既知の
不一致 19 件（行レベルセキュリティの再帰、権限、サーバー内部、意図的な相違 3 件）は
`internal/analyze/testdata/regress_baseline.txt` に列挙してある。検査器とランタイムは 4 つの example が使う表面を
カバーし、マイグレーション側はシナリオを埋め込み PostgreSQL で往復させている。

スイート全体は約 30 秒。コーパスは `internal/analyze/testdata/tools/fetch-regress.sh` が一度取得する
（無ければテストは Skip）。埋め込み PostgreSQL は `~/.cache/sqlshape` にキャッシュされ、マイグレーションのテストには
`pg_dump` 17 以上が `PATH` に要る（無ければ Skip）。`.github/workflows/test.yml` がそのキャッシュ込みで走らせる。

## ライセンス

Apache License 2.0（[LICENSE](LICENSE)）。埋め込んだ `pg_catalog` データと PostgreSQL から移植した検証規則は
PostgreSQL License の下で使っている（[NOTICE](NOTICE)）。
