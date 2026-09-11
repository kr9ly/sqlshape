# PostgreSQL

[English](postgres.md)

sqlshapeのうちPostgreSQLに属するものを1枚にまとめる。スキーマが名乗るバージョン、検査器が何を埋め込みどう検証しているか、pgxの上のランタイム、マイグレーションコマンド。規則そのものは[checks.ja.md](checks.ja.md)にあり、どのデータベースでも同じである。規則の中に出てくる名前・番号・型が、`postgres`を名乗るスキーマではどこから来るのか、がこのページの中身になる。

## スキーマがバージョンを名乗る

```sql
-- sqlshape: postgres 17
CREATE TABLE ...
```

`schema.sql`は、どのバージョンのPostgreSQL向けに書かれているかを1回宣言する。17か18。この宣言が判定の土台になる。スキーマと全部の文を読む文法、型・関数・演算子を解決するカタログ、マイグレーション系コマンドが起動するPostgreSQLのバージョンが、ここで決まる。宣言の無いスキーマは読まない。

新しいバージョンだけが受け付ける構文（`RETURNING old.*`、`WITHOUT OVERLAPS`、`NOT ENFORCED`、`VIRTUAL`な生成列は18のもの）は、古いバージョンを宣言していれば構文エラーになる。そのバージョンのサーバに流したときと同じ結果である。PostgreSQLのバージョンを上げる作業は、この数字を変えて検査器の報告を読むことになる。

## 検査器が埋め込んでいるもの

パーサは宣言したバージョンのlibpg_queryで、WebAssemblyとして埋め込みwazeroで動かす。サーバが読める文は検査器も同じように読む。SELECTとDML、MERGE、CTE、ウィンドウ関数、GROUPING SETS、SQL/JSON、範囲型、18の`RETURNING old` / `new`と時制キー、citextやhstoreなどの拡張、ビュー・関数（SQLとPL/pgSQLの本体まで）・トリガ・ポリシーを含むDDL。アナライザーはそのバージョンの`pg_catalog`から組み上げたpure Goの実装で、検査時にPostgreSQLへ接続することはない。Cコンパイラもリンクするライブラリも要らない。初回だけモジュールのコンパイルに1秒ほどかかり、結果はユーザーのキャッシュディレクトリ（Linuxでは`~/.cache/sqlshape`）に置かれる。

判定の裏付けはPostgreSQL自身の回帰テストである。`src/test/regress`の文を、アナライザーと同じバージョンの本物のPostgreSQLに並走させ、パラメータの型・結果列・エラーの判定が一致することを確認している。一致しないのは17で22,103文のうち19件、18で23,384文のうち31件。全件を`check/postgres/analyze/testdata/regress_baseline_<version>.txt`に列挙してあり、どれも静的解析では判定できないもの（行レベルセキュリティの再帰、権限、サーバ内部のエラー）か、検査器が正しくサーバのDescribeには見えないもの（INSERTの`RETURNING old`がNULLであること）である。この突き合わせは`go test ./...`の一部なので、新しい不一致が出ればテストが失敗する。

## 規則が使うもの

- 型。PostgreSQLの各型をpgxがどのGo型で読み書きするかは[下のGo型の表](#go型の表)にある。表に無い型や、自分の型で受けたい型は、Goの型に`// sqlshape: type <PGの型>`と書いて結びつける。
- 制約名。失敗モードの名前はPostgreSQLが制約に付ける名前そのものである（[下の表](#制約の名前)）。規則は[書き込みの失敗に備える](checks.ja.md#書き込みの失敗に備える)。
- `One`の証明の材料は、主キー、`UNIQUE`制約、一意インデックス、文が述語を繰り返す部分一意インデックス、既知の点で固定した時制キー（`WITHOUT OVERLAPS`、18）。`DEFERRABLE`なキーは何も証明しない（[1行だけ返す](checks.ja.md#1行だけ返すone)）。
- スキーマ自身も検査する。SQL関数とPL/pgSQL関数の本体、ビュー、トリガ、行レベルセキュリティのポリシーはロード時に解析され、PL/pgSQLの`RAISE`はその関数を呼ぶ文の失敗モードに加わる。
- PostgreSQLのスキーマ（`CREATE SCHEMA app`）は関係の名前の一部である。`-schemas=a_api,b_private`はパッケージが参照してよいスキーマを絞る。1つのデータベースを複数サービスで使うときの境界になる（[checks.ja.md](checks.ja.md#パッケージは自分のスキーマだけを参照する-schemaspostgresql)）。

PostgreSQLにだけあるもの: `postgres.Copy`と`postgres.MatView`（[下](#ランタイム-pgx)）、PL/pgSQL、ドメイン、複合型と配列、`-schemas`、`// sqlshape: type`、マイグレーションコマンド。

## Go型の表

pgxが実際にscan / encodeできる組み合わせを、稼働中のPostgreSQLで確認したもの。列を受けるときも、パラメータを渡すときも同じ表に従う。

| PostgreSQL | Go |
|---|---|
| `bool` | `bool` |
| `smallint` / `integer` / `bigint` | `int16` / `int32` / `int64` / `int`（狭いGo型で受けると`bigint into int32`のような注記が付く） |
| `real` / `double precision` | `float32` / `float64`（`double precision into float32`は注記付き） |
| `numeric` | `string`（全桁が保たれる）、`pgtype.Numeric`、`big.Rat`、`shopspring/decimal.Decimal`、`apd.Decimal`。floatや整数でも受けられるが精度の注記が付く |
| `text` / `varchar` / `char` / `name` / `citext`などテキスト系の拡張型 | `string`（`string`はパラメータとしてはどの型にも渡せる） |
| `bytea` | `[]byte` |
| `uuid` | `uuid.UUID`（パッケージは問わない）、`[16]byte`、`string` |
| `timestamptz` / `timestamp` / `date` | `time.Time`（`timestamp`と`date`はタイムゾーンや時刻が失われるので`-strict`で注記） |
| `time` | `time.Time`、`string` |
| `interval` | `pgtype.Interval`（月・日・マイクロ秒を分けたまま保持する）。`time.Duration`はこれらを固定長に均してしまうため、PostgreSQL自身のカレンダー演算と日単位でずれ得る旨の常時の注記が付く |
| `json` / `jsonb` | `[]byte`、`json.RawMessage`、`string`、またはpgxがunmarshalできる構造体・スライス・マップ |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)`（各要素は単体の`T`のパラメータ/列と同じ判定を受ける。注記も含めて）。PostgreSQLは配列の要素自体がNOT NULLであることを保証しない――列自身の`NOT NULL`は配列値全体が`NULL`になることを禁じるだけである――ため、`NULL`を受けられない要素型（ポインタでないもの）には常時の注記が付き、`-strict`では拒否としても報告される。`integer[]`に対する`[]int32`、複合型の配列に対する`[]Item`はどちらも`NULL`要素を安全に受けるために`[]*int32`/`[]*Item`が要る（複合型の要素はより厄介で、pgxは`NULL`要素をエラーにせずゼロ値の構造体として黙って復号する） |
| 範囲型 | `pgtype.Range[T]`。`T`はサブタイプと照合される（ユーザー定義の範囲型も同様） |
| 多重範囲型 | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | `pgtype`の対応する型 |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enum、seed済みlookupテーブルのキー、CHECKによる値集合 | Goのnamed string type（[型に意味を持たせる](checks.ja.md#型に意味を持たせる)） |
| ドメイン | 基底型に対応するGo型、またはドメインに結びつけたnamed type |
| 複合型、レコード | 構造体 |

表に無い型、あるいは自前の型で受けたい型は、Go側の型にdocコメントで対応するPostgreSQL型を宣言する:

```go
// sqlshape: type money_amount
type Money struct{ ... }   // sql.Scanner / driver.Valuer を実装する
```

検査器は、SQL側が`money_amount`（その配列と、それを基底型とするドメインを含む）である位置でだけ`Money`を受け入れ、それ以外の位置では報告する。値の変換は型自身の`sql.Scanner` / `driver.Valuer`に任せる（Scannerにはテキスト形式が渡る）。

## 制約の名前

名前を付けた制約はその名前で呼ぶ。名前を付けなかった制約にはPostgreSQLが付ける名前がそのまま使われるので、診断・expect行・実行時エラーで同じ文字列になる。

| 制約 | キー | 例 |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| 列`(a)`の`REFERENCES` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| 列をちょうど1つだけ参照するテーブルの`CHECK` `(a)` | `<table>_<a>_check`（複数列を参照するか、列を参照しないCHECKは`<table>_check`） | `orders_total_check` |
| ドメインの`CHECK` | `<domain>_check` | `yen_check` |
| `EXCLUDE (a, b)` | `<table>_<a>_<b>_excl` | `reservations_room_during_excl` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| ドメインの`NOT NULL` | ドメインの名前（`public`以外はスキーマ修飾） | `email` |
| トリガーが送出するエラー | SQLSTATE、または`-- sqlshape: error`で付けた名前 | `P0401`、`OrderTooLarge` |
| ビューの`WITH CHECK OPTION` | SQLSTATE（PostgreSQL自身の44000エラーは制約名を持たない） | `44000` |

同じ名前になる制約が2つあると、PostgreSQLと同様に番号が付く（`orders_total_check1`）。生成した名前がPostgreSQLの63バイトという識別子の上限を超える場合は、マルチバイト文字を途中で切らないよう、PostgreSQLと同じやり方で切り詰める。

## ランタイム: pgx

```
$ go get github.com/kr9ly/sqlshape/v2            # Query / One: 検査器が読む宣言
$ go get github.com/kr9ly/sqlshape/postgres/v2   # pgxの上で実行する
```

`github.com/kr9ly/sqlshape/postgres/v2`は宣言した文をpgxの上で実行する。実行先の`postgres.DB`は`*pgx.Conn`、`*pgxpool.Pool`、`pgx.Tx`のどれでもよいので、同じ文をトランザクションの中でもそのまま実行できる。`{{.X}}`はワイヤ上で`$n`パラメータになる。

```go
for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error]、逐次読み出し
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // 先頭行。無ければ ErrNoRows
tag, err    := postgres.Exec(ctx, db, MarkPaid, p)                 // pgconn.CommandTag。行は捨てる

u, err     := postgres.Get(ctx, db, UserByEmail, p)                // One: 無ければ ErrNoRows
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)               // One: ok が有無
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)               // One: 1行も触らなければ ErrNoRows
```

どのランタイムでも同じこと（行のマッピング、`One`、検査済みのSQLだけが走る保証、expect行の名前で返るエラー）は[runtime.ja.md](runtime.ja.md)にある。pgxで足されるもの:

- `Stmt.Unprepared()`はサーバ側のprepared statementを使わずに実行する複製を返す。プランナが毎回実際の値でプランを作るので、パラメータの分布が偏っていてpgxのstatement cacheが汎用プランに落ち着いてしまう文に使う。それ以外はpgxのprepared statementキャッシュが展開ごとに働く。
- ネストした行とユーザー定義型。`array_agg(row(o.id, o.total))`、`array_agg(o)`、`row(...)`、複合型の列は、構造体か構造体のスライスにフィールドごとに読み込む。無名のrecordは位置で、名前付きの複合型はその列順で対応する。複合型のパラメータも構造体か構造体のスライスから同じ規則でエンコードする。pgxはユーザー定義型（enum、複合型、ドメイン、範囲、多重範囲、それらの配列）を知らないと復号できない。`Run`は結果に必要な型を初めて出会った接続でロードするが、無名recordの中にネストした型は読む前に見えないので、その場合とプールでは一度全部登録する:

  ```go
  cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
  	return postgres.LoadUserTypes(ctx, conn)
  }
  ```

  pgx自身のローダーが読まない拡張のスカラー型（`citext`、`hstore`、`ltree`など）も登録する。`hstore`はpgxのhstoreコーデック（`map[string]*string`）、他はテキストとして。`// sqlshape: type money_amount`で束縛を宣言した型は、その型自身の`sql.Scanner` / `driver.Valuer`に任せる。ランタイムはその列についてPostgreSQLにテキスト形式を要求し、要求は文ごとに記憶する。その型がその後にdropして作り直されていたら（マイグレーション。同じ名前で新しいOID）、PostgreSQLの「cached plan must not change result type」に気づいて要求を1回やり直す。
- エラー。制約違反（SQLSTATEのクラス23）は`*postgres.ConstraintError`として返る。PostgreSQLが報告した`Code`、`Constraint`、`Table`、`Column`、`Detail`を持ち、`*pgconn.PgError`を包む。`Key()`はexpect行の綴りである。ドメインが起こす`NOT NULL`だけはPostgreSQLが表も列も返さないので、`Key()`はドメインの名前になる。`postgres.Violates(err, key)`で判定する。`postgres.ErrNoRows`は`pgx.ErrNoRows`である。
- `Batch`は複数の文を1往復で送る（`pgx.Batch`）:

  ```go
  b := postgres.NewBatch()
  orders := postgres.Queue(b, ListOrders, ListParams{Status: &paid})
  paid   := postgres.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
  if err := b.Send(ctx, db); err != nil { ... }
  rows, err := orders.Rows()    // []Order。First() で先頭行
  tag, err  := paid.Tag()
  ```

  `BatchDB`は`SendBatch`を持つもの、つまり接続・プール・トランザクションのどれでもよい。バッチの途中では型をロードできないので、ユーザー定義のenumや複合型を使うなら先に`LoadUserTypes`を呼ぶ。行に`sql.Scanner`型を持つ文はpgxのバッチに乗せられない（バッチはテキスト形式を要求しない）ので、`Send`がバッチの直後にキューの順で普通のクエリとして実行する。キューに入れた文の違反は`Run`と同じ規則で`*ConstraintError`になる。`Send`は最初の違反を返し、その後ろの文は実行されない（`ErrNotSent`）。
- `Copy`は`pgx.CopyFrom`による`COPY ... FROM`:

  ```go
  var loadItems = postgres.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

  n, err := loadItems.From(ctx, db, items)           // []Item
  n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
  ```

  各列には`R`のうちその列に対応するフィールドが入る。列を指定しなければ全フィールドが自分の名前の列に入り、スカラーの`R`は1列に入る。検査器はテーブル、列、各列の型とフィールドの型、指定しなかった列に既定値があることを確かめる（[checks.ja.md](checks.ja.md#copyで一括ロードするpostgresql)）。
- `MatView`はマテリアライズドビューを名指す。`Refresh`と`RefreshConcurrently`（後者はビューに一意インデックスが要る。`-strict`が確かめる）。

  ```go
  var OrderStats = postgres.MatView("order_stats")

  err := OrderStats.Refresh(ctx, db)
  err := OrderStats.RefreshConcurrently(ctx, db)
  ```

## マイグレーション

`sqlshape diff`、`apply`、`verify-schema`は、データベースと`schema.sql`の差分からDDLを導き、そのDDLが本当に`schema.sql`に至ることを確かめてから実行する。両側を`pg_dump`の出力として比較するので、宣言したバージョンの`pg_dump`が`PATH`に要り、`schema.sql`を読むために宣言したバージョンの埋め込みPostgreSQL（初回にダウンロードされ、`~/.cache/sqlshape`にキャッシュされる）を起動する。[migrations.ja.md](migrations.ja.md)は全部PostgreSQLのものである。

## License

The PostgreSQL side of the checker (`check/postgres`) embeds `pg_catalog` data and validation
rules ported from PostgreSQL under the PostgreSQL License, see
[check/postgres/NOTICE](../check/postgres/NOTICE). The runtime (`postgres`) is Apache License 2.0,
like the declarations.
