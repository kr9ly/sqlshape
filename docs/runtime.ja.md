# ランタイム

[English](runtime.md)

文は`sqlshape`パッケージで宣言し（`Query`、`One`。依存は無い）、DBごとのランタイムモジュールで実行する。`github.com/kr9ly/sqlshape/postgres/v2`はpgxの上で実行するランタイムで、実行先の`postgres.DB`は`*pgx.Conn`、`*pgxpool.Pool`、`pgx.Tx`のどれでもよいので、同じ文をトランザクションの中でもそのまま実行できる。検査器が読むのは宣言だけなので、自前のランタイムで実行してもよい。その場合に得られないものは末尾に書く。

## 文の実行

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error]、逐次読み出し
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // 最初の行。無ければ ErrNoRows
tag, err    := postgres.Exec(ctx, db, ListOrders, p)               // pgconn.CommandTag。行は捨てる
```

`One[R, P]`は`Single`を返す。`Render`と`Unprepared`は`Stmt`と同じで、実行方法は3つある:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := postgres.Get(ctx, db, UserByEmail, p)    // 無ければ ErrNoRows
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)   // ok が有無を表す
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)      // 1行も対象にならなければ ErrNoRows
```

3つとも、2行目が返ってきたら`ErrManyRows`を返す。検査器は1行以下しか返らないことをスキーマから証明しているので、これが起きるのは、証明の根拠になった一意制約が実際のデータベースでは外れているときである。

`Stmt.Unprepared()`は、サーバー側のprepared statementを使わずに実行するコピーを返す。プランナーが毎回、実際のパラメータ値でプランを作る。パラメータの値の分布が偏っていて、pgxのstatement cacheが汎用プランに固定されると遅くなる文に使う。それ以外の文では、prepared statementのキャッシュは展開ごとにpgxが管理する。

## 行のマッピング

結果列は名前でフィールドに対応づけられる。`col:"..."`タグ、次に`db:"..."`タグ、次にフィールド名をsnake_caseにしたものの順で探す。埋め込み構造体のフィールドは平坦化される。NULLを受けられるフィールド（ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`）はNULLをゼロ値として受ける。この展開の結果に列が無いフィールド（一部の分岐だけが選ぶ列）もゼロ値のままになる。`R`がスカラーなら1列を直接受ける。`numeric`は`string`で受けると全桁が保たれる。

Goのenum型に`Known() bool`を実装しておくと（`Labelled`インターフェース）、マッパーはこのビルドが知らないラベルを受け取ったとき、switchで扱えない値をアプリケーションに渡す代わりに`*UnknownLabelError`を返す。

## ネストした行とユーザー定義型

`array_agg(row(o.id, o.total))`、`array_agg(o)`、`row(...)`、複合型の列は、構造体または構造体のスライスにフィールドごとに読み込まれる。無名のレコードは位置で、名前付きの複合型は列の順序で対応づける。複合型のパラメータ（SQL側が`money_amount`を期待する位置の`{{.Price}}`、`order_items[]`を期待する位置の`{{.Items}}`）も、同じ規則で構造体や構造体のスライスからencodeされる。

pgxはユーザー定義型（enum、複合型、ドメイン、範囲型、多重範囲型、およびそれらの配列）をdecodeする前に、その型を知っていなければならない。`Run`は結果に必要な型を、初めて出会った時点でその接続に登録する。ただし無名レコードの内側にネストした型はscanする前には見えないので、その場合とプールを使う場合は、接続時にまとめて登録する:

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
	return postgres.LoadUserTypes(ctx, conn)
}
```

pgx自身の型ローダーが読まない拡張のスカラー型（`citext`、`hstore`、`ltree`など）も登録される。`hstore`はpgxのhstore codec（`map[string]*string`）で、それ以外はテキストとして（`string`、配列なら`[]string`）扱う。`money`のようにpgxにcodecの無い型を持つテーブルも読み込める。

宣言型バインディング（`// sqlshape: type money_amount`）を持つ型は、その型自身の`sql.Scanner` / `driver.Valuer`で変換される。ランタイムはその列についてPostgreSQLにテキスト形式を要求するので、Scannerには値のテキスト表現が渡る。この要求は文ごとに覚えておくので、以降の実行では余計な往復が省ける。もしその後に型が削除・再作成されていたら（マイグレーション、名前は同じでOIDだけ変わる）、ランタイムはPostgreSQLが返す「cached plan must not change result type」に気づいて要求を1回だけ作り直すので、再起動なしに文は動き続ける。

## エラー

制約違反（SQLSTATEクラス23）は`*ConstraintError`として返る。PostgreSQLが報告した`Code`、`Constraint`、`Table`、`Column`、`Detail`を持ち、元の`*pgconn.PgError`を包んでいる。`Key()`はexpect行に書くのと同じ表記の名前を返す。制約名、またはNOT NULLなら`table.column`である。expect行で名指したSQLSTATE（トリガーの`P0401`、または`-- sqlshape: error`で付けた名前）も同じように包まれる。

```go
_, err := postgres.First(ctx, db, CreateCustomer, p)
if postgres.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

この名前は検査器が列挙したものと同じなので、コードが処理していない違反があれば、それはexpect行に書いてあるのに扱っていない違反である（[checks.ja.md](checks.ja.md#書き込みの失敗に備える)）。`ErrNoRows`は`pgx.ErrNoRows`と同じもので、`IsNoRows(err)`で判定できる。

## バッチ

`Batch`は複数の文を1往復で送る（`pgx.Batch`）:

```go
b := postgres.NewBatch()
orders := postgres.Queue(b, ListOrders, ListParams{Status: &paid})
paid   := postgres.QueueOne(b, MarkPaid, struct{ ID int64 }{id})
if err := b.Send(ctx, db); err != nil { ... }
rows, err := orders.Rows()    // []Order。First() なら最初の行
tag, err  := paid.Tag()
```

`BatchDB`は`SendBatch`を持つもので、接続・プール・トランザクションのどれでもよい。バッチの途中では型を登録できないので、ユーザー定義のenumや複合型を使う文があるなら、先に`LoadUserTypes`を呼んでおく（プールなら`AfterConnect`で）。結果に`sql.Scanner`型が含まれる文はpgxのバッチには乗せられない（バッチではテキスト形式を要求できない）ので、`Send`はそれをバッチの直後に、キューに入れた順で通常のクエリとして実行する。

キューに入れた文の制約違反や`-- sqlshape: expect`のSQLSTATEは、`Run`と同じ規則で`*ConstraintError`に包まれる。行を返す文として積んだか、`Exec`（`RETURNING`なし）として積んだかは関係ない。`Send`は最初に失敗した文のエラーを返し、それより後にキューへ入れた文は実行されない。それらの`Rows` / `Tag`は`ErrNotSent`を返す。

## 一括ロード

```go
var loadItems = postgres.Copy[Item]("order_items", "order_id", "line_no", "sku", "qty")

n, err := loadItems.From(ctx, db, items)           // []Item
n, err := loadItems.FromSeq(ctx, db, seq)          // iter.Seq[Item]
```

`Copy`は`pgx.CopyFrom`による`COPY ... FROM`である。各列には、それに対応する`R`のフィールド（タグまたはsnake_case、埋め込み構造体は平坦化）から値が入る。列を指定しなければ各フィールドが自分の名前の列に入り、`R`がスカラーなら1列に入る。検査器はテーブルと列の存在、各列の型とフィールドの対応、指定しなかった列に既定値があることを確かめる。

## マテリアライズドビュー

```go
var OrderStats = postgres.MatView("order_stats")

err := OrderStats.Refresh(ctx, db)              // 完了まで読み手はブロックされる
err := OrderStats.RefreshConcurrently(ctx, db)  // ビューに一意インデックスが必要
```

検査器は名前が`schema.sql`にあることを確かめ、`-strict`では`RefreshConcurrently`を呼んでいるビューに一意インデックスがあることも確かめる。

## 検査済みのSQLのみが実行できる

実行のたびに、テンプレートから組み立てたSQLが、検査器が同じ分岐の組み合わせについて検査したSQLと一字一句一致することを確認する。一致しなければクエリは投げずにエラーになる:

```
sqlshape: rendered SQL differs from the checked expansion [if@64:then]: the runtime evaluator and the checker disagree; please report this
```

これが出るのはsqlshapeの不具合なので、報告してほしい。通常の使い方で出ることはない。

確認できない場合が2つある。`{{range}}`が3要素以上のとき（検査は2要素までで行う）と、分岐の組み合わせが256を超えて代表だけが検査されたとき（[templates.ja.md](templates.ja.md#分岐が多いとき)）。この2つでは、分岐の形が検査したものと同じであることだけを確認して実行する。

## 自前のランタイムでは得られないもの

検査器が認識するのは宣言であってランタイムではない（`-query`で自前のマーカー関数を登録できる。[flags.ja.md](flags.ja.md)）。次の3つの約束はランタイムのもので、`sqlshape/postgres`で実行したときだけ成立する: 送る SQL が検査器の確かめた展開とバイト単位で一致すること（上の節）、違反が expect 行の綴りの名前で`ConstraintError`として返ること、`One`の文が2行目を返したらエラーになること。

## 本物のPostgreSQLでのテスト

`pgtest`は`schema.sql`を適用した埋め込みPostgreSQL（初回にダウンロードされ、`~/.cache/sqlshape`にキャッシュされる）を一時ディレクトリに起動する。`Close`で消える:

```go
schemaSQL, _ := pgtest.ReadSchema("schema.sql")   // schema/ ディレクトリでもよい
db, err := pgtest.Start(ctx, schemaSQL)
defer db.Close()

if err := db.Verify(ctx, ListOrders, UserByEmail, CreateCustomer); err != nil {
	t.Fatal(err)
}
conn := db.Conn()   // *pgx.Conn。db.ConnString() もある
```

`Verify`は各文のすべての展開をサーバーでprepareし、PostgreSQLが返すパラメータ型、結果列の名前と型、または構文エラーを、検査器の結論と比較する。不一致があれば、その文についての検査器の判定は信用できないということであり、エラーには不一致ごとにSQLと両者の判定が列挙される。アプリケーションのテストの隣に置いておけば、静的検査の結論が実際に使うPostgreSQLで成り立つことをテストスイートが保証する。`db.Conn()`はビュー・関数・トリガーを直接叩くための接続である。
