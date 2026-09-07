# マイグレーション

[English](migrations.md)

`schema.sql`がデータベースの唯一の定義であり、マイグレーションファイルは書かない。`sqlshape`が稼働中のデータベースと`schema.sql`を比較してDDLを生成し、そのDDLを当てれば本当に`schema.sql`の状態になることを確認してから実行する。

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql に至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト検出: データベースが schema.sql と違う箇所
```

## 何を比較するか

両側とも`pg_dump`の出力として読むので、比較されるのは`schema.sql`の書き方ではなく、PostgreSQLが実際に保持している定義である。`'x'`と`'x'::text`、`IN (...)`と`= ANY (ARRAY[...])`のような書き方の違いは差分にならない。`-- sqlshape:`のディレクティブはデータベースの状態ではないので比較対象に入らない。

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

ドメインの基底型、範囲型のサブタイプ、`INHERITS`、パーティショニング、`OF type`の変更はDDLとしては出さず、`-- `で始まる注記として出力する。人が手順を決める必要がある変更である。

## apply

`sqlshape apply -db DSN up.sql`はDDLファイルを受け取り（手で編集したものでもよい）、その結果を確認してから実行する。データベースの現在のスキーマにこのDDLを当てたものが、`schema.sql`と一致しなければならない。列の順序の違いだけは許容して注記する（PostgreSQLは列を途中に挿入できないので、列を落として追加し直すと順序は永久にずれる）。それ以外の違いがあれば拒否する。確認が通れば、DDLを1つのトランザクションで実行する。

- `-packages ./...`を付けると、DDLが落とすか型を変える列にまだ依存しているGoの文があれば拒否する。`-force`で無視して実行できる。
- `-dry-run`は確認だけで止まる。
- `-no-transaction`はDDLをトランザクションで包まずにそのまま実行する。`CREATE INDEX CONCURRENTLY`や`ALTER TYPE ... ADD VALUE`のようにトランザクション内で実行できない文があるときに使う。

## verify-schema

`sqlshape verify-schema -db DSN`は、データベースが`schema.sql`と違う箇所を列挙する。ドリフト、手で当てたマイグレーション、追従が遅れた環境を見つけるために使う。違いがあれば終了コード1、一致していれば0を返す。

## seed済みテーブル

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

検査器も同じ行を値集合として使う。キー列、またはそれを参照する列に使われたGoのnamed typeは、enumのラベルと同じ規則でこの行と比較される（[checks.ja.md](checks.ja.md#型に意味を持たせる)）。値集合の置き場としてlookupテーブルを推奨する理由はここにある。値の追加・ラベル変更・並び替え・廃止はどれも1行の変更と`MERGE` 1文で済むが、enumでは型を作り直してすべての列に当て直すことになる。

## 必要な環境

- `pg_dump`。`PATH`にあるか、`$SQLSHAPE_PG_DUMP`で指定する。メジャーバージョンは対象データベース以上であること。
- 比較のために、`sqlshape`は専用のPostgreSQL 17を初回にダウンロードして`~/.cache/sqlshape`（`$SQLSHAPE_PG_CACHE`）にキャッシュし、そこで`schema.sql`を実行する。初回はダウンロードに数秒かかり、以後は0.25秒程度で起動する。利用者のデータベースはこの用途には使わない。
- `-schema PATH`で`schema.sql`、または`*.sql`を名前順に適用する`schema/`ディレクトリを指定できる。既定は作業ディレクトリから上に辿って最初に見つかるもの。

終了コードは、0が差分なし、1が何かを見つけた（差分、ドリフト、拒否されたapply）、2が使い方か環境のエラー。
