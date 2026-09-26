# MySQL

[English](mysql.md)

sqlshapeのうちMySQLに属するものを1枚にまとめる。スキーマが名乗るバージョンとサーバ設定、規則が使う型と制約名、`database/sql`の上のランタイム、マイグレーションコマンド、検査器が何を埋め込みどう検証しているか。規則そのものは[checks.ja.md](checks.ja.md)にあり、どのデータベースでも同じである。規則に出てくる名前・番号・型が、`mysql`を名乗るスキーマではどこから来るのか——それがこのページの中身である。

MySQLで使い始めるとき、列を受けるGo型、接続のしかた、失敗モードの名前を確かめたいときに読む。診断に載ったMySQLのエラー番号の意味は[mysql-errors.ja.md](mysql-errors.ja.md)にある。

- [スキーマがバージョンとサーバ設定を名乗る](#スキーマがバージョンとサーバ設定を名乗る)
- [Go型の表](#go型の表)
- [ランタイム: database/sql](#ランタイム-databasesql): [接続](#接続)、[文の実行](#文の実行)、[サーバの設定の確認](#起動時にサーバの設定を確かめる)
- [制約名と失敗モード](#制約名と失敗モード)（[トリガやルーチンが送出するエラー](#トリガやルーチンが送出するエラー)を含む）
- [規則が使うもの](#規則が使うもの): `One`の証明、[グループ化](#グループ化)
- [MySQLに無いもの](#mysqlに無いもの)
- [マイグレーション](#マイグレーション)
- [検査器が予測するエラー](#検査器が予測するエラー)
- [検査器が埋め込んでいるもの](#検査器が埋め込んでいるもの)

## スキーマがバージョンとサーバ設定を名乗る

```sql
-- sqlshape: mysql 8.4
-- sqlshape: server sql_mode = 'ANSI,STRICT_ALL_TABLES'
-- sqlshape: server lower_case_table_names = 1
CREATE TABLE ...
```

`schema.sql`は、どのバージョンのMySQL向けのスキーマかを宣言する。対応しているのは8.4。宣言すると、MySQL自身の文法がスキーマとすべての文を解析し、MySQLの規則で式に型が付く。値の違う宣言が2つある場合や、`postgres`を同時に名乗る場合はスキーマのエラーになる。

宣言が決めるのは文を判定する規則であって、接続するサーバを制限するものではない。プログラムはgo-sql-driver/mysqlが接続できるサーバ（他のバージョンのMySQL、MariaDBやその他のMySQL互換データベースを含む）の上で動く。ただし判定は8.4の規則に従うので、そのサーバの挙動が違う箇所では、判定がサーバの実際の動きと一致することは保証しない（[README: 制約事項](../README.ja.md#制約事項)）。

文の判定を変えるサーバ変数は、バージョンと並べて1行に1つ宣言する。検査器の前提と本番の接続を同じ設定に揃えるための行である（[checks.ja.md](checks.ja.md#スキーマはサーバの設定を名乗るserver)）。MySQLで読むのは3つ:

- `sql_mode`。カンマ区切りの名前（大文字小文字は問わない）、空文字列、組み合わせモードの`ANSI`と`TRADITIONAL`を受け付け、組み合わせはサーバと同じに展開する。判定を変えるモードは次のとおり。
  - 字句解析のビット（`ANSI_QUOTES`、`PIPES_AS_CONCAT`、`IGNORE_SPACE`、`NO_BACKSLASH_ESCAPES`、`HIGH_NOT_PRECEDENCE`、`REAL_AS_FLOAT`）: パーサがテキストをどう読むか
  - `ONLY_FULL_GROUP_BY`: グループ検査を行うかどうか
  - 厳密モード（`STRICT_TRANS_TABLES`か`STRICT_ALL_TABLES`）: 文字列関数がNULL可になるか、`NOT NULL`列への`NULL`が失敗モードになるか
  - `NO_UNSIGNED_SUBTRACTION`: 符号なし同士の減算が符号付きになる

  残りのモードは実行時にしか効かないので、宣言はそのまま受け付ける。
- `lower_case_table_names`。0は表名とビュー名を大文字小文字で区別する（Linuxの既定）、1は小文字に変換して保持する、2は綴りを保ったまま区別せずに照合する。2は大文字小文字を区別しないファイルシステム（macOSやWindows）専用で、区別するファイルシステムでは`mysqld`が警告を出して0で起動するため、2を宣言したスキーマとはずれが生じる（そのずれは`mysql.Verify`が検出して返す）。
- `max_sp_recursion_depth`。0〜255。0（既定）なら、自分自身を`CALL`するPROCEDUREは呼び出しのたびに1456になる。0より大きければ、呼び出しがどの深さまで達するかは静的に決まらないので、検査器は何も予測しない。

宣言が無ければ、Linuxで初期化したままの8.4を仮定する。すなわち既定の`sql_mode`（`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`）、`lower_case_table_names = 0`、`max_sp_recursion_depth = 0`である。

## Go型の表

go-sql-driver/mysqlが`parseTime=true`で実際に返す型を、動いているサーバと照合して表にしてある。整数は`int64`（`BIGINT UNSIGNED`は`uint64`）、`DECIMAL`はその文字列、時刻型は`time.Time`か文字列、バイナリ文字列とJSONは`[]byte`で届く。比較と論理演算子の結果は`bigint(1)`で、`bool`で受けられる。`TINYINT(1)`も同じ。

| MySQL | Go |
|---|---|
| `TINYINT` / `SMALLINT` / `MEDIUMINT` / `INT` / `YEAR` | `int64` / `int32` / `int`（`UNSIGNED`なら`uint64` / `uint32` / `uint`も）。`TINYINT(1)`は`bool`も |
| `BIGINT` | `int64` / `int`（`UNSIGNED`なら`uint64` / `int64`）。`bigint(1)`は`bool`も |
| `DECIMAL` | `string` |
| `FLOAT` | `float32` / `float64` |
| `DOUBLE` | `float64` |
| `BIT` | `[]byte` |
| `CHAR` / `VARCHAR` / `TEXT` / `ENUM` / `SET` | `string` / `[]byte` |
| `BINARY` / `VARBINARY` / `BLOB` | `[]byte` |
| `JSON` | `[]byte` / `string` |
| `DATE` / `DATETIME` / `TIMESTAMP` | `time.Time` / `string` |
| `TIME` | `string` |
| `ENUM`列、キーの同一性 | Goのnamed type（[型に意味を持たせる](checks.ja.md#型に意味を持たせる)）。`ENUM`は`CHECK (col IN (...))`と同じ値集合 |

MySQLには`// sqlshape: type`の束縛は無い。束縛先となる名前付きの型がMySQLに無いためである。

### 列の型と違う型で届く結果

宣言された列の型と、結果集合で実際に届く型が違うケースがいくつかある。

- ウィンドウ関数の整数（`MIN(id) OVER ()`、`FIRST_VALUE`、`NTH_VALUE`、`LAG`、`LEAD`など）は、ウィンドウの一時表を経由してクライアントに届くため型が広がる。`INT`と`BIGINT`は`BIGINT`に、`TINYINT` / `SMALLINT` / `MEDIUMINT`は`INT`になり、符号は保たれ、`YEAR`は`INT UNSIGNED`になる。ふつうの集約は列の型を保つ（`SMALLINT`の`MIN(small)`は`SMALLINT`）。`ROLLUP`のグループ列も型を保つが、実行計画がたまたまグループ化を一時表に落とした場合だけは広がる——この場合は検査器は予測しない
- `ROLLUP`の下では、グループ列を読む結果列がnullableになる（超集約行でNULLになるため）。選択リストの定数はならない
- `DATE'...'` / `TIME'...'` / `TIMESTAMP'...'`リテラルはその型を持ち（小数桁は書かれた桁数）、NULLにならない。サーバがちょうどその型として読めないもの——時刻部のある`DATE`、時刻部の無い`TIMESTAMP`、`NO_ZERO_IN_DATE`下のゼロの月、`-14:00`〜`+14:00`の外の時差——は文のエラー1525になる。`<=>`も被演算子にかかわらずNULLにならない
- `USER()`、`CURRENT_USER()`、`DATABASE()`、`SCHEMA()`、`VERSION()`、`CURRENT_ROLE()`はバイナリ文字列ではなく文字列（utf8mb3）である

## ランタイム: database/sql

```
$ go get github.com/kr9ly/sqlshape/v2         # Query / One: 検査器が読む宣言
$ go get github.com/kr9ly/sqlshape/mysql/v2   # database/sql と go-sql-driver/mysql の上で実行する
```

`github.com/kr9ly/sqlshape/mysql/v2`は、宣言した文を`database/sql`とgo-sql-driver/mysqlを通して実行する。`mysql.DB`は`*sql.DB`、`*sql.Tx`、`*sql.Conn`のどれでもよい。テンプレートの`{{.X}}`はワイヤ上ではMySQLの位置指定`?`になり、引数はプレースホルダの出現順に並べ直される（2回使ったパラメータは2回送る）。

### 接続

DSNには`parseTime=true`が必須である。上のGo型の表はこれを前提にしている。無いとドライバは`DATE`・`DATETIME`・`TIMESTAMP`の列をバイト列で返し、`time.Time`のフィールドでは受けられない。

```go
db, err := sql.Open("mysql", "app:secret@tcp(localhost:3306)/app?parseTime=true")
```

既に同じ値を持つ行へ書く`ExecOne`のUPDATEもその行を数えてほしいなら、`clientFoundRows=true`も付ける（[下](#文の実行)）。

### 文の実行

```go
for o, err := range mysql.Run(ctx, db, ListOrders, p) { ... }   // iter.Seq2[Order, error]、逐次読み出し
orders, err := mysql.Collect(ctx, db, ListOrders, p)            // []Order
first, err  := mysql.First(ctx, db, ListOrders, p)              // 無ければ ErrNoRows（sql.ErrNoRows）
res, err    := mysql.Exec(ctx, db, MarkPaid, p)                 // sql.Result

u, err     := mysql.Get(ctx, db, UserByEmail, p)                // One: 無ければ ErrNoRows
u, ok, err := mysql.Find(ctx, db, UserByEmail, p)               // One: ok が有無
res, err   := mysql.ExecOne(ctx, db, MarkOrderPaid, p)          // One: 1行も触らなければ ErrNoRows
```

どのランタイムでも同じこと（行のマッピング、`One`、検査済みのSQLだけが走る保証、expect行の名前で返るエラー）は[runtime.ja.md](runtime.ja.md)にある。MySQL固有のもの:

- 受け型はドライバのもので、上の表のとおり。`DECIMAL`はその文字列で届くので、自前のmoney型で包める。
- 制約違反は`*mysql.ConstraintError`として返り、その`Key`は[下](#制約名と失敗モード)のスキーマ上の名前である。`mysql.Violates(err, key)`で判定する。トリガやルーチン自身の`SIGNAL`も同じように返り、キーは[下](#トリガやルーチンが送出するエラー)のとおりである。
- `ExecOne`は`RowsAffected`で判定する。MySQLは変更のあった行を数えるので、既に同じ値を持つ行へのUPDATEは、DSNに`clientFoundRows=true`が無いと`ErrNoRows`になる。`INSERT ... ON DUPLICATE KEY UPDATE`と`REPLACE`がその1行について報告する0・1・2行は、どれも1行とみなす。

### 起動時にサーバの設定を確かめる

`mysql.Verify(ctx, db, schemaSQL)`は、接続のセッションの`@@sql_mode`とサーバの`lower_case_table_names`を読み、スキーマの宣言（無ければ初期化したままの8.4の既定値）と違えばエラーを返す。DSNの`sql_mode=...`、プールのセッション初期化、別の設定で立てたサーバは、検査器が判定に使わなかった規則で文を走らせることになるからである。8.4が定義しないモード名（他のバージョンのMySQLやMariaDBでは使われうる）は、報告せずに照合から外す。エラーになるのは8.4が知っているモードの違いだけで、そのメッセージは外したモードの名前も挙げる。プールを開いた直後に1回呼ぶ。

```go
if err := mysql.Verify(ctx, db, schemaSQL); err != nil { ... }
```

## 制約名と失敗モード

失敗モードの名前はMySQLが制約に付ける名前そのもので、番号はMySQLのエラー番号である。

| 制約 | 名前 | エラー |
|---|---|---|
| 主キー | `PRIMARY` | 1062 |
| `UNIQUE`キー | キーの名前 | 1062（サーバが自分で番号を振るキーと、NULLが避けるキーは違反しない。プレフィックスキー`UNIQUE (c(10))`と式キー`UNIQUE ((n * 2))`は読む列を通して違反する） |
| 外部キー | `CONSTRAINT`名。無ければ`<table>_ibfk_<n>` | 子側は1452、親側は1451（`ON DELETE` / `ON UPDATE CASCADE`を追う） |
| `CHECK` | `CONSTRAINT`名。無ければ`<table>_chk_<n>` | 3819 |
| `NOT NULL` | `<table>.<column>` | 1048 |
| ビューの`WITH CHECK OPTION` | ビュー自身の名前（MySQLのエラーメッセージはビューを名指しする。PostgreSQLの44000は何も名指ししない） | 1369 |
| ビュー経由のINSERTが代入しない、既定値の無い基底列 | ビュー自身の名前（サーバのメッセージは列でなくビューを名指しする。ビューがその列を見せているかどうかは関係ない） | 1423 |

どの違反がありうるかは、文の形によっても変わる:

- `INSERT IGNORE`は何にも違反しない
- `ON DUPLICATE KEY UPDATE`はINSERTのキー違反を吸収する
- `REPLACE`はキーには違反せず、参照している外部キーには違反しうる（1451）
- 厳密モードでなければ、`NOT NULL`列への`NULL`を拒むのは1行の`INSERT`と`REPLACE`（その`ON DUPLICATE KEY UPDATE`を含む）だけ。複数行、`INSERT ... SELECT`、`UPDATE`は型の暗黙の既定値を警告付きで格納するので、これらには1048を挙げない
- `UPDATE IGNORE`は、キーや`NOT NULL`の違反と同じように`WITH CHECK OPTION`ビューの1369も吸収する。トリガ自身の`SIGNAL`はどの`IGNORE`も吸収しないのと対照的である

実行時のエラーは`mysql.Violates(err, key)`が同じ名前で判定する。`Run` / `Exec`の外で走らせた文のエラーは、`mysql.WrapError(err)`で先に同じ包み方にしておく。

列が決して格納できないリテラルは、失敗モードではなく文自身のエラーになる（[mysql-errors.ja.md](mysql-errors.ja.md#列に格納する値)）。

失敗モードにしないもの: 長さ・範囲・`ENUM`値の切り詰め（1265 / 1406 / 1366 / 1264）。これらは型の側の性質（パラメータのGo型、リテラル自身の値集合）なので、型検査の側で捕まえる。

### トリガやルーチンが送出するエラー

トリガとストアドルーチンは、自分の失敗モードを、発火させた文・呼び出した文に持ち込む。表への文は、その表のトリガのうち該当する事象のものの失敗モードを引き継ぐ（INSERT / UPDATE / DELETE）。`REPLACE`はINSERTとDELETEのトリガを、`ON DUPLICATE KEY UPDATE`はINSERTとUPDATEのトリガを発火させる。トリガ・ルーチンの本体自身の書き込みは、その書き込み自身の失敗モード——スキーマの制約、その書き込みが発火させるトリガの失敗モード（再帰は打ち切り）——を本体の失敗モードに持ち込む。

`SIGNAL`のキーは、[ランタイム](#文の実行)がエラーを読み戻すのと同じ規則で決まる:

- `MYSQL_ERRNO`を設定した`SIGNAL`は、その番号の10進表記がキーになる。組み込みの番号を名乗っても同じで、SQLSTATE `'23000'`の下で`SET MYSQL_ERRNO = 1062`とすればキーは`1062`になる。メッセージに書かれていないキー名を探しにいくことはない
- `MYSQL_ERRNO`を設定しない`SIGNAL`は、SQLSTATEがキーになる
- SQLSTATEクラス`01`は警告なので失敗モードにならない。未処理のクラス`02`は1643、それ以外の未処理は1644になる

名前付き`CONDITION`はその値に解決する。値の無い`RESIGNAL`は、最も内側の`HANDLER`が処理中のものをそのまま再送する。`DECLARE ... HANDLER FOR`は、そのブロック内の一致する失敗モードを吸収する（`SQLEXCEPTION`はクラス`01`と`02`以外の全部、`SQLWARNING` / `NOT FOUND`はそのクラス、SQLSTATEや番号そのものは一致するもの）。`INSERT` / `UPDATE IGNORE`はトリガのSIGNALを何も吸収しない（文はそれでも失敗する）。`SELECT ... INTO`は、`One`が使うのと同じ証明で「多くとも1行」と示せない限り1172を持つ（1行も無い場合はNOT FOUNDの警告で、失敗にはならない）。

`CREATE TRIGGER` / `FUNCTION` / `PROCEDURE`の上に書く`-- sqlshape: error <key> = <Name>`は、PostgreSQLの同じ注釈（[checks.ja.md](checks.ja.md#トリガーが送出するエラーには名前を付ける)）と同じもので、`<key>`にNameを与える。プログラムはそれを`sqlshape.Error(<key>)`で写し取り、vetがスキーマと両方向に照合する。expect行ではNameとキーのどちらで綴っても同じである。実行時に`mysql.Violates`が比べるのはコード（`30001`）で、`sqlshape.Error("30001")`から作った`sqlshape.Failure`はそのコードだけを運んでいる。

本体の作成時にサーバが拒むものと、ルーチン呼び出しが持ちうる番号は[mysql-errors.ja.md](mysql-errors.ja.md#トリガとストアドルーチンとイベント)にある。

### WITH CHECK OPTIONのビューを通した書き込み

`WITH CHECK OPTION`を宣言したビュー経由の書き込みは、[checks.ja.md](checks.ja.md#宣言の仕組み)のとおり基底表の`require pinned(<列>)`を満たせる（`sqlshape check`は`ok(view)`と出す）。裏付けは上の1369そのもので、CHECK OPTIONを宣言していないビューはこの経路を持たない（そこを経由した書き込みは行をビューから静かに外すだけで、サーバは拒まない）。MySQL固有の点:

- `WITH CHECK OPTION`とだけ書けばCASCADEDなので、下位の全ビューのWHEREも数える（結合の向こうにあっても）。`WITH LOCAL CHECK OPTION`はそのビュー自身で止まるが、自前のCHECK OPTIONを宣言した下位ビューはサーバが検査し続けるので数える
- `UPDATE`にも`INSERT` / `REPLACE`にも効く。`DELETE`はCHECK OPTIONを検査しない
- 下位のビューが自分のCHECK OPTIONを宣言していれば、書き込む先のビューに宣言が無くても検査され、1369は書き込む先のビューを名指しする

## 規則が使うもの

[checks.ja.md](checks.ja.md)の規則は、MySQLのスキーマにもそのまま適用される。変わるのは、判定を下すアナライザーがPostgreSQL用からMySQL用になることだけである。結果列・パラメータとGo型の対応、NULLの扱い、型の意味、失敗モード、`One`の証明、第2部の宣言のすべて（`visible where`、`pinned`、`via view`、`EXISTS`、`aggregate`、`transitions`、`never`、`paired`、`single`、`sensitive`、`context`）が同じに働く。診断にはMySQLのエラー番号とメッセージ文がそのまま載る（`Unknown column 'nope' in 'field list' (MySQL error 1054)`）。`{{.X}}`はワイヤ上では`?`になる。

### `One`の証明

`PRIMARY KEY`と列全体にかかる`UNIQUE`キー、`LIMIT 1`、`GROUP BY`の無い集約から証明する。集約はどこに置かれていてもよい（`COALESCE(SUM(total), 0)`も`SUM(total)`と同じく1行。サブクエリが自分の列だけで集約するものはそのサブクエリの証明になる）。MySQLには部分インデックスが無い。

### グループ化

MySQLは`sql_mode`の`ONLY_FULL_GROUP_BY`で検査を行い、検査器も同じ検査を同じ番号で行う。グループ化または集約する問い合わせでは、SELECTリスト、`HAVING`、`ORDER BY`、ウィンドウの`PARTITION BY` / `ORDER BY`の各式が、`GROUP BY`の式か、集約か、グループ列に関数従属する列だけからできていなければならない（1055。`GROUP BY`が無ければ1140）。サーバが認める従属は検査器が認める従属と同じである:

- `PRIMARY`か`UNIQUE`キーが既知になった表の全列（NULL可のキー列は、そのNULLを退ける条件があるときだけ）
- `WHERE`と内部結合の`col = col`と`col = リテラル`
- 外部結合の`ON`がNULL可側に与えるもの
- 派生表やビューの本体が出力列を通して与えるもの
- `ROLLUP`の下ではグループ式そのものだけ

隣り合う検査も一緒に行う。

| 規則 | エラー |
|---|---|
| `HAVING`で集約の外に書く列は、SELECTリストの列か別名か`GROUP BY`の列でなければならない | 1054 |
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

### 1文で2つの書き込み

次の2つの形は、スキーマの規約（`require`、`visible where`）を判定するとき、書き込みを2つ持つ文として扱われる:

| 文 | 記録する書き込み | 理由 |
|---|---|---|
| `INSERT ... ON DUPLICATE KEY UPDATE` | INSERTと、枝が代入する列へのUPDATE | 枝は既存行を動かす |
| `REPLACE` | INSERTとDELETE | キーが衝突すると既存行を先に削除する（その表の`AFTER DELETE`トリガが発火する） |

## MySQLに無いもの

MySQLに無いものはMySQLでは検査しない: `Batch`、`Copy`、`MatView`、PL/pgSQL、ドメイン、複合型と配列、`-schemas`（MySQLのスキーマは1つのデータベース）、`// sqlshape: type`、マイグレーションのseed表。

## マイグレーション

`sqlshape diff`、`apply`、`verify-schema`は、MySQLのスキーマにもPostgreSQLと同じに働く。データベースと`schema.sql`の差分がDDLになり、DDLは到達する状態で検査され、それから実行される。両側はサーバ自身の`SHOW CREATE TABLE` / `SHOW CREATE VIEW`で読み、目標側は`-db`のサーバ上の一時データベースで正準化する。MySQLのDDLは暗黙にコミットされるので、DDLは1文ずつ実行する。何を比較するか、`enum`宣言の形、必要な環境は[migrations.ja.md](migrations.ja.md#mysql)にある。

## 検査器が予測するエラー

MySQLのエラー番号が載った診断には2種類ある。エラーは確実に起きるもので、文を実行するたびに同じように失敗するため、vetが報告し、文を直す必要がある。失敗モードは実行時に一部の行や値で起こりうるもので、文のexpect行に書き、戻ってきたときは`mysql.Violates`が一致する。上の制約名は失敗モードである。それ以外に検査器が文の形から予測するもの——列に格納する値、定数の式・変換・関数引数、空間型の値、トリガとルーチンの本体、名前解決、ビュー、結果集合を返さない文——は、[mysql-errors.ja.md](mysql-errors.ja.md)に話題ごとにまとめ、エラー番号の索引を付けてある。

## 検査器が埋め込んでいるもの

パーサと字句解析器はMySQL 8.4自身のもので、サーバのソースから切り出して（文法はアクションを剥がし、字句解析器はそのまま）WebAssemblyにしてある。関数の表も同じソースから読む。アナライザーはその構文木の上に建てたpure Goの実装で、検査時にMySQLへ接続することはない。MySQLのパーサを抱えているため、検査器のMySQL側（`check/mysql`）はGNU General Public License v2である。リンクされるのは`sqlshape`バイナリだけで、利用者のプログラムには入らない。

判定は動いている`mysqld`と照合してある。型を付けた5,033文（組み込み関数の全部について引数の組み合わせごとの結果型）、エラーになる65文、`ONLY_FULL_GROUP_BY`検査の376文、既定でない`sql_mode`の下の22文と6つの書き込みが8.4と一致する。このページと[mysql-errors.ja.md](mysql-errors.ja.md)に書いたサーバの挙動は、すべて`mysqld` 8.4に対して実測したものである。

MySQL自身のテストコーパスも並走させてある。`mysql-test/t`の1,281ファイル（約137,000文）を`mysqld`とアナライザーの両方に流し、SELECTの列は名前・型ファミリー・nullabilityで、エラーは番号で突き合わせる。両者が一致しないと分かっている文は2,525件。1件ずつベースライン（`check/mysql/internal/analyze/testdata/corpus_baseline.txt`）に固定してあり、新しい不一致が出るとビルドが落ちる。各行には不一致の種別が付いている: アナライザーが予測しないエラー、別の番号で予測するエラー、型の違う列、ローダーが扱わないスキーマ構文。このファイルが、MySQLアナライザーがまだサーバと同じには判定できないものの正直な一覧である。

## License

The MySQL side of the checker (`check/mysql`) carries MySQL's own parser and is under the GNU
General Public License v2, see [cmd/sqlshape/LICENSE](../cmd/sqlshape/LICENSE); it is linked into
the `sqlshape` binary, a development tool, and nothing under it is linked into your program. The
runtime (`mysql`) is Apache License 2.0, like the declarations.
