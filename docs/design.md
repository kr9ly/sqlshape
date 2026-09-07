# 設計上の裁定

sqlshapeを開発する人（と、開発を手伝うエージェント）向けに、設計の判断を「何を決めたか・なぜか・何を棄てたか」の形で残す。振る舞いの説明は利用者向けの[README](../README.ja.md)と`docs/`にあり、ここには繰り返さない。判断が変わったら該当項目を書き換える。経緯はgitの履歴とコミットメッセージにある。

## 出発点

RDBMSを使うアプリケーションに要るのは4つ。SQLの構文と型の検査、型安全な値のバインド、結果の構造体へのマッピング、スキーマから導けるマイグレーション。既存のツールは、このうち2〜3個を満たして残りを落とすか、4つ揃えるためにSQLをDSLに翻訳させる形を取る（jOOQ / Kysely / Drizzle）。前の3つは「SQLが正」、最後は「アプリの型定義が正」と真実の源が割れていることが、その背景にあると見ている。sqlshapeは真実の源を`schema.sql`とSQLそのものに置き、アプリの型は照合される側にする。

## 核となる裁定

### 検査器はpure Goで持ち、本物のPostgreSQLはテストのオラクルにする

決めたこと。PostgreSQLのカタログ（`pg_type` / `pg_proc` / `pg_operator` / `pg_cast` / `pg_aggregate`）を本物のPGからCOPYでdumpしてTSVとして埋め込み、型変換はマニュアル10章の規則をそのまま実装する。埋め込みPostgreSQL（embedded-postgres）は差分テストの正解役としてだけ使い、検査時には起動しない。

理由。nullabilityはPGのDescribeが返さないので自前で式の木を歩く必要があり、そうするなら型も同じ木の走査に載せた方が解析器が1つで済む。WHEREやJOINの条件に現れる列参照はDescribeでは見えず、消費者索引（どの文がどの列を読むか）には全数の参照が要る。

棄てた案。① embedded PGにschema.sqlを流してPREPARE / Describeで聞く（当初案。起動コスト、nullabilityと参照の全数が取れない）。② libpg_queryの手法を`analyze.c`まで広げて偽カタログで動かす（研究課題に近い）。③ PGをwasmで動かす（実用段階か未確認）。④ カタログをPGソースの`.dat`から生成する（COPY dumpならデフォルト値解決済み・OID確定・拡張も同じ経路）。

忠実度の測り方。PGの回帰テスト`src/test/regress`の全文を本物のPGと並走させ、パラメータ型・結果列・エラーの一致を`go test`でゲートする（`internal/analyze/regress_test.go`）。残る不一致は環境依存のcollation、RLSの再帰、権限、サーバー内部エラー、意図的な相違3件で、いずれも静的解析の外。

意図的な相違。ドメインの演算結果はドメインを保つ（PGは基底型に落とす）。配列リテラルの要素に対するCHECKの定数評価。PG 17のリライタが絡む1件。

### テンプレートは有限の出力集合を持ち、全部展開して検査する

決めたこと。SQLはGoの`text/template`のサブセットで書く。`{{.X}}`は必ず`$n`になり、分岐は`if` / `else if` / `with` / `range`のみ。rangeは0・1・2回で代表する。組み合わせが256を超えたら疎（全オフ・全オン・各単独オン）に落とす。実行時は描画結果を同じ分岐シグネチャの静的展開とバイト比較し、違えば実行しない。

理由。文字列結合が危険なのは出力集合が無限で不透明だから。分岐だけなら有限で、全展開すれば動的SQLもそのまま検査できる。2-way SQL（Doma / uroboroSQL）は同じ形だが既定の1形しか検査しない。

棄てた案。`{{switch}}`（`else if eq`で書ける。構文を増やさない）。分岐ごとに結果型が変わるクエリをタグ付きunionで受ける生成（省略可能な投影をnullableフィールドで受ける方が単純）。分岐の独立性の自動判定（疎検査の規則を固定した方が説明できる）。`{{define}}` / `{{template}}`（Goの定数連結で足りる。値がSQLテキストにならない唯一の共有機構を保つ）。

### コード生成ではなくvet

決めたこと。構造体もSQLも人が書き、検査器は合っているかだけを見る。`go/analysis`アナライザーとして`go vet -vettool`に乗せる。構造体の書き起こしはSuggestedFixで出す。

理由。生成物が無いのでgoplsのリネーム・参照検索がそのまま効く。既存コードの定数SQL文字列はディレクティブ0個のテンプレートとして後付けで検査対象になる。検査できない文（非定数）を数えれば検査カバレッジが数値になる。

棄てた案。sqlcのようなクエリからの関数生成。goplsへの直接統合（サードパーティのアナライザーを読めない）。

### `schema.sql`が唯一の定義で、マイグレーションはそこから導く

決めたこと。スキーマの正は`schema.sql`（または`schema/`ディレクトリ）。検査器はこれを空のカタログに載せて読み、テストは`pgtest`でこれを適用した本物のPGを立て、DBの実状態は`sqlshape diff` / `apply`が寄せる。マイグレーションファイルは持たない。

理由。「マイグレーションに書いたスキーマしか使えない」を制約として検査するのではなく、それ以外の状態が作れないようにする。副産物としてDROPの影響分析、ドリフト検出、死んだスキーマの検出が同じ基盤で出る。

### 意味は登録ではなく使用箇所から束縛する

決めたこと。Goのnamed typeがenum・lookupのキー・CHECKの値集合・キー列・ドメインに出会った箇所で、その型をその意味に束縛する。束縛はfactでパッケージを越え、以後の使用箇所すべてで照合する。明示的な登録APIは置かない。

理由。vetの思想（生成物なし・後付け可）に揃える。使っていない型は検査から外れるが、使っていなければ問題も起きない。

拡張。ID型はFKグラフから同一性を導く（`orders.user_id`の同一性は`users.id`）。ドメインはSQLの中でも不透明な単位として扱い、PGが許す`yen + gram`を報告する。PGより厳しい型検査を意図している。

### 値集合の本命はseed済みlookupテーブル、enumは例外

決めたこと。値集合の置き場としてlookupテーブル + FKを推奨し、`-strict`はenum列にそう言う。lookupテーブルの行は`schema.sql`の普通の`INSERT ... VALUES`で書き、`Relation.Seed`として読む。INSERTには冪等性の規則（定数キー・定数値・`ON CONFLICT`なし・キー重複なし）を課す。

理由。enumはDROP VALUEが無く、ラベルの削除や並び替えが「型の作り直し + 全列USING + 依存ビュー再作成」になる。lookupテーブルならMERGE 1文で済み、削除はFKが止め、属性も持て、ビュー経由で分析側にも名前が届く。検査精度はseedを値集合として読むことでenumと同等になった。

棄てた案。`-- @data`のようなコメント構文で行を宣言する（INSERTなら型・NOT NULL・FK・CHECKの検査がそのまま効き、psqlにも流せる）。

### 失敗モードはexpect行で宣言し、名前はPGの命名に揃える

決めたこと。書き込みの各展開が違反しうる制約をカタログから列挙し、テンプレートの`-- sqlshape: expect`行と両方向で比較する。無名の制約はPGの自動命名（`<table>_<cols>_key` / `_fkey` / `_check` / `_pkey`、ドメインは`<domain>_check`、NOT NULLは`table.column`）で呼ぶ。ランタイムはクラス23のエラーを同じキーの`ConstraintError`に写す。トリガーの独自SQLSTATEは`-- sqlshape: error CODE = Name`で名前を与える。

理由。診断・宣言・実行時エラーが同じ文字列で結ばれ、どの違反を処理すべきかがコードに書かれた状態が保たれる。命名をPGに揃えるのは、実行時エラーが運ぶ制約名と一致させるため。

### `One`はスキーマから証明する

決めたこと。`One`と宣言した文は、すべてのFROM項目の一意キーが等値で固定されているか（JOIN・ビュー・サブクエリ・CTEを通して追跡）、または集約・`LIMIT 1`・単一行INSERTかを、展開ごとに証明する。証明できなければエラー。実行時に2行目が来たら`ErrManyRows`。

理由。「1件返るはず」という解釈をDBの制約で裏付ける。証明できない`One`はスキーマ側に一意制約が足りていないことの発見になる。

### マイグレーションは自前のdiff / apply

決めたこと。sqldefは使わず自前で持つ。比較は両側とも「PGを通した正準形」（DBはpg_dump、`schema.sql`は埋め込みPGに載せてpg_dump）。`diff`はDDLを出すだけ、`apply`はDDLファイルを入力に取り、終点比較（現DB + DDLをembedded PGで再現し`schema.sql`と一致するか）で確認してから1トランザクションで実行する。diffだけでは決められないこと（rename、enumラベルの削除先、backfill）は`-- @migrate`行で宣言し、宣言と差分の不整合はどちら向きもエラー。

理由。schema層がDDL全般のオブジェクトモデルを持ち、embedded PGで生成DDLを機械検証できるので、外部ツールの出力を検査する層より自前で出す方が短い。両側正準形にするのは、直接ロードとdumpで式の綴りが違うため（`'x'`と`'x'::text`）。diffとapplyを分けるのは、間に人の手編集（順序・分割・USING・backfill差し込み）が入る前提だから。終点比較なら手編集の中身を問わない。

受け入れた冗長と限界。列順の差は許容して注記する（PGは列を途中に挿せないので、弾くとapplyが二度と通らなくなる）。renameした列を含む制約・FKはDROP + ADDで出る（正しいが再検証が走る。痛くなったらprops比較にrename写像を当てる）。domainの基底型・rangeのサブタイプ・INHERITS・PARTITION・OF typeの変更は注記のみ。pg_dumpは利用者環境のもの（embedded PGにクライアントツールは同梱されない。pg_catalog直読みは必要になったら）。

棄てた案。sqldefのラップ。pg_catalog直読みによる逆ロード（pg_dumpの方が正準形が得やすい）。

### 行レベルセキュリティはスキーマの一部として検査するが、絞るのはDB

決めたこと。`CREATE POLICY`の条件式を型検査し、ポリシーとENABLE / FORCEの設定をdiff / applyで運ぶ。`-require-columns`はポリシーが列を固定していれば満たしたとみなす。ポリシーのUSING式から`visible where`相当の条件を導出して文に要求することはしない。

理由。RLSはデータベースが絞る仕組みで、文に同じ条件を繰り返させるのは誤り。

未対応。ポリシーのUSINGをnullabilityや`One`の証明に使うこと。PL/pgSQL関数のSECURITY DEFINER到達（本体の解析待ち）。

### スコープ外

FETCH（カーソルの列は静的に決まらない）。EXPLAINの実行（embedded PGの統計は本番と違う。性能の助言は構造的に判定できるもの、インデックスの先頭列とビューへの述語押し込みに限る）。PostgreSQL以外のRDBMS（共通化しようとした瞬間にDSLに戻る）。

## コード配置

| package | 役割 |
|---|---|
| `sqlshape` | 公開API: `Query[R, P]` / `One[R, P]` / `Batch` / `Copy[R]` / `MatView`、pgx上のランタイム（行マッパー、型登録、`ConstraintError`、描画のバイト比較） |
| `pgtest` | schema.sqlを適用した本物のPGをアプリケーションのテストに提供する。`Verify`がアナライザーとPGを突き合わせる |
| `internal/expand` | テンプレートの全展開。`{{.X}}` → `$n`と`P`上のパス |
| `internal/schema` | libpg_queryでschema.sqlをカタログの上に載せる。テーブル・ビュー・enum・ドメイン・複合型・関数・制約・ポリシー・seed |
| `internal/catalog` | 埋め込みのpg_catalog（PG 17の型・関数・演算子・キャスト・集約）と拡張のdump |
| `internal/analyze` | アナライザー本体。10章の型変換、スコープ、DML、`$n`推論、nullability、カーディナリティ、違反の列挙、PG互換のエラー |
| `internal/vet` | `go/analysis`アナライザー。結果列 ↔ `R`、`$n` ↔ `P`、束縛、expect行、境界、SuggestedFix |
| `internal/oracle` | 差分テストのオラクルとしての本物のPG（embedded-postgres）。検査時には使わない |
| `internal/dump` / `diff` / `migrate` / `consumers` / `cli` | マイグレーション側。pg_dumpによる正準形、オブジェクト単位のdiff、DDL生成と終点検証、消費者索引、サブコマンド |

## 検討中

- PL/pgSQLの本体を解析する。DB側にロジックを置く方針なのに、トリガー関数と書き込み関数の大半を占めるPL/pgSQLが読めないと、関数経由の失敗モード・消費者索引・SECURITY DEFINERの到達がすべて本体の手前で止まる。現在の`-- sqlshape: error`注釈は本体が読めないことの代替。パーサはlibpg_queryの`pg_query_parse_plpgsql`。本体内のSQL文は既存のアナライザーで検査し、足すのはPL側の層: `DECLARE`（`%TYPE` / `%ROWTYPE` / `RECORD`）、代入と`INTO`、`FOR`の変数、制御フロー、`RETURN`の型、トリガーの`NEW` / `OLD` / `TG_OP`、`RAISE ... USING ERRCODE`からの失敗モード。`EXECUTE`は定数文字列のみ検査し、それ以外は未検査として報告する。先行例はplpgsql_check
- 2-way SQL構文（`/*{{.X}}*/'lit'`）。psqlでそのまま流せるテンプレート。expandとRenderの前段で同じ変換を入れる
- ORMからの移行支援。ORMが発行したSQLを観測して`Query[R, P]`に起こす
- PGの新バージョンへの追従を機械化する。回帰コーパスの取り込み → 差分 → 修正のループ
