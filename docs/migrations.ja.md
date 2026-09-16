# マイグレーション

[English](migrations.md)

`schema.sql`がデータベースの唯一の定義であり、マイグレーションファイルは書かない。`sqlshape`が稼働中のデータベースと`schema.sql`を比較してDDLを生成し、そのDDLを当てれば本当に`schema.sql`の状態になることを確認してから実行する。コマンドはPostgreSQLとMySQLの両方で使える。どちらに話すかはスキーマの宣言が決め、MySQLで違うところは下の[MySQL](#mysql)の節にまとめてある。

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

## 何を比較するか

PostgreSQLでは両側とも`pg_dump`の出力として読むので、比較されるのは`schema.sql`の書き方ではなく、PostgreSQLが実際に保持している定義である。`'x'`と`'x'::text`、`IN (...)`と`= ANY (ARRAY[...])`のような書き方の違いは差分にならない。`-- sqlshape:`のディレクティブはデータベースの状態ではないので比較対象に入らない。

比較はオブジェクト単位で行う。テーブル、列、制約、インデックス、ビュー、関数、型、ドメイン、enum、トリガー、ルール、ポリシー、行セキュリティの設定、シーケンス、拡張、コメントが対象で、seed済みテーブルは行単位でも比較する。

## diff

`sqlshape diff -db DSN`は、データベースを`schema.sql`の状態にするDDLを依存順に出力する。drop（依存している側から）、rename、alter、add、最後にseedの`MERGE`の順である。これは提案であって、そのまま実行するものではない。手元のデータに対して順序や形が合わないなら編集する。`ALTER COLUMN ... TYPE`には`USING`が必要になることがあり、大きなテーブルには`CREATE INDEX CONCURRENTLY`を使いたいことがあり、backfillは2つのステップの間に入れたいことがある。

`-from other.sql`を指定すると、データベースの代わりに2つのスキーマファイルを比較する。前リリースの`schema.sql`と比べるときに使う。`-packages ./...`を指定すると、Goの文を現在のスキーマに対して索引し、DDLが落とすか型を変える列を読んでいる文をDDLの中にコメントとして列挙する。

## diffだけでは決められないことを宣言する

2つのスキーマの差分を見ても決められないことがある。renameなのかdropしてaddしたのか、削除するenumのラベルを持つ行は何に置き換えるのか、新しい`NOT NULL`列に既存の行では何を入れるのか、である。これらは`schema.sql`に`-- @migrate`行で宣言する:

```sql
-- @migrate rename orders.state -> orders.status      列、またはテーブルのrename。左が現在の名前、右が新しい名前
-- @migrate drop orders.legacy                        この列（またはテーブル）を落としてよい
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

`drop`も`rename`も宣言されていないのに消えるテーブルや列があればエラーになる（DDLは出力される）。データが失われる変更は必ず宣言を要求する、ということである。逆に、差分と食い違う宣言（元が存在しないrename、まだ残っている列のdrop）もエラーになるので、古い宣言が残り続けることはない。宣言はデータベースの現状から`schema.sql`への1ステップを記述するもので、適用したら消す。

renameは`RENAME TO` / `RENAME COLUMN`になる。renameした列を含む制約や外部キーは`DROP`して`ADD`する形で出る。正しい手順だが、制約の再検証が走る。enumのラベル削除は型の作り直しになる（PostgreSQLには`DROP VALUE`が無い）。旧型をrenameし、新型を作り、その型を使うすべての列に`ALTER COLUMN ... TYPE ... USING CASE ...`を当て、それらのテーブルに依存するビューを作り直し、旧型を落とす。enumの配列を持つ列があれば問題として報告する。`backfill`は移行後のスキーマで型検査され、列が作られた後に`UPDATE`として出力される。

パーティションもほかと同じスキーマである。新しいパーティション表は`PARTITION BY`付きで作り、子ができてからATTACHする。既存の親への子の追加は`CREATE TABLE ... PARTITION OF ... FOR VALUES ...`、消える子は行を持っていくので`-- @migrate drop`の宣言を要求した上で`DROP TABLE`、表が子になる・子でなくなるのは`ATTACH PARTITION` / `DETACH PARTITION`、境界の変更はDETACHしてATTACHする（計画中の全DETACHを全ATTACHより前に出すので、動く境界が隣と重なることはない）。子の列は親に従うので、計画が子の列を直接ALTERすることはない。行を持つ表を後からパーティション化する、パーティション化をやめる、別のキーや戦略に移す、はどれもDDLになる。計画は表をRENAMEし、宣言された形をパーティションごと新しく作り、全行を親経由でINSERTしてPostgreSQLのrouterに配らせ、コメント・トリガ・ポリシー・RULE・被参照の外部キー・sequence（bigserialは同じsequenceを使い続け、identity列は最大値の続きから）を新しい表に付け直し、旧表を落とす。どのパーティションにも入らない行はサーバのエラーになるので、データを覆っていない`schema.sql`は何も失わずに`apply`を止める。

ドメインの基底型、範囲型のサブタイプ、`INHERITS`、`OF type`の変更はDDLとしては出さず、`-- `で始まる注記として出力する。人が手順を決める必要がある変更である。

列の`ALTER COLUMN ... TYPE`も、新しい型が`numeric`の精度・スケール、`varchar(n)` / `char(n)`の長さ、`time` / `timestamp`系の秒以下の精度、`bit(n)`の長さのいずれかを狭める場合は同様に注記が付く。同じ基底型どうしのALTERはPostgreSQLが`USING`無しでそのまま実行してしまい、既存の値は無警告で丸められる、あるいは切り詰められる。注記はあくまで警告であってブロックはしない——diffの他の部分と同じ「提案であり、人が手を入れる」という契約に沿っているので、無編集のまま適用すればやはり値は丸められる、または切り詰められる。適用前に`USING`を足すか、先にデータを直しておくこと。

## apply

`sqlshape apply -db DSN up.sql`はDDLファイルを受け取り（手で編集したものでもよい）、その結果を確認してから実行する。データベースの現在のスキーマにこのDDLを当てたものが、`schema.sql`と一致しなければならない。列の順序の違いだけは許容して注記する（PostgreSQLは列を途中に挿入できないので、列を落として追加し直すと順序は永久にずれる）。それ以外の違いがあれば拒否する。確認が通れば、DDLを1つのトランザクションで実行する。

- `-packages ./...`を付けると、DDLが落とすか型を変える列にまだ依存しているGoの文があれば拒否する。`-force`で無視して実行できる。
- `-dry-run`は確認だけで止まる。
- `-no-transaction`はDDLをトランザクションで包まずにそのまま実行する。`CREATE INDEX CONCURRENTLY`や`ALTER TYPE ... ADD VALUE`のようにトランザクション内で実行できない文があるときに使う。

## verify-schema

`sqlshape verify-schema -db DSN`は、データベースが`schema.sql`と違う箇所を列挙する。ドリフト、手で当てたマイグレーション、追従が遅れた環境を見つけるために使う。違いがあれば終了コード1、一致していれば0を返す。

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

MySQLでは両側をサーバ自身の描き方で読む。全部の表とビューに`SHOW CREATE TABLE` / `SHOW CREATE VIEW`をかけ、`schema.sql`を読むのと同じローダーで読む。比較されるのは`schema.sql`の書き方ではなくMySQLが保持している定義なので、`INT`と`int(11)`、`0`と書いた既定値と`'0'`で保持されたもの、サーバが名前を付けたキー（`orders_ibfk_1`、`orders_chk_1`）は差分にならない。目標側の正準形は、`-db`のサーバ上に一時データベース（`sqlshape_scratch_<乱数>`）を作って`schema.sql`を適用し、読み返してから落として得る。マイグレーションの対象そのもののサーバが、そのバージョンと設定（`-- sqlshape: server`）で正規化するということである。`-from`（テキスト同士、サーバ無し）では`PATH`の`mysqld`を使う。

比較はオブジェクトごとに行う。

- 表: エンジン、文字集合、照合、行フォーマット、コメント、パーティショニング（`RANGE`、`LIST`、`RANGE COLUMNS`、`LIST COLUMNS`、`HASH`、`KEY`、`LINEAR`、`ALGORITHM`、`HASH` / `KEY`によるサブパーティション、各パーティションの境界とコメント。ローダーが構造にしない形——サブパーティションの個別定義や`TABLESPACE`オプション——は見えない差分ではなく問題として報告する）
- 列: 型、サーバが綴った定義全体、位置（MySQLは列を並べ替えられるので、順序の違いは差分であり、計画は`MODIFY COLUMN ... AFTER`で直す）
- キー、外部キー、CHECK制約
- ビュー
- トリガとストアドプロシージャ・関数: `SHOW CREATE TRIGGER` / `SHOW CREATE PROCEDURE` / `SHOW CREATE FUNCTION`で読み戻し、DEFINERを落とした定義テキストで比較する
- イベント: スケジュール、`STARTS` / `ENDS`、`ON COMPLETION`、状態、コメント、本体を`SHOW CREATE EVENT`で読み戻して比較する。`schema.sql`がサーバに任せた時刻——省いた`STARTS`、式で書いた`STARTS`（`CURRENT_TIMESTAMP + INTERVAL 1 DAY`）、式の`AT`——はイベント作成時に埋まる（`SHOW CREATE EVENT`は作成時刻のリテラルとして読み戻す、測定済み）ので比較しない。リテラルで書いた時刻は書いたとおりに比較する

比較しないのは、MySQLのローダーがまだ知らないseed行。`ON COMPLETION PRESERVE`の無い一回限りのイベント（`AT ...`）は実行後にサーバが消すので、`verify-schema`はそれ以降「無い」と報告する。これはイベント自身の定義であって、ドリフトではない。既存の表の`AUTO_INCREMENT=<n>`カウンタはデータであってスキーマではないので、これも比較しない。新規の表が自分で宣言した`AUTO_INCREMENT=<n>`はスキーマの決定であり、作成時にそのまま届く。

計画はMySQL自身の定義を使う。

| 変更 | DDL |
|---|---|
| 新しい表 | 正準の`CREATE TABLE` |
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
| パーティションの`COMMENT` | 新しいコメントでの`REORGANIZE PARTITION ... INTO`（行は動かない、測定済み） |
| 変わった、または消えるトリガ・プロシージャ・関数 | `DROP`してから`CREATE`（MySQLには`CREATE OR REPLACE TRIGGER`が無い） |
| 変わった、または消えるイベント | `DROP EVENT`してから`CREATE EVENT`。目標のテキストそのまま（`STARTS`を省いていれば、新しいイベントはマイグレーションを実行した時刻から始まる） |

順序は、マイグレーション自身の手順が互いにつまずかないように決める。

1. トリガの`DROP`は表のDROPより前（表ごと消えるトリガは`DROP TABLE`が黙って持っていくので出さない）
2. 消える表は、それを参照する外部キーを先に落とす
3. ルーチンの`CREATE`はビューより前、かつ表より前（ビューが関数を呼ぶことがある）
4. トリガの`CREATE`はbackfillの後。新しく足したトリガがマイグレーション自身の書き込みで発火しないためである

`-- @migrate`の宣言は同じだが2点違う。MySQLではENUMは列の型なので、`enum`は列を名指す（`-- @migrate enum orders.status: drop 'canceled' using 'cancelled'`）。計画は型を狭める前に行を更新する。パーティションは表ではないので、落とす宣言は`-- @migrate drop partition orders.p0`と書く。

表の`DEFAULT CHARSET` / `COLLATE`の変更は、自分の照合を持たない文字列列の全部に`MODIFY COLUMN`も出す。表オプションだけではそれらの列は旧エンコーディングのまま残り（測定済み）、`apply`の後の計画が空にならない。

`apply`はDDLを1文ずつ実行する。MySQLのDDLは暗黙にコミットされるのでスクリプトはトランザクションにならず、`-no-transaction`は効かない。ある文が失敗したら、`apply`はどの文かと、その前の何文が適用済みかを言う。その状態から`sqlshape diff`をかければ残りが出る。

## 計画がどう検証されているか

`diff`が書くDDLは、`apply`が実行するのと同じやり方で本物のサーバに判定させている。手で選んだ例ではなく生成器で（`check/postgres/migrate`と`check/mysql/migrate`の`TestMigrateProbe`）。計画器の語彙からスキーマを乱数で作り、変異を1〜5個重ねて目的のスキーマにし（変異は自分の`-- @migrate`宣言も書く）、全部の表に3行ずつ入れて、元のスキーマを持つサーバで計画を実行する。合格の条件は三つ。サーバが何も拒まない、実行後に読み返した正準形が目的のスキーマと一致する（PostgreSQLは列順を除く）、そこから再び計画すると空になる。落ちた組は最小化し、直したものはサーバのエラーとともに回帰テストとして固定する（`probe_findings_test.go`）。乱数の組に加えて、各変異を単独で適用した組を毎回1つずつ作るので、どの変更の種類も引きに依存しない。PostgreSQLのprobeは17と18（`TestMigrateProbe18`）の両方に対して、それぞれのバージョンの語彙で回す。

生成器が届くべき範囲は、勘ではなく定義している。計画の入力はdiffの出力そのものなので、diffが報告し得る変更の全種（表の追加、列の型の変更、制約がDEFERRABLEになる、…）をdiff自身の比較関数から列挙し、ゲート（固定seedの200組）は、その全種が「どれかの組で現れる」か「理由付きで到達不能に挙げてある」かのどちらかでなければ落ちる。理由として認めるのは二つだけ。計画器がその変更にDDLを書かず注記か問題として返すもの（PostgreSQLの`INHERITS`、`OF type`、domainの基底型、rangeのサブタイプ、行を持つ表のパーティションキー）と、構造的に現れないもの（PostgreSQL 17のサーバに対するPostgreSQL 18の構文、pg_dumpがその形で描かない変更）。組み合わせと順序は乱数の組に任せる。PostgreSQLは103種のうち17で94種、18で98種、MySQLは48種全部に届いていて、未到達はどれも計画が問題として止めるものか、現れ得ないものである。見つかった計画器の穴の大半は本物のサーバが拒む順序の問題で、全部が回帰テストになっている。

届かないもの。生成器のモデルの外にあるスキーマの形（レガシーな綴り、拡張の型、巨大な表）と、データに依存する失敗（backfillの値、ロック時間）。これらは利用者の`schema.sql`とともにやって来るもので、`apply`が実行前に行う終点の確認が受け止める。

## 必要な環境

PostgreSQL:

- `pg_dump`。`PATH`にあるか、`$SQLSHAPE_PG_DUMP`で指定する。メジャーバージョンは対象データベース以上であること。
- 比較のために、`sqlshape`はスキーマが宣言したバージョン（`-- sqlshape: postgres 17`）の専用PostgreSQLを初回にダウンロードして`~/.cache/sqlshape`（`$SQLSHAPE_PG_CACHE`）にキャッシュし、そこで`schema.sql`を実行する。接続先のデータベースが宣言と違うメジャーバージョンで動いているときは、その旨をstderrに出して続行する。DDLは宣言したバージョンの規則で判定される。初回はダウンロードに数秒かかり、以後は0.25秒程度で起動する。利用者のデータベースはこの用途には使わない。

MySQL:

- `-db`では、接続ユーザーに`CREATE DATABASE`と`DROP DATABASE`の権限が要る（一時データベースのため）。サーバの`lower_case_table_names`はスキーマが宣言した値（宣言が無ければ0）でなければならず、違えばコマンドは止まる。サーバがスキーマの宣言と違うバージョンのMySQLで動いているときは、その旨を出して続行する。
- `-from`（テキスト同士）では、`PATH`の`mysqld`（`nix-shell -p mysql84`、ディストリビューションのパッケージ、サーバtarballの`bin/`）をスキーマの宣言した設定で起こす。

共通:

- `-schema PATH`で`schema.sql`、または`*.sql`を名前順に適用する`schema/`ディレクトリを指定できる。既定は作業ディレクトリから上に辿って最初に見つかるもの。

終了コードは、0が差分なし、1が何かを見つけた（差分、ドリフト、拒否されたapply）、2が使い方か環境のエラー。
