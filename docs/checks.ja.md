# 検査器が確かめること

[English](checks.md)

検査器はパッケージ内の`sqlshape.Query[R, P](template)`、`sqlshape.One[R, P](template)`、`sqlshape.Copy[R](...)`、`sqlshape.MatView(...)`をすべて見つけ、テンプレートを分岐の全組み合わせに展開し（[templates.ja.md](templates.ja.md)）、展開した各SQLを`schema.sql`に対して解析して、その結果をGoの型と突き合わせる。このページでは、何と何を突き合わせるのかを、読者が持つ問いごとにまとめる。

## 形: Goの構造体はSQLと合っているか

結果列と`R`の対応。展開したすべてのSQLについて、すべての結果列が`R`のフィールドのどれか1つに名前で対応しなければならない。対応の順序は`col:"..."`タグ、`db:"..."`タグ、フィールド名をsnake_caseにしたもの。受け手の無い列も、対応する列の無いフィールドも報告される。名前の無い列（`SELECT 1 + 1`）や同名の列が2つある場合（`o.id, u.id`）は対応づけられないので、検査器はどの列に別名を付けるべきかを言う。`R`がスカラー（`int64`、`string`など）なら、SQLは1列だけを返さなければならない。

nullability。NULLになりうる列は、NULLを受けられるフィールドで受けなければならない。ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`、または`sql.Scanner`を実装した型である。列がNULLになりうるかは、カタログ（`NOT NULL`、主キー、`GENERATED`）、WHERE句（`col IS NOT NULL`、`col = ...`）、関数（引数がNULLでないstrict関数、既定値がNULLでない`coalesce`）から判定し、JOIN（外部結合の内側はNULLになりうる）とビュー（ビュー自身のWHERE句で絞られる）を通して伝える。判定はどちらの側からでも上書きできる。Go側ならフィールドタグ`col:",notnull"`、SQL側ならテンプレートの`-- sqlshape: not null col, col`行、関数の戻り値なら`schema.sql`の`CREATE FUNCTION`の上の`-- sqlshape: not null`。

一部の分岐だけが選ぶ列。ある分岐でだけSELECTされる列は、NULLを受けられるフィールドで受ける。その列を選ばない分岐ではnilのままになる。どの分岐も選ばない列に対応するフィールドはエラーである。

パラメータと`P`の対応。`{{.X}}`はそれぞれ`$n`パラメータになり、検査器はその`$n`が使われている場所からPostgreSQL側で必要な型を推論する（`WHERE id = $1`なら`bigint`、`= ANY($1)`なら配列）。パスは`P`の上で解決され（`.Filter.Name`はネストした構造体を辿り、`range`の変数はスライスの要素を指す）、その位置のGo型を下の型表と照らす。型が合わない、精度が落ちる変換になる（`int64`を`integer`に渡す）、SQL側でNULLが必要になりうるのにGo型がNULLを表せない、のいずれも報告される。どの展開でも読まれない`P`のフィールドは`-strict`で報告される。

埋め込み構造体。`type Row struct { Base; Note *string }`のように埋め込むと、Baseのフィールドは自分のフィールドとして列を受ける（2つのフィールドが同じ列を受けようとするとエラー）。`{{.ID}}`も`P`の昇格フィールドに届く。名前付きの構造体フィールド、または`col:"..."`タグを付けた埋め込みフィールドは、平坦化されずにネストした行として扱われる。

ネストした行。`array_agg(row(o.id, o.total))`、`array_agg(o)`、`row(...)`、複合型の列は、構造体または構造体のスライスで受ける。無名のレコードなら構造体のフィールドと位置で、名前付きの複合型なら名前と順序で対応づける。ランタイムはフィールドごとに読み込む（[runtime.ja.md](runtime.ja.md#ネストした行とユーザー定義型)）。

複合型のパラメータ。SQL側が`money_amount`を期待する位置の`{{.Price}}`には構造体を、`order_items[]`を期待する位置の`{{.Items}}`には構造体のスライスを渡す。フィールドと型の列の対応づけはネストした行と同じ規則である。

Go型の表。pgxが実際にscan / encodeできる組み合わせを、稼働中のPostgreSQLで確認したもの:

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
| `T[]` | `[]Go(T)` |
| 範囲型 | `pgtype.Range[T]`。`T`はサブタイプと照合される（ユーザー定義の範囲型も同様） |
| 多重範囲型 | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | `pgtype`の対応する型 |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enum、seed済みlookupテーブルのキー、CHECKによる値集合 | Goのnamed string type（[下記](#意味-その値は何を表しているか)） |
| ドメイン | 基底型に対応するGo型、またはドメインに結びつけたnamed type |
| 複合型、レコード | 構造体 |

宣言型バインディング。Goの型に、それがどのPostgreSQL型を運ぶのかをdocコメントで宣言できる:

```go
// sqlshape: type money_amount
type Money struct{ ... }
```

こうすると検査器は、SQL側が`money_amount`（その配列と、それを基底型とするドメインを含む）である位置でだけ`Money`を受け入れ、それ以外の位置では報告する。値の変換は型自身の`sql.Scanner` / `driver.Valuer`に任せる（Scannerにはテキスト形式が渡る）。複合型をdecimalのラッパーで受けたいときや、拡張の型を自前の型で受けたいときに使う。宣言は型と一緒にパッケージを越えて効く。

COPY。`sqlshape.Copy[R]("order_items", "order_id", "line_no", ...)`はINSERTと同じように検査される。テーブルと列が存在すること、各列の型がそれを供給するフィールドと合うこと、指定しなかった列にはすべて既定値があるか生成列であることを確かめる。

## 意味: その値は何を表しているか

意味を持つ列に使われたGoのnamed typeは、登録なしに、その使用箇所からその意味に結びつけられる。以後、その型が現れるすべての場所で結びつきが検査される（パッケージを越える場合は`go/analysis`のfactで伝わる）。

enum、lookupテーブル、CHECKによる値集合。enumの列、seed済みlookupテーブルのキー、`CHECK (col IN (...))`の付いた列に使われたGoのnamed string typeは、その値集合に結びつけられる。型付き定数はラベルと両方向に比較される。定数の無いラベルも、ラベルの無い定数も報告され、`T("typo")`のような変換も、全ラベルを網羅していない`switch`も報告される。型に`Known() bool`を実装しておくと、行マッパーはこのビルドが知らないラベルを`*UnknownLabelError`として拒否する。

seed済みlookupテーブルとは、行を`schema.sql`に普通の`INSERT ... VALUES`で書いておくテーブルのことである。その行はスキーマの一部として扱われる。検査器はキー列の値を、キー列やそれを参照する列が使われるすべての場所で値集合として使い、マイグレーションはテーブルの内容を宣言どおりに揃え続ける（[migrations.ja.md](migrations.ja.md#seed済みテーブル)）。値集合の置き場としてはこれを推奨する。行にラベルや並び順を持たせられ、使用中の行は外部キーが守り、行を消せば値を廃止でき、JOINすれば分析側にも名前が届くからである。`-strict`はenumの列すべてにこのことを注記する。

キーの同一性。キー列（主キー、または外部キーで主キーから派生した列）に使われたGoのnamed typeは、そのキーの同一性に結びつけられる。`orders.id`を期待する位置に`UserID`を渡すと、どちらも`bigint`であっても報告される。複合キーは位置で結びつく。

ドメイン。ドメインの列に使われたGoのnamed typeはそのドメインに結びつけられる。別のドメインの型を渡したり、ドメインを期待する位置に基底型を渡したりすると報告される。SQLの中でも、ドメインは基底型とは別の単位として扱われる。`price_yen + weight_g`や`balance > total`は、PostgreSQLは受け付けるが検査器は報告する。リテラルとパラメータは相手の単位を引き継ぎ、`yen + yen`、`yen * n`、`abs(yen)`、`coalesce(yen, 0)`はyenのままで、基底型への明示的なキャストで単位が外れる。

表現の忠実さ（`-strict`）。enum・ドメイン・キー列を無名のGo型で受けていると、上記の検査ができないので報告される。`timestamp` / `date`を`time.Time`で受けている場合、enumのパラメータがポインタでない場合（ゼロ値`""`はラベルではないので実行時に失敗する）も報告される。

既定値の所有者（`-strict`）。`DEFAULT`のある列に、NULLを表せない型のパラメータで常に値を書き込んでいると、データベース側の既定値は決して使われない。どちらが既定値を持つのかを決めるべきである（列を`{{if}}`で囲めばデータベース側の既定値が使われる）。

## 失敗モード: この書き込みは何で失敗しうるか

INSERT / UPDATE / DELETE / MERGEの各展開について、検査器は違反しうる制約を列挙する。一意制約と主キー、外部キーの両方向（挿入する行が存在しない親を参照する、削除する行がまだ子から参照されている）、CHECK、ドメインのCHECK、書き込む値がNULLになりうる場合のNOT NULLである。テンプレートはこれらを宣言しなければならない:

```sql
-- sqlshape: expect order_items_pkey, order_items_order_id_fkey, order_items_qty_check
INSERT INTO order_items (order_id, line_no, sku, qty, price) VALUES (...)
```

起こりうるのに宣言していない違反も、宣言しているのにどの展開でも起こりえない違反も報告されるので、expect行は常に正確な一覧として保たれる。実行時には、SQLSTATEクラス23のエラーが同じキーを持つ`ConstraintError`に包まれて返り、`sqlshape.Violates(err, "order_items_qty_check")`で判定できる（[runtime.ja.md](runtime.ja.md#エラー)）。

キーの名前。名前付きの制約はその名前がキーになる。名前を付けなかった制約にはPostgreSQLが付ける名前がそのまま使われるので、診断に出る名前、expect行に書く名前、実行時エラーの名前は同じ文字列になる:

| 制約 | キー | 例 |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| 列`(a)`の`REFERENCES` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| 列`(a)`を参照するテーブルの`CHECK` | `<table>_<a>_check`（複数列を参照するか、列を参照しないCHECKは`<table>_check`） | `orders_total_check` |
| ドメインの`CHECK` | `<domain>_check` | `yen_check` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| トリガーが送出するエラー | SQLSTATE、または`-- sqlshape: error`で付けた名前 | `P0401`、`OrderTooLarge` |

同じ名前になる制約が2つあると、PostgreSQLと同様に番号が付く（`orders_total_check1`）。診断には由来も書かれる: `may violate customers_email_key (UNIQUE (email) on customers, SQLSTATE 23505)`。

パラメータ経由のNOT NULL。NULLになりうる値が`{{.X}}`であるとき、`X`のGo型がnilを表せない型（`string`はNULLを送れない）なら、その違反は候補から外れる。ポインタ、スライス、マップなら残る。

トリガー。エラーを送出するトリガー関数には`schema.sql`で注釈を付ける:

```sql
-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger ...
```

このSQLSTATEは（付けた名前で）、トリガーが付いているテーブルへのINSERT / UPDATE / DELETEのうち、トリガーが発火するイベントの失敗モードに加わる。

関数経由。ユーザー定義関数の呼び出しは、その本体の失敗モード（本体が呼ぶ関数のもの、宣言したエラーも含む）を引き継ぐ。`SELECT place_order({{.CustomerID}}, {{.Note}})`は、中で実行されるINSERTと同じように外部キー、ドメインのCHECK、トリガーのSQLSTATEを宣言しなければならない。診断には`through place_order()`と書かれる。本体でパラメータに由来するとわかったNOT NULLは、呼び出し側の引数まで辿られる。`STRICT`な関数はNULLでは呼ばれないので、その引数からの違反は候補から外れる。

## カーディナリティ: `One`は本当に1行以下か

`sqlshape.One[R, P]`は結果が1行以下であることを主張し、検査器はそれを展開ごとに証明する。SELECTが1行以下と言えるのは、FROMに現れるすべてのテーブルについて、その一意キー（主キー、`UNIQUE`、一意インデックス、またはWHERE句が同じ条件を含む部分一意インデックス）が、リテラル・パラメータ・外側の参照・相関の無いスカラーサブクエリのいずれかと等値で固定されているときである。等値はJOIN（外部結合のON句はNULLになりうる側だけを固定する）、ビュー、サブクエリ、CTEを通して追跡される。`GROUP BY`の無い集約、定数の`LIMIT 0` / `LIMIT 1`、FROMの無いSELECT、1行の`VALUES`、1行の`INSERT ... RETURNING`も1行以下と見なす。`FULL JOIN`は決して1行以下にならない。`{{if .ID}} AND id = {{.ID}} {{end}}`はelse側の展開で証明に失敗するが、それがこの検査の意図である。実行時には`Get`は無ければ`ErrNoRows`を返し、`Find`は有無を返し、データベースが証明に反して2行返したらどちらも`ErrManyRows`を返す。

## 境界: このコードは何を見てよいか

可視性ポリシー。`schema.sql`でテーブルに注釈を付ける:

```sql
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (...)
```

こうすると、このテーブルを読むすべての文はこの条件を持たなければならない。ビューも含む。この条件を持つビューを通して読めば、そのビューの利用側は条件を満たしたことになる。意図的に条件なしで読む文には`-- sqlshape: unfiltered memos`と書いて除外する。`RETURNING`の列は直前に書いた行なので検査しない。

行レベルセキュリティ。ポリシーはスキーマの一部として扱われる。`CREATE POLICY`の条件式はPostgreSQLと同じ規則でテーブルに対して型検査され（boolean、集約やウィンドウ関数は不可、ドメインの単位を尊重）、ポリシーと`ENABLE / FORCE ROW LEVEL SECURITY`の設定はテーブルと一緒にdiffとapplyを通る。行セキュリティを有効にしていないテーブルにポリシーがあればスキーマの問題として報告する。`-strict`ではさらに、行セキュリティが有効なのにポリシーが無いテーブル（所有者以外には行が見えない）、そのテーブルに到達する`SECURITY DEFINER`関数（テーブルが`FORCE`していない限り、所有者の権限ではポリシーが適用されない）、`current_setting(name, true)`を読むポリシー（設定していないセッションでは何も見えなくなるが、エラーにはならない）を報告する。ポリシーの条件を文の側で繰り返すことは要求しない。絞り込むのはデータベースの仕事である。

行の所属。`-require-columns=tenant_id`を指定すると、すべての文は、その列を持つ各テーブルでその列を等値で固定しなければならない。INSERTはその列に値を入れなければならない。行レベルセキュリティのポリシーがその列を固定していれば、それでも要件を満たす（ただしテーブルが`FORCE ROW LEVEL SECURITY`でなければ所有者には効かないので、`-strict`で注記する）。

テーブルへのアクセス。`-no-table-reads`はテーブルの読み取りを禁じる。SELECTも、書き込みの中の読み取り部分もビューを通さなければならないが、INSERT / UPDATE / DELETE / MERGEの対象としてテーブルを使うことはできる。`-no-tables`はテーブルへの参照を一切禁じる。アプリケーションはビューを読み、関数を呼ぶだけで、テーブルはデータベースの内部になる。`-schemas=a_api,b_private`は、そのパッケージが参照してよいスキーマを限定する。1つのデータベースを複数のサービスで使うときの境界になる。

生のドライバ呼び出し。実行時に組み立てた文字列でpgxや`database/sql`の`Query` / `Exec`を呼ぶことは、テンプレートの保証が届かない穴である。`-raw-sql=constant`（既定）はそのSQL引数が定数であることを要求し、`-raw-sql=forbid`はsqlshapeを通らない文をすべて拒否する（`-raw-sql-allow=pkg/...`で除外パッケージを指定できる）。`-raw-sql=allow`でこの検査は切れる。

## スキーマ自体の問題

`schema.sql`は読み込み時に、それ自体も一度解析される。`LANGUAGE sql`の関数の本体（パラメータがスコープに入り、`RETURNS`の形が検査される）、ビュー、ポリシーはPostgreSQLがCREATE時に行うのと同じ型検査を受け、seedのINSERTは冪等性と型を検査される。結果はパッケージ内の最初の`Query`の位置に`sqlshape: schema ...`として報告される。アナライザーが出す助言でない注記（ドメインの不一致、常に偽になる条件、照合順序の衝突）もそのまま診断になる。

`-strict`ではスキーマと文についての助言が加わる。一覧は[flags.ja.md](flags.ja.md#-strict)。
