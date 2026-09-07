# ランタイム

[English](runtime.md)

`sqlshape` パッケージは検査済みの文を pgx 上で実行する。`DB` は文の実行先で、`*pgx.Conn`、`*pgxpool.Pool`、`pgx.Tx`
のどれも満たすので、文はそのままトランザクション上で走る。

## 文

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range ListOrders.Run(ctx, db, p) { ... }   // iter.Seq2[Order, error]、ストリーム
orders, err := ListOrders.Collect(ctx, db, p)            // []Order
first, err  := ListOrders.First(ctx, db, p)              // 最初の行、無ければ ErrNoRows
tag, err    := ListOrders.Exec(ctx, db, p)               // pgconn.CommandTag、行は捨てる
```

`One[R, P]` は `Single` を返す。`Render` と `Unprepared` は同じで、実行方法は 3 つ:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := UserByEmail.Get(ctx, db, p)    // 無ければ ErrNoRows
u, ok, err := UserByEmail.Find(ctx, db, p)   // ok が有無
tag, err   := MarkPaid.Exec(ctx, db, p)      // 1 行も触らなければ ErrNoRows
```

3 つとも 2 行目が来たら `ErrManyRows` を返す。検査器が来ないと証明したので、データベースが証明の下で変わったということ。

`Stmt.Unprepared()` はサーバー側 prepared statement なしで走るコピーを返す。プランナーが毎回実際の値でカスタムプランを
作る。パラメータの値分布が偏っていて、pgx の statement cache が汎用プランに落ち着いてしまう文に使う。それ以外では
prepared statement のキャッシュは pgx のもので、展開ごと。

## 行マッピング

結果列は名前でフィールドに対応づけられる: `col:"..."` タグ、次に `db:"..."`、次にフィールド名の snake_case。
埋め込み struct は平坦化される。nullable なフィールド（ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`）は NULL を
零値として受け、この展開の結果に列が無い nullable なフィールドは零値のまま（一部の分岐だけが選ぶ列）。スカラーの `R`
は単一の列を受ける。`numeric` を `string` で受けると全桁が残る。

Go の enum 型は `Known() bool`（`Labelled` インターフェース）を実装できる。マッパーはこのビルドが知らないラベルを、
switch できない値をアプリケーションに渡す代わりに `*UnknownLabelError` で拒否する。

## ネストした行とユーザー型

`array_agg(row(o.id, o.total))`、`array_agg(o)`、`row(...)`、複合型の列は struct または struct のスライスにフィールド
ごとに読まれる。無名レコードは位置で、名前付き複合型は列順で。複合型パラメータ（SQL が `money_amount` を期待する
`{{.Price}}`、`order_items[]` を期待する `{{.Items}}`）は同じ規則で struct や struct のスライスから encode される。

pgx はユーザー定義型（enum、複合型、ドメイン、範囲型、多重範囲型、それらの配列）を decode する前に知っていなければ
ならない。`Run` は結果が必要とする型を初めて出会った時点で接続に読み込む。無名レコードの内側にネストした型は scan 前に
見えないので、その場合とプールでは一度に全部登録する:

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
	return sqlshape.LoadUserTypes(ctx, conn)
}
```

pgx 自身のローダーが読まない拡張のスカラー型（`citext`、`hstore`、`ltree`、…）も登録される: `hstore` は pgx の hstore
codec（`map[string]*string`）で、それ以外はテキストとして（`string`、配列は `[]string`）。`money` など pgx に codec の
無い型を持つテーブルも読み込める。

宣言型バインディング（`// sqlshape: type money_amount`）を持つ型は自身の `sql.Scanner` / `driver.Valuer` に任される。
ランタイムはその列について PostgreSQL にテキスト形式を求めるので、Scanner は値のテキスト形を受ける。

## エラー

制約違反（SQLSTATE クラス 23）は PostgreSQL が報告した `Code`、`Constraint`、`Table`、`Column`、`Detail` を持つ
`*ConstraintError` として返り、`*pgconn.PgError` をラップする。`Key()` はテンプレートの expect 行が書くとおりの違反:
制約名、または NOT NULL なら `table.column`。expect 行が名指した SQLSTATE（トリガーの `P0401`、または
`-- sqlshape: error` で付けた名前）も同じようにラップされる。

```go
_, err := CreateCustomer.First(ctx, db, p)
if sqlshape.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

名前は検査器が列挙したものなので、コードが扱わない違反は expect 行が予告していたもの
（[checks.ja.md](checks.ja.md#失敗モード-この書き込みは何で失敗しうるか)）。`ErrNoRows` は `pgx.ErrNoRows`。
`IsNoRows(err)` で判定できる。

## バッチ

`Batch` は複数の文を 1 往復で送る（`pgx.Batch`）:

```go
b := sqlshape.NewBatch()
orders := sqlshape.Queue(b, ListOrders, ListParams{Status: &paid})
paid   := sqlshape.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
if err := b.Send(ctx, db); err != nil { ... }
rows, err := orders.Rows()    // []Order。First() で最初の行
tag, err  := paid.Tag()
```

`BatchDB` は `SendBatch` を持つもの: 接続、プール、トランザクション。バッチの途中では型を読み込めないので、ユーザー enum や
複合型が絡むなら先に `LoadUserTypes` を呼ぶ（プールなら `AfterConnect`）。行に `sql.Scanner` 型を持つ文は pgx のバッチに
乗れない（バッチはテキスト形式を求めない）。`Send` はそれをバッチの直後にキュー順で普通のクエリとして走らせる。

## 一括ロード

```go
var loadItems = sqlshape.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

n, err := loadItems.From(ctx, db, items)           // []Item
n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
```

`Copy` は `pgx.CopyFrom` による `COPY ... FROM`: 各列はそれに束縛される `R` のフィールドから供給される（タグまたは
snake_case、埋め込み struct は平坦化）。列を与えなければ各フィールドが自分の名前の列を供給し、スカラーの `R` は 1 列を
供給する。検査器はテーブル、列、各列の型とフィールドの対応、省いた列がすべて既定値を持つことを検証する。

## マテリアライズドビュー

```go
var OrderStats = sqlshape.MatView("order_stats")

err := OrderStats.Refresh(ctx, db)              // 完了まで読み手はブロックされる
err := OrderStats.RefreshConcurrently(ctx, db)  // ビューに一意インデックスが要る
```

検査器は名前を schema.sql と照合し、`-strict` では `RefreshConcurrently` の呼び出しに対して一意インデックスの存在を
確認する。

## ランタイムは検査器が見ていない SQL を拒否する

`Render(p)` はテンプレートを `p` に対して評価し、`$n` プレースホルダ付きの SQL と順序どおりの引数を返す。これは
テンプレート意味論の 2 つ目の実装なので、実行前に同じ分岐シグネチャの静的展開とバイト単位で比較し、違えばクエリでなく
エラーになる。比較せずに信用するのは 2 つ: 3 回以上の `range` は静的な双子が無いので 2 回反復の形で検査済みとし、
検査器が疎に展開したテンプレート（[templates.ja.md](templates.ja.md#分岐が多いとき)）。

## 実 PostgreSQL でのテスト

`pgtest` は schema.sql を適用した埋め込み PostgreSQL（初回にダウンロード、`~/.cache/sqlshape` にキャッシュ）を
一時ディレクトリで起動し、`Close` で消える:

```go
schemaSQL, _ := pgtest.ReadSchema("schema.sql")   // または schema/ ディレクトリ
db, err := pgtest.Start(ctx, schemaSQL)
defer db.Close()

if err := db.Verify(ctx, ListOrders, UserByEmail, CreateCustomer); err != nil {
	t.Fatal(err)
}
conn := db.Conn()   // *pgx.Conn。db.ConnString() もある
```

`Verify` は各文のすべての展開をサーバーで prepare し、PostgreSQL のパラメータ型・結果列の名前と型・または拒否を、
検査器の結論と比較する。不一致はその文についての検査器の判定が信用できないということで、エラーは各々を SQL と両者の
記述付きで列挙する。アプリケーション自身のテストの隣に置けば、静的検査がそのコードの走る PostgreSQL で成り立つ証拠を
テストスイートが持つ。`db.Conn()` はビュー・関数・トリガーを叩くための素の接続。
