# マイグレーション

[English](migrations.md)

`schema.sql`がデータベースの唯一の定義であり、マイグレーションファイルは書かない。`sqlshape`が稼働中のデータベースと`schema.sql`を比較してDDLを生成し、そのDDLを当てれば本当に`schema.sql`の状態になることを確認してから実行する。コマンドはPostgreSQLとMySQLの両方で使える。どちらに話すかはスキーマの宣言が決め、MySQLで違うところは下の[MySQL](#mysql)の節にまとめてある。

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

`schema.sql`の変更をデータベースに出す前に読むページである。初めてなら[必要な環境](#必要な環境)とコマンドの節（[diff](#diff)、[apply](#apply)、[verify-schema](#verify-schema)）で足りる。同じ変更を複数の環境に当てるなら、[複数の環境とロールバック](#複数の環境とロールバック)に進め方がある。

## 必要な環境

### PostgreSQL

- `pg_dump`。`PATH`にあるか、`$SQLSHAPE_PG_DUMP`で指定する。メジャーバージョンは対象データベース以上であること。データベースを使わない`diff -from`でも要る。比較に使う専用のPostgreSQLもこれで読み戻すからである。
- 比較のために、`sqlshape`はスキーマが宣言したバージョン（`-- sqlshape: postgres 17`）の専用PostgreSQLで`schema.sql`を実行する。利用者のデータベースはこの用途には使わない。この専用サーバは初回にMaven Central（`io.zonky.test`のembedded-postgresバイナリ）からダウンロードし、`~/.cache/sqlshape/pg-<リリース>`にキャッシュする（`$SQLSHAPE_PG_CACHE`でキャッシュの場所を変えられる）。初回はネットワーク接続が要り、ダウンロードに数秒かかる。以後は0.25秒程度で起動する。
- ミラーを指定する設定は無い。ネットワークに出られないマシン（閉じたCIランナーなど）では、一度コマンドを実行した同じOS・アーキテクチャのマシンから、中身の揃った`pg-<リリース>`ディレクトリを`$SQLSHAPE_PG_CACHE`の下にコピーしておく。そうすればダウンロードは起きない。

### MySQL

- `-db`では、接続ユーザーに`CREATE DATABASE`と`DROP DATABASE`の権限が要る。コマンドは実行中だけそのサーバに一時データベース（`sqlshape_scratch_<乱数>`）を作り、終われば落とす。
- サーバの`lower_case_table_names`はスキーマが宣言した値（宣言が無ければ0）でなければならず、違えばコマンドは止まる。
- `-from`（テキスト同士、サーバ無し）では、`PATH`の`mysqld`（ディストリビューションのパッケージ、サーバtarballの`bin/`、`nix-shell -p mysql84`）をスキーマの宣言した設定で起こす。

### 接続文字列

`-db`にはドライバ自身の形式の接続文字列を渡す:

```
# PostgreSQL: libpq の接続文字列（同じ文字列を pg_dump にも渡す）
$ sqlshape verify-schema -db "postgres://app:secret@db.example.com:5432/app?sslmode=require"

# MySQL: go-sql-driver/mysql の DSN。URL ではない
$ sqlshape verify-schema -db "app:secret@tcp(db.example.com:3306)/app"
```

### スキーマとサーバのバージョン

`-schema PATH`で`schema.sql`、または`*.sql`を名前順に適用する`schema/`ディレクトリを指定できる。既定は作業ディレクトリから上に辿って最初に見つかるもの。

接続先のデータベースが宣言と違うメジャーバージョン（MySQLでは違うバージョン）で動いているときは、その旨をstderrに出して続行する。DDLは宣言したバージョンの規則で判定される。この判定を通っても、そのサーバでDDLが同じように振る舞うことは保証しない。そこで失敗することも、違う動きをすることもある。

## diff

`sqlshape diff -db DSN`は、データベースを`schema.sql`の状態にするDDLを依存順に出力する。drop（依存している側から）、rename、alter、add、最後にseedの`MERGE`の順である。これは提案であって、そのまま実行するものではない。手元のデータに対して順序や形が合わないなら編集する。`ALTER COLUMN ... TYPE`には`USING`が必要になることがあり、backfillは2つのステップの間に入れたいことがある。

`-from other.sql`を指定すると、現在の状態をデータベースではなくスキーマのテキスト（たとえば前リリースの`schema.sql`）から取り、そこから`schema.sql`に至るDDLを出力する。`-packages ./...`を指定すると、Goの文を現在のスキーマに対して索引し、DDLが落とすか型を変える列を読んでいる文をDDLの中にコメントとして列挙する。

## diffだけでは決められないことを宣言する

2つのスキーマの差分を見ても決められないことがある。renameなのかdropしてaddしたのか、削除するenumのラベルを持つ行は何に置き換えるのか、新しい`NOT NULL`列に既存の行では何を入れるのか、である。これらは`schema.sql`に`-- @migrate`行で宣言する:

```sql
-- @migrate rename orders.state -> orders.status
-- @migrate drop orders.legacy
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

`rename 旧 -> 新`は列かテーブルの名前を変える。左が現在の名前、右が新しい名前である。`drop`はその列（またはテーブル）を落としてよいという宣言。`enum`は消すラベルと、その行が移るラベルを書く。`backfill テーブル.列 = 式 [where 条件]`は、そのテーブルを変えるステップの中で列に値を入れる。このステップで追加するか定義が変わる列を埋めるか、テーブルに加わる`CHECK`、`UNIQUE`、外部キーに先立って行を直す。宣言は1行で完結させる。同じ行の後ろに説明などを続けてはいけない。

宣言はデータベースの現状から`schema.sql`への1ステップを記述するもので、すべてのデータベースに適用したら消す。`drop`も`rename`も宣言されていないのに消えるテーブルや列があればエラーになる（DDLは出力される）。データが失われる変更は必ず宣言を要求する、ということである。逆に、差分と食い違う宣言もエラーになるので、古い宣言は検出される。元が存在しないrename、まだ残っている列のdrop、変わらないenum、そしてステップがテーブルを何も変えないbackfill（列が既にデータベースにあり、テーブルの他の列・制約・インデックス・トリガも`schema.sql`のとおりにある）である。ステップを終えた後に残ったbackfillは、放っておけば以後の計画のたびに実行され、その時点で行が持っている値を上書きしてしまう。そのため報告し、DDLには含めない。後のステップが同じテーブルをまた変えるときは、この方法では検出されないので、ステップを終えたbackfillは消しておくこと。

### 宣言が生むDDL

renameは`RENAME TO` / `RENAME COLUMN`になる。renameした列を含む制約や外部キーは`DROP`して`ADD`する形で出る。正しい手順だが、制約の再検証が走る。enumのラベル削除は型の作り直しになる（PostgreSQLには`DROP VALUE`が無い）。旧型をrenameし、新型を作り、その型を使うすべての列に`ALTER COLUMN ... TYPE ... USING CASE ...`を当て、それらのテーブルに依存するビューを作り直し、旧型を落とす。enumの配列を持つ列があれば問題として報告する。`backfill`は移行後のスキーマで型検査され、列ができた時点で`UPDATE`として出力される。新しい列なら追加の後、`NOT NULL`になる列ならその前、テーブルに加わる`CHECK`、`UNIQUE`、外部キーがあればそれらの前である。

### パーティション

パーティションもほかと同じスキーマである:

| 変更 | DDL |
|---|---|
| 新しいパーティション表 | `CREATE TABLE ... PARTITION BY ...`。子ができてからATTACHする |
| 既存の親への子の追加 | `CREATE TABLE ... PARTITION OF ... FOR VALUES ...` |
| 消える子 | `DROP TABLE`。行を持っていくので、その表の`-- @migrate drop`の宣言が要る |
| 表が子になる・子でなくなる | `ATTACH PARTITION` / `DETACH PARTITION` |
| 境界の変更 | DETACHしてATTACHする。計画中の全DETACHを全ATTACHより前に出すので、動く境界が隣と重なることはない |
| 行を持つ表を後からパーティション化する、パーティション化をやめる、別のキーや戦略に移す | 表をRENAMEし、宣言された形をパーティションごと新しく作り、全行を親経由でINSERTしてPostgreSQLに配らせ、コメント・トリガ・ポリシー・RULE・被参照の外部キー・sequence（bigserialは同じsequenceを使い続け、identity列は最大値の続きから）を新しい表に付け直し、旧表を落とす |

子の列は親に従うので、計画が子の列を直接ALTERすることはない。どのパーティションにも入らない行はサーバのエラーになるので、データを覆っていない`schema.sql`は何も失わずに`apply`を止める。

### 注記として出る変更

ドメインの基底型、範囲型のサブタイプ、`INHERITS`、`OF type`の変更はDDLとしては出さず、`-- `で始まる注記として出力する。人が手順を決める必要がある変更である。

列の`ALTER COLUMN ... TYPE`も、新しい型が`numeric`の精度・スケール、`varchar(n)` / `char(n)`の長さ、`time` / `timestamp`系の秒以下の精度、`bit(n)`の長さのいずれかを狭める場合は同様に注記が付く。同じ基底型どうしのALTERはPostgreSQLが`USING`無しでそのまま実行してしまい、既存の値は無警告で丸められる、あるいは切り詰められる。注記はあくまで警告であってブロックはしないので、無編集のまま適用すればやはり値は丸められる、または切り詰められる。適用前に`USING`を足すか、先にデータを直しておくこと。

## apply

`sqlshape apply -db DSN up.sql`はDDLファイルを受け取り（手で編集したものでもよい）、何かを実行する前にその結果を確認する。データベースの現在のスキーマにこのDDLを当てたものが、`schema.sql`と一致しなければならない。列の順序の違いだけは許容して注記する（PostgreSQLは列を途中に挿入できないので、列を落として追加し直すと順序は永久にずれる）。それ以外の違いがあれば拒否する。確認が通れば、DDLを1つのトランザクションで実行する。途中の文が失敗すれば全体が戻る。ファイルに`BEGIN`や`COMMIT`は書かないこと。中に`COMMIT`があると、後の文が失敗してもそれより前の分は確定したまま残る。

- `-packages ./...`を付けると、DDLが落とすか型を変える列にまだ依存しているGoの文があれば拒否する。`-force`で無視して実行できる。
- `-dry-run`は確認だけで止まる。
- `-no-transaction`はapply自身のトランザクションで包まずにファイルを送る。ファイルは1回の要求として送られるので、PostgreSQLはそれでも全体を1つの暗黙のトランザクションとして実行する。

`CREATE INDEX CONCURRENTLY`のようにトランザクションブロックの中で実行できない文は、`-no-transaction`の有無にかかわらず`apply`を通らない。実行前の確認がファイルをトランザクションの中で流すので、そこで拒否される。そうした文だけ先に手で実行し、残りを`apply`する。確認はそのインデックスが既にある状態から行われる。

## verify-schema

`sqlshape verify-schema -db DSN`は、データベースが`schema.sql`と違う箇所を列挙する。ドリフト、手で当てたマイグレーション、追従が遅れた環境を見つけるために使う。違いがあれば終了コード1、一致していれば0を返す。

## 終了コード

- `diff`: DDLを出力したら0（空でないDDLも含む）。`drop`や`rename`の宣言なしに消えるテーブルや列があるとき、または差分と食い違う宣言があるときは1（DDLは出力される）。
- `verify-schema`: データベースが`schema.sql`と一致すれば0、違えば1。
- `apply`: 適用したら0。何も実行する前に拒否したら1。実行中に文が失敗したら2（PostgreSQLではDDLは戻り、MySQLではそれより前の文が適用済みのまま残る）。
- 全コマンド共通: 使い方か環境のエラーは2。

## 複数の環境とロールバック

`sqlshape`は何を適用したかをデータベースに記録しない。自前のテーブルは作らず、MySQLの一時データベースもコマンドの実行中にしか存在しない。各環境がどの状態にあるかは、その環境に対する`verify-schema`の結果で確かめる。

### 1つの変更を複数のデータベースに当てる

`-- @migrate`の宣言は、ある状態からの1ステップを記述する。すべての環境がそのステップを終えるまで`schema.sql`に残し、すべての環境で`verify-schema`が0を返したら消す。残している間は、既にステップを終えた環境への`diff -db`がその宣言でエラー（終了コード1）になる。遅れている環境にはまだ宣言が要るので、これは想定どおりである。

PostgreSQLでは、DDLをリリースごとに1本作り、同じファイルをすべての環境に当てられる。前リリースの`schema.sql`を`diff -from`に渡すか、代表の環境に`diff -db`をかけて作り、一度だけレビューと編集をする。条件は、当てる環境が前リリースの`schema.sql`に対して`verify-schema`で0を返すことである。それ以外の状態の環境（ドリフトしたもの、既にステップを終えたもの）には、`apply`が何も実行する前に拒否する。

MySQLでは、同じファイルが使えるのは設定の同じサーバの間だけである。DDLには、それを作ったサーバが補った値（新しい表の既定の文字集合と照合など）がそのまま書かれ、`-from`は`PATH`の`mysqld`の既定値で補う。設定の違うサーバには、`apply`が実行前にそのファイルを拒否する。その環境のDDLは、その環境に対する`diff -db`で作る。

どの場合も、`apply`が実行前に行う確認が安全弁になる。このデータベースを`schema.sql`の状態にしないファイルは実行されない。

### applyは1つずつ実行する

`apply`はロックを取らない。1つのデータベースに同時に2つ走らせると、どちらも確認を通り、後の方がサーバのエラーで失敗する（PostgreSQLでは戻る）。データだけを変えるファイルなら2回実行される。マイグレーションは、データベースごとに1つのCIジョブにするなどして直列にすること。

### 2回走っても害のないbackfill

ステップを終えた後に`schema.sql`に残ったbackfillは、そのテーブルがステップの後のままである限りエラーになるので、`diff`が再び出力することはない。ただし、後のステップが同じテーブルを変えると、そのステップの一部として扱われる。値の入った行には触れない条件（`where status is null`）を付けて書いておくこと。まだ値の無い行だけを更新する`UPDATE`なら、後の計画から、あるいはDDLファイルを手でもう一度実行して2回走っても害が無い。

### ロールバック

downマイグレーションは無い。PostgreSQLで失敗した`apply`は既に戻っている。MySQLでは成功した文の後で止まるので、その状態から`sqlshape diff`をかければ残りが出る。

適用済みのステップを取り消すには、前の状態に向かって前進させる。前の`schema.sql`に戻し、通常のステップと同じように`diff`、レビュー、`apply`を行う。戻すステップには戻すステップ自身の宣言が要る（逆向きのrename、前の版に無い列の`drop`）。ステップで落としたものは戻らず、seed済みテーブルの`MERGE`は前の版に無い行を削除する。`apply -dry-run`で、何かを実行する前にそのファイルが前の状態に至るかを確かめられる。

## 何を比較するか

PostgreSQLでは両側とも`pg_dump`の出力として読むので、比較されるのは`schema.sql`の書き方ではなく、PostgreSQLが実際に保持している定義である。`'x'`と`'x'::text`、`IN (...)`と`= ANY (ARRAY[...])`のような書き方の違いは差分にならない。`-- sqlshape:`のディレクティブはデータベースの状態ではないので比較対象に入らない。

比較はオブジェクト単位で行う。テーブル、列、制約、インデックス、ビュー、関数、型、ドメイン、enum、トリガー、ルール、ポリシー、行セキュリティの設定、シーケンス、拡張、コメントが対象で、seed済みテーブルは行単位でも比較する。所有者、権限（`GRANT`）、テーブルスペースは比較しない。

## seed済みテーブル（PostgreSQL）

行を`schema.sql`に普通の`INSERT ... VALUES`で書いておくテーブルをseed済みテーブルと呼ぶ。その行はスキーマの一部として扱われる。

```sql
CREATE TABLE order_statuses (
    code       text PRIMARY KEY,
    label      text NOT NULL,
    sort_order integer NOT NULL
);
INSERT INTO order_statuses (code, label, sort_order) VALUES
    ('pending',   'Awaiting payment', 10),
    ('paid',      'Paid',             20),
    ('shipped',   'Shipped',          30),
    ('cancelled', 'Cancelled',        90);
```

このINSERTは冪等でなければならない。`schema.sql`を2回適用しても同じ意味になるように、という条件である。具体的には、行を識別するキー（主キー、またはNOT NULL列の上の部分でない一意制約）をすべての行が定数で与えていること、値がすべて定数式であること（volatileやstableな関数、サブクエリ、`DEFAULT`は不可）、`ON CONFLICT`が無いこと、同じキーの行が2つ無いこと。違反はスキーマの問題として報告される。INSERT自体も他の文と同じく型検査される。

比較のたびに、宣言された行はデータベースから読み戻されてキーで比較される。`schema.sql`で行を追加・変更・削除すれば、列の変更と同じように`diff`と`verify-schema`に現れる。生成されるDDLの最後には、内容が違うテーブルごとに`MERGE`が1文ずつ付く。宣言から消えた行はこのMERGEで削除される。外部キーでつながったseed済みテーブル同士は、親から順にMERGEし、削除は子から順に行う。INSERTの上に`-- sqlshape: seed`と書くとseedは追加のみになり、宣言に無い行もテーブルに残る。

INSERTそのものを消すと、そのテーブルは普通のテーブルに戻る。行はスキーマの一部ではなくなるので、`diff`と`verify-schema`は行を読まなくなり、生成されるDDLも行を削除しない。データベースの行はそのまま残る。テーブルを空にしたいなら、seedを残したまま先に行を消すか、`DELETE`を自分で書く。

検査器も同じ行を値集合として使う。キー列、またはそれを参照する列に使われたGoのnamed typeは、enumのラベルと同じ規則でこの行と比較される（[checks.ja.md](checks.ja.md#型に意味を持たせる)）。値集合の置き場としてlookupテーブルを推奨する理由はここにある。値の追加・ラベル変更・並び替え・廃止はどれも1行の変更と`MERGE` 1文で済むが、enumでは型を作り直してすべての列に当て直すことになる。

## MySQL

MySQLでは両側をサーバ自身の描き方で読む。全部の表とビューに`SHOW CREATE TABLE` / `SHOW CREATE VIEW`をかけ、`schema.sql`と同じやり方で読む。比較されるのは`schema.sql`の書き方ではなくMySQLが保持している定義なので、`INT`と`int(11)`、`0`と書いた既定値と`'0'`で保持されたもの、サーバが名前を付けたキー（`orders_ibfk_1`、`orders_chk_1`）は差分にならない。目標側は、`-db`のサーバ上の一時データベースに`schema.sql`を適用して得る。マイグレーションの対象そのもののサーバが、そのバージョンと設定（`-- sqlshape: server`）で描くということである。`-from`（テキスト同士、サーバ無し）では`PATH`の`mysqld`を使う。

比較はオブジェクトごとに行う。

- 表: エンジン、文字集合、照合、行フォーマット、コメント、パーティショニング（`RANGE`、`LIST`、`RANGE COLUMNS`、`LIST COLUMNS`、`HASH`、`KEY`、`LINEAR`、`ALGORITHM`、`HASH` / `KEY`によるサブパーティション、各パーティションの境界・コメント・明示の`SUBPARTITION`名。`sqlshape`が比較できない形——パーティションやサブパーティションの`TABLESPACE`や`MAX_ROWS`オプション——は見逃さず問題として報告する）
- 列: 型、サーバが綴った定義全体、位置（MySQLは列を並べ替えられるので、順序の違いは差分であり、計画は`MODIFY COLUMN ... AFTER`で直す）
- キー、外部キー、CHECK制約
- ビュー
- トリガとストアドプロシージャ・関数: `SHOW CREATE TRIGGER` / `SHOW CREATE PROCEDURE` / `SHOW CREATE FUNCTION`で読み戻し、DEFINERを落とした定義テキストで比較する
- イベント: スケジュール、`STARTS` / `ENDS`、`ON COMPLETION`、状態、コメント、本体を`SHOW CREATE EVENT`で読み戻して比較する。`schema.sql`がサーバに任せた時刻——省いた`STARTS`、式で書いた`STARTS`（`CURRENT_TIMESTAMP + INTERVAL 1 DAY`）、式の`AT`——はイベント作成時に埋まる（`SHOW CREATE EVENT`は作成時刻のリテラルとして読み戻す）ので比較しない。リテラルで書いた時刻は書いたとおりに比較する

比較しないのは、MySQLではまだ`sqlshape`が読まないseed行。`ON COMPLETION PRESERVE`の無い一回限りのイベント（`AT ...`）は実行後にサーバが消すので、`verify-schema`はそれ以降「無い」と報告する。これはイベント自身の定義であって、ドリフトではない。既存の表の`AUTO_INCREMENT=<n>`カウンタはデータであってスキーマではないので、これも比較しない。新規の表が自分で宣言した`AUTO_INCREMENT=<n>`はスキーマの決定であり、作成時にそのまま届く。

計画はMySQL自身の定義を使う。

| 変更 | DDL |
|---|---|
| 新しい表 | サーバが描いたとおりの`CREATE TABLE` |
| 変わった列 | 目標の定義による`ALTER TABLE ... MODIFY COLUMN` |
| 変わったキー・外部キー・CHECK | `DROP`と`ADD` |
| 変わったビュー | `CREATE OR REPLACE VIEW` |
| パーティション化される表、種類やキーが変わる表 | `ALTER TABLE ... PARTITION BY ...`（行の配り直しはサーバがやる。どのパーティションにも入らない行があればサーバのエラー1526） |
| パーティション化をやめる表 | `ALTER TABLE ... REMOVE PARTITIONING` |
| `RANGE`の末尾に足すパーティション | `ADD PARTITION` |
| 消える`RANGE`パーティション | `DROP PARTITION`。行を持っていくので`-- @migrate drop partition orders.p0`の宣言が要る |
| `RANGE`の境界の移動、`MAXVALUE`の前への挿入 | `REORGANIZE PARTITION ... INTO (...)`（行の移動はサーバがやる） |
| `LIST`パーティションの追加・消滅・値リストの変更 | `ADD PARTITION`、同じ宣言の下での`DROP PARTITION`、変わった値リストは全部まとめて1つの`REORGANIZE PARTITION ... INTO`（値が残る2つのパーティションの間を動くとき、文の間で行が浮かない） |
| `HASH` / `KEY`のパーティション数 | `ADD PARTITION PARTITIONS n` / `COALESCE PARTITION n` |
| `LINEAR`、`KEY`の列や`ALGORITHM`、サブパーティションの変更 | `ALTER TABLE ... PARTITION BY ...`の全文（行の配り直しはサーバがやる。行は失わない） |
| パーティションの`COMMENT` | 新しいコメントでの`REORGANIZE PARTITION ... INTO`（行は動かない） |
| 変わった、または消えるトリガ・プロシージャ・関数 | `DROP`してから`CREATE`（MySQLには`CREATE OR REPLACE TRIGGER`が無い） |
| 変わった、または消えるイベント | `DROP EVENT`してから`CREATE EVENT`。目標のテキストそのまま（`STARTS`を省いていれば、新しいイベントはマイグレーションを実行した時刻から始まる） |

順序は、マイグレーション自身の手順が互いにつまずかないように決める。

1. トリガの`DROP`は表のDROPより前（表ごと消えるトリガは`DROP TABLE`が黙って持っていくので出さない）
2. 消える表は、それを参照する外部キーを先に落とす
3. ルーチンの`CREATE`はビューより前、かつ表より前（ビューが関数を呼ぶことがある）
4. トリガの`CREATE`はbackfillの後。新しく足したトリガがマイグレーション自身の書き込みで発火しないためである

`-- @migrate`の宣言は同じだが2点違う。MySQLではENUMは列の型なので、`enum`は列を名指す（`-- @migrate enum orders.status: drop 'canceled' using 'cancelled'`）。計画は型を狭める前に行を更新する。パーティションは表ではないので、落とす宣言は`-- @migrate drop partition orders.p0`と書く。

表の`DEFAULT CHARSET` / `COLLATE`の変更は、自分の照合を持たない文字列列の全部に`MODIFY COLUMN`も出す。表オプションだけではそれらの列は旧エンコーディングのまま残り、`apply`の後の計画が空にならない。

`apply`はDDLを1つの接続で1文ずつ実行する。その接続は、スキーマが宣言した`sql_mode`（宣言が無ければ8.4の既定値）、つまり一時データベースがDDLを判定したときのモードで動く。サーバのグローバル設定には依らない。MySQLのDDLは暗黙にコミットされるのでスクリプトはトランザクションにならず、`-no-transaction`は効かない。ある文が失敗したら、`apply`はどの文かと、その前の何文が適用済みかを言う。その状態から`sqlshape diff`をかければ残りが出る。

## 計画がどう検証されているか

`diff`が書くDDLは本物のサーバで検証している。生成したスキーマの組を、全部の表に行を入れた状態で使い、計画が問題として止める変更と現れ得ない変更を除いて、diffが報告し得る変更の全種に届かせる。組が合格するのは、サーバが計画を受け付け、実行後の読み戻しが目的のスキーマと一致し、そこから再び計画すると空になるときだけである。届かないのは、生成する形の外にあるスキーマと、データに依存する失敗（backfillの値、ロック時間）である。それらは利用者の`schema.sql`とともにやって来るもので、`apply`が実行前に行う確認が受け止める。
