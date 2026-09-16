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

### 制約名と失敗モード

失敗モードの名前はMySQLが制約に付ける名前そのもので、番号はMySQLのエラー番号である。

| 制約 | 名前 | エラー |
|---|---|---|
| 主キー | `PRIMARY` | 1062 |
| `UNIQUE`キー | キーの名前 | 1062（サーバが自分で番号を振るキーと、NULLが避けるキーは違反しない） |
| 外部キー | `CONSTRAINT`名。無ければ`<table>_ibfk_<n>` | 子側は1452、親側は1451（`ON DELETE` / `ON UPDATE CASCADE`を追う） |
| `CHECK` | `CONSTRAINT`名。無ければ`<table>_chk_<n>` | 3819 |
| `NOT NULL` | `<table>.<column>` | 1048 |
| ビューの`WITH CHECK OPTION` | ビュー自身の名前（MySQLのエラーメッセージは名指しする。PostgreSQLの44000は何も名指ししない） | 1369 |

文の形が一覧を変える。

- `INSERT IGNORE`は何にも違反しない
- `ON DUPLICATE KEY UPDATE`はINSERTのキー違反を吸収する
- `REPLACE`はキーには違反せず、参照している外部キーには違反しうる（1451）
- 厳密モードでなければ、`NOT NULL`列への`NULL`を拒むのは1行の`INSERT`と`REPLACE`（その`ON DUPLICATE KEY UPDATE`を含む）だけで、複数行、`INSERT ... SELECT`、`UPDATE`は型の暗黙の既定値を警告付きで格納するので、それらには1048を挙げない
- `UPDATE IGNORE`は`WITH CHECK OPTION`ビューの1369も、キーや`NOT NULL`の違反と同じように吸収する（測定済み）。トリガ自身の`SIGNAL`はどの`IGNORE`も吸収しないのと対照的である

`mysql.Violates(err, key)`は実行時のエラーを同じ名前で判定する。

同じ2つの形は、義務判定（x/obligation）にとっては書き込みが2つある文である:

| 文 | 記録する書き込み | 理由 |
|---|---|---|
| `INSERT ... ON DUPLICATE KEY UPDATE` | INSERTと、枝が代入する列へのUPDATE | 枝は既存行を動かす |
| `REPLACE` | INSERTとDELETE | キーが衝突すると既存行を先に削除する（その表の`AFTER DELETE`トリガが発火する、測定済み） |

`WITH CHECK OPTION`を宣言したビュー経由の書き込みは、基底表の`require pinned(<列>)`を履行する（`Discharge.Path`は`ByView`）:

- ビュー自身のWHEREがその列を等値で固定しているとき。`WITH CHECK OPTION`と書くだけならCASCADEDなので下位の全ビューのWHEREも数える（結合の向こうにあっても。測定済み）。`WITH LOCAL CHECK OPTION`はそのビュー自身で止まるが、自前のCHECK OPTIONを宣言した下位ビューはサーバが検査し続けるので数える（測定済み）
- 裏付けは上の1369そのもの。CHECK OPTIONを宣言していないビューはこの経路を持たない（そこを経由した書き込みは行をビューから静かに外すだけで、サーバは拒まない）
- 今のところ`UPDATE`だけ。ビュー経由の`INSERT`と`DELETE`はアナライザーがまだ読まない（この機能より前からある穴）

失敗モードでないもの: 長さ・範囲・`ENUM`値の切り詰め（1265 / 1406 / 1366 / 1264）。型の側の性質（パラメータのGo型、リテラル自身の値集合）なので、そちらで捕まえる。

### トリガとストアドルーチン

ローダーは`CREATE TRIGGER` / `CREATE PROCEDURE` / `CREATE FUNCTION`（`DEFINER`、`IF NOT EXISTS`、特性句込み）と`DROP` / `ALTER`（特性のみ）を、表と同じように読む。`DELIMITER`は要らない。本体の`;`は1つの複合文の内側として読まれ、mysqlクライアント互換の`DELIMITER x`行もそのまま読める。CREATE時にサーバ自身が拒むものは、未知の表と同じくProblemになる: 表が無い（1146）、トリガ・ルーチンが既にある（1359 / 1304）、DROPしようとしたトリガ・ルーチンが無い（1360 / 1305）、`FOLLOWS` / `PRECEDES`が指す先のトリガが無い（3011）。`DROP TABLE`は表のトリガを道連れにし、`RENAME TABLE`はトリガを付け替える。

`CREATE EVENT`も同じように読む（2つのスケジュール形、`STARTS` / `ENDS`、`ON COMPLETION`、`ENABLE` / `DISABLE`、`COMMENT`）。`ALTER EVENT`（書いた句がその部分を置き換える。スケジュールを書けばスケジュール全体、`RENAME TO`は名前）と`DROP EVENT`も適用する。プログラムの文が実行するものはイベントに届かないので、本体を読むのはスキーマ自身のためである。サーバは`CREATE`時に本体を何も検査しない（無い表への`DELETE`も受け付け、実行のたびに失敗する。測定済み）。検査器はそれをスキーマのproblemとして報告し、本体の各文はルーチンと同じように義務の判定にかける。イベントの中の`RETURN`は1313。マイグレーションのコマンドはイベントを管理する（[migrations.ja.md](migrations.ja.md#mysql)）。

本体はスキーマごとに1回読む。PostgreSQLのPL/pgSQL関数本体の読み方と同じ位置づけである（[checks.ja.md](checks.ja.md#トリガーが送出するエラーには名前を付ける)）。`IF` / `CASE` / `LOOP` / `WHILE` / `REPEAT`、ラベル付きブロックと`LEAVE` / `ITERATE`、`RETURN`、`SET`、`SELECT ... INTO`、カーソル（`DECLARE` / `OPEN` / `FETCH` / `CLOSE`）、`CALL`、`SIGNAL` / `RESIGNAL`、`DECLARE ... HANDLER FOR`を歩く。名前はサーバと同じ規則で解決する。

- `NEW.col` / `OLD.col`はトリガ表の列として型付けする。無い列は1054
- INSERTトリガでの`OLD`、DELETEトリガでの`NEW`は1363
- `OLD`への代入、`NEW`へのBEFORE以外での代入は1362
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

`ORDER BY`、`GROUP BY`、`HAVING`はサーバと同じにSELECTリストの別名を見る（`GROUP BY`では同名の表の列が勝つ）。派生表には別名が要る（1248）。`QUALIFY`は8.4がハイパーグラフオプティマイザ無しで拒むとおりに拒む（6037）。`USING`と`NATURAL`の結合は共通列を1つにまとめる（修飾の無い名前は左側に解決し、`SELECT *`は1回だけ並べる）。表名とビュー名は`lower_case_table_names`の言うとおりに照合し、列名とキー名は大文字小文字を区別しない。書く表をサブクエリが読む`UPDATE` / `DELETE`はサーバが拒むとおりに拒む。表そのものなら1093（`WHERE`・`EXISTS`・`IN`のどれでも）、その表の上のビューなら1443。派生表で包めば実体化されて通り、同じ表からの`INSERT ... SELECT`も通る（実測）。

### ビュー

ビューは、`ALGORITHM=TEMPTABLE`と書かれているか、問い合わせがマージできない形（`GROUP BY`・`HAVING`・`DISTINCT`・`LIMIT`・集合演算・ウィンドウ関数・選択リスト内のサブクエリ）でない限り、読む側の問い合わせにマージされる（サーバ自身の`is_mergeable`）。マージされるビュー経由の書き込みは基底表に届き、それ以外のビューへの書き込みは1288になる。サーバがマージしないビューに付けた`WITH CHECK OPTION`は、サーバが`CREATE`を拒む（1368）のと同じくスキーマの読み込み時に拒む。`CREATE OR REPLACE VIEW`は前の定義を置き換える。`ALTER VIEW`も同じく置き換える（ビューが存在しなければ1146、その名前が表なら1347）。

### 結果集合を返さない文

`SELECT` / `INSERT` / `UPDATE` / `DELETE` / `CALL`のほかに、検査器は次の文を読む（`Exec`で実行するもの）。

- `LOAD DATA [LOCAL] INFILE ... INTO TABLE t`はファイルの行のINSERTである。対象は基底表でなければならず（ビューは1288）、列リストはその表で解決し（`@var`はフィールドを受け取るだけで列には代入しない）、`SET`の代入は式を列に対して型付けし、その中のプレースホルダは列の型を取る。失敗モードはINSERTのもの——埋める列にかかるキー・外部キー・CHECK（1062 / 1452 / 3819）、`IGNORE`が警告に変えること、`REPLACE`が衝突する行を先に消すこと——だが、サーバが変える点が2つある（実測）。`NOT NULL`列のフィールドがNULLになりうることは1048でなく1263（`SET col = NULL`は1048のまま）、列リストから外した`NOT NULL`列は1364でなく型の暗黙の既定値を取る。
- `LOCK TABLES`が名指す表は存在しなければならず（1146。ビューもロックできる）、別名は重複してはならない（1066）。`UNLOCK TABLES`は何も解決しない。どちらにもパラメータは無い。
- `SELECT ... INTO OUTFILE` / `INTO DUMPFILE`は、`INTO`の位置がどちらでも包んでいる`SELECT`と同じく型付けするが、列は返さない。行はサーバ上のファイルに書かれる。末尾の`FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`は`SELECT`の列をそのまま残す。

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
