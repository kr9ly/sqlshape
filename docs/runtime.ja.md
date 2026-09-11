# ランタイム

[English](runtime.md)

文は`sqlshape`パッケージで宣言し（`Query`、`One`。依存は無い）、使うデータベースのランタイムモジュールで実行する。`github.com/kr9ly/sqlshape/postgres/v2`はpgxの上（[postgres.ja.md](postgres.ja.md#ランタイム-pgx)）、`github.com/kr9ly/sqlshape/mysql/v2`は`database/sql`の上（[mysql.ja.md](mysql.ja.md#ランタイム-databasesql)）。このページは両方が同じにやることを書く。例はpostgresの関数で示すが、mysqlの関数も同じ名前と同じ形をしている。検査器が読むのは宣言だけなので、自前のランタイムで実行してもよい。その場合に得られないものは末尾に書く。

## 文の実行

```go
var ListOrders = sqlshape.Query[Order, ListParams](`...`)

for o, err := range postgres.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error]、逐次読み出し
orders, err := postgres.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := postgres.First(ctx, db, ListOrders, p)              // 最初の行。無ければ ErrNoRows
tag, err    := postgres.Exec(ctx, db, MarkPaid, p)                 // ドライバの結果（pgconn.CommandTag / sql.Result）。行は捨てる
```

`One[R, P]`は`Single`を返す。`Render`は`Stmt`と同じで、実行方法は3つある:

```go
var UserByEmail = sqlshape.One[User, struct{ Email string }](`...`)

u, err     := postgres.Get(ctx, db, UserByEmail, p)    // 無ければ ErrNoRows
u, ok, err := postgres.Find(ctx, db, UserByEmail, p)   // ok が有無を表す
tag, err   := postgres.ExecOne(ctx, db, MarkPaid, p)      // 1行も対象にならなければ ErrNoRows
```

3つとも、2行目が返ってきたら`ErrManyRows`を返す。検査器は1行以下しか返らないことをスキーマから証明しているので、これが起きるのは、証明の根拠になった一意制約が実際のデータベースでは外れているときである。

## 行のマッピング

結果列は名前でフィールドに対応づけられる。`col:"..."`タグ、次に`db:"..."`タグ、次にフィールド名をsnake_caseにしたものの順で探す。埋め込み構造体のフィールドは平坦化される。NULLを受けられるフィールド（ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`のようなドライバのNULL可の値型）はNULLをゼロ値として受ける。この展開の結果に列が無いフィールド（一部の分岐だけが選ぶ列）もゼロ値のままになる。`R`がスカラーなら1列を直接受ける。`numeric` / `DECIMAL`は`string`で受けると全桁が保たれる。列やパラメータをどのGo型で受けられるかはデータベースごとの表にある（[postgres.ja.md](postgres.ja.md#規則が使うもの)、[mysql.ja.md](mysql.ja.md#go型の表)）。

Goのenum型に`Known() bool`を実装しておくと（`Labelled`インターフェース）、マッパーはこのビルドが知らないラベルを受け取ったとき、switchで扱えない値をアプリケーションに渡す代わりに`*UnknownLabelError`を返す。

## エラー

制約違反はランタイムの`*ConstraintError`として返る（`postgres.ConstraintError`は`*pgconn.PgError`を、`mysql.ConstraintError`はドライバのエラーを包む）。サーバが報告した内容を持ち、`Key()`はexpect行に書くのと同じ表記の名前を返す。データベースが制約に付ける名前、またはNOT NULLなら`table.column`である（[postgres.ja.md](postgres.ja.md#規則が使うもの)、[mysql.ja.md](mysql.ja.md#制約名と失敗モード)）。expect行で名指したSQLSTATE（PostgreSQLのトリガの`P0401`、または`-- sqlshape: error`で付けた名前）も同じように包まれる。

```go
_, err := postgres.First(ctx, db, CreateCustomer, p)
if postgres.Violates(err, "customers_email_key") {
	return ErrEmailTaken
}
```

この名前は検査器が列挙したものと同じなので、コードが処理していない違反があれば、それはexpect行に書いてあるのに扱っていない違反である（[checks.ja.md](checks.ja.md#書き込みの失敗に備える)）。`ErrNoRows`はドライバのもの（`pgx.ErrNoRows`、`sql.ErrNoRows`）で、`IsNoRows(err)`で判定できる。

## 検査済みのSQLのみが実行できる

実行のたびに、テンプレートから組み立てたSQLが、検査器が同じ分岐の組み合わせについて検査したSQLと一字一句一致することを確認する。一致しなければクエリは投げずにエラーになる:

```
sqlshape: rendered SQL differs from the checked expansion [if@64:then]: the runtime evaluator and the checker disagree; please report this
```

これが出るのはsqlshapeの不具合なので、報告してほしい。通常の使い方で出ることはない。

確認できない場合が2つある。`{{range}}`が3要素以上のとき（検査は2要素までで行う）と、分岐の組み合わせが256を超えて代表だけが検査されたとき（[templates.ja.md](templates.ja.md#分岐が多いとき)）。この2つでは、分岐の形が検査したものと同じであることだけを確認して実行する。

## データベースのランタイムが足すもの

上の関数の他に、各ランタイムはそのドライバとデータベースにあって相手に無いものを持つ。pgxでは`Unprepared`、ネストした行とユーザー定義型と`LoadUserTypes`、`Batch`、`Copy`、`MatView`（[postgres.ja.md](postgres.ja.md#ランタイム-pgx)）。`database/sql`では`ExecOne`の`RowsAffected`の読み方と、サーバ設定を確かめる`mysql.Verify`（[mysql.ja.md](mysql.ja.md#ランタイム-databasesql)）。

## 自前のランタイムでは得られないもの

検査器が認識するのは宣言であってランタイムではない（`-query`で自前のマーカー関数を登録できる。[flags.ja.md](flags.ja.md)）。次の3つの約束はランタイムのもので、`sqlshape/postgres`か`sqlshape/mysql`で実行したときだけ成立する: 送る SQL が検査器の確かめた展開とバイト単位で一致すること（上の節）、違反が expect 行の綴りの名前で`ConstraintError`として返ること、`One`の文が2行目を返したらエラーになること。
