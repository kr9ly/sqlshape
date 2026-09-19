# MySQL

[English](mysql.md)

sqlshapeのうちMySQLに属するものを1枚にまとめる。スキーマが名乗るバージョンとサーバ設定、検査器が何を埋め込みどう検証しているか、規則が使う型と制約名、`database/sql`の上のランタイム、マイグレーションコマンド。規則そのものは[checks.ja.md](checks.ja.md)にあり、どのデータベースでも同じである。規則の中に出てくる名前・番号・型が、`mysql`を名乗るスキーマではどこから来るのか、がこのページの中身になる。

## スキーマがバージョンとサーバ設定を名乗る

```sql
-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'
-- sqlshape: server lower_case_table_names = 1
CREATE TABLE ...
```

`schema.sql`はどのバージョンのMySQL向けかを名乗る。持っているのは8.4。MySQL自身の文法がスキーマと全部の文を読み、MySQLの規則が式に型を付ける。値の違う宣言が2つある、または`postgres`も同時に名乗る、はNG。

文の判定を変えるサーバ変数は、バージョンと並べて1行に1つ宣言する。検査器と本番の接続を同じ設定に揃えるための行である（[checks.ja.md](checks.ja.md#スキーマはサーバの設定を名乗るserver)）。MySQLが読むのは3つ:

- `sql_mode`。カンマ区切りの名前（大文字小文字は問わない）、空文字列、組み合わせモードの`ANSI`と`TRADITIONAL`。組み合わせはサーバと同じに展開する。判定を変えるモードは次のとおり。
  - 字句解析のビット（`ANSI_QUOTES`、`PIPES_AS_CONCAT`、`IGNORE_SPACE`、`NO_BACKSLASH_ESCAPES`、`HIGH_NOT_PRECEDENCE`、`REAL_AS_FLOAT`）: パーサがテキストをどう読むか
  - `ONLY_FULL_GROUP_BY`: グループ検査の有無
  - 厳密モード（`STRICT_TRANS_TABLES`か`STRICT_ALL_TABLES`）: 文字列関数がNULL可になるか、`NOT NULL`列への`NULL`が失敗モードになるか
  - `NO_UNSIGNED_SUBTRACTION`: 符号なし同士の減算が符号付きになる

  残りは実行時にしか効かないので、そのまま受け付ける。
- `lower_case_table_names`。0は表名とビュー名を大文字小文字で区別する（Linuxの既定）、1は小文字にして持つ、2は綴りを保って区別せずに照合する。2は大文字小文字を区別しないファイルシステム（macOSやWindows）専用であり、区別するファイルシステムでは`mysqld`が警告を出して0で起動するため、2を宣言したスキーマとずれる（`mysql.Verify`がそのずれを返す）。
- `max_sp_recursion_depth`。0〜255。0（既定）なら自分自身を`CALL`するPROCEDUREは呼び出しのたびに1456、0より大きければ呼び出しが達する深さは静的に決まらないので検査器は何も予測しない。

宣言が無ければ、Linuxで初期化したままの8.4を仮定する。既定の`sql_mode`（`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`）、`lower_case_table_names = 0`、`max_sp_recursion_depth = 0`である。

## 検査器が埋め込んでいるもの

パーサと字句解析器はMySQL 8.4自身のもので、サーバのソースから切り出して（文法はアクションを剥がし、字句解析器はそのまま）WebAssemblyにしてある。関数の表も同じソースから読む。アナライザーはその木の上のpure Goの実装で、検査時にMySQLへ接続することはない。MySQLのパーサを抱えているので、検査器のMySQL側（`check/mysql`）はGNU General Public License v2である。リンクされるのは`sqlshape`バイナリだけで、利用者のプログラムには入らない。

判定は動いている`mysqld`と照合してある。型を付けた5,033文（組み込み関数の全部について引数の組み合わせごとの結果型）、エラーになる65文、`ONLY_FULL_GROUP_BY`検査の376文、既定でない`sql_mode`の下の22文と6つの書き込みが8.4と一致する。

## 規則が使うもの

[checks.ja.md](checks.ja.md)の規則はMySQLのスキーマにも同じにかかる。判定するのがPostgreSQLではなくMySQLのアナライザーになるだけである。結果列とパラメータとGo型の対応、NULLの扱い、型の意味、失敗モード、`One`の証明、第2部の宣言すべて（`visible where`、`pinned`、`via view`、`EXISTS`、`aggregate`、`transitions`、`never`、`paired`、`single`、`sensitive`、`context`）。診断はMySQLのエラー番号とメッセージ文を運ぶ（`Unknown column 'nope' in 'field list' (MySQL error 1054)`）。`{{.X}}`はワイヤ上で`?`になる。

### Go型の表

go-sql-driver/mysqlが`parseTime=true`で実際に返す型で、動いているサーバと照合してある。整数は`int64`（`BIGINT UNSIGNED`は`uint64`）、`DECIMAL`はその文字列、時刻型は`time.Time`か文字列、バイナリ文字列とJSONは`[]byte`で届く。比較と論理演算子は`bigint(1)`で、`bool`で受けられる。`TINYINT(1)`も同じ。

| MySQL | Go |
|---|---|
| `TINYINT` / `SMALLINT` / `MEDIUMINT` / `INT` / `YEAR` | `int64` / `int32` / `int`（`UNSIGNED`なら`uint64` / `uint32` / `uint`も）。`TINYINT(1)`は`bool`も |
| `BIGINT` | `int64` / `int`（`UNSIGNED`なら`uint64` / `int64`）。`bigint(1)`は`bool`も |
| `DECIMAL` | `string` |
| `FLOAT` / `DOUBLE` | `float32` / `float64` / `float64` |
| `BIT` | `[]byte` |
| `CHAR` / `VARCHAR` / `TEXT` / `ENUM` / `SET` | `string` / `[]byte` |
| `BINARY` / `VARBINARY` / `BLOB` | `[]byte` |
| `JSON` | `[]byte` / `string` |
| `DATE` / `DATETIME` / `TIMESTAMP` | `time.Time` / `string` |
| `TIME` | `string` |
| `ENUM`列、キーの同一性 | Goのnamed type（[型に意味を持たせる](checks.ja.md#型に意味を持たせる)）。`ENUM`は`CHECK (col IN (...))`と同じ値集合 |

MySQLには`// sqlshape: type`の束縛は無い。束縛先となる名前付きの型がMySQLに無いためである。

書かれた型と結果の型が違うものがいくつかある（いずれも実測）。

- ウィンドウ関数の整数（`MIN(id) OVER ()`、`FIRST_VALUE`、`NTH_VALUE`、`LAG`、`LEAD`など）はウィンドウの一時表を通ってクライアントに届くので広がる。`INT`と`BIGINT`は`BIGINT`に、`TINYINT` / `SMALLINT` / `MEDIUMINT`は`INT`になり、符号は保たれ、`YEAR`は`INT UNSIGNED`になる。ふつうの集約は列の型を保つ（`SMALLINT`の`MIN(small)`は`SMALLINT`）。`ROLLUP`のグループ列も、実行計画がたまたまグループ化を一時表に落とす場合を除いて型を保つ。その場合は検査器は予測しない
- `ROLLUP`の下では、グループ列を読む列がnullableになる（超集約行でNULLになる）。選択リストの定数はならない
- `DATE'...'` / `TIME'...'` / `TIMESTAMP'...'`リテラルはその型を持ち（小数桁は書かれた桁数）、NULLにならない。サーバがちょうどその型として読めないもの（時刻部のある`DATE`、時刻部の無い`TIMESTAMP`、`NO_ZERO_IN_DATE`下のゼロの月、`-14:00`〜`+14:00`の外の時差）は文のエラー1525になる。`<=>`も被演算子にかかわらずNULLにならない
- `USER()`、`CURRENT_USER()`、`DATABASE()`、`SCHEMA()`、`VERSION()`、`CURRENT_ROLE()`はバイナリ文字列でなく文字列（utf8mb3）である

### 制約名と失敗モード

失敗モードの名前はMySQLが制約に付ける名前そのもので、番号はMySQLのエラー番号である。

| 制約 | 名前 | エラー |
|---|---|---|
| 主キー | `PRIMARY` | 1062 |
| `UNIQUE`キー | キーの名前 | 1062（サーバが自分で番号を振るキーと、NULLが避けるキーは違反しない。プレフィックスキー`UNIQUE (c(10))`と式キー`UNIQUE ((n * 2))`は読む列を通して違反する） |
| 外部キー | `CONSTRAINT`名。無ければ`<table>_ibfk_<n>` | 子側は1452、親側は1451（`ON DELETE` / `ON UPDATE CASCADE`を追う） |
| `CHECK` | `CONSTRAINT`名。無ければ`<table>_chk_<n>` | 3819 |
| `NOT NULL` | `<table>.<column>` | 1048 |
| ビューの`WITH CHECK OPTION` | ビュー自身の名前（MySQLのエラーメッセージは名指しする。PostgreSQLの44000は何も名指ししない） | 1369 |
| ビュー経由のINSERTが代入しない、既定値の無い基底列 | ビュー自身の名前（サーバのメッセージは列でなくビューを名指しする。ビューがその列を見せているかどうかは関係ない） | 1423 |

文の形が一覧を変える。

- `INSERT IGNORE`は何にも違反しない
- `ON DUPLICATE KEY UPDATE`はINSERTのキー違反を吸収する
- `REPLACE`はキーには違反せず、参照している外部キーには違反しうる（1451）
- 厳密モードでなければ、`NOT NULL`列への`NULL`を拒むのは1行の`INSERT`と`REPLACE`（その`ON DUPLICATE KEY UPDATE`を含む）だけで、複数行、`INSERT ... SELECT`、`UPDATE`は型の暗黙の既定値を警告付きで格納するので、それらには1048を挙げない
- `UPDATE IGNORE`は`WITH CHECK OPTION`ビューの1369も、キーや`NOT NULL`の違反と同じように吸収する（測定済み）。トリガ自身の`SIGNAL`はどの`IGNORE`も吸収しないのと対照的である

`mysql.Violates(err, key)`は実行時のエラーを同じ名前で判定する。`Run` / `Exec`の外で走らせた文のエラーは`mysql.WrapError(err)`で先に同じ包み方にする。

列が決して格納できないリテラルは失敗モードでなく文自身のエラーである。厳密モード（既定）では毎回同じように失敗するからだ。検査器は`INSERT ... VALUES`、`UPDATE ... SET`、`ON DUPLICATE KEY UPDATE`、`REPLACE`で列に直接書かれたリテラルに、サーバの`Field::store`の規則をそのまま当てる（いずれも実測）。

| 格納先 | 拒まれるもの | エラー |
|---|---|---|
| 整数列 | 範囲外の値（実数・小数は丸めた後で判定） | 1264 |
| | 数を含まない文字列（`'abc'`、`''`） | 1366 |
| | 数の後ろに他の文字が続く文字列（`'12a'`） | 1265 |
| `YEAR` | 0、1〜99、1901〜2155以外 | 1264 |
| `DECIMAL(M,D)` | D桁に丸めた後で整数部がM-D桁を超えるもの。`UNSIGNED`への負数 | 1264 |
| `FLOAT` | 単精度の範囲を超える値 | 1264 |
| `DATE` / `DATETIME` / `TIMESTAMP` | `str_to_datetime`が読めない、範囲外、`sql_mode`が禁じる月・日・日付のゼロ（`NO_ZERO_IN_DATE`、`NO_ZERO_DATE`）、その月に無い日（`ALLOW_INVALID_DATES`でなければ）の文字列。`number_to_datetime`が拒む数。1970-01-01 00:00:01〜2038-01-19 03:14:07 UTCの外の`TIMESTAMP`（セッションのタイムゾーンで結論が変わらない範囲だけ判定） | 1292 |
| `TIME` | `str_to_time`が読めない文字列、60以上の分・秒、838時間超 | 1292 |
| `CHAR(n)` / `VARCHAR(n)` / `BINARY(n)` / `VARBINARY(n)` | 末尾の空白を除いてn文字（バイナリならnバイト）を超える文字列 | 1406 |
| `ENUM` | どのメンバーの名前でもなく（照合順序どおりに比較）メンバーの番号でもない文字列。1〜メンバー数の外の数 | 1265 |
| `SET` | 集合に無いメンバーを含むリスト | 1265 |
| 空間型 | 数と文字列。どちらも正しい形のジオメトリを持てない | 1416 |

読まないもの: `INSERT IGNORE` / `UPDATE IGNORE`と`sql_mode`が厳密でないスキーマ（値は警告付きで調整して格納される。`STRICT_TRANS_TABLES`だけの下では非トランザクショナルな表は最初の行だけ厳密）、ルーチンやトリガの本体の中の値、16進・ビットのリテラル、`JSON`と`BIT`の列、サーバが畳み込む式（`100 + 28`）。

### 空間型

ジオメトリの値は7つの型（`POINT`〜`GEOMETRYCOLLECTION`）のどれかで、列はそのどれか、または`GEOMETRY`（何でも）で宣言される。検査器は、コンストラクタ（`POINT(1, 1)`）、型付きのリーダ（`ST_PointFromText`）、定数のテキストやWKBが型を決めるとき値の型を知り、それで判定する（いずれも実測）。

| 文 | エラー |
|---|---|
| サーバのWKTリーダが拒む定数を渡した`ST_GeomFromText`（型付きの変種も）: 壊れたテキスト、1点の`LINESTRING`、4点未満か閉じていないポリゴンのリング、`(x y)`と`x y`を混ぜたか空の`MULTIPOINT`。拒まれる定数WKB（壊れている、末尾に余りのバイト）を渡した`ST_GeomFromWKB`。`ST_GeomFromWKB`にジオメトリ値を渡す | 3037 |
| 別の型の定数を渡した型付きリーダ（`ST_PointFromText('LINESTRING(...)')`。`GEOMCOLL`系は`MULTI*`も受ける） | 3516 |
| 0〜4294967295の外の定数SRID | 1690 |
| 引数1つの`LINESTRING(...)`。`POINT`定数で作ったリングが4点未満か閉じていない`POLYGON(...)` | 3037 |
| 別のジオメトリ型と分かる引数を渡した`LINESTRING` / `POLYGON` / `MULTI*`。ジオメトリを渡した算術・ビット演算子、数値関数、`BETWEEN`（比較は許される） | 1210 |
| 空間列に格納する値が列の型の内部形式でない: 数値や文字列、4バイトのSRID+リトルエンディアンの正しいWKBでない16進 / `UNHEX`定数、別のジオメトリ型の定数 | 1416。`sql_mode`にかかわらず、`IGNORE`でも |

型付きの空間列に別のジオメトリ型のnullableな式を格納する（`LINESTRING`列を`POINT`列へ）と、NULLでない値のたびに失敗する。これは失敗モード`1416`で、`mysql.Violates(err, "1416")`が一致する。読まないもの: 列が宣言するSRID（3643）、定数SRIDが空間参照系を指すか（3548）、関数自身の実行時の検査（縮退したリングへの`ST_Centroid`）、GeoJSON。

### 定数の算術

サーバが計算できない定数式は文自身の1690で、行を読む前に判定される（すべて実測。item_func.ccの計算どおり）:

| 式 | エラー |
|---|---|
| 厳密な結果が`BIGINT`（オペランドのどちらかが符号なしなら`BIGINT UNSIGNED`）に収まらない整数の`+` `-` `*` `DIV`（`9223372036854775807 * 2`、`CAST(1 AS UNSIGNED) - 2`、`-9223372036854775808 DIV -1`。文字列オペランドは演算子を`DOUBLE`に、小数オペランドは`DECIMAL`にする。どちらも判定しない）。`ABS(-9223372036854775808)`。整数の範囲を超える`ROUND(n, -k)` | 1690 `BIGINT [UNSIGNED] value is out of range` |
| 無限大になる`DOUBLE`の結果: `1e308 + 1e308`、`1e300 / 1e-300`、`EXP(710)`、`POW(2, 1024)`、`COT(0)`、`DEGREES(1e307)`。`FLT_MAX`を超える`CAST(x AS FLOAT)` | 1690 `DOUBLE value is out of range` |
| 関数や演算子が計算した`DOUBLE`を`BIGINT`の範囲外で`CAST(f AS SIGNED / UNSIGNED)`する（`CAST(POW(2, 63) AS SIGNED)`。リテラルは丸め込まれるだけ） | 1690。内側の式の名で |
| 定数nが1〜1024の外の`RANDOM_BYTES(n)` | 1690 `length value is out of range` |

定数がどこにあるかで、文が失敗するか行が失敗するかが決まる。`WHERE`・`HAVING`・`ON`ではオプティマイザが行を読む前に畳み込む（キーのある列との`id = 9223372036854775807 + 1`、`IN (...)`、`LIKE`、条件の項）ので文が失敗する。`FROM`の無い問い合わせの選択項目、派生表やCTEのもの、`INSERT ... VALUES`の値も同じ。`FROM`のある問い合わせの選択項目、`UPDATE`の`SET`、`ON DUPLICATE KEY UPDATE`の代入、索引の無い列との比較、`HAVING`での集約との比較は行ごとに実行される。文は失敗モード`1690`を持ち（`mysql.Violates(err, "1690")`が一致する）、行の無い問い合わせは失敗しない。サーバが評価しないものは放っておく: `AND` / `OR`を決めた定数の後ろ（`1 = 0 AND x`）、定数条件の`IF` / `CASE` / `COALESCE` / `IFNULL`が取らない枝、定数の`IN`が一致した後ろのリスト、NULLになりえないxの`x IS NULL`、`GROUP BY` / `ORDER BY`の項目、`EXISTS`のサブクエリの選択リスト、`LIMIT 0`。読まないもの: トリガやルーチンの本体、16進 / ビットリテラルのオペランド、ユーザー変数、ウィンドウの`ORDER BY`、`DECIMAL`の桁溢れ（65桁）。

同じ2つの形は、義務判定（x/obligation）にとっては書き込みが2つある文である:

| 文 | 記録する書き込み | 理由 |
|---|---|---|
| `INSERT ... ON DUPLICATE KEY UPDATE` | INSERTと、枝が代入する列へのUPDATE | 枝は既存行を動かす |
| `REPLACE` | INSERTとDELETE | キーが衝突すると既存行を先に削除する（その表の`AFTER DELETE`トリガが発火する、測定済み） |

`WITH CHECK OPTION`を宣言したビュー経由の書き込みは、基底表の`require pinned(<列>)`を履行する（`Discharge.Path`は`ByView`）:

- ビュー自身のWHEREがその列を等値で固定しているとき。`WITH CHECK OPTION`と書くだけならCASCADEDなので下位の全ビューのWHEREも数える（結合の向こうにあっても。測定済み）。`WITH LOCAL CHECK OPTION`はそのビュー自身で止まるが、自前のCHECK OPTIONを宣言した下位ビューはサーバが検査し続けるので数える（測定済み）
- 裏付けは上の1369そのもの。CHECK OPTIONを宣言していないビューはこの経路を持たない（そこを経由した書き込みは行をビューから静かに外すだけで、サーバは拒まない）
- `UPDATE`にも`INSERT` / `REPLACE`にも効く（`DELETE`はCHECK OPTIONを検査しない。実測）。下位のビューが自分のCHECK OPTIONを宣言していれば、書き込む先のビューに宣言が無くても検査され、1369は書き込む先のビューを名指しする（実測）

失敗モードでないもの: 長さ・範囲・`ENUM`値の切り詰め（1265 / 1406 / 1366 / 1264）。型の側の性質（パラメータのGo型、リテラル自身の値集合）なので、そちらで捕まえる。

### 定数の変換

厳密モードの`INSERT` / `REPLACE` / `UPDATE` / `DELETE`（`IGNORE`の外）は、定数の変換が出す警告をエラー1292 `ER_TRUNCATED_WRONG_VALUE`に格上げする。`SELECT`・`SET`・`DO`・非厳密の書き込みでは同じ式が警告つきで実行される（行があっても。すべてmysqldで実測）:

| 式 | 失敗する条件 |
|---|---|
| 定数の`CAST` / `CONVERT`を`DATE` / `DATETIME`へ | 値がセッションのゼロ日付フラグの下でdatetimeでない（`CAST('2004-10-0' AS DATE)`、`CAST(65 AS DATETIME)`） |
| ... `TIME`へ | `str_to_time`が拒む、または838時間を超える |
| ... `YEAR`へ | 文字列の先頭の整数の後ろに何かが続く（`'2020extra'`、`'2020.5'`）か、値が0〜99・1901〜2155の外。数字が全く無い文字列は警告なしの0 |
| ... `CHAR(n)`へ | 値の文字列形がnより長い（`CAST(1000 AS CHAR(3))`） |
| ... `SIGNED` / `UNSIGNED`へ | 文字列が整数そのものでない（`'abc'`、`'1.5'`、`'1e2'`）か、絶対値が`BIGINT UNSIGNED`を超える。範囲外の小数 |
| ... `DOUBLE` / `FLOAT` / `DECIMAL`へ | 文字列が数を持たない、後ろに余りがある、doubleの読みが溢れる（`'1e999'`） |
| 定数の`TIMESTAMP(x)` | xがdatetimeでない（時刻だけの文字列、モードが禁じる`TIMESTAMP('0000-00-00 10:00:00')`） |
| `+` `-` `*` `/`の文字列オペランド | 数そのものでない（`10E+0 + 'a'`。`''`は静かに0） |
| ビット演算子の文字列オペランド | 整数そのものでない（`1 >> ''`） |
| 数値列と定数文字列の比較 | 文字列が数を持たない（`WHERE i = '1invalid'`。`''`・`' 1'`・`'1e1'`・`'1.5'`は通る）。`BETWEEN`は行ごと |

失敗の落ちる場所は上の1690と同じ — `INSERT ... VALUES`の値や条件の項そのものは文のエラー、`UPDATE`の`SET`や`FROM`つき選択項目は失敗モード`1292`（`mysql.Violates(err, "1292")`が一致する）— だが、比較のオペランドにある定数だけは常に行ごとに実行されるので、`WHERE d = CAST('2004-10-0' AS DATE)`はエラーでなく違反になる。

文の種類にもモードにもよらず失敗する変換が2つある:

- `DATE` / `DATETIME` / `TIMESTAMP`の値とdatetimeでない定数文字列の比較は、比較がどこにあっても — 選択項目、`JOIN`の`ON`、`HAVING`、`ORDER BY` / `GROUP BY`の項目、死んだ枝でも — 文の1525 `Incorrect DATETIME value`。厳密モードの書き込みでは格納形の1292になる（`BETWEEN`と`IN`は先に変換しない。`TIME`と`YEAR`の列はこの規則でない）
- ゼロの月か日の上に時差を書いたdatetime文字列・リテラルは時差の変換で失敗する: 1292 `Truncated incorrect temporal value`

`TIMESTAMP`列に格納する`DATE'...'` / `TIMESTAMP'...'`リテラルは1970-01-01 00:00:01〜2038-01-19 03:14:07.999999 UTCに収まらなければならない。時差が書いてあれば厳密に（範囲はUTCで定義されているのでセッションのタイムゾーンは判定を動かせない）、無ければセッションのタイムゾーンが判定を動かせない場合だけ判定する。小数秒は先に列の精度へ丸められ、繰り上がりも数える（`TIMESTAMP'1970-01-01 00:00:00.999999+00:00'`は`TIMESTAMP(0)`では00:00:01になり格納される）。`TIMESTAMP`列は`ALLOW_INVALID_DATES`を無視し、実在する月日の上の年0も範囲で検査する。`DATETIME`はどちらも受ける。

読まないもの: 関数の結果の格納（`STR_TO_DATE(...)`）、時間型キャストの値を列の格納検査へ運ぶこと、`UPDATE`の`ORDER BY`の定数、時間型の列とスカラサブクエリ定数の比較、今のモードでは格納できない列の`DEFAULT`、`TIME`列自身の行ごとの文字列変換。

### トリガとストアドルーチン

ローダーは`CREATE TRIGGER` / `CREATE PROCEDURE` / `CREATE FUNCTION`（`DEFINER`、`IF NOT EXISTS`、特性句込み）と`DROP` / `ALTER`（特性のみ）を、表と同じように読む。`DELIMITER`は要らない。本体の`;`は1つの複合文の内側として読まれ、mysqlクライアント互換の`DELIMITER x`行もそのまま読める。CREATE時にサーバ自身が拒むものは、未知の表と同じくProblemになる: 表が無い（1146）、トリガ・ルーチンが既にある（1359 / 1304）、DROPしようとしたトリガ・ルーチンが無い（1360 / 1305）、`FOLLOWS` / `PRECEDES`が指す先のトリガが無い（3011）。`DROP TABLE`は表のトリガを道連れにし、`RENAME TABLE`はトリガを付け替える。

`CREATE EVENT`も同じように読む（2つのスケジュール形、`STARTS` / `ENDS`、`ON COMPLETION`、`ENABLE` / `DISABLE`、`COMMENT`）。`ALTER EVENT`（書いた句がその部分を置き換える。スケジュールを書けばスケジュール全体、`RENAME TO`は名前）と`DROP EVENT`も適用する。プログラムの文が実行するものはイベントに届かないので、本体を読むのはスキーマ自身のためである。サーバは`CREATE`時に本体を何も検査しない（無い表への`DELETE`も受け付け、実行のたびに失敗する。測定済み）。検査器はそれをスキーマのproblemとして報告し、本体の各文はルーチンと同じように義務の判定にかける。イベントの中の`RETURN`は1313。マイグレーションのコマンドはイベントを管理する（[migrations.ja.md](migrations.ja.md#mysql)）。

本体はスキーマごとに1回読む。PostgreSQLのPL/pgSQL関数本体の読み方と同じ位置づけである（[checks.ja.md](checks.ja.md#トリガーが送出するエラーには名前を付ける)）。`IF` / `CASE` / `LOOP` / `WHILE` / `REPEAT`、ラベル付きブロックと`LEAVE` / `ITERATE`、`RETURN`、`SET`、`SELECT ... INTO`、カーソル（`DECLARE` / `OPEN` / `FETCH` / `CLOSE`）、`CALL`、`SIGNAL` / `RESIGNAL`、`DECLARE ... HANDLER FOR`を歩く。名前はサーバと同じ規則で解決する。

- `NEW.col` / `OLD.col`はトリガ表の列として型付けする。無い列は1054
- INSERTトリガでの`OLD`、DELETEトリガでの`NEW`は1363
- `OLD`への代入（イベントより先に見る。INSERTトリガでも同じ）、`NEW`へのBEFORE以外での代入は1362。トリガの外の`SET NEW.col` / `SET OLD.col`は1193（そこでの`NEW.col`の読み取りは、サーバが実行時にしか解決しない列参照）
- NOT NULL列の`NEW.col`もBEFOREトリガの中ではNULLになりうる。AFTERトリガでは宣言どおりで、サーバ自身のNOT NULL検査は両者の間で走る（測定済み）
- `DECLARE`した変数やルーチンの引数は同名の列より優先して解決する

本体の作成時にサーバ自身が拒むものは、ここでもエラーになる。

| 構文 | エラー |
|---|---|
| 対応ラベルの無い`LEAVE` / `ITERATE` | 1308 |
| FUNCTION以外での`RETURN` | 1313 |
| `RETURN`の無いFUNCTION | 1320 |
| 未宣言のカーソルや変数、`FETCH`の列数不一致 | 1324 / 1327 / 1328 |
| `SELECT ... INTO`の列数不一致 | 1222 |
| 結果集合を返すトリガ・関数（自身のINTO無しトップレベル`SELECT`、または`CALL`した先のPROCEDURE自身のもの） | 1415 |
| 本体内の`COMMIT` / `START TRANSACTION` / DDL文 | 1422 |
| トリガが自分の表に書く | 1442。タイミング×イベント×書き込みの18通り全部で（測定済み）。常に失敗するので、発火する文にではなくトリガの定義に報告する |
| 同じブロック内で同名の変数を2回`DECLARE`する | 1331 |
| 同じブロック内で同名の`DECLARE ... CONDITION`か`DECLARE ... CURSOR`を2回（変数とカーソルの同名は名前空間が別で許される） | 1332 / 1333 |
| 同じブロック内の2つの`HANDLER`が同じ条件値を名指す（`SQLEXCEPTION`と`SQLSTATE '45000'`のように重なるだけなら許される） | 1413 |
| トリガまたはFUNCTION内の動的SQL（`PREPARE` / `EXECUTE` / `DEALLOCATE PREPARE`）と`FLUSH`（PROCEDUREは対象外） | 1336 |
| 本体内の`LOCK TABLES` / `UNLOCK TABLES`・`LOAD DATA`・`ALTER VIEW`（PROCEDUREも対象。`SELECT ... INTO OUTFILE`は結果集合を返さないのでトリガや関数の中でも許される） | 1314 |
| 5文字でないSQLSTATEリテラル（`SIGNAL SQLSTATE '4500'`、それを名指す`CONDITION`や`HANDLER`） | CREATE時に1407。5文字ならどんな文字でも受理する（測定済み） |
| `MYSQL_ERRNO = 0`を設定する`SIGNAL` / `RESIGNAL` | 1231。常に失敗する |
| どの`HANDLER`の外でも届く値の無い`RESIGNAL` | 1645。常に失敗する |
| 自分自身を`CALL`するPROCEDURE | `max_sp_recursion_depth`が0（既定）のあいだ再帰呼び出しのたびに1456（0より大きく宣言すれば何も予測しない）。直接の自己再帰だけで、別ルーチン経由で自身に戻るものは対象外 |
| 自分自身を呼ぶFUNCTION | 呼び出しのたびに1424（`CREATE`は通る。この設定は関数には効かない） |
| 上流で使用中の表に書き戻すトリガの連鎖（`INSERT INTO x`がxのトリガを発火してyに書き、yのトリガがxに書く）、またはロック付きで読み戻す連鎖（`SELECT ... FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`。ロック無しの読みは衝突しない、測定済み） | 1442。どちらのトリガも自分の表には書いていなくても。`CALL`したルーチンが続ける連鎖も同じに数え、連鎖のどこかで見つかった確実な失敗（1442、1456）は発火した文に報告する |

本体の失敗モードのうち、`SIGNAL`と同じ「しうる」の形のもの:

| 構文 | 失敗モード |
|---|---|
| `ELSE`の無い`CASE`（simple・searchedいずれも） | 1339「Case not found for CASE statement」。どの`WHEN`にも一致しないとき |
| 自身の`SET MYSQL_ERRNO`を持つ値の無い`RESIGNAL` | 捕まえた`SIGNAL`の番号でなく、その番号で再送する |

トリガ・ルーチンの本体自身の書き込みは、その書き込み自身の失敗モード（本文の制約、その書き込みが起こす自分自身のトリガの失敗モード、再帰は打ち切り）を本体の失敗モードに持ち込む。`SIGNAL`のキーは、番号を設定していれば`MYSQL_ERRNO`の10進表記（組み込みの番号でも同じ。SQLSTATE `'23000'`の下で`SET MYSQL_ERRNO = 1062`とすればキーは`1062`で、メッセージに無いキー名を探しにいかない。測定済み）、無ければSQLSTATE（[ランタイム](#ランタイム-databasesql)がエラーを読み戻すのと同じ規則）。SQLSTATEクラス`01`は警告で失敗モードにならず、未処理のクラス`02`は1643、それ以外の未処理は1644になる。名前付き`CONDITION`はその値に解決し、値の無い`RESIGNAL`は最も内側の`HANDLER`が処理中のものをそのまま再送する。`CREATE TRIGGER` / `FUNCTION` / `PROCEDURE`の上に書く`-- sqlshape: error <key> = <Name>`は、PostgreSQLの同じ注釈（[checks.ja.md](checks.ja.md#トリガーが送出するエラーには名前を付ける)）と同じもので、`<key>`にNameを与え、プログラムはそれを`sqlshape.Error(<key>)`で写し取り、vetがスキーマと両方向に照合する。expect行と`mysql.Violates`が判定に使うのは相変わらずキーそのもの（`<Name>`でなく`30001`）だが、`<Name>`で綴っても同じことになる——`sqlshape.Error("30001")`から作った`sqlshape.Failure`はそのコードだけを運んでいるので。`DECLARE ... HANDLER FOR`はそのブロック内の一致する失敗モードを吸収する（`SQLEXCEPTION`はクラス`01`と`02`以外の全部、`SQLWARNING` / `NOT FOUND`はそのクラス、SQLSTATEや番号そのものは一致するもの）。`INSERT` / `UPDATE IGNORE`はトリガのSIGNALを何も吸収しない（測定済み: 文はそれでも失敗する）。`SELECT ... INTO`は、`One`が使うのと同じ証明で多くとも1行と示せない限り1172を持つ（1行も無ければNOT FOUNDで警告、失敗にはならない）。

表への文は、その表のトリガのその事象向けの失敗モードを引き継ぐ: INSERT / UPDATE / DELETE。`REPLACE`はINSERTとDELETEのトリガを、`ON DUPLICATE KEY UPDATE`はINSERTとUPDATEのトリガを発火させる（測定済み）。

カタログに無い名前のFUNCTION呼び出しはスキーマのルーチンに解決する（組み込み関数と同名で未修飾なら組み込みが勝つ、サーバと同じ。`db.f`はルーチンを名指す）。引数の数が違えば1318、そのルーチンが無ければ1305、結果は`RETURNS`の型で常にNULL可（宣言した型に関わらず`RETURN`はNULLを返しうるので、そうでないという静的な証明は無い）。本体自身の失敗モード（SIGNAL、書き込みの違反、それが発火するもの）は呼び出した文へ持ち込まれる。呼び出し文が読むか書く表に関数自身が書く場合は、実行のたびに1442になる（表を読むだけでも十分。列を読まず`FROM`に名前を挙げるだけでも同じ、測定済み）。書き込みは入れ子の呼び出しを通して数える（tに書くプロシージャを`CALL`する関数も、そうする別の関数を`RETURN`する関数も、同じように衝突する）。本体の中では文ごとに衝突を見る。tに書くfを`UPDATE t SET v = f(v)`で呼ぶルーチンは`CALL`すると1442、`SELECT COUNT(*) INTO n FROM t; SET @x = f(1)`と2文に分かれていれば衝突しない。トリガ自身の表は本体の全文と衝突する（`other`のトリガが`SET @y = g(1)`でotherに書くgを呼べば1442。発火した文が読んでいるだけの表はトリガの表ではないので衝突しない、測定済み）。本体内の`CALL`はトップレベルと同じに解決し（1305 / 1318 / 1414）、呼び先の失敗モードと書き込みは本体のものになる。`CREATE FUNCTION`の上に`-- sqlshape: not null`を書くと、その関数が決してNULLを返さないと宣言でき、PostgreSQLで[checks.ja.md](checks.ja.md#NULLになりうる列はNULLを受けられる型で受ける)が説明しているのと同じ指示子である。呼び出しはNOT NULLとして型付けされる。PROCEDUREやTRIGGERはこの指示子を拒む。どちらも値を返さないので、記述する対象が無い。

`CALL p(...)`はIN / INOUT引数をその引数宣言の型で型付けし、OUT / INOUT引数は変数でなければならない（1414）。文自身のfactsは`Kind Call`。変数として数えるもの:

| 引数 | 変数か |
|---|---|
| 宣言した変数、引数、`?` | 数える |
| `BEFORE`トリガ自身の本体内の`NEW.col` | 数える（サーバ自身の1414メッセージがこの例外を名指ししている） |
| `OLD.col`、`AFTER`トリガの`NEW.col` | 数えない |
結果列は本体自身のINTO無し`SELECT`から取る: 無ければ列無し、1つならその列、複数あって形が揃えば1つの列リストとして合意、形が割れていれば検査器自身のエラー（mysqld自身が拒むものではない——実行時にどの経路を通ったかでどちらかの結果集合を返すだけである）。失敗モードは本体自身のもの。

### `One`の証明

`PRIMARY KEY`と列全体にかかる`UNIQUE`キー、`LIMIT 1`、`GROUP BY`の無い集約から証明する。集約はどこに置かれていてもよい（`COALESCE(SUM(total), 0)`も`SUM(total)`と同じく1行。サブクエリが自分の列だけで集約するものはそのサブクエリのもの）。MySQLには部分インデックスが無い。

### グループ化

MySQLは`sql_mode`の`ONLY_FULL_GROUP_BY`の検査を行い、検査器も同じ番号で同じ検査を行う。グループ化または集約する問い合わせでは、SELECTリスト、`HAVING`、`ORDER BY`、ウィンドウの`PARTITION BY` / `ORDER BY`の各式が、`GROUP BY`の式か、集約か、グループ列に関数従属する列だけからできていなければならない（1055。`GROUP BY`が無ければ1140）。サーバが認める従属は検査器が認める従属と同じである。

- `PRIMARY`か`UNIQUE`キーが既知になった表の全列（NULL可のキー列は、そのNULLを退ける条件があるときだけ）
- `WHERE`と内部結合の`col = col`と`col = リテラル`
- 外部結合の`ON`がNULL可側に与えるもの
- 派生表やビューの本体が出力列を通して与えるもの
- `ROLLUP`の下ではグループ式そのものだけ

隣り合う検査も一緒に行う。

| 規則 | エラー |
|---|---|
| `HAVING`で集約の外に書く列はSELECTリストの列か別名か`GROUP BY`の列でなければならない | 1054 |
| `DISTINCT`があるとき、SELECTリストに無い`ORDER BY`の式が読めるのはSELECTリストの列だけ | 3065 |
| 他で集約していない問い合わせの`ORDER BY`の集約 | 3029 |
| 集合演算の`ORDER BY`の集約 | 3028 |

```sql
SELECT email, count(*) FROM users GROUP BY name
-- Expression #1 of SELECT list is not in GROUP BY clause and contains nonaggregated column
-- 'users.email' which is not functionally dependent on columns in GROUP BY clause; this is
-- incompatible with sql_mode=only_full_group_by (MySQL error 1055)

SELECT name, count(*) FROM users GROUP BY id            -- OK: id は主キー
```

### 名前解決

`ORDER BY`、`GROUP BY`、`HAVING`はサーバと同じにSELECTリストの別名を見る。裸の名前だけでなく式の中でも（`ORDER BY c + 1`、`GROUP BY CONCAT(f1)`。`GROUP BY`では同名の表の列が勝ち、別名付きで書いた項目は別名の無い同名の列に勝ち、同名の別々の項目が2つあれば1052）。外側のブロックの選択リスト・`GROUP BY`・`HAVING`・`ORDER BY`に置かれた入れ子の問い合わせはそのブロックの別名も見るが、`WHERE`や`ON`に置かれたものは見ない（`SELECT name c, (SELECT 1 FROM orders WHERE note = c) FROM users`は通り、同じサブクエリを`WHERE`に置くと1054）。サブクエリより後で宣言された別名は1247（`forward reference in item list`）、集約の別名は入れ子の問い合わせ自身の`HAVING`から以外は1247（`reference to group function`）で、`GROUP BY`に置かれたものからは常に1247。ウィンドウ関数の別名は3594。`HAVING`はブロック自身の表に対して名前を解決しない。選択リストにも`GROUP BY`にも無い列は最上位では1054で、サブクエリの中では外側のブロックの列になる（`WHERE EXISTS (SELECT 1 FROM orders HAVING id)`は外側の`id`を読む）。`_rowid`は基底表の最初のキー（サーバの並び: `PRIMARY`、`NOT NULL`列だけの一意キー、残り）が`NOT NULL`の整数型（`INT`系、`YEAR`、`BIT`）1列の一意キーであるときその列を指す。修飾した参照か、表が1つだけのレベルで。ビューと派生表には無い。`INSERT ... SELECT ... ON DUPLICATE KEY UPDATE`の代入は対象表と`SELECT`の表に対して解決する（両方にある修飾の無い名前は1052）。ただし`SELECT`がグループ化・集約されていれば対象表だけを見る。`VALUES(c)`は常に対象表の列で、選択リストの別名はそこから見えない。派生表には別名が要る（1248）。`QUALIFY`は8.4がハイパーグラフオプティマイザ無しで拒むとおりに拒む（6037）。`USING`と`NATURAL`の結合は共通列を1つにまとめる（修飾の無い名前は左側に解決し、`SELECT *`は1回だけ並べる）。表名とビュー名は`lower_case_table_names`の言うとおりに照合し、列名とキー名は大文字小文字を区別しない。書く表をサブクエリが読む`UPDATE` / `DELETE`はサーバが拒むとおりに拒む。表そのものなら1093（`WHERE`・`EXISTS`・`IN`のどれでも）、その表の上のビューなら1443。派生表で包めば実体化されて通り、同じ表からの`INSERT ... SELECT`も通る（実測）。

### ビュー

ビューは、`ALGORITHM=TEMPTABLE`と書かれているか、問い合わせがマージできない形（`GROUP BY`・`HAVING`・`DISTINCT`・`LIMIT`・集合演算・ウィンドウ関数・選択リスト内のサブクエリ）でない限り、読む側の問い合わせにマージされる（サーバ自身の`is_mergeable`）。マージされるビュー経由の書き込みは基底表に届き、それ以外のビューへの書き込みは1288になる（`INSERT`では同じ拒否が1471と綴られる）。マージされるビューはサーバ自身の2つの書き込みフラグを持つ。自分の問い合わせのFROMの葉について、どれかの葉が更新可能（基底表か、更新可能なビュー）なら更新可能、全部の葉が挿入可能なら挿入可能、どれかの葉が外部結合のNULL側に居ればどちらでもない——だから派生表や`TEMPTABLE`ビューとの結合は更新可能のまま挿入不能で、`TEMPTABLE`ビューの上のビューはどちらでもない。書き込みごとの規則（すべてサーバで実測）:

- `INSERT`は挿入可能フラグを要る（無ければ1471）。フィールド——列リスト、無ければビューの全列——はビューに対して解決し（1054、1136）、フィールドの中の派生列はその列の1348、外の派生列と、同じ基底列が2つのビュー列の裏に立つ形は1471。`COLLATE`の包みは透過する（サーバの`field_for_view_update`）。結合ビューではさらに列リストが必須で（1394）、名指す列は`ON DUPLICATE KEY UPDATE`の代入も含めて基底表1つに収まらなければならず（1393）、`REPLACE`は届かない（1395。削除の半分があるため）。失敗モードは基底表自身のもの（1062ほか）で、代入されないまま残る既定値の無い基底列は表の1364でなくビューの1423になる
- `UPDATE`は更新可能フラグを要り（無ければ1288）、素の列なら代入できる。ビュー自身の選択リストの式はその列の1348、ビューの中の実体化された葉の列はビューの1288、結合ビュー越しの代入は基底表1つに収めること（1393）
- `DELETE`は更新可能で葉が1つのビューならよい（派生列だけのビューでも構わない）。葉が2つ以上なら1395、それ以外（共通テーブル式を対象にした形も）は1288
- 書く対象のビュー自身をサブクエリが読めば対象そのものの扱いで1093になる（同じ基底表の上の別のビューなら1443）

サーバがマージしないビューに付けた`WITH CHECK OPTION`は、サーバが`CREATE`を拒む（1368）のと同じくスキーマの読み込み時に拒む。`CREATE OR REPLACE VIEW`は前の定義を置き換える。`ALTER VIEW`も同じく置き換える（ビューが存在しなければ1146、その名前が表なら1347）。

### 結果集合を返さない文

`SELECT` / `INSERT` / `UPDATE` / `DELETE` / `CALL`のほかに、検査器は次の文を読む（`Exec`で実行するもの）。

- `LOAD DATA [LOCAL] INFILE ... INTO TABLE t`はファイルの行のINSERTである。対象は基底表でなければならず（ビューは1288）、列リストはその表で解決し（`@var`はフィールドを受け取るだけで列には代入しない）、`SET`の代入は式を列に対して型付けし、その中のプレースホルダは列の型を取る。失敗モードはINSERTのもの——埋める列にかかるキー・外部キー・CHECK（1062 / 1452 / 3819）、`IGNORE`が警告に変えること、`REPLACE`が衝突する行を先に消すこと——だが、サーバが変える点が2つある（実測）。`NOT NULL`列のフィールドがNULLになりうることは1048でなく1263（`SET col = NULL`は1048のまま）、列リストから外した`NOT NULL`列は1364でなく型の暗黙の既定値を取る。
- `LOCK TABLES`が名指す表は存在しなければならず（1146。ビューもロックできる）、別名は重複してはならない（1066）。`UNLOCK TABLES`は何も解決しない。どちらにもパラメータは無い。
- `SELECT ... INTO OUTFILE` / `INTO DUMPFILE`は、`INTO`の位置がどちらでも包んでいる`SELECT`と同じく型付けするが、列は返さない。行はサーバ上のファイルに書かれる。`SELECT ... INTO @var` / `INTO var`も同じで、行は変数に入る（数は一致しなければならない。1222）。末尾の`FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`は`SELECT`の列をそのまま残す。
- `INSERT INTO t VALUES ()`（全行が空。列リストの有無は問わない）は既定値の行を挿入する。どの列にも代入しないので1136にはならず、既定値の無い`NOT NULL`列を外していれば通常どおり1364である。

### MySQLに無いもの

MySQLに無いものは検査しない。`Copy`と`MatView`、PL/pgSQL、ドメイン、複合型と配列、`-schemas`（MySQLのスキーマは1つのデータベース）、`// sqlshape: type`、マイグレーションのseed表。

## ランタイム: database/sql

```
$ go get github.com/kr9ly/sqlshape/v2         # Query / One: 検査器が読む宣言
$ go get github.com/kr9ly/sqlshape/mysql/v2   # database/sql と go-sql-driver/mysql の上で実行する
```

`github.com/kr9ly/sqlshape/mysql/v2`は宣言した文を`database/sql`とgo-sql-driver/mysqlを通して実行する。`mysql.DB`は`*sql.DB`、`*sql.Tx`、`*sql.Conn`のどれでもよい。テンプレートの`{{.X}}`はワイヤ上ではMySQLの位置指定`?`になり、引数はプレースホルダの出現順に並べ直される（2回使ったパラメータは2回送る）。

```go
for o, err := range mysql.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error]、逐次読み出し
orders, err := mysql.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := mysql.First(ctx, db, ListOrders, p)              // 無ければ ErrNoRows（sql.ErrNoRows）
res, err    := mysql.Exec(ctx, db, MarkPaid, p)                 // sql.Result

u, err     := mysql.Get(ctx, db, UserByEmail, p)                // One: 無ければ ErrNoRows
u, ok, err := mysql.Find(ctx, db, UserByEmail, p)               // One: ok が有無
res, err   := mysql.ExecOne(ctx, db, MarkPaid, p)               // One: 1行も触らなければ ErrNoRows
```

どのランタイムでも同じこと（行のマッピング、`One`、検査済みのSQLだけが走る保証、expect行の名前で返るエラー）は[runtime.ja.md](runtime.ja.md)にある。MySQL固有のもの:

- 受け型はドライバのもので、上の表のとおり。`DECIMAL`はその文字列で届くので、自前のmoney型で包める。
- 制約違反は`*mysql.ConstraintError`として返り、その`Key`は[上](#制約名と失敗モード)のスキーマ上の名前である。`mysql.Violates(err, key)`で判定する。トリガやルーチン自身の`SIGNAL`も同じように返り、キーは[上](#トリガとストアドルーチン)のとおりである。
- `ExecOne`は`RowsAffected`で判定する。MySQLは変更のあった行を数えるので、既に同じ値の行へのUPDATEはDSNに`clientFoundRows=true`が無いと`ErrNoRows`になる。`INSERT ... ON DUPLICATE KEY UPDATE`と`REPLACE`がその1行について報告する0・1・2行は、どれも1行とみなす。
- `Batch`、`Copy`、`MatView`は無い。
- `mysql.Verify(ctx, db, schemaSQL)`は接続のセッションの`@@sql_mode`とサーバの`lower_case_table_names`を読み、スキーマの宣言（無ければサーバの既定値）と違えばエラーを返す。DSNの`sql_mode=...`、プールのセッション初期化、別の設定で立てたサーバは、検査器が判定に使わなかった規則で文を走らせることになる。プールを開いた直後に1回呼ぶ。

  ```go
  if err := mysql.Verify(ctx, db, schemaSQL); err != nil { ... }
  ```

## マイグレーション

`sqlshape diff`、`apply`、`verify-schema`はMySQLのスキーマにもPostgreSQLと同じに働く。データベースと`schema.sql`の差分がDDLになり、DDLは到達する状態で検査され、それから実行される。両側はサーバ自身の`SHOW CREATE TABLE` / `SHOW CREATE VIEW`で読み、目標側は`-db`のサーバ上の一時データベースで正準化する。MySQLのDDLは暗黙にコミットされるので、DDLは1文ずつ実行する。何を比較するか、`enum`宣言の形、必要な環境は[migrations.ja.md](migrations.ja.md#mysql)にある。

## License

The MySQL side of the checker (`check/mysql`) carries MySQL's own parser and is under the GNU
General Public License v2, see [cmd/sqlshape/LICENSE](../cmd/sqlshape/LICENSE); it is linked into
the `sqlshape` binary, a development tool, and nothing under it is linked into your program. The
runtime (`mysql`) is Apache License 2.0, like the declarations.
