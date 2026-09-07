# マイグレーション

[English](migrations.md)

`schema.sql`がデータベースの唯一の定義。書くべきマイグレーションファイルは無い: `sqlshape`バイナリが稼働中の
データベースと`schema.sql`を比較してDDLを導き、そのDDLが本当に`schema.sql`に至ることを検査し、実行する。

```
$ sqlshape diff -db "$DSN" > up.sql         # データベースの状態から schema.sql へ至る DDL
$ $EDITOR up.sql                            # 並べ替え、分割、USING の追加、backfill の差し込み
$ sqlshape apply -db "$DSN" -packages ./... up.sql
$ sqlshape verify-schema -db "$DSN"         # ドリフト: データベースが schema.sql と違う箇所
```

## 何を比較するか

両側とも`pg_dump`を通して読むので、比較されるのは`schema.sql`の綴りではなくPostgreSQL自身が保持するもの:
`'x'`と`'x'::text`、`IN (...)`と`= ANY (ARRAY[...])`は差ではない。`-- sqlshape:`ディレクティブはデータベースの
状態ではないので比較しない。

比較はオブジェクト単位（テーブル、列、制約、インデックス、ビュー、関数、型、ドメイン、enum、トリガー、ルール、
ポリシー、行セキュリティのフラグ、シーケンス、拡張、コメント）と、seed済みテーブルについては行単位。

## diff

`sqlshape diff -db DSN`はデータベースを`schema.sql`へ持っていくDDLを依存順に出力する: drop（依存側から）、
rename、alter、add、最後にseedの`MERGE`。これは提案で、DDLは読むためのもの。手元のデータに対して順序や形が
合わなければ編集する（`ALTER COLUMN ... TYPE`には`USING`が要る、大きなテーブルには`CREATE INDEX CONCURRENTLY`、
backfillは2つのステップの間に入る）。

`-from other.sql`はデータベースの代わりに2つのスキーマテキストを比較する。前リリースの`schema.sql`など。
`-packages ./...`はGoの文を現スキーマに対して索引し、DDLが落とすか型を変える列を読む文をすべてDDL内のコメントとして
列挙する。

## diffが見えないものを宣言する

2つのスキーマのdiffは、renameとdrop + addの区別、削除したenum値がどのラベルになるべきか、新しい`NOT NULL`列に
既存行が何を持つべきかを決められない。それらは`schema.sql`の`-- @migrate`行で宣言する:

```sql
-- @migrate rename orders.state -> orders.status      列、またはテーブル: rename 旧 -> 新
-- @migrate drop orders.legacy                        この列（またはテーブル）は消えてよい
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

renameの左辺は今の名前、右辺はこれからの名前。`drop`も`rename`も宣言せずに消えるテーブルや列はエラー
（DDLは出力される）なので、データ損失は必ず予告される。diffが裏付けない宣言（存在しない元のrename、まだある列のdrop）
もエラーなので、古い宣言は残れない: 宣言はデータベースの現状から`schema.sql`へのステップを記述し、適用したら消す。

renameは`RENAME TO` / `RENAME COLUMN`になる。renameされた列にかかる制約や外部キーは`DROP` + `ADD`で出る。
正しいが制約の再検証が走る。enumラベルの削除は型を作り直す（PostgreSQLに`DROP VALUE`は無い）: 旧型をrename、
新型を作成、それを持つ全列に`ALTER COLUMN ... TYPE ... USING CASE ...`、それらのテーブル上のビューを再作成、旧型をdrop。
enumの配列列は問題として報告される。`backfill`は目標スキーマで型検査され、列ができた後に`UPDATE`として出る。

ドメインの基底型、範囲型のサブタイプ、`INHERITS`、パーティショニング、`OF type`の変更はDDLではなく運用者向けの
`-- `注記として出力される。

## apply

`sqlshape apply -db DSN up.sql`はDDLファイル（手で編集したかどうかは問わない）を受け取り、終点で検査する:
データベースの現スキーマ + DDLが`schema.sql`として読み戻せなければならない。列順は許容して注記する（PostgreSQLは
列を途中に挿せないので、dropして再追加した列は順序が永久にずれる）。それ以外はすべて拒否。通ればDDLを1つの
トランザクションで実行する。

- `-packages ./...`はGoの文を現スキーマに対して索引し、DDLが落とすか型を変える列に依存する文が残っていれば拒否する。
  `-force`はそれでも実行する。
- `-dry-run`は検証で止まる。
- `-no-transaction`はDDLをそのまま実行する。`CREATE INDEX CONCURRENTLY`、`ALTER TYPE ... ADD VALUE`など
  トランザクション内で走れない文のため。

## verify-schema

`sqlshape verify-schema -db DSN`はデータベースが`schema.sql`と違う箇所を列挙する: ドリフト、手で当てた
マイグレーション、遅れた環境。差があれば終了コード1、一致すれば0。

## seed済みテーブル

行が`schema.sql`に普通の`INSERT ... VALUES`で書かれているテーブルはseed済みテーブルで、行はスキーマの一部。

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

INSERTは冪等でなければならない。スキーマテキストを2回適用しても同じ意味になるように: 行はすべての行が定数で与える
キー（主キー、またはNOT NULL列上の非部分一意制約）で識別され、値は定数式（volatile / stableな関数、サブクエリ、
`DEFAULT`は不可）、`ON CONFLICT`は無く、同じキーは2度現れない。違反はスキーマの問題。INSERTは他の文と同じく
型検査もされる。

すべての比較で宣言された行はデータベースから読み戻されてキーでdiffされるので、`schema.sql`での行の追加・変更・削除は
列と同じように`diff`と`verify-schema`に現れる。プランは内容が違うテーブルごとに`MERGE` 1文で終わり、宣言に無くなった
行は削除する。外部キーで結ばれたseed済みテーブル同士は親からMERGEし、削除は子から。INSERTの上の`-- sqlshape: seed`
はseedを追加型にする: 宣言に無い行は残る。

検査器は同じ行を値集合として読む: キー列やそれを参照する列に出会ったGoのnamed typeはenumのラベルと同じように
それらとdiffされる（[checks.ja.md](checks.ja.md#意味-その値は何を表すか)）。lookupテーブルを値集合の置き場として
推奨する理由はここにある: 値の追加・ラベル変更・並び替え・退役はそれぞれ1行の変更と`MERGE`で済み、enumなら
全列の下で型を作り直すことになる。

## 要件

- `pg_dump`が`PATH`にあるか`$SQLSHAPE_PG_DUMP`で名指されていること。メジャーバージョンはデータベース以上。
- 比較は`schema.sql`を、`sqlshape`が初回にダウンロードして`~/.cache/sqlshape`（`$SQLSHAPE_PG_CACHE`）に
  キャッシュする専用のPostgreSQL 17上で走らせる。初回はダウンロードに数秒、以後は0.25秒で起動する。
  利用者のデータベースはこれには使われない。
- `-schema PATH`は`schema.sql`、または`*.sql`を名前順に適用する`schema/`ディレクトリ。既定は作業ディレクトリから
  上に辿って最も近いもの。

終了コード: 0は差なし、1は指摘（diff、ドリフト、拒否されたapply）、2は使い方か環境のエラー。
