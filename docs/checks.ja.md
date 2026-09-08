# 検査器が確かめること

[English](checks.md)

検査器はパッケージ内の`sqlshape.Query[R, P](template)`、`sqlshape.One[R, P](template)`、`sqlshape.Copy[R](...)`、`sqlshape.MatView(...)`をすべて見つけ、テンプレートを分岐の全組み合わせに展開し（[templates.ja.md](templates.ja.md)）、展開した各SQLを`schema.sql`に対して解析して、その結果をGoの型と突き合わせる。このページは、書く場面ごとに、何がNGで何がOKかを、実際に出る診断と一緒に並べたもの。診断は英語で出るので、そのまま載せている。

三部に分かれる。**第1部**は何も宣言しなくても全部の文にかかる検査。結果とパラメータの形、型の意味、失敗モード、`One`の証明。**第2部**は`schema.sql`に宣言して初めてかかる規約。読み取り条件、列の固定、集約、状態機械、ラベル付きの列。**第3部**は文の外側の検査。文を囲むGoコードと、スキーマ自体。
## 目次

- [第1部 — すべての文にかかる検査](#第1部--すべての文にかかる検査)
  - [SELECTの結果を受ける](#selectの結果を受ける)
  - [パラメータを渡す](#パラメータを渡す)
  - [型に意味を持たせる](#型に意味を持たせる)
  - [書き込みの失敗に備える](#書き込みの失敗に備える)
  - [1行だけ返す（`One`）](#1行だけ返すone)
  - [COPYで一括ロードする](#copyで一括ロードする)
- [第2部 — スキーマが宣言する規約](#第2部--スキーマが宣言する規約)
  - [宣言の仕組み](#宣言の仕組み)
  - [必ず付ける読み取り条件（`visible where`）](#必ず付ける読み取り条件visible-where)
  - [列を必ず固定する（`pinned`、`-require-columns`）](#列を必ず固定するpinned-require-columns)
  - [テーブルを直接読まない（`via view`、`-no-table-reads` / `-no-tables`）](#テーブルを直接読まないvia-view-no-table-reads---no-tables)
  - [表をまたぐ述語には証人が要る（`EXISTS`）](#表をまたぐ述語には証人が要るexists)
  - [集約にはルート経由で触り、1文で1つだけ触る（`aggregate`）](#集約にはルート経由で触り1文で1つだけ触るaggregate)
  - [ステータス列は宣言した遷移でしか動かさない（`transitions`）](#ステータス列は宣言した遷移でしか動かさないtransitions)
  - [追記専用、対になる書き込み、1行だけの削除（`never`、`paired`、`single`）](#追記専用対になる書き込み1行だけの削除neverpairedsingle)
  - [ラベル付きの列は許された文脈でしか読まない（`sensitive`、`may read`）](#ラベル付きの列は許された文脈でしか読まないsensitivemay-read)
  - [呼び出し元ごとに規約を変える（`context`）](#呼び出し元ごとに規約を変えるcontext)
  - [Goの外のSQLも同じ判定を受ける（`sqlshape check`）](#goの外のsqlも同じ判定を受けるsqlshape-check)
- [第3部 — 文の外側](#第3部--文の外側)
  - [sqlshapeを通さないSQLを書かない（`-raw-sql`）](#sqlshapeを通さないsqlを書かない-raw-sql)
  - [パッケージは自分のスキーマだけを参照する（`-schemas`）](#パッケージは自分のスキーマだけを参照する-schemas)
  - [スキーマ自体の問題](#スキーマ自体の問題)

## 第1部 — すべての文にかかる検査

### SELECTの結果を受ける

結果を受ける構造体`R`について、検査器はすべての結果列に受けるフィールドがあり、すべてのフィールドに対応する列があり、型とNULLの扱いが合っていることを確かめる。分岐のあるテンプレートでは、展開したすべてのSQLについて確かめる。

#### 結果列とフィールドは名前で対応する

対応は`col:"..."`タグ、`db:"..."`タグ、フィールド名をsnake_caseにしたもの、の順で探す。

NG

```go
type Order struct {
	ID           int64
	CustomerName string // field Order.CustomerName has no result column
}
```

```sql
SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id
--           ^ result column "name" has no field in Order
```

OK。列に別名を付けるか、タグで対応を指定する。

```sql
SELECT o.id, c.name AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id
```

```go
type Order struct {
	ID           int64
	CustomerName string `col:"name"`
}
```

#### 名前の無い列と同名の列には別名を付ける

NG

```sql
SELECT id, count(*) FROM orders GROUP BY id
--         ^ result column 2 has no name: give it an alias (... AS name) so it can bind to a field of Order

SELECT o.id, c.id FROM orders o JOIN customers c ON c.id = o.customer_id
--           ^ result columns 1 and 2 are both named "id": alias one of them (... AS other_name)
```

OK

```sql
SELECT id, count(*) AS n FROM orders GROUP BY id
SELECT o.id, c.id AS customer_id FROM orders o JOIN customers c ON c.id = o.customer_id
```

#### NULLになりうる列はNULLを受けられる型で受ける

NULLを受けられる型は、ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`、`sql.Scanner`を実装した型。

NG

```go
type User struct {
	ID        int64
	DeletedAt time.Time // field DeletedAt is time.Time but column "deleted_at" may be NULL (use a pointer, or tag it `col:",notnull"` if you know better)
}
```

```sql
SELECT id, deleted_at FROM users
```

OK

```go
type User struct {
	ID        int64
	DeletedAt *time.Time
}
```

補足。列がNULLになりうるかは、NOT NULL制約と主キー、WHERE句（`deleted_at IS NOT NULL`や`deleted_at = ...`があればNULLではない）、外部結合（内側の列はNULLになりうる）、関数（引数がNULLでない`strict`関数の結果はNULLでない、`coalesce(x, 0)`はNULLでない。ただし一部のstrictな組み込み関数・演算子は報告するものが無いとNULLを返す。`meta ->> 'key'`もその一つ）、ビュー自身のWHERE句から判定する。ビューは下敷きの列のNOT NULLをPostgreSQL自身と同じように動的に追いかける。後から`ALTER TABLE ... DROP NOT NULL`がベーステーブルに入れば、ビューの連鎖を通じても、ビュー自身のWHERE句をくぐり抜けても反映される。判定より自分の方が正しいと分かっているなら、Go側は`col:",notnull"`タグ、SQL側はテンプレートの`-- sqlshape: not null deleted_at`行で上書きできる。関数の戻り値は`schema.sql`の`CREATE FUNCTION`の直上に`-- sqlshape: not null`と書く。

#### 一部の分岐だけが選ぶ列はNULLを受けられる型で受ける

NG

```go
type Order struct {
	ID    int64
	Total string // field Order.Total is not selected in every branch [if@11:else]: make it a pointer so those branches leave it nil
}
```

```sql
SELECT id {{if .WithTotal}}, total{{end}} FROM orders
```

OK

```go
type Order struct {
	ID    int64
	Total *string
}
```

補足。選ばない分岐ではフィールドはnilのまま。どの分岐も選ばない列に対応するフィールドは`has no result column`になる。

#### 列の型とフィールドの型は下の表に従う

NG

```go
type Order struct {
	ID    int64
	Total float64 // field Total is float64 but column "total" is numeric(12,2)
}
```

```sql
SELECT id, total FROM orders
```

OK

```go
type Order struct {
	ID    int64
	Total string // 全桁を保つ。decimal.Decimal（shopspring/decimal）でもよい
}
```

#### ネストした行は構造体で受ける

`array_agg(row(...))`、`array_agg(t)`、`row(...)`、複合型の列は構造体、または構造体のスライスで受ける。無名の`row(...)`はフィールドの位置で、名前付きの複合型は名前と順序で対応づける。

NG。構造体のフィールドの順序が複合型の列の順序と違う。

```sql
-- schema.sql
CREATE TYPE order_item AS (sku text, qty integer);
```

```go
type Item struct {
	Qty int32 // field Items.Qty is at position 1 but the row type's column 1 is "sku" (fields are scanned in order)
	Sku string
}
type Order struct {
	ID    int64
	Items []Item
}
```

```sql
SELECT o.id, array_agg((i.sku, i.qty)::order_item) AS items
  FROM orders o JOIN order_items i ON i.order_id = o.id
 GROUP BY o.id
```

OK

```go
type Item struct {
	Sku string
	Qty int32
}
```

#### 1列だけ返すSQLはスカラーで受けられる

OK

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM orders`)
```

NG

```go
var Count = sqlshape.Query[int64, struct{}](`SELECT id, total FROM orders`)
// R is int64 but the query returns 2 columns
```

#### 埋め込み構造体は平坦化される

OK

```go
type Base struct {
	ID        int64
	CreatedAt time.Time
}
type Order struct {
	Base
	Total string
}
```

```sql
SELECT id, created_at, total FROM orders
```

NG。2つのフィールドが同じ列を受けようとしている。

```go
type Order struct {
	Base
	ID    int64 // Order: fields Base.ID and ID both bind to column "id"
	Total string
}
```

補足。名前付きの構造体フィールド、または`col:"..."`タグを付けた埋め込みフィールドは、平坦化されずにネストした行として扱われる。

#### Go型の表

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
| `interval` | `time.Duration`、`pgtype.Interval` |
| `json` / `jsonb` | `[]byte`、`json.RawMessage`、`string`、またはpgxがunmarshalできる構造体・スライス・マップ |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)`（各要素は単体の`T`のパラメータ/列と同じ判定を受ける。注記も含めて） |
| 範囲型 | `pgtype.Range[T]`。`T`はサブタイプと照合される（ユーザー定義の範囲型も同様） |
| 多重範囲型 | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | `pgtype`の対応する型 |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enum、seed済みlookupテーブルのキー、CHECKによる値集合 | Goのnamed string type（[下記](#型に意味を持たせる)） |
| ドメイン | 基底型に対応するGo型、またはドメインに結びつけたnamed type |
| 複合型、レコード | 構造体 |

表に無い型、あるいは自前の型で受けたい型は、Go側の型にdocコメントで対応するPostgreSQL型を宣言する:

```go
// sqlshape: type money_amount
type Money struct{ ... }   // sql.Scanner / driver.Valuer を実装する
```

検査器は、SQL側が`money_amount`（その配列と、それを基底型とするドメインを含む）である位置でだけ`Money`を受け入れ、それ以外の位置では報告する。値の変換は型自身の`sql.Scanner` / `driver.Valuer`に任せる（Scannerにはテキスト形式が渡る）。

### パラメータを渡す

`{{.X}}`はSQLの中では`$n`パラメータになる。検査器は`$n`が使われている場所からPostgreSQL側で必要な型を推論し（`WHERE id = $1`なら`bigint`、`= ANY($1)`なら配列）、`P`の対応するフィールドの型がそれに合うことを確かめる。型の対応は上のGo型の表に従う。

#### パラメータの型は使われる場所の型に合わせる

NG

```go
type Params struct {
	ID int64 // parameter .ID is int64 but SQL expects uuid
}
```

```sql
SELECT id, email FROM users WHERE id = {{.ID}}   -- id は uuid
```

OK

```go
type Params struct {
	ID uuid.UUID
}
```

補足。`string`はどの型のパラメータにも渡せる（テキスト形式で送られ、PostgreSQL側で解釈される）。パラメータが渡す先の列より広い型だとオーバーフローの注記が出る。`integer`の列に`int64`なら`parameter .ID: int64 into integer may overflow`（`real`の列に`float64`でも同様）。配列パラメータでも要素ごとに同じ注記が付く。`smallint[]`に`[]int64`なら`parameter .Tags: int64 into smallint may overflow`。

#### NULLを渡しうるパラメータはポインタにする

`{{if .Name}} AND name = {{.Name}} {{end}}`のように、値が無いことを分岐で表すフィールドはポインタかスライスにする。非ポインタの`string`はNULLを送れないので、常に値がある前提になる。

NG。enumのパラメータがポインタでない（`-strict`）。

```go
type Params struct {
	Status OrderStatus // parameter .Status is a non-pointer OrderStatus: its zero value "" is not a label of enum order_status and fails at runtime (SQLSTATE 22P02) when unset
}
```

OK

```go
type Params struct {
	Status *OrderStatus
}
```

#### ネストしたパスと`range`

`{{.Filter.Name}}`は`P`のフィールド`Filter`のフィールド`Name`を指す。`{{range .Items}} … {{.Sku}} … {{end}}`の中の`{{.Sku}}`はスライス`Items`の要素型のフィールドを指す。

OK

```go
type Params struct {
	Filter struct{ Name *string }
	Items  []struct{ Sku string; Qty int32 }
}
```

```sql
SELECT id FROM products
 WHERE true {{if .Filter.Name}} AND name = {{.Filter.Name}} {{end}}
   AND sku IN ({{range $i, $it := .Items}}{{if $i}}, {{end}}{{$it.Sku}}{{end}})
```

#### 複合型のパラメータは構造体で渡す

SQL側が複合型を期待する位置には構造体を、その配列を期待する位置には構造体のスライスを渡す。フィールドと複合型の列の対応づけはネストした行と同じ規則。

OK

```sql
-- schema.sql
CREATE TYPE order_item AS (sku text, qty integer);
CREATE FUNCTION place_order(customer_id bigint, items order_item[]) RETURNS bigint ...
```

```go
type Item struct {
	Sku string
	Qty int32
}
type Params struct {
	CustomerID int64
	Items      []Item
}
```

```sql
SELECT place_order({{.CustomerID}}, {{.Items}})
```

#### 使っていないフィールドを`P`に残さない（`-strict`）

```go
type Params struct {
	ID    int64
	Limit int32 // sqlshape: parameter field Limit is never used by the template
}
```

```sql
SELECT id FROM orders WHERE id = {{.ID}}
```

#### 既定値のある列に常に値を送らない（`-strict`）

```go
type NewOrder struct {
	CustomerID int64
	Status     OrderStatus // parameter .Status always sends a value into orders.status, so its DEFAULT never applies: decide which side owns the default (make the column conditional with {{if}} to use the database's)
}
```

```sql
INSERT INTO orders (customer_id, status) VALUES ({{.CustomerID}}, {{.Status}})
```

OK。データベースの既定値を使うなら列ごと分岐にする。アプリケーションが常に決めるなら、列の`DEFAULT`を外す。

```sql
INSERT INTO orders (customer_id {{if .Status}}, status{{end}})
VALUES ({{.CustomerID}} {{if .Status}}, {{.Status}}{{end}})
```

### 型に意味を持たせる

`bigint`や`text`のままでは区別できない値、たとえばenumのラベル、テーブルのID、金額の単位は、Goではnamed typeで表す。検査器はnamed typeが使われた箇所からそれをSQL側の意味（enum、lookupテーブルのキー、CHECKの値集合、主キー、ドメイン）に結びつけ、以後の使用箇所で食い違いを報告する。登録は要らない。結びつきはパッケージを越えて効く。

#### enumやlookupテーブルの値はnamed typeの定数と一致させる

enumの列、seed済みlookupテーブルのキー、`CHECK (col IN (...))`の付いた列に使われたnamed string typeについて、型付き定数とラベルを両方向に比較する。

```sql
-- schema.sql
CREATE TABLE order_statuses (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO order_statuses VALUES ('pending', '保留'), ('paid', '支払済'), ('shipped', '発送済');
CREATE TABLE orders (..., status text NOT NULL REFERENCES order_statuses(code));
```

`OrderStatus`と`order_statuses`は名前で結びついているわけではない。型は、文の中で列と出会った場所で値集合に束縛される。下の文では`{{.Status}}`が`orders.status`に流れ、その列の外部キーが`order_statuses(code)`を指しているので、`OrderStatus`はこのlookupテーブルのGo側になる。束縛はfactとして書き出され、その型を使うすべてのパッケージで効く。列と一度も出会わない型は検査されない。

```go
var ByStatus = sqlshape.Query[Order, struct{ Status OrderStatus }](`
SELECT id, total FROM orders WHERE status = {{.Status}}`)
```

NG

```go
type OrderStatus string

const (
	Pending  OrderStatus = "pending"
	Paid     OrderStatus = "paid"
	Canceled OrderStatus = "canceled" // sqlshape: OrderStatus has constant "canceled" which is not a label of value set of order_statuses (lookup table)
)
// sqlshape: value set of order_statuses (lookup table) has label "shipped" but OrderStatus has no constant for it
```

OK

```go
const (
	Pending OrderStatus = "pending"
	Paid    OrderStatus = "paid"
	Shipped OrderStatus = "shipped"
)
```

補足。`OrderStatus("typo")`のような変換は`sqlshape: OrderStatus("typo") is not a label of ...`、すべてのラベルを扱っていない`switch`は`sqlshape: switch on OrderStatus does not handle ... labels: shipped`として報告される。型に`Known() bool`を実装しておくと、実行時にこのビルドが知らないラベルを受け取ったとき、行マッパーが`*UnknownLabelError`を返す。値集合の置き場としてはenumよりseed済みlookupテーブルを推奨する（[migrations.ja.md](migrations.ja.md#seed済みテーブル)）。

#### 別のテーブルのIDを渡さない

主キーの列、または外部キーで主キーから派生した列に使われたnamed typeは、そのテーブルのIDとして扱われる。束縛は値集合と同じく使用箇所から来る。下の`UserID`が`users.id`を表すのは、どこかの文が`users.id`（かそこへの外部キー）の位置にそれを渡したから。

```go
type UserID int64
type OrderID int64

var User = sqlshape.One[User, struct{ ID UserID }](`SELECT id, email FROM users WHERE id = {{.ID}}`)
```

NG

```go

type Params struct {
	ID UserID // sqlshape: parameter .ID is UserID, which stands for key users.id elsewhere, but here meets key orders.id
}
```

```sql
SELECT total FROM orders WHERE id = {{.ID}}
```

OK

```go
type Params struct {
	ID OrderID
}
```

#### 単位の違うドメインを混ぜない

ドメインの列に使われたnamed typeはそのドメインに結びつけられる。SQLの中でも、ドメインは基底型とは別の単位として扱う。PostgreSQL自身は基底型に戻して演算を許すが、検査器は報告する。

```sql
-- schema.sql
CREATE DOMAIN yen AS bigint;
CREATE DOMAIN gram AS integer;
CREATE TABLE products (id bigint PRIMARY KEY, price yen NOT NULL, weight gram NOT NULL);
```

NG

```sql
SELECT id FROM products WHERE price + weight > 1000
--                            ^ domain mismatch: yen + gram: mixes yen with gram (cast to the base type to drop the domain)
```

```go
type Params struct {
	Max int64 // sqlshape: parameter .Max carries domain yen as a plain int64; declare a named type to have it checked   （-strict）
}
```

OK

```go
type Yen int64

type Params struct {
	Max Yen
}
```

```sql
SELECT id FROM products WHERE price > {{.Max}}
```

補足。リテラルとパラメータは相手の単位を引き継ぐ。`price + 100`、`price * 2`、`abs(price)`、`coalesce(price, 0)`はyenのまま。単位を外したいときは基底型へ明示的にキャストする（`price::bigint + weight::bigint`）。

#### 情報が落ちる型で受けない（`-strict`）

```go
type Event struct {
	At  time.Time // field At: timestamp without time zone into time.Time: which zone the value is in becomes the application's implicit choice (prefer timestamptz)
	Day time.Time // field Day: date into time.Time: a zone conversion can move the day (keep it at UTC midnight or use a civil date type)
}
```

`timestamptz`ならこの注記は出ない。`date`を`time.Time`で受けるなら、UTCの0時として扱うことをコードの側で決めておく。

### 書き込みの失敗に備える

INSERT / UPDATE / DELETE / MERGEは制約違反で失敗しうる。検査器は各展開について違反しうる制約を列挙し、テンプレートがそれを`-- sqlshape: expect`行で宣言していることを確かめる。実行時には、宣言した名前で`ConstraintError`が返る（[runtime.ja.md](runtime.ja.md#エラー)）。

#### 違反しうる制約はexpect行に宣言する

NG

```sql
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id
--                     ^ may violate customers_email_key (UNIQUE (email) on customers, SQLSTATE 23505); add `-- sqlshape: expect customers_email_key` to the template or make it impossible
```

OK

```sql
-- sqlshape: expect customers_email_key
INSERT INTO customers (email, name) VALUES ({{.Email}}, {{.Name}}) RETURNING id
```

```go
_, err := CreateCustomer.First(ctx, db, p)
if sqlshape.Violates(err, "customers_email_key") { ... }
```

補足。列挙されるのは、一意制約と主キー、外部キーの両方向（挿入する行が存在しない親を参照する、削除する行がまだ子から参照されている）、EXCLUDE制約、CHECK、ドメインのCHECK、書き込む値がNULLになりうる場合のNOT NULL。パラメータ由来のNULLは、そのフィールドがnilを表せない型（`string`など）なら候補から外れる。

参照される側のキーを変えるDELETEやUPDATEは、外部キー自体が変更を拒む場合（`NO ACTION` / `RESTRICT`）だけでなく、その`ON DELETE` / `ON UPDATE`アクションが参照する側の行に対して行うことを通じても失敗しうる。`SET NULL`は参照列のNOT NULL制約に触れうる、`SET DEFAULT`は同じ外部キーに再び触れうる（デフォルト値が親に存在するとは限らない）うえデフォルトが無ければ同じNOT NULLにも触れうる、`CASCADE`は参照する側の行を削除（または更新）し、それはさらに一段下で同じように検査される——したがって`ON DELETE CASCADE`が連鎖する外部キーは、何段も先のテーブルで失敗することがある。

`WITH [LOCAL | CASCADED] CHECK OPTION`付きのビューへの書き込みは、SQLSTATE 44000でも失敗しうる（PostgreSQL自身のこのエラーは制約名を持たないため、注釈の無いトリガーのSQLSTATEと同様に、expect行はSQLSTATEそのものをキーにする）。`CASCADED`（オプションを修飾子無しで書いたときの既定）は、このビューだけでなく、さらに下位にある更新可能なビューのWHERE句も検査対象にする。

#### 起こりえない違反を宣言しない

NG

```sql
-- sqlshape: expect orders_total_check
--                  ^ expects orders_total_check but no expansion can violate it
UPDATE orders SET note = {{.Note}} WHERE id = {{.ID}}
```

expect行は「この文が失敗しうる理由の正確な一覧」として保たれる。不要になった宣言は消す。

#### 制約の名前

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
| トリガーが送出するエラー | SQLSTATE、または`-- sqlshape: error`で付けた名前 | `P0401`、`OrderTooLarge` |
| ビューの`WITH CHECK OPTION` | SQLSTATE（PostgreSQL自身の44000エラーは制約名を持たない） | `44000` |

同じ名前になる制約が2つあると、PostgreSQLと同様に番号が付く（`orders_total_check1`）。生成した名前がPostgreSQLの63バイトという識別子の上限を超える場合は、マルチバイト文字を途中で切らないよう、PostgreSQLと同じやり方で切り詰める。

#### トリガーが送出するエラーには名前を付ける

検査器はPL/pgSQLのトリガー本体を読むので、中の`RAISE EXCEPTION ... USING ERRCODE = 'P0401'`だけで`P0401`は失敗モードに加わる（`ERRCODE`の無い`RAISE`は`P0001`）。注釈はそのコードに名前を付けるためのもので、expect行と`Violates`でその名前を使えるようになる:

```sql
-- schema.sql
-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total > 1000000 THEN RAISE EXCEPTION 'order too large' USING ERRCODE = 'P0401'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_size BEFORE INSERT OR UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION check_order_size();
```

```sql
-- sqlshape: expect OrderTooLarge, orders_customer_id_fkey
INSERT INTO orders (customer_id, total) VALUES ({{.CustomerID}}, {{.Total}})
```

トリガーが付いているテーブルへの、トリガーが発火するイベント（この例ではINSERTとUPDATE）の失敗モードに、そのSQLSTATEが加わる。

#### RAISE無しで失敗するPL/pgSQL文

PL/pgSQLの一部の文は`RAISE`が無くても失敗しうる。検査器はそのSQLSTATEも、明示的な`RAISE`と同様に本体の失敗モードへ加える。

- `SELECT ... INTO STRICT`と`EXECUTE ... INTO STRICT`は、問い合わせが1行も返さなければ`P0002`（no_data_found）、2行以上返せば`P0003`（too_many_rows）。問い合わせが1行以下だと証明できる場合（`One`が使うのと同じ証明）は`P0003`を落とす。
- マッチする`WHEN`が無く`ELSE`も無い`CASE`文は`20000`（case_not_found）。
- `ASSERT`は条件が偽なら`P0004`（assert_failure）。

`RAISE ... USING ERRCODE = <式>`は、`<式>`が文字列リテラルの場合、リテラルの初期値のまま再代入されない変数の場合、あるいは`EXCEPTION`ハンドラの中で捕捉したSQLSTATEをそのまま再送出する`SQLSTATE`そのものの場合に、静的に解決する。それ以外の式はSQLSTATEを特定できないままとし、`P0001`と決め打ちせずその旨を検査器が報告する。

`BEGIN ... EXCEPTION WHEN ... END`ブロックは、`WHEN`の条件がカバーするものをすべて捕捉する — 条件名、エラークラス名（そのクラスの全コードにマッチする。たとえば`integrity_constraint_violation`はどの`23xxx`にもマッチする）、リテラルの`SQLSTATE '...'`、`OTHERS`のいずれでも。捕捉された失敗モードは呼び出し元まで届かない。ハンドラ自身が新たに送出するものは届く。

#### 関数を呼ぶ文は、関数の中の失敗モードも宣言する

ユーザー定義関数の呼び出しは、その本体（`LANGUAGE sql`でも`plpgsql`でも）の失敗モードを引き継ぐ。本体の書き込みが違反しうる制約、その書き込みが発火させるトリガー、本体が呼ぶ関数、本体が送出するSQLSTATEである。

```sql
SELECT place_order({{.CustomerID}}, {{.Note}})
--     ^ may violate orders_customer_id_fkey (FOREIGN KEY (customer_id) on orders REFERENCES customers, SQLSTATE 23503, through place_order()); add `-- sqlshape: expect orders_customer_id_fkey` to the template or make it impossible
```

補足。本体でパラメータに由来するとわかったNOT NULLは、呼び出し側の引数まで辿られる。`STRICT`な関数はNULLでは呼ばれないので、その引数からの違反は候補から外れる。

### 1行だけ返す（`One`）

`sqlshape.One[R, P]`は1行以下しか返さないと宣言するもので、検査器はそれを展開ごとにスキーマから証明する。証明できなければエラー。実行時には`Get`は無ければ`ErrNoRows`、`Find`は有無を返し、データベースが証明に反して2行返したらどちらも`ErrManyRows`を返す。

#### 一意キーを等値で固定する

NG

```go
var ByName = sqlshape.One[User, struct{ Name string }](`
SELECT id, email FROM users WHERE name = {{.Name}}`)
// One: cannot prove at most one row: users: no unique key is fixed by equality (keys: (id), (email))
```

OK

```go
var ByEmail = sqlshape.One[User, struct{ Email string }](`
SELECT id, email FROM users WHERE email = {{.Email}}`)
```

補足。1行以下と言えるのは、FROMに現れるすべてのテーブルについて、その一意キー（主キー、`UNIQUE`、一意インデックス、またはWHERE句が同じ条件を含む部分一意インデックス）がリテラル・パラメータ・外側の参照・相関の無いスカラーサブクエリのいずれかと等値で固定されているとき。等値はJOIN（外部結合のON句はNULLになりうる側だけを固定する）、ビュー、サブクエリ、CTEを通して追跡する。`GROUP BY`の無い集約、定数の`LIMIT 0` / `LIMIT 1`、FROMの無いSELECT、1行の`VALUES`、1行の`INSERT ... RETURNING`も1行以下と見なす。`FULL JOIN`は決して1行以下にならない。`DEFERRABLE`と宣言したキーも同様——一意性がコミットまで検査されないため、そのトランザクションが生きている間は同じ値を持つ2行が存在しうる。

#### すべての分岐で証明できなければならない

NG

```go
var Find = sqlshape.One[User, struct{ ID *int64 }](`
SELECT id, email FROM users WHERE true {{if .ID}} AND id = {{.ID}} {{end}}`)
// One: cannot prove at most one row: users: no unique key is fixed by equality (keys: (id), (email)) [if@39:else]
```

`.ID`がnilの分岐では条件が無くなり、全行が返る。それを見つけるのがこの検査の意図なので、この文は`Query`にするか、`.ID`を非ポインタにして分岐を外す。

### COPYで一括ロードする

`sqlshape.Copy[R]("order_items", "order_id", "line_no", ...)`はINSERTと同じように検査される。テーブルと列が存在すること、各列の型がその列に値を入れるフィールドと合うこと、指定しなかった列にはすべて既定値があるか生成列であること。

NG

```go
type Item struct {
	OrderID int64
	Sku     string
}
var Load = sqlshape.Copy[Item]("order_items", "order_id", "sku")
// Copy into order_items: column "line_no" is NOT NULL without a default and is not copied
```

OK

```go
type Item struct {
	OrderID int64
	LineNo  int16
	Sku     string
}
var Load = sqlshape.Copy[Item]("order_items", "order_id", "line_no", "sku")
```

## 第2部 — スキーマが宣言する規約

チームの規約のうち、SQLの形として現れるものは検査器に強制させられる。論理削除の条件を必ず付ける、テナント列で必ず絞る、テーブルを直接読まずビューを通す、ステータス列は決めた遷移でしか動かさない、といったもの。規約は`schema.sql`の、対象のテーブルの直上に書く。宣言しない限り何も効かない。

### 宣言の仕組み

テーブル（またはビュー）が**義務**を宣言し、そのテーブルに触る文は、自分が証明できることでその義務を**履行**する。一般形は`CREATE TABLE`か`CREATE VIEW`の直上のディレクティブ。

```sql
-- sqlshape: require <what> [on <kinds>]
```

| `<what>` | 文に求めること | `on`の既定 |
|---|---|---|
| SQLのboolean式（`deleted_at IS NULL`、`status <> 'closed' AND amount > 0`、`EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)`） | その表の行についてこれが成り立つこと。各conjunctがWHERE / ONから含意される（等値、IS NULL、IS NOT NULL）か、字面どおり現れる。`EXISTS`は証人が要る（[後述](#表をまたぐ述語には証人が要るexists)） | `read` |
| `pinned(tenant_id)` | 読み・UPDATE・DELETEではその列を一つの値に固定する（`= {{.X}}`、リテラル、外側の参照）。INSERTでは値を入れる | `all` |
| `immutable(tenant_id)` | UPDATEでその列に代入しない | `update` |
| `via view` | その表を直接参照しない（`on`に書き込みを含めなければ、書き込みの対象にはできる） | `read` |
| `never` | そういう文が存在しないこと。追記専用の表（`require never on update, delete`） | `write` |
| `paired(outbox)` | 同じ文で名指しの表にも書くこと（書き込みCTE） | `insert` |
| `single` | 高々1行しか触らないと証明できること（`One`の証明） | `delete` |

`<kinds>`は`select` / `insert` / `update` / `delete`のカンマ区切りか、まとめ書きの`read`（SELECTと、WHEREで行を読むUPDATE / DELETE / MERGEの対象）、`write`、`all`。宣言の中の`$n`は「行を見る前に決まっている何かの値」— パラメータ、リテラル、外側の参照 — を指し、その番号のパラメータという意味ではない。Goテンプレートの`{{.X}}`はそういう値の一つ。

`require`行ではないが義務に展開される宣言が3つある。`visible where <expr>`（`require <expr> on read`のもとの綴り）、`aggregate`、`transitions`。`sensitive`は列にラベルを付ける。vetのフラグ`-require-columns=tenant_id`・`-no-table-reads`・`-no-tables`は、その列を持つ全表への`require pinned(tenant_id)`、全表への`require via view`、`require via view on all`の略記。

義務の履行経路は5つで、見ておくべきものは`-strict`が報告する。

1. 文自身のWHERE / ON / SET
2. ビュー経由。ビューの定義はスキーマ読み込み時にそれ自身が判定され、ビューの読み手はその中の表について再度判定されない（関数の本体も同じ扱い）
3. 行レベルセキュリティのポリシー。USINGがそれを成り立たせていれば、行セキュリティの対象ロールについて履行される（表が`FORCE ROW LEVEL SECURITY`でなければ`-strict`が所有者への注記を出す）
4. 複合外部キーをまたいで。`FOREIGN KEY (order_id, tenant_id) REFERENCES orders (id, tenant_id)`があれば、`order_id = orders.id`で結合し`orders.tenant_id`が固定されていれば`order_items.tenant_id`も固定されている
5. 文側のopt-out。`-- sqlshape: unfiltered orders`（述語型の義務）か`-- sqlshape: waive orders pinned(tenant_id)`（宣言どおりの綴りで1つ。`waive orders`だけならその表の義務を全部）。opt-outは`-strict`で報告される

表の出現ごとに判定する。自己結合やサブクエリでもう一度その表を読めば、そこでも義務を負う。`RETURNING`は判定しない。ビューに付けた義務はビューの読み手への義務で、ビューの定義文は中の表の義務を自分で履行する側。

### 必ず付ける読み取り条件（`visible where`）

```sql
-- schema.sql
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (...);
```

NG

```sql
SELECT id, body FROM memos WHERE user_id = {{.UserID}}
-- rows of memos are visible where deleted_at IS NULL: add that predicate for memos, or opt out with `-- sqlshape: unfiltered memos`
```

OK

```sql
SELECT id, body FROM memos WHERE user_id = {{.UserID}} AND deleted_at IS NULL
```

```sql
-- 意図的に全行を読む文
-- sqlshape: unfiltered memos
SELECT id, body FROM memos WHERE id = {{.ID}}
```

補足。ビューも同じ規則で検査される。条件を持つビューを通して読めば、そのビューの利用側は条件を満たしたことになる。`RETURNING`の列は直前に書いた行なので検査しない。

### 列を必ず固定する（`pinned`、`-require-columns`）

```sql
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (...);
```

NG

```sql
SELECT id, total FROM orders WHERE id = {{.ID}}
-- orders.tenant_id is not pinned: every statement on orders must fix tenant_id by equality (or assign it)
```

OK

```sql
SELECT id, total FROM orders WHERE id = {{.ID}} AND tenant_id = {{.TenantID}}
```

補足。INSERTは`tenant_id`に値を入れなければならない。行レベルセキュリティのポリシーが`tenant_id`を固定していれば、それでも要件を満たす。ただしテーブルが`FORCE ROW LEVEL SECURITY`でなければ所有者には効かないので、`-strict`で`orders.tenant_id is pinned by policy ... for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner`と注記される。

楽観ロックは同じ宣言をバージョン列に、書き込みだけに付けたもの。

```sql
-- sqlshape: require pinned(version) on update, delete
CREATE TABLE orders (..., version int NOT NULL DEFAULT 1);
CREATE TRIGGER orders_bump_version BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION bump_version();
```

```sql
UPDATE orders SET status = {{.Status}} WHERE id = {{.ID}} AND version = {{.Version}}
-- OK。`AND version = ...`が無いと:
-- orders.version is not pinned: every statement on orders must fix version by equality (or assign it)
```

補足。版を上げるのはトリガ、見た版を名指しするのは文、何も当たらなかった`One`のUPDATEは`ErrNoRows`を返す（先を越された）。

### テーブルを直接読まない（`via view`、`-no-table-reads` / `-no-tables`）

```sql
-- sqlshape: require via view
CREATE TABLE orders (...);
```

NG（`-no-table-reads`）

```sql
SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id
-- table orders is read directly; with -no-table-reads application code reads views (tables are written, not read)
```

OK

```sql
SELECT id, customer_name FROM order_summary
```

補足。`-no-table-reads`はINSERT / UPDATE / DELETE / MERGEの対象としてテーブルを使うことは許す。`-no-tables`は書き込みも含めてテーブルへの参照を一切禁じる（`table orders is referenced directly; with -no-tables application code reads views and calls functions only`）。

### 表をまたぐ述語には証人が要る（`EXISTS`）

表をまたぐ述語は`EXISTS`で書く。文の側は、それを成り立たせる**証人**を持っていなければならない。同じレベルで本体が成り立つように結合した表（内部結合。外部結合のONは対象の行を絞らない）か、文自身の否定なし`EXISTS` / `IN (SELECT ...)`で本体が成り立つもの。

```sql
-- sqlshape: require EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)
CREATE TABLE shipments (...);
```

OK（3つとも）

```sql
SELECT s.carrier FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.tenant_id = {{.T}}
SELECT carrier FROM shipments s WHERE EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = {{.T}})
SELECT carrier FROM shipments WHERE order_id IN (SELECT id FROM orders WHERE tenant_id = {{.T}})
```

NG（外部結合、否定したサブクエリ）

```sql
SELECT s.carrier FROM shipments s LEFT JOIN orders o ON o.id = s.order_id AND o.tenant_id = {{.T}}
SELECT carrier FROM shipments s WHERE NOT EXISTS (SELECT 1 FROM orders x WHERE x.id = s.order_id AND x.tenant_id = {{.T}})
```

### 集約にはルート経由で触り、1文で1つだけ触る（`aggregate`）

集約（DDDの一貫性の単位）は義務の束で、ルートの直上に1行で宣言する。

```sql
-- sqlshape: aggregate orders (order_items, order_notes)
CREATE TABLE orders (...);
```

各子表への`require pinned(<ordersへの外部キー列>) on all`（子はルート経由で触る。鍵を固定するか、ルートの鍵で結合する）と、集約の全表への`alone`（1文は1集約にしか触らない）に展開される。集約をまたぐ読みはビューの仕事で、どの集約にも属さない表（lookup）は自由に結合できる。子表はルートか、集約の他のメンバーへの外部キーを持っていなければならない。孫はぶら下がる親の鍵を固定し、ルートまでの結合は1リンクずつ判定される。

NG

```sql
SELECT o.status, v.id FROM orders o JOIN invoices v ON v.order_id = o.id WHERE o.id = {{.ID}}
-- orders belongs to aggregate orders and this statement also touches invoices of aggregate invoices: one statement, one aggregate (read across aggregates through a view)
```

`lock <列>`を付けると、ルートのバージョンが集約全体のロックになる。

```sql
-- sqlshape: aggregate orders (order_items, order_item_tags) lock version
```

ルートは`pinned(version) on update, delete`（見た版を名指しする）を負い、各子表はUPDATE / DELETEで、外部キーをたどった先（孫なら親を経由して）のルート行がその版であることの`EXISTS`を負う。

```sql
UPDATE order_items i SET qty = {{.Q}} FROM orders o WHERE o.id = i.order_id AND o.version = {{.V}} AND i.id = {{.ID}}
```

補足。版を上げるのはDBの仕事のまま（ルートのトリガ。子から上げるなら子のトリガがルートを更新する）。何も当たらなかった`One`の書き込みは`ErrNoRows`を返す。

### ステータス列は宣言した遷移でしか動かさない（`transitions`）

```sql
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled, paid -> refunded
CREATE TABLE orders (...);
```

その列をある状態にSETするUPDATEは、WHEREで現在の状態をその状態の前状態のいずれかに固定していなければならない。compare-and-setなので、2人の書き手が同じ行を同時に動かせない。

OK

```sql
UPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND status = 'submitted'
```

NG

```sql
UPDATE orders SET status = 'paid' WHERE id = {{.ID}}
-- orders.status: SET status = 'paid' must fix the current state in WHERE (status = 'submitted')
UPDATE orders SET status = 'paid' WHERE id = {{.ID}} AND status = 'draft'
-- orders.status: draft -> paid is not a declared transition (paid comes from submitted)
```

補足。遷移先は宣言した状態のリテラルに限る。`SET status = {{.S}}`は前状態の集合を検査できないので拒否される。INSERTは遷移ではないので検査しない。この宣言はseed済みの`(from_status, to_status)`lookup表と同じ形で、DB側ではトリガで守れる。

### 追記専用、対になる書き込み、1行だけの削除（`never`、`paired`、`single`）

```sql
-- sqlshape: require never on update, delete
CREATE TABLE ledger (...);
-- sqlshape: require paired(outbox) on insert
-- sqlshape: require single on delete
CREATE TABLE orders (...);
```

`never`は、そういう文が存在しないことを求める。`ledger`へのUPDATE / DELETEは、単独でも`WITH`の中でもNG（`ledger is declared \`require never on update, delete\`: no statement may do this to it`）。

`paired(outbox)`は、`orders`へのINSERTが同じ文で`outbox`にも書くことを求める。outboxの行がそれの告げる書き込みと一緒に動くので、トランザクション層は要らない。

```sql
WITH o AS (INSERT INTO orders (...) VALUES (...) RETURNING id)
INSERT INTO outbox (id, payload) SELECT id, 'created' FROM o
-- OK。ordersだけのINSERTだと:
-- a write to orders must also write outbox in the same statement (a data-modifying WITH): require paired(outbox) on insert
```

`single`は、DELETEが高々1行しか触らないことを`One`と同じ証明で求める。

```sql
DELETE FROM orders WHERE id = {{.ID}}              -- OK
DELETE FROM orders WHERE status = 'cancelled'
-- orders requires a single-row DELETE: fix a unique key by equality (the One proof)
```

### ラベル付きの列は許された文脈でしか読まない（`sensitive`、`may read`）

```sql
-- sqlshape: sensitive pii: email, phone
CREATE TABLE orders (...);
-- sqlshape: context billing: may read pii
CREATE VIEW order_contacts AS SELECT id, email, left(phone, 3) || '***' AS phone_masked FROM orders;
```

ラベルの付いた列は、そのラベルを`may read`する文脈（[文脈](#呼び出し元ごとに規約を変えるcontext)）でしか参照できない。SELECT句でもWHEREでも同じ。値を書き込むのは読むことではない。ラベルは列をそのまま通すビューを通って伝わり、式（マスクした列）で止まる。

```sql
SELECT email FROM orders WHERE id = {{.ID}}            -- billing以外ではNG
-- orders.email is pii: this context may not read it (a context with `may read pii`, or a view that masks it)
SELECT email FROM order_contacts WHERE id = {{.ID}}    -- これもNG。ビューがemailをそのまま通している
SELECT phone_masked FROM order_contacts                -- OK。式にはラベルが付かない
```

### 呼び出し元ごとに規約を変える（`context`）

運用スクリプトはテナントなしで走る、分析者はビューしか読まない、請求だけは個人情報を読む。**文脈（context）**はその差分を表ごとに宣言し、パッケージか`check`の実行が一つを選ぶ。

```sql
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete
-- sqlshape: context analyst: require via view
-- sqlshape: context billing: may read pii
CREATE TABLE orders (...);
```

```go
// Package ops は運用スクリプトを実行する。
//
// sqlshape: context ops
package ops
```

`waive <body>`はその文脈の中で基底の義務を外す（宣言どおりの綴りで名指し）。`require ...`はその文脈だけの義務を足す。`may read <label>`はラベルの読み取りを許す。パッケージはパッケージコメントで文脈を名乗る。無ければvetの`-context`フラグ、`sqlshape check -context ops`はファイルに対して選ぶ。文脈を選ばなければ基底の義務だけが効く。

### Goの外のSQLも同じ判定を受ける（`sqlshape check`）

同じ判定は、Goコードの外のSQLにも使える。運用のUPDATE、backfill、エージェントがこれから流すクエリ。

```
$ sqlshape check -schema schema.sql ops.sql     # または ... < ops.sql
ops.sql:2: ok orders: require pinned(tenant_id)
ops.sql:6: waived orders: require pinned(tenant_id): orders: `require pinned(tenant_id)` is waived by this statement
ops.sql:8: FAIL orders: visible where deleted_at IS NULL: rows of orders are visible where ...
sqlshape: 2 finding(s)
```

判定は全部、履行経路つきで出る（`ok`、`ok(policy)`、`ok(fk)`、`waived`）。そのスクリプトに何が許されたかの監査ログを兼ねる。`-quiet`は失敗だけを出す。義務を履行できない文か解析に失敗する文があれば終了コード1。文の直上の`-- sqlshape:`行はその文のもので、`-context`・`-require-columns`・`-no-tables`・`-no-table-reads`はvetと同じく受け付ける。

## 第3部 — 文の外側

### sqlshapeを通さないSQLを書かない（`-raw-sql`）

実行時に組み立てた文字列でpgxや`database/sql`の`Query` / `Exec`を呼ぶことは、テンプレートの保証が届かない穴になる。

NG（既定の`-raw-sql=constant`）

```go
rows, err := pool.Query(ctx, "SELECT id FROM orders WHERE "+where)
// sqlshape: SQL passed to Query must be a constant: a string built at run time can carry injected SQL; write the dynamic parts as a sqlshape.Query template (or pass -raw-sql=allow)
```

OK

```go
rows, err := pool.Query(ctx, "SELECT id FROM orders WHERE status = $1", status)
```

補足。`-raw-sql=forbid`は定数であってもsqlshapeを通らない文をすべて拒否する（`pgxpool.Query executes SQL outside sqlshape; with -raw-sql=forbid every statement goes through sqlshape.Query / One / Copy (or list the package in -raw-sql-allow)`）。移行中のパッケージは`-raw-sql-allow=pkg/...`で除外する。

### パッケージは自分のスキーマだけを参照する（`-schemas`）

`-schemas=a_api,b_private`は、そのパッケージが参照してよいPostgreSQLのスキーマを限定する。1つのデータベースを複数サービスで使うときの境界。

```sql
SELECT id FROM c_private.orders
-- c_private.orders is outside the schemas this code may reference (a_api,b_private)
```

### スキーマ自体の問題

`schema.sql`は読み込み時に、それ自体も一度解析される。`LANGUAGE sql`と`LANGUAGE plpgsql`の関数の本体、ビュー、ポリシーはPostgreSQLがCREATE時に行うのと同じ型検査を受け、seedのINSERTは冪等性と型を検査される。問題はパッケージ内の最初の`Query`の位置に`sqlshape: schema ...`として報告される。

PL/pgSQLの本体は、変数をスコープに入れた上で文ごとに検査される。`DECLARE`した変数はその型で（`%TYPE`と`%ROWTYPE`はスキーマから解決する）、record変数はそれを埋めたクエリの形で（`FOR r IN SELECT ...`、`SELECT ... INTO r`）、トリガー関数の`NEW`と`OLD`は`CREATE TRIGGER`で結びつけられた各テーブルの行の型で、それに関数のパラメータ、`FOUND`、`TG_OP`などのトリガー変数、例外ハンドラ内の`SQLSTATE`と`SQLERRM`。代入と`RETURN`は宣言された型と、`RETURN QUERY`は`RETURNS TABLE`と照合される。変数と列の両方に当たる名前はPostgreSQLと同じく曖昧としてエラーになる。`EXECUTE`は定数文字列ならその文として検査されるが、実行時に組み立てた文字列は検査できないので、`-strict`で知らせる:

```sql
CREATE FUNCTION purge(tbl text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'DELETE FROM ' || quote_ident(tbl);
  -- sqlshape: schema: function purge: line 3: EXECUTE runs SQL built at run time, which is not checked (a constant string would be)
END $$;
```

```sql
-- schema.sql
CREATE VIEW order_summary AS
SELECT o.id, c.nmae AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id;
-- sqlshape: schema schema.sql: view order_summary: column "nmae" does not exist (SQLSTATE 42703)
```

補足。行レベルセキュリティのポリシーもここで検査される。`CREATE POLICY`の条件式はboolean型で、集約やウィンドウ関数を含まず、ドメインの単位を守っていなければならない。行セキュリティを有効にしていないテーブルにポリシーがあれば報告する。ポリシーの条件を文の側で繰り返すことは要求しない。絞り込むのはデータベースの仕事である。

`-strict`ではスキーマと文についての助言が加わる。一覧は[flags.ja.md](flags.ja.md#-strict)。
