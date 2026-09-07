# テンプレート

[English](templates.md)

文のSQLはパラメータ型`P`上のGo `text/template`。検査器はそれが生成しうるすべてのSQLテキストに展開して各々を
検査し、ランタイムはそれを描画して、検査器が見ていないテキストを拒否する。展開はlint時に起こるので、テンプレートは
文字列定数（リテラル、または定数の連結）でなければならない。

## サブセット

値アクション。`{{.Field}}`、`{{.Outer.Inner}}`、`{{.}}`、`range`内の`{{$x}}`は素のフィールド参照。それぞれが
SQL内の`$n`パラメータになり、テキストにはならない: 値は文を変えられない。これが注入保証の全体。関数呼び出し、
メソッド呼び出し、パイプラインは値の位置では拒否される。

分岐。`{{if}}` / `{{else if}}` / `{{else}}` / `{{end}}`と`{{with}}`は両方向に展開される。`{{range}}`は0・1・2回で
展開され、空の場合、要素1つの場合、要素間の区切りをカバーする。structのスライスに対するrangeは反復ごとに固有の
`$n`パラメータを持つ。`{{switch}}`は無い: `{{if eq .Sort "a"}} … {{else if eq .Sort "b"}} … {{end}}`と書く。

条件は組み込みの`not`、`and`、`or`、`eq`、`ne`、`lt`、`le`、`gt`、`ge`、`len`、`index`、文字列と数値の定数、
フィールド参照を使える。nilポインタ、空のスライスやマップ、ゼロの数値、空文字列は`text/template`と同じく偽。

非対応。`{{define}}` / `{{template}}`（代わりにGoの定数でSQLを共有する。下記）、カスタム関数、rangeの要素以外の変数。

## パラメータと`P`

`{{.Filter.Name}}`は`P`のフィールド`Filter`のフィールド`Name`に解決される。`{{range .Items}} … {{.Sku}} … {{end}}`
はスライス`Items`の要素型に解決される。スカラーのrange内の`{{.}}`は要素そのもの。埋め込みstructは昇格する。
アナライザーはSQL内の位置から各`$n`に要るPostgreSQLの型を推論し、そのパスのGo型は合っていなければならない
（[checks.ja.md](checks.ja.md#形-goコードは文に合っているか)）。2箇所で使われる同じフィールドは両方に合わなければ
ならない。`{{if}}`が調べるだけのフィールドは`P`に存在しなければならないが、型は何でもよい。

## ディレクティブ

ディレクティブは検査器が読むSQLコメント。テンプレート内では:

| ディレクティブ | 意味 |
|---|---|
| `-- sqlshape: expect users_email_key, orders.total, P0401` | この書き込みが違反しうる制約・NOT NULL列・SQLSTATE。検査器はリストを正確に保つ（[checks.ja.md](checks.ja.md#失敗モード-この書き込みは何で失敗しうるか)） |
| `-- sqlshape: not null total, note` | これらの結果列はアナライザーの導出に関わらずNULLにならない（`col:",notnull"`タグの双子） |
| `-- sqlshape: unfiltered memos` | この文は意図的に`memos`を`visible where`の述語なしで読む |

`schema.sql`内では:

| ディレクティブ | 直上 | 意味 |
|---|---|---|
| `-- sqlshape: visible where deleted_at IS NULL` | `CREATE TABLE` | テーブルのすべての読みはこの述語を持たなければならない |
| `-- sqlshape: not null` | `CREATE FUNCTION` | 関数の結果はNULLにならない |
| `-- sqlshape: error P0401 = OrderTooLarge` | トリガーの`CREATE FUNCTION` | トリガーはこのSQLSTATEをraiseする。そのテーブルへの文はこの名前でexpectしなければならない |
| `-- sqlshape: seed` | `INSERT ... VALUES` | seedは追加型: 宣言に無い行はテーブルに残る（[migrations.ja.md](migrations.ja.md#seed済みテーブル)） |
| `-- @migrate ...` | 任意 | マイグレーションの意図（[migrations.ja.md](migrations.ja.md#diffが見えないものを宣言する)） |

Go側ではdocコメントとして: 型宣言の上の`// sqlshape: type money_amount`はその型をPostgreSQLの型に束縛する
（[checks.ja.md](checks.ja.md#形-goコードは文に合っているか)）。

## 共有フラグメント

テンプレートは定数でなければならず、Goの定数は連結できる:

```go
const tenantFilter = " AND tenant_id = {{.TenantID}}"

var ListOrders = sqlshape.Query[Order, ListParams](base + tenantFilter)
var ListItems  = sqlshape.Query[Item, ItemParams](itemsBase + tenantFilter)
```

全体はなおコンパイル時定数なので、検査器は1つのテンプレートとして展開する。フラグメントが読むすべてのフィールドを
`P`が持つことを求め、フラグメントについての診断はフラグメント自身の行に出る。値は決してSQLテキストにならないので、
これが注入保証を保つ唯一の共有機構。実行時に組んだテンプレート（`fmt.Sprintf`、変数）は報告される:
`query template must be a string constant`。

## 危険

テンプレート構文が許してもSQLでは見た目どおりに動かないものは報告される:

- 文字列リテラル内のアクション: `LIKE '%{{.Q}}%'`はパラメータでなくテキスト（`'%' || {{.Q}} || '%'`と書く）
- SQLコメント内のアクションは効果が無い
- `ORDER BY` / `GROUP BY`項目としての裸のパラメータ: `ORDER BY {{.Sort}}`は値が名指す列ではなく定数で並べる
  （代わりに分岐する: `{{if eq .Sort "total"}} total {{else}} id {{end}}`）

## 分岐が多いとき

分岐の組み合わせが256以下（独立した`{{if}}` 8個）なら全組み合わせを検査する。それを超えるとテンプレートは
疎に検査される: 全分岐オフ、全分岐オン、各分岐単独オン。独立した`AND`述語はそれでも完全にカバーされる。各々が他の
有無両方で見られるから。疎なテンプレートでは2つ変わる: `-strict`が疎であることを報告し、ランタイムは描画を検査済み
集合と比較できないので分岐の形を信用する（[runtime.ja.md](runtime.ja.md#ランタイムは検査器が見ていないsqlを拒否する)）。
大きなテンプレートをGo側で選ぶ2つの文に分ければ完全な検査に戻る。

## 診断は分岐を名指す

一部の展開でだけ成り立つ指摘は、それが見つかった分岐シグネチャを伴う。`[if@64:then]`や`[range@120:x2]`:
テンプレート内のアクションのバイトオフセットと、どちらに進んだか。すべての展開に共通の問題は接尾辞なしで一度だけ報告される。
