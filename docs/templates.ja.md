# テンプレート

[English](templates.md)

SQLはGoの`text/template`のサブセットで書く。入力はパラメータ型`P`の値で、`{{.X}}`と書いた箇所が`$n`のプレースホルダになり、`{{if}}`や`{{range}}`で文の形を切り替えられる。検査器は分岐の全組み合わせを展開して検査し、ランタイムは検査済みのSQLだけを実行する。展開はlint時に行うので、テンプレートは文字列定数でなければならない。

## 使える構文

値。`{{.Field}}`、`{{.Outer.Inner}}`、`{{.}}`、`range`の中の`{{$x}}`。それぞれがSQLの中では`$n`パラメータになり、値が文字列として埋め込まれることはない。関数呼び出し、メソッド呼び出し、パイプラインは値の位置では使えない。

分岐。`{{if}}` / `{{else if}}` / `{{else}}` / `{{end}}`、`{{with}}`、`{{range}}`。`{{switch}}`は無いので`{{if eq .Sort "a"}} … {{else if eq .Sort "b"}} … {{end}}`と書く。

条件式。組み込みの`not`、`and`、`or`、`eq`、`ne`、`lt`、`le`、`gt`、`ge`、`len`、`index`と、文字列・数値の定数、フィールド参照。nilポインタ、空のスライスやマップ、0、空文字列は`text/template`と同じく偽。

使えないもの。`{{define}}` / `{{template}}`（代わりにGoの定数を連結する。後述）、独自関数、rangeの要素以外の変数。

```sql
SELECT o.id, o.total, o.created_at
  FROM orders o
 WHERE true
   {{if .CustomerID}} AND o.customer_id = {{.CustomerID}} {{end}}
   {{if .Statuses}}   AND o.status = ANY({{.Statuses}})   {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total DESC {{else}} o.created_at DESC {{end}}, o.id
 {{with .Limit}} LIMIT {{.}} {{end}}
```

この文は`{{if}}`が3つと`{{with}}`が1つなので16通りに展開され、16通りすべてが検査される。

## パラメータと`P`

`{{.Filter.Name}}`は`P`のフィールド`Filter`のフィールド`Name`を指す。`{{range .Items}} … {{.Sku}} … {{end}}`の中の`{{.Sku}}`はスライス`Items`の要素のフィールドを指す。スカラーのスライスに対するrangeの中の`{{.}}`は要素そのもの。埋め込み構造体のフィールドは昇格して見える。

各`{{.X}}`の型は、SQLの中で使われている場所から決まる（`WHERE id = {{.ID}}`なら`bigint`）。合っていなければ報告される。詳細は[checks.ja.md](checks.ja.md#パラメータを渡す)。`{{if .Flag}}`で調べるだけで値としては使わないフィールドは`P`に存在すればよく、型は問わない。

`{{range}}`は空、1要素、2要素の3通りで展開される。`IN`の中身のように要素の間に区切りが要る場所は、`{{if $i}}, {{end}}`のように書く。

```sql
SELECT id FROM products
 WHERE sku IN ({{range $i, $it := .Items}}{{if $i}}, {{end}}{{$it.Sku}}{{end}})
```

## ディレクティブ

ディレクティブは検査器が読むSQLコメント。テンプレートの中で使うもの:

| ディレクティブ | 意味 |
|---|---|
| `-- sqlshape: expect users_email_key, orders.total, P0401` | この書き込みが違反しうる制約、NOT NULL列、SQLSTATEの一覧。検査器はこの一覧が正確であることを保つ（[checks.ja.md](checks.ja.md#書き込みの失敗に備える)） |
| `-- sqlshape: not null total, note` | これらの結果列はNULLにならない、と検査器の判定を上書きする（`col:",notnull"`タグのSQL側版） |
| `-- sqlshape: unfiltered memos` | この文は意図的に`memos`を`visible where`の条件なしで読む（述語型の義務だけを外す） |
| `-- sqlshape: waive orders pinned(tenant_id), audit` | この文は`orders`の義務を1つ（宣言どおりの綴りで名指し）、`audit`の義務を全部外す。外したことは`-strict`で報告される（[checks.ja.md](checks.ja.md#規約をschemasqlに宣言するrequire)） |

`schema.sql`の中で使うもの:

| ディレクティブ | 置く場所 | 意味 |
|---|---|---|
| `-- sqlshape: visible where deleted_at IS NULL` | `CREATE TABLE`の直上 | このテーブルを読む文はすべてこの条件を持たなければならない（`require deleted_at IS NULL on read`の略記） |
| `-- sqlshape: require pinned(tenant_id)` / `require pinned(version) on update, delete` | `CREATE TABLE` / `CREATE VIEW`の直上 | そのリレーションに触る文すべてへの義務。述語、`pinned(列)`、`immutable(列)`、`via view`、`never`、`paired(表)`、`single`のいずれかに、任意で`on select, insert, update, delete`を付ける（[checks.ja.md](checks.ja.md#規約をschemasqlに宣言するrequire)） |
| `-- sqlshape: aggregate orders (order_items, order_notes) [lock version]` | ルートの`CREATE TABLE`の直上 | これらの表で1つの集約を成す。子表はルートの鍵で固定し、1文は1集約にしか触らない。`lock`を付ければ書き込みはルートの版を名指しする（[checks.ja.md](checks.ja.md#規約をschemasqlに宣言するrequire)） |
| `-- sqlshape: transitions status: draft -> submitted, submitted -> paid \| cancelled` | `CREATE TABLE`の直上 | この列は状態機械。SETするUPDATEはWHEREで現在の状態を前状態に固定する |
| `-- sqlshape: sensitive pii: email, phone` | `CREATE TABLE`の直上 | この列はラベルを持つ。`may read pii`の文脈だけが参照できる（ビュー経由も同じ。マスクの式でラベルは外れる） |
| `-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete` | `CREATE TABLE` / `CREATE VIEW`の直上 | 名前つき文脈での義務の差分。パッケージコメントの`// sqlshape: context ops`、vetの`-context`、`check -context`のいずれかで選ぶ（[checks.ja.md](checks.ja.md#規約をschemasqlに宣言するrequire)） |
| `-- sqlshape: unfiltered orders` / `waive orders pinned(tenant_id)` | `CREATE VIEW`の直上 | ビュー定義自身が文としてopt-outする |
| `-- sqlshape: not null` | `CREATE FUNCTION`の直上 | この関数の戻り値はNULLにならない |
| `-- sqlshape: error P0401 = OrderTooLarge` | 関数の`CREATE FUNCTION`の直上 | この関数が送出するSQLSTATEに名前を付け、expect行と`Violates`でその名前を使えるようにする。PL/pgSQL本体の`RAISE`は注釈なしでもコードで検出される |
| `-- sqlshape: seed` | `INSERT ... VALUES`の直上 | このseedは追加のみ。宣言に無い行もテーブルに残す（[migrations.ja.md](migrations.ja.md#seed済みテーブル)） |
| `-- @migrate ...` | どこでも | マイグレーションの意図の宣言（[migrations.ja.md](migrations.ja.md#diffだけでは決められないことを宣言する)） |

Goのコードの中では、型宣言のdocコメントに`// sqlshape: type money_amount`と書くと、その型をPostgreSQLの型に結びつけられる（[checks.ja.md](checks.ja.md#go型の表)）。パッケージコメントの`// sqlshape: context ops`は、そのパッケージが判定される義務の文脈を選ぶ。

## SQLを共有する

テンプレートは定数でなければならないが、Goの定数は連結できる。共通の条件やSELECT句はGoの定数にして連結する。

OK

```go
const tenantFilter = " AND tenant_id = {{.TenantID}}"

var ListOrders = sqlshape.Query[Order, ListParams](base + tenantFilter)
var ListItems  = sqlshape.Query[Item, ItemParams](itemsBase + tenantFilter)
```

連結した全体を1つのテンプレートとして展開する。フラグメントが参照するフィールドは`P`に無ければならず、フラグメントに関する診断はフラグメントが定義されている行に出る。

NG。実行時に組み立てたテンプレートは検査できない。

```go
var ListOrders = sqlshape.Query[Order, ListParams](fmt.Sprintf(base, table))
// sqlshape: query template must be a string constant
```

## 危険な書き方

テンプレートとしては書けるが、SQLとしては見た目どおりに動かないものは報告される。

### 文字列リテラルの中に`{{.X}}`を置かない

NG

```sql
SELECT id FROM products WHERE name LIKE '%{{.Q}}%'
--                                        ^ {{.Q}} is inside a string literal: it becomes text, not a parameter (write '%' || {{.Q}} || '%' to concatenate)
```

OK

```sql
SELECT id FROM products WHERE name LIKE '%' || {{.Q}} || '%'
```

### `ORDER BY`の項目にパラメータを直接置かない

NG。値が指す列ではなく、定数で並べ替えることになる。

```sql
SELECT id, total FROM orders ORDER BY {{.Sort}}
--                                    ^ ORDER BY {{.Sort}} sorts by a constant, not by the column the value names: branch on it instead ({{if eq .Sort "total"}} total {{else}} id {{end}})
```

OK

```sql
SELECT id, total FROM orders ORDER BY {{if eq .Sort "total"}} total {{else}} id {{end}}
```

### コメントの中の`{{.X}}`は何もしない

```sql
SELECT id FROM orders -- {{.Note}}
--                       ^ {{.Note}} is inside a comment and has no effect
```

## 分岐が多いとき

分岐の組み合わせが256通り（独立した`{{if}}`が8個）を超えると、全組み合わせは検査されず、代表的な組み合わせだけが検査される。`-strict`を付けると次の診断で知らされる:

```
sqlshape: 512 branch combinations exceed 256: checked sparsely (all branches off, all on, each on alone); the runtime cannot compare renderings with the checked set
```

独立した`AND`条件の並びなら、この代表だけでも各条件の有無は網羅される。分岐同士が絡み合っている（ある分岐が別の分岐の中にある、`ORDER BY`と`WHERE`が連動する）なら見落としが起きうるので、テンプレートをGo側で選ぶ2つの文に分ける。分ければ全組み合わせの検査に戻る。

## 診断の読み方

一部の展開でだけ起きる問題には、どの分岐で起きたかが末尾に付く。

```
field Order.Total is not selected in every branch [if@11:else]: make it a pointer so those branches leave it nil
```

`if@11:else`は、テンプレートの11バイト目にある`{{if}}`が偽だった展開、という意味。`range@120:x2`なら120バイト目の`{{range}}`が2要素だった展開。すべての展開で共通の問題は、この接尾辞なしで一度だけ報告される。
