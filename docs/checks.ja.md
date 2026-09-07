# 検査器が検証するもの

[English](checks.md)

パッケージ内の`sqlshape.Query[R, P](template)`、`sqlshape.One[R, P](template)`、`sqlshape.Copy[R](...)`、
`sqlshape.MatView(...)`はすべて`go/analysis`アナライザーが見つけ、テンプレートを分岐の全組み合わせに展開し
（[templates.ja.md](templates.ja.md)）、各展開を`schema.sql`に対して解析し、その結論をGoの型と比較する。
このページは何を比較するかを、答える問いごとにまとめたもの。

## 形: Goコードは文に合っているか

結果列 ↔ `R`。すべての展開のすべての結果列は`R`のちょうど1つのフィールドに名前で束縛されなければならない。
順に`col:"..."`タグ、`db:"..."`タグ、フィールド名のsnake_case。受け手の無い列も、列の無いフィールドも報告される。
名前の無い列（`SELECT 1 + 1`）や同名の2列（`o.id, u.id`）は束縛できず、検査器はどれに別名を付けるべきかを言う。
`R`がスカラー（`int64`、`string`、…）なら文は1列を返さなければならない。

nullability。NULLになりうる列にはnullableなフィールドが要る: ポインタ、スライス、マップ、`sql.Null*`、`pgtype.*`、
または`sql.Scanner`を実装する型。アナライザーはカタログ（`NOT NULL`、主キー、`GENERATED`）、述語
（`WHERE col IS NOT NULL`、`col = ...`）、関数（非NULL入力に対するstrict関数、非NULL既定値の`coalesce`）から
nullabilityを導き、JOIN（外部結合の内側はnullableになる）とビュー（ビュー自身のWHEREが列を絞る）を通して伝える。
主張はどちら側からでも上書きできる: フィールドタグ`col:",notnull"`、テンプレート行`-- sqlshape: not null col, col`
（SQL側の双子）、schema.sqlの`CREATE FUNCTION`の上の`-- sqlshape: not null`（その結果について）。

省略可能な投影。一部の分岐だけが選ぶ列はnullableなフィールドで受けてよく、選ばない分岐ではnilのまま。
どの分岐も選ばないフィールドはやはりエラー。

パラメータ ↔ `P`。`{{.X}}`はそれぞれ`$n`パラメータになり、アナライザーは使われた場所から各`$n`に要る
PostgreSQLの型を推論する（`WHERE id = $1` → `bigint`、`= ANY($1)` → 配列）。パスは`P`上で解決され
（`.Filter.Name`はネストしたstructへ、`range`の変数はスライスの要素へ）、Goの型は下の型表と照らされる。
不一致、桁落ちする変換（`int64`を`integer`へ）、SQL側がNULLを要するかもしれない場所の非nullableなGo型は報告される。
どの展開も読まない`P`のフィールドは`-strict`で報告される。

埋め込みstructは平坦化される: `type Row struct { Base; Note *string }`はBaseの列を自分の列として受ける
（2つのフィールドが1列を束縛するのはエラー）。`{{.ID}}`は`P`の昇格フィールドに届く。名前付きのstructフィールド、
または`col:"..."`タグを持つ埋め込みフィールドは、代わりにネストした行になる。

ネストした行。`array_agg(row(o.id, o.total))`、`array_agg(o)`、`row(...)`、複合型の列はstructまたはstructの
スライスで受ける。無名レコードならstructのフィールドは位置で、名前付き複合型なら名前と順序で照合される。
ランタイムはフィールドごとに読む（[runtime.ja.md](runtime.ja.md#ネストした行とユーザー型)）。

複合型パラメータ。SQLが`money_amount`を期待する場所の`{{.Price}}`はstructを、`order_items[]`を期待する場所の
`{{.Items}}`はstructのスライスを取る。フィールドは型の列とネストした行と同じ規則で並べられる。

Go型表はpgxが実際にscan / encodeするものを、稼働中のPostgreSQLで検証したもの:

| PostgreSQL | Go |
|---|---|
| `bool` | `bool` |
| `smallint` / `integer` / `bigint` | `int16` / `int32` / `int64` / `int`（狭いGo型は注記付きで受理: `bigint into int32`） |
| `real` / `double precision` | `float32` / `float64`（`double precision into float32`は注記） |
| `numeric` | `string`（全桁保持）、`pgtype.Numeric`、`big.Rat`、`shopspring/decimal.Decimal`、`apd.Decimal`。floatや整数は精度の注記付きで受理 |
| `text` / `varchar` / `char` / `name` / `citext`などテキスト系の拡張型 | `string`（`string`はどんなパラメータ型としてもencodeできる） |
| `bytea` | `[]byte` |
| `uuid` | `uuid.UUID`（どのパッケージでも）、`[16]byte`、`string` |
| `timestamptz` / `timestamp` / `date` | `time.Time`（`-strict`は`timestamp`と`date`がゾーン / 時刻を失うことを注記） |
| `time` | `time.Time`、`string` |
| `interval` | `time.Duration`、`pgtype.Interval` |
| `json` / `jsonb` | `[]byte`、`json.RawMessage`、`string`、またはpgxがunmarshalできるstruct / スライス / マップ |
| `inet` | `netip.Addr` / `netip.Prefix` |
| `cidr` | `netip.Prefix` |
| `macaddr` | `net.HardwareAddr` / `string` |
| `hstore` | `map[string]*string` |
| `T[]` | `[]Go(T)` |
| 範囲型 | `pgtype.Range[T]`、`T`はサブタイプと照合される（ユーザー定義の範囲型も） |
| 多重範囲型 | `pgtype.Multirange[pgtype.Range[T]]` |
| `bit` / `point` / `tsvector` | `pgtype`の値 |
| `xml` / `money` / `tsquery` / `jsonpath` / `timetz` | `string` |
| `oid` | `uint32` |
| enum、seed済みlookupのキー、CHECKの値集合 | Goのnamed string type（[下記](#意味-その値は何を表すか)） |
| ドメイン | 基底型のGo型、またはドメインに束縛したnamed type |
| 複合型、レコード | struct |

宣言型バインディング。Goの型はdocコメントで自分が運ぶPostgreSQLの型を名指しできる:

```go
// sqlshape: type money_amount
type Money struct{ ... }
```

検査器はSQLが`money_amount`（その配列と、その上のドメインを含む）を持つ場所でだけ`Money`を受理し、それ以外では
報告する。ワイヤ形式は型自身の`sql.Scanner` / `driver.Valuer`に任せる（Scannerはテキスト形式を受ける）。
複合型をdecimalのラッパーに、拡張型をnamed typeにするのはこの方法で。バインディングは型と一緒にパッケージを跨ぐ。

COPY。`sqlshape.Copy[R]("order_items", "order_id", "line_no", ...)`はINSERTと同じように検査される:
テーブルと列は存在しなければならず、各列の型はそれを供給するフィールドに合っていなければならず、
省いた列はすべて既定値を持つか生成列でなければならない。

## 意味: その値は何を表すか

意味を持つ列に出会ったGoのnamed typeは、登録なしに使用箇所からそれに束縛され、以後その型が現れる
すべての場所で束縛が検査される（パッケージ跨ぎは`go/analysis`のfactで）。

enum、lookupテーブル、CHECKの値集合。enum列、seed済みlookupテーブルのキー、`CHECK (col IN (...))`を持つ列に
出会ったGoのnamed string typeはその値集合に束縛される。型付き定数はラベルと両方向にdiffされる: 定数の無いラベルも
ラベルの無い定数も報告され、`T("typo")`変換も、全ラベルを網羅しない`switch`も報告される。型は`Known() bool`を
実装してよく、行マッパーはこのビルドが知らないラベルを`*UnknownLabelError`で拒否する。

seed済みlookupテーブルは、行がschema.sqlに普通の`INSERT ... VALUES`で書かれているテーブル。行はスキーマの一部で、
検査器はキー列の値を、キーやそれを参照する列が使われるあらゆる場所で値集合として使い、マイグレーションはテーブルの内容を
宣言に揃え続ける（[migrations.ja.md](migrations.ja.md#seed済みテーブル)）。値集合の置き場として推奨する。
行はラベルと並び順を持てる、使用中の行は外部キーが守る、行を消せば値を退役させられる、JOINで分析側にも名前が届く。
`-strict`はすべてのenum列でそう言う。

キーの同一性。キー列（主キー、または外部キーで主キーから派生した列）に出会ったGoのnamed typeはその同一性に束縛される。
`orders.id`を期待する場所に渡された`UserID`は、両方`bigint`でも報告される。複合キーは位置で束縛される。

ドメイン。ドメインに出会ったGoのnamed typeはそれに束縛される。ドメインの混用や、ドメインを期待する場所への基底型は
報告される。SQL内ではドメインは不透明な単位: `price_yen + weight_g`や`balance > total`はPostgreSQLが受理しても
報告される。リテラルとパラメータは単位を引き継ぎ、`yen + yen`、`yen * n`、`abs(yen)`、`coalesce(yen, 0)`はyenのまま、
基底型への明示キャストで単位が落ちる。

忠実さ（`-strict`）。無名のGo型で運ばれるenum・ドメイン・キー列は検査できないので報告される。`time.Time`で受けた
`timestamp` / `date`、非ポインタのenumパラメータ（零値`""`はラベルでなく実行時に失敗する）も報告される。

既定値の所有者（`-strict`）。`DEFAULT`を持つ列に常に書き込む非nullableなパラメータは、データベースの既定値が
決して適用されないということ。どちらが所有するか決める（列を`{{if}}`で囲めばデータベース側の既定値が使われる）。

## 失敗モード: この書き込みは何で失敗しうるか

INSERT / UPDATE / DELETE / MERGEの各展開について、アナライザーは違反しうる制約を列挙する。一意キーと主キー、
外部キーの両方向（挿入した行が存在しない親を参照する、削除した行がまだ参照されている）、CHECK、ドメインのCHECK、
書き込む値がNULLになりうるときのNOT NULL。テンプレートはそれらを宣言しなければならない:

```sql
-- sqlshape: expect order_items_pkey, order_items_order_id_fkey, order_items_qty_check
INSERT INTO order_items (order_id, line_no, sku, qty, price) VALUES (...)
```

行に無い起こりうる違反も、どの展開でも起こりえない宣言も報告されるので、行は正確な契約のまま保たれる。
実行時にクラス23のエラーは同じキーの`ConstraintError`にラップされ、
`sqlshape.Violates(err, "order_items_qty_check")`で読み戻せる（[runtime.ja.md](runtime.ja.md#エラー)）。

キー。名前付き制約はその名前。無名の制約にはPostgreSQLが付ける名前が付くので、診断のキー、expect行のキー、
実行時エラーのキーは同じ文字列になる:

| 制約 | キー | 例 |
|---|---|---|
| `PRIMARY KEY` | `<table>_pkey` | `orders_pkey` |
| `UNIQUE (a, b)` | `<table>_<a>_<b>_key` | `customers_email_key` |
| `(a)`上の`REFERENCES` | `<table>_<a>_fkey` | `orders_customer_id_fkey` |
| `(a)`を参照するテーブル`CHECK` | `<table>_<a>_check`（複数列、または列を参照しないCHECKは`<table>_check`） | `orders_total_check` |
| ドメインの`CHECK` | `<domain>_check` | `yen_check` |
| `NOT NULL` | `<table>.<column>` | `orders.total` |
| トリガーのエラー | SQLSTATE、または`-- sqlshape: error`で付けた名前 | `P0401`、`OrderTooLarge` |

同じ名前になる2つ目の制約にはPostgreSQLと同様に番号が付く（`orders_total_check1`）。診断は由来を書き出す:
`may violate customers_email_key (UNIQUE (email) on customers, SQLSTATE 23505)`。

パラメータからのNOT NULL。NULLになりうる値が`{{.X}}`のとき、`X`のGo型がnilになりえなければ（`string`は
NULLを送らない）その違反は落とされる。ポインタ、スライス、マップは残す。

トリガー。raiseするトリガー関数はschema.sqlで注釈する:

```sql
-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger ...
```

そのSQLSTATE（付けた名前で）は、トリガーが付いたテーブルへのINSERT / UPDATE / DELETEの失敗モードに、
発火するイベントについて加わる。

関数経由。ユーザー関数の呼び出しはその本体の失敗モード（呼び出す関数のもの、宣言したエラーも含めて）を運ぶので、
`SELECT place_order({{.CustomerID}}, {{.Note}})`は中のINSERTがするのと同じように外部キー、ドメインのCHECK、
トリガーのコードを宣言する。診断は`through place_order()`と言う。本体がパラメータに帰すNOT NULLは呼び出しの
引数に辿られ、`STRICT`関数はNULLでは呼ばれもしないので、その引数は違反を落とす。

## カーディナリティ: `One`は2行返しうるか

`sqlshape.One[R, P]`は1行以下を主張し、検査器はすべての展開について別々に証明する。SELECTが単一なのは、
すべてのFROM項目が一意キー（主キー、`UNIQUE`、一意インデックス、または文が述語を繰り返す部分一意インデックス）を
リテラル・パラメータ・外側参照・非相関スカラーサブクエリとの等値で固定されているとき。等値はJOIN
（外部結合のONはnullable側だけ固定する）、ビュー、サブクエリ、CTEを通して追う。`GROUP BY`の無い集約、
定数の`LIMIT 0` / `LIMIT 1`、FROMの無いSELECT、1行の`VALUES`、単一行の`INSERT ... RETURNING`も単一。
`FULL JOIN`は決して単一でない。`{{if .ID}} AND id = {{.ID}} {{end}}`はelse分岐で落ちる。それが狙い。
実行時は`Get`が`ErrNoRows`を返し、`Find`は有無を報告し、データベースが証明に反したらどちらも`ErrManyRows`を返す。

## 境界: このコードは何を見てよいか

可視性ポリシー。schema.sqlで注釈したテーブル

```sql
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (...)
```

はあらゆる場所でその述語を通して読まれなければならない。ビューも含む。述語を繰り返すビューは、その読み手について
満たしたことになる。テンプレートは`-- sqlshape: unfiltered memos`で明示的に外れる。`RETURNING`リストは
検査しない（今書いた行だから）。

行レベルセキュリティはスキーマの一部。`CREATE POLICY`の述語はPostgreSQLと同じようにテーブルに対して型検査され
（boolean、集約やウィンドウ関数なし、ドメインを尊重）、ポリシーと`ENABLE / FORCE ROW LEVEL SECURITY`は
テーブルと一緒にdiffとapplyを通る。行セキュリティが無効なテーブル上のポリシーはスキーマの問題。`-strict`では:
ポリシーの無い行セキュリティ有効（所有者以外は行が見えない）、テーブルに到達する`SECURITY DEFINER`関数
（所有者の権限はテーブルがFORCEしない限りポリシーを飛ばす）、`current_setting(name, true)`を読むポリシー
（設定しなかったセッションは黙って行が見えなくなる）。検査器は文にポリシーの述語を繰り返させない。絞るのはデータベース。

行の所属。`-require-columns=tenant_id`はすべての文に、その列を持つ各テーブルで等値による固定を求める。INSERTは
代入しなければならない。列を固定する行レベルセキュリティのポリシーは要件を満たす（`-strict`では、テーブルが行セキュリティを
`FORCE`していないときに注記。所有者には固定が効かないから）。

テーブルアクセス。`-no-table-reads`はテーブルの読みを禁じる: SELECTと、書き込みの読み部分はビューを通す。テーブルは
INSERT / UPDATE / DELETE / MERGEの対象にはなれる。`-no-tables`はテーブルへの直接参照をすべて禁じる: アプリケーション
コードはビューを読み、関数を呼び、テーブルはデータベースの私的な側。`-schemas=a_api,b_private`はパッケージが参照して
よいスキーマを限る（1つのデータベース上のサービス境界）。

生のドライバ呼び出し。実行時に組んだ文字列でのpgxや`database/sql`の`Query` / `Exec`は、テンプレートの保証が
覆わない穴。`-raw-sql=constant`（既定）はそのSQL引数が定数であることを求め、`-raw-sql=forbid`はsqlshapeを
通らない文をすべて拒否し（`-raw-sql-allow=pkg/...`でパッケージを除外）、`-raw-sql=allow`は切る。

## スキーマの問題

`schema.sql`自身も読み込み時に一度解析される。すべての`LANGUAGE sql`関数の本体（パラメータがスコープに入り、
`RETURNS`の形を検査）、すべてのビュー、すべてのポリシーがPostgreSQLのCREATE時と同じように型検査され、
seedのINSERTは冪等性と型を検査される。結果はパッケージ最初の`Query`の位置に`sqlshape: schema ...`として
報告される。アナライザーの非助言的な注記（ドメインの不一致、常に偽の述語、照合順序の衝突）も診断になる。

`-strict`ではスキーマと文についての助言的な指摘が加わる。一覧は [flags.ja.md](flags.ja.md#-strict)。
