# テンプレート

[English](templates.md)

文のSQLは、パラメータ型`P`を入力とするGoの`text/template`として書く。検査器はそれが生成しうるすべてのSQLに展開して各々を検査し、ランタイムは検査済みのSQLのみを実行する。展開はlint時に行うので、テンプレートは文字列定数（リテラル、または定数の連結）でなければならない。

## 使える構文

値アクション。`{{.Field}}`、`{{.Outer.Inner}}`、`{{.}}`、`range`の中の`{{$x}}`は、単純なフィールド参照である。それぞれがSQLの中では`$n`パラメータになり、文字列としては埋め込まれない。値がSQL文の構造を変えることはできず、SQLインジェクションはこれで防がれる。値の位置での関数呼び出し、メソッド呼び出し、パイプラインは拒否される。

分岐。`{{if}}` / `{{else if}}` / `{{else}}` / `{{end}}`と`{{with}}`は真偽の両方に展開される。`{{range}}`は0回・1回・2回で展開され、空の場合、要素が1つの場合、要素の間の区切り文字がある場合をカバーする。構造体のスライスに対するrangeでは、反復ごとに別の`$n`が割り当てられる。`{{switch}}`は無いので、`{{if eq .Sort "a"}} … {{else if eq .Sort "b"}} … {{end}}`と書く。

条件式。組み込みの`not`、`and`、`or`、`eq`、`ne`、`lt`、`le`、`gt`、`ge`、`len`、`index`と、文字列・数値の定数、フィールド参照が使える。nilポインタ、空のスライスやマップ、0、空文字列は`text/template`と同じく偽として扱われる。

使えないもの。`{{define}}` / `{{template}}`（代わりにGoの定数でSQLを共有する。後述）、独自関数、rangeの要素以外の変数。

## パラメータと`P`

`{{.Filter.Name}}`は`P`のフィールド`Filter`のフィールド`Name`を指す。`{{range .Items}} … {{.Sku}} … {{end}}`の中の`{{.Sku}}`はスライス`Items`の要素型のフィールドを指す。スカラーのスライスに対するrangeの中の`{{.}}`は要素そのものである。埋め込み構造体のフィールドは昇格して見える。検査器はSQLの中の位置から各`$n`に必要なPostgreSQL型を推論し、そのパスのGo型が合っているかを確かめる（[checks.ja.md](checks.ja.md#パラメータを渡す)）。同じフィールドを2箇所で使うなら両方に合わなければならない。`{{if}}`で調べるだけで値としては使わないフィールドは、`P`に存在すればよく、型は問わない。

## ディレクティブ

ディレクティブは検査器が読むSQLコメントである。テンプレートの中で使うもの:

| ディレクティブ | 意味 |
|---|---|
| `-- sqlshape: expect users_email_key, orders.total, P0401` | この書き込みが違反しうる制約、NOT NULL列、SQLSTATEの一覧。検査器はこの一覧が正確であることを保つ（[checks.ja.md](checks.ja.md#書き込みの失敗に備える)） |
| `-- sqlshape: not null total, note` | これらの結果列はNULLにならない、とアナライザーの判定を上書きする（`col:",notnull"`タグのSQL側版） |
| `-- sqlshape: unfiltered memos` | この文は意図的に`memos`を`visible where`の条件なしで読む |

`schema.sql`の中で使うもの:

| ディレクティブ | 置く場所 | 意味 |
|---|---|---|
| `-- sqlshape: visible where deleted_at IS NULL` | `CREATE TABLE`の直上 | このテーブルを読む文はすべてこの条件を持たなければならない |
| `-- sqlshape: not null` | `CREATE FUNCTION`の直上 | この関数の戻り値はNULLにならない |
| `-- sqlshape: error P0401 = OrderTooLarge` | トリガー関数の`CREATE FUNCTION`の直上 | このトリガーはこのSQLSTATEを送出する。対象テーブルへの文はこの名前でexpectしなければならない |
| `-- sqlshape: seed` | `INSERT ... VALUES`の直上 | このseedは追加のみ。宣言に無い行もテーブルに残す（[migrations.ja.md](migrations.ja.md#seed済みテーブル)） |
| `-- @migrate ...` | どこでも | マイグレーションの意図の宣言（[migrations.ja.md](migrations.ja.md#diffだけでは決められないことを宣言する)） |

Goのコードの中では、型宣言のdocコメントに`// sqlshape: type money_amount`と書くと、その型をPostgreSQLの型に結びつけられる（[checks.ja.md](checks.ja.md#go型の表)）。

## 共有フラグメント

テンプレートは定数でなければならないが、Goの定数は連結できる:

```go
const tenantFilter = " AND tenant_id = {{.TenantID}}"

var ListOrders = sqlshape.Query[Order, ListParams](base + tenantFilter)
var ListItems  = sqlshape.Query[Item, ItemParams](itemsBase + tenantFilter)
```

連結した全体もコンパイル時定数なので、検査器は1つのテンプレートとして展開する。フラグメントが参照するフィールドは`P`が持っていなければならず、フラグメントに関する診断はフラグメントが定義されている行に出る。値がSQLの文字列になることは無いので、SQLインジェクションの保証を保ったままSQLを共有できる唯一の方法がこれである。実行時に組み立てたテンプレート（`fmt.Sprintf`や変数）は`query template must be a string constant`として報告される。

## 危険な書き方

テンプレートとしては書けるが、SQLとしては見た目どおりに動かないものは報告される:

- 文字列リテラルの中のアクション。`LIKE '%{{.Q}}%'`と書くと`{{.Q}}`はパラメータではなく文字列の一部になる。`'%' || {{.Q}} || '%'`と書く
- SQLコメントの中のアクション。何の効果も無い
- `ORDER BY` / `GROUP BY`の項目に直接置いたパラメータ。`ORDER BY {{.Sort}}`は値が指す列ではなく定数で並べ替える。代わりに`{{if eq .Sort "total"}} total {{else}} id {{end}}`のように分岐する

## 分岐が多いとき

分岐の組み合わせが256以下（独立した`{{if}}`が8個まで）なら全組み合わせを検査する。それを超えると疎に検査する。全分岐オフ、全分岐オン、各分岐を単独でオン、の組み合わせだけを見る。独立した`AND`条件の並びならこれでも各条件が有る場合と無い場合の両方を見られるので、検査の網羅性は落ちない。疎に検査されたテンプレートでは2つのことが変わる。`-strict`で疎であることが報告され、ランタイムは描画結果を検査済みの集合と比較できないので、分岐の形だけを信用する（[runtime.ja.md](runtime.ja.md#検査済みのsqlのみが実行できる)）。大きなテンプレートをGo側で選ぶ2つの文に分ければ、全組み合わせの検査に戻る。

## 診断は分岐を示す

一部の展開でだけ起きる問題には、それが見つかった分岐のシグネチャが付く。`[if@64:then]`や`[range@120:x2]`のように、テンプレート内のアクションのバイト位置と、どちらに進んだかを示す。すべての展開で共通の問題は、シグネチャなしで一度だけ報告される。
