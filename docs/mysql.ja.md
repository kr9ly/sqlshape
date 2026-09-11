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

文の判定を変えるサーバ変数は、バージョンと並べて1行に1つ宣言する。検査器と本番の接続を同じ設定に揃えるための行である（[checks.ja.md](checks.ja.md#スキーマはサーバの設定を名乗るserver)）。MySQLが読むのは2つ:

- `sql_mode`。カンマ区切りの名前（大文字小文字は問わない）、空文字列、組み合わせモードの`ANSI`と`TRADITIONAL`。組み合わせはサーバと同じに展開する。パーサは字句解析のビット（`ANSI_QUOTES`、`PIPES_AS_CONCAT`、`IGNORE_SPACE`、`NO_BACKSLASH_ESCAPES`、`HIGH_NOT_PRECEDENCE`、`REAL_AS_FLOAT`）を読む。`ONLY_FULL_GROUP_BY`はグループ検査の有無を決める。厳密モード（`STRICT_TRANS_TABLES`か`STRICT_ALL_TABLES`）は文字列関数がNULL可になるかと、`NOT NULL`列への`NULL`が失敗モードになるかを決める。`NO_UNSIGNED_SUBTRACTION`は減算を符号付きにする。残りは実行時にしか効かないので、そのまま受け付ける。
- `lower_case_table_names`。0は表名とビュー名を大文字小文字で区別する（Linuxの既定）、1は小文字にして持つ、2は綴りを保って区別せずに照合する。

宣言が無ければ、Linuxで初期化したままの8.4を仮定する。既定の`sql_mode`（`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`）と`lower_case_table_names = 0`である。

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

失敗モードの名前はMySQLが制約に付ける名前そのものである。主キーは`PRIMARY`、`UNIQUE`キーはキーの名前、外部キーは`CONSTRAINT`名（無ければ`<table>_ibfk_<n>`）、`CHECK`は`CONSTRAINT`名（無ければ`<table>_chk_<n>`）、`NOT NULL`は`<table>.<column>`。エラー番号はMySQLのもの。キーは1062（サーバが自分で番号を振るキーと、NULLが避けるキーは違反しない）、外部キーは1452と1451（親側は`ON DELETE` / `ON UPDATE CASCADE`を追う）、`NOT NULL`は1048、`CHECK`は3819。`INSERT IGNORE`は何にも違反しない。`ON DUPLICATE KEY UPDATE`はINSERTのキー違反を吸収する。`REPLACE`はキーには違反せず、参照している外部キーには違反しうる（1451）。厳密モードでなければ、`NOT NULL`列への`NULL`を拒むのは1行の`INSERT`と`REPLACE`（その`ON DUPLICATE KEY UPDATE`を含む）だけで、複数行、`INSERT ... SELECT`、`UPDATE`は型の暗黙の既定値を警告付きで格納するので、それらには1048を挙げない。`mysql.Violates(err, key)`は実行時のエラーを同じ名前で判定する。

### `One`の証明

`PRIMARY KEY`と列全体にかかる`UNIQUE`キー、`LIMIT 1`、`GROUP BY`の無い集約から証明する。MySQLには部分インデックスが無い。

### グループ化

MySQLは`sql_mode`の`ONLY_FULL_GROUP_BY`の検査を行い、検査器も同じ番号で同じ検査を行う。グループ化または集約する問い合わせでは、SELECTリスト、`HAVING`、`ORDER BY`、ウィンドウの`PARTITION BY` / `ORDER BY`の各式が、`GROUP BY`の式か、集約か、グループ列に関数従属する列だけからできていなければならない（1055。`GROUP BY`が無ければ1140）。サーバが認める従属は検査器が認める従属と同じである。`PRIMARY`か`UNIQUE`キーが既知になった表の全列（NULL可のキー列は、その NULL を退ける条件があるときだけ）、`WHERE`と内部結合の`col = col`と`col = リテラル`、外部結合の`ON`がNULL可側に与えるもの、派生表やビューの本体を出力列を通して。`ROLLUP`はグループ式そのものしか認めない。`HAVING`で集約の外に書く列はSELECTリストの列か別名か`GROUP BY`の列でなければならない（1054）。`DISTINCT`があるとき、SELECTリストに無い`ORDER BY`の式が読めるのはSELECTリストの列だけ（3065）。他で集約していない問い合わせの`ORDER BY`の集約（3029）と集合演算の`ORDER BY`の集約（3028）は拒む。

```sql
SELECT email, count(*) FROM users GROUP BY name
-- Expression #1 of SELECT list is not in GROUP BY clause and contains nonaggregated column
-- 'users.email' which is not functionally dependent on columns in GROUP BY clause; this is
-- incompatible with sql_mode=only_full_group_by (MySQL error 1055)

SELECT name, count(*) FROM users GROUP BY id            -- OK: id は主キー
```

### 名前解決

`ORDER BY`、`GROUP BY`、`HAVING`はサーバと同じにSELECTリストの別名を見る（`GROUP BY`では同名の表の列が勝つ）。派生表には別名が要る（1248）。`QUALIFY`は8.4がハイパーグラフオプティマイザ無しで拒むとおりに拒む（6037）。`USING`と`NATURAL`の結合は共通列を1つにまとめる（修飾の無い名前は左側に解決し、`SELECT *`は1回だけ並べる）。表名とビュー名は`lower_case_table_names`の言うとおりに照合し、列名とキー名は大文字小文字を区別しない。

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
- 制約違反は`*mysql.ConstraintError`として返り、その`Key()`は[上](#制約名と失敗モード)のスキーマ上の名前である。`mysql.Violates(err, key)`で判定する。
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
