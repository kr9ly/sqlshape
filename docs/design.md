# 設計上の裁定

sqlshapeを開発する人（と、開発を手伝うエージェント）向けに、設計の判断を「何を決めたか・なぜか・何を棄てたか」の形で残す。振る舞いの説明は利用者向けの[README](../README.ja.md)と`docs/`にあり、ここには繰り返さない。判断が変わったら該当項目を書き換える。経緯はgitの履歴とコミットメッセージにある。

## 出発点

RDBMSを使うアプリケーションに要るのは4つ。SQLの構文と型の検査、型安全な値のバインド、結果の構造体へのマッピング、スキーマから導けるマイグレーション。既存のツールは、このうち2〜3個を満たして残りを落とすか、4つ揃えるためにSQLをDSLに翻訳させる形を取る（jOOQ / Kysely / Drizzle）。前の3つは「SQLが正」、最後は「アプリの型定義が正」と真実の源が割れていることが、その背景にあると見ている。sqlshapeは真実の源を`schema.sql`とSQLそのものに置き、アプリの型は照合される側にする。

## 核となる裁定

### 検査器はpure Goで持ち、本物のPostgreSQLはテストのオラクルにする

決めたこと。PostgreSQLのカタログ（`pg_type` / `pg_proc` / `pg_operator` / `pg_cast` / `pg_aggregate`）を本物のPGからCOPYでdumpしてTSVとして埋め込み、型変換はマニュアル10章の規則をそのまま実装する。埋め込みPostgreSQL（embedded-postgres）は差分テストの正解役としてだけ使い、検査時には起動しない。

理由。nullabilityはPGのDescribeが返さないので自前で式の木を歩く必要があり、そうするなら型も同じ木の走査に載せた方が解析器が1つで済む。WHEREやJOINの条件に現れる列参照はDescribeでは見えず、消費者索引（どの文がどの列を読むか）には全数の参照が要る。

棄てた案。① embedded PGにschema.sqlを流してPREPARE / Describeで聞く（当初案。起動コスト、nullabilityと参照の全数が取れない）。② libpg_queryの手法を`analyze.c`まで広げて偽カタログで動かす（研究課題に近い）。③ PGをwasmで動かす（実用段階か未確認）。④ カタログをPGソースの`.dat`から生成する（COPY dumpならデフォルト値解決済み・OID確定・拡張も同じ経路）。

忠実度の測り方。PGの回帰テスト`src/test/regress`の全文を本物のPGと並走させ、パラメータ型・結果列・エラーの一致を`go test`でゲートする（`check/postgres/analyze/regress_test.go`）。残る不一致は環境依存のcollation、RLSの再帰、権限、サーバー内部エラー、意図的な相違3件で、いずれも静的解析の外。

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

### PL/pgSQLの本体も解析する

決めたこと。`LANGUAGE plpgsql`の関数本体をlibpg_queryのPL/pgSQLパーサで構造に分け、埋め込まれた各SQL断片を既存のアナライザーで検査する（`check/postgres/analyze/plpgsql.go`）。PL変数は関数パラメータと同じ経路（`funcParam`）でスコープに入れ、`plVar`印で`$n`から外し、変数と列の衝突はPostgreSQLの既定（`variable_conflict = error`）どおりエラーにする。record変数の形はそれを埋めたクエリから取り、トリガー関数は`CREATE TRIGGER`の結びつきごとに`NEW` / `OLD`をそのテーブルの行型として解析する（結びつきの無いトリガー関数は解析しない）。`RAISE`のSQLSTATEは本体から拾い、`-- sqlshape: error`注釈は名前を付ける役に退く。`EXECUTE`は定数文字列だけ検査し、それ以外は`-strict`の助言。

理由。DB側にロジックを置く方針なのに、トリガー関数と書き込み関数の大半を占めるPL/pgSQLが読めないと、関数経由の失敗モード・消費者索引・SECURITY DEFINERの到達がすべて本体の手前で止まり、READMEの主張が成り立たない。パーサが本体を構造化してくれるので、足したのはPL側の層だけで済んだ。

対象外。動的SQL（`EXECUTE`の非定数）、カーソル経由で取り出した行の型（`FETCH INTO`の先は形が不明なrecordとして扱い、フィールド参照は型不明で通す）。

### 境界の規則は義務として宣言し、文の事実で判定する

決めたこと。`visible where`・`-require-columns`・`-no-table-reads` / `-no-tables`は、表（ビュー）が参照側の文に課す**義務**の実例として一つの仕組みに載せる。宣言は`schema.sql`の`-- sqlshape: require <body> [on <kinds>]`（bodyはSQLのboolean式、`pinned(列)`、`immutable(列)`、`via view`）。アナライザーは判定せず、文が証明できること（葉・正規化述語・等値の閉包・代入集合）を`x/facts`のデータとして出し、`internal/obligation`が「事実 ⇒ 義務」を判定して履行経路（文自身 / ビュー / ポリシー / 複合FK / waive）ごと返す。`aggregate`は義務の束を生む宣言、`context`は文脈ごとの差分、`sqlshape check`はGoの外の文への同じ判定の入口。従来の構文とフラグは略記として残す。経緯・設計・ロードマップは[obligations.md](obligations.md)。

理由。規則を一つずつ実装すると「ポリシーが満たす」「ビューが運ぶ」のような特例が規則の数だけ増える。判定を一本にすると新しい規則は宣言で済み、方言を足すときも「Factsを出すアナライザー」と「述語を落とすLowerer」だけで判定を共有できる。核（アナライザー・スキーマ・vet）はobligationを知らない。

棄てた案。文をまたぐ規則（トランザクション内の対）を扱う層（分析の単位は文、という核の裁定と衝突する）。方言をまたぐ義務DSL（SQL式と少数の構造述語で足りる）。opt-outの命名規則によるパッケージ→文脈の写像（暗黙は読めない。パッケージコメントの`// sqlshape: context`とフラグで明示する）。

### PostgreSQLの版は`schema.sql`が宣言し、版ごとに本物の文法とカタログで判定する

決めたこと。`schema.sql`は`-- sqlshape: postgres 18`と1行で、どの版のPostgreSQL向けかを宣言する。宣言は必須で、無いファイルは読まない。この1行が、スキーマと全部の文を読む文法、型・関数・演算子を解決するカタログ、`pgtest`とマイグレーション系コマンドが起動するPostgreSQLの版を決める。パーサはlibpg_queryを版ごとにWebAssemblyへビルドして埋め込み、wazeroで実行する（`check/postgres/pgparse`）。カタログは版ごとにその版の埋め込みPostgreSQLからdumpしたTSV（`check/postgres/catalog/data/<major>/`）、オラクルとregressコーパスも版ごと。ノードのGo型は最新版の`pg_query.proto`から生成した1組だけを持ち、どの版の木もパーサのJSON出力をprotojsonで読む。古い版で名前が違うフィールドは読む前にJSON上で最新版の形に書き換える（`pgparse/upgrade.go`。17→18は`returningList`→`returningClause.exprs`、`is_enforced=true`、`generated_kind="s"`と、生の木には現れない3フィールドの削除）。版で判定が変わる規則はアナライザー内の版分岐（RETURNING old/new、セッションTZの略称、数値フィールドの厳格化、aclitemの引用符、JSON_VALUEの照合衝突、jsonpath引数の型、CTE越しの外側集約）。`diff` / `apply` / `verify-schema`は接続先の`server_version`が宣言と違うメジャー版なら警告して続行する。

理由。宣言版より新しい構文をその版の本物の文法が拒否するので、gram.yの差分表を持たずに済む。cgoが消えて`go install`にCツールチェーンが要らず、1つのランナーで全プラットフォームをクロスコンパイルできる。wasmモジュールは隔離されるので版ごとのパーサを同じバイナリに複数積める。JSON経路を選んだのは、protobufのフィールド番号をlibpg_queryが版ごとに振り直すためで（17→18で267フィールド、enum値434が変わった）、JSON経路のほうがprotobuf経路より速くもあった（1文215µs対273µs）。書き換え漏れはprotojsonが未知フィールドとして拒否するので黙って消えない。

棄てた案。① pg_query_goをforkしてcgoのまま版を積む（版ごとに別バイナリになる。pg_query_goのリリースを待つ形も変わらない）。② 版を宣言しないスキーマに最新のサポート版を既定として当てる（当初案。「既定が動いて何が判定されたか分からない」より、1行足してもらう方がよい。既定`pgparse.Default`はコードから組むスキーマ用のAPI上のフォールバックとして残る）。③ 版ごとのGo型を持つ（アナライザーを型で分岐させることになる）。④ 版ごとにprotobufのフィールド番号を変換する（JSONで済む）。

忠実度。regressコーパスをその版の本物と並走させた結果、17は22,103文で既知の不一致35、18は23,384文で47（`testdata/regress_baseline_<major>.txt`）。18の47は17の既知集合と、RETURNING old/newのnullability 10件（INSERTのold・DELETEのnewはアナライザーがnullable、オラクルは列のNOT NULLしか見えない）、DO INSTEADルール付きDELETE RETURNINGの1件（アナライザーはPGと同じ0A000、オラクルのDescribeが空を返す）。pgparseの`TestCorpus`は17の43,226文が書き換え経由で読め、18のJSON経路がprotobuf経路と全一致することを検問する。

追従。新しいメジャー版は「版を1つ足す」作業で、手順は[`check/postgres/pgparse/README.md`](../check/postgres/pgparse/README.md)にまとめてある。libpg_queryのリリースを待つ形は変わらない（18は8か月遅れた）。サポート窓はPGの5年に合わせるが、まず17と18の2版でこの構造を作った。メジャー間で判定が変わるのは、構文の追加、カタログの追加・オーバーロードによる曖昧化・稀な削除、予約語の追加、ごく稀な型規則の変更で、regressコーパスの差分がそれを列挙する。

#### 検査器が本体で、ランタイムは DB ごとの別ライブラリ（2.0）

裁定 2026-09-10。sqlshape の約束は「`schema.sql`に対してテンプレートの有限展開を静的に検査する」で、それはマーカー`sqlshape.Query[R, P](tmpl)` / `One`ひとつで閉じる。SafeQL が使うクライアントライブラリを問わないのと同じ位置に立つ: 検査対象はマーカーで、ランタイムは「あると便利な 1 実装」。

- ルートモジュール`github.com/kr9ly/sqlshape/v2`は依存ゼロの核だけを持つ: `Query` / `One` / `Stmt` / `Render`（テンプレートの意味論 — `{{.X}}`のプレースホルダ化と分岐の有限集合）と、依存の無い検査器の共有部（`x/expand` / `dialect` / `facts` / `obligation`）。利用者の go.mod に pgx も wazero も載らない
- ランタイムは DB ごとの別モジュールで、互いに似ていなくてよい。`sqlshape/postgres`は pgx の自然な形（`iter.Seq2`、Batch、Copy、pgtype のスキャナ、`MatView`、`ConstraintError`）、`sqlshape/mysql`は database/sql の形（`?`描画、`*sql.Rows`）。共通シグネチャは作らない。受け型の表はドライバごと
- マーカーは設定できる。既定は`sqlshape.Query` / `One`、`-query=pkg.Func`で利用者自身の generic 関数`F[R, P any](string) T`を登録できる。ランタイムを自作してよい。定数の生 SQL（`db.Query(ctx, "SELECT …")`）は R / P 無しの薄い検査（構文・名前・パラメータ個数）
- 失うものは明示する: `-raw-sql=forbid`の「検査済み SQL しか実行されない」、`expect`行と`ConstraintError`の対応、`One`の実行時`ErrManyRows`はランタイム側の契約で、sqlshape のランタイムを使ったときだけ成立する。docs は「静的に約束すること」と「ランタイムを使えば加わること」を分けて書く
- 検査器も DB ごとのモジュール: `sqlshape/check/postgres`（Apache。analyze / schema / catalog / pgparse / oracle / verify / dump / diff / migrate。パッケージは公開だがツール向けで互換性の約束はしない）と`sqlshape/check/mysql`（GPLv2、旧`mysql/`）。`sqlshape/pgtest`は check/postgres の上。`sqlshape/cmd/sqlshape`（GPLv2）が vet と cli を内包して全部を積む。7 モジュールを 1 つの版番号でロックステップ、2.0.0 から
- DB × 言語の行列: Go の各 DB が`sqlshape/<db>`、TypeScript は別リポジトリの`@sqlshape/<db>`、`check/<db>`は言語非依存で共有、言語フロントエンド（vet）は`cmd/`側
- 退けた案: ルートに PG ランタイムを残して MySQL だけ別モジュール（依存の混入が残る）、ランタイム間で`Run(ctx, db, stmt, p)`の関数形を揃える（揃える理由が無い）

## スコープ外

FETCH（カーソルの列は静的に決まらない）。EXPLAINの実行（embedded PGの統計は本番と違う。性能の助言は構造的に判定できるもの、インデックスの先頭列とビューへの述語押し込みに限る）。初期リリースではPostgreSQL以外のRDBMS（下記「検討中」のMySQL参照）。

## コード配置

| package | 役割 |
|---|---|
| `sqlshape` | 宣言の核（依存ゼロ）: `Query[R, P]` / `One[R, P]` / `Stmt` / `Render`、行の束縛規則（`Fields`）、`Labelled` |
| `postgres` | pgx 上のランタイム（別モジュール）: `Run` / `Collect` / `First` / `Exec` / `Get` / `Find`、`Batch` / `Copy[R]` / `MatView`、行マッパー、型登録、`ConstraintError`、描画のバイト比較） |
| `pgtest` | schema.sqlを適用した本物のPGをアプリケーションのテストに提供する。`Verify`がアナライザーとPGを突き合わせる |
| `x/expand` | テンプレートの全展開。`{{.X}}` → `$n`と`P`上のパス |
| `check/postgres/pgparse` | パーサ。libpg_queryを版ごとにWebAssemblyへビルドして埋め込み、wazeroで実行する。`Version.Parse` / `Deparse` / `SplitWithScanner` / `ParsePlPgSqlToJSON`と、最新版のprotoから生成したノード型 |
| `check/postgres/schema` | libpg_queryでschema.sqlをカタログの上に載せる。テーブル・ビュー・enum・ドメイン・複合型・関数・制約・ポリシー・seed |
| `check/postgres/catalog` | 埋め込みのpg_catalog（版ごとの型・関数・演算子・キャスト・集約、`data/<major>/`）と拡張のdump |
| `check/postgres/analyze` | アナライザー本体。10章の型変換、スコープ、DML、`$n`推論、nullability、カーディナリティ、違反の列挙、PG互換のエラー。文の事実（`facts.Facts`）の生産と、義務の述語を事実の言語に落とす`Lower` |
| `x/facts` | アナライザーと義務検査の間のデータ契約。文種・スコープの木・葉・正規化述語・等値の辺・固定列・代入集合。パーサのノードを含まず、方言を知らない |
| `check/postgres/obligation` | 境界の規則。schema.sqlの`require` / `visible where`とvetのフラグを義務に読み、事実に対して履行を判定する（[obligations.md](obligations.md)）。`analyze`にも`vet`にも依存しない |
| `cmd/sqlshape/internal/vet` | `go/analysis`アナライザー。結果列 ↔ `R`、`$n` ↔ `P`、束縛、expect行、SuggestedFix。境界の規則は`obligation`に委ね、フラグを義務に展開して渡す |
| `check/postgres/oracle` | 差分テストのオラクルとしての本物のPG（embedded-postgres）。検査時には使わない |
| `check/postgres/dump` / `diff` / `migrate` / `consumers` / `cli` | マイグレーション側。pg_dumpによる正準形、オブジェクト単位のdiff、DDL生成と終点検証、消費者索引、サブコマンド |

## 検討中

- MySQLを、PostgreSQLとは別の実装としてサポートする。初期リリースの範囲外。方言をまたぐ共通DSLは作らない（作った瞬間にSQLをDSLに翻訳させる形に戻る）。代わりにプラットフォームごとに、そのDBの構文と型規則を実装したアナライザーと、そのDBのdumpを使うマイグレーション支援を持つ。共有するのはテンプレートの展開、`go/analysis`のフロントエンド、Go側の型照合の枠組み
  - ライセンスと構成（裁定 2026-09-09、改訂）: MySQLサーバはGPLv2で、Apache 2.0のsqlshapeと同じ成果物には混ぜられない（libpg_queryの形が成立したのはPostgreSQL Licenseが寛容だったから）。守るのは「利用者のランタイムにGPLが混入しない」の1点で、優先するのは利用者の使いやすさ。MySQLとPostgreSQLで別のバイナリを使い分けさせることはしない。そこでモノレポの中でモジュールを2種に分ける。① Apache 2.0のまま残るもの: 利用者のコードにリンクされる`sqlshape`（ランタイム）と`pgtest`、`pgtest`が使うPostgreSQL側の解析（`check/postgres/analyze` / `schema` / `catalog` / `pgparse` / `oracle` / `verify`）、テンプレート展開、義務、vetのフロントエンド。import pathは`github.com/kr9ly/sqlshape/v2`のまま動かさない。② GPLv2に置くもの: MySQLの文法・字句解析器・関数表を切り出したwasmとそのアナライザー（`github.com/kr9ly/sqlshape/check/mysql/v2`、ネストしたモジュール）、そして両方を積む1つのバイナリ`cmd/sqlshape`（ネストしたモジュール、GPLv2。Apache→GPLの一方向importは互換）。バイナリは`schema.sql`の宣言（`-- sqlshape: postgres 18` / `-- sqlshape: mysql 8.4`）で方言を選び、vetとしてもマイグレーションとしても同じ1つで済む。利用者のアプリにリンクされるのは①だけなので、GPLは開発ツールに閉じる（gccと同じ）。`internal/`の参照はパスの接頭辞で判定されるのでネストしたモジュールからも通る見込みだが、これとネストしたモジュールのタグ付け（`cmd/sqlshape/vX.Y.Z`）と`go install`の解決はスパイクで確認する。退けた案: MySQL対応を別リポジトリ・別バイナリ`sqlshape-mysql`にする（当初の裁定。利用者にDBごとのバイナリを使い分けさせることになる）、パーサを別プロセスにしてパイプで木を渡す（FSFは密なデータ交換を結合とみなし得るとしていて灰色、構造も歪む）、Apache 2.0の互換パーサ（TiDB parser / Vitess）を使う（原則を捨てる）、抽出を事実に限定して文法を手書きする（一番品質が欲しい文法が手書きになる）
    - バージョン（裁定 2026-09-10）: 3 モジュールは 1 つの版番号でロックステップ、全部 1.x（`cmd/sqlshape`の`/v2`は外した。MySQL 追加は additive なので minor が semver 上も正しく、ランタイムの import path も動かさない）。同一コミットに`vX.Y.Z` / `mysql/vX.Y.Z` / `cmd/sqlshape/vX.Y.Z`を打ち、`scripts/release.sh`が go.mod の相互 require を揃えてコミット・タグ付けする。release.yml はルートの`v*`で発火。退けた案: モジュールごとに独立した番号（root 1.x / mysql 0.x / cmd 2.x、多段の手順が要る）、root も`/v2`に上げて 2.x で揃える（ランタイム利用者の import path が変わる）
  - パーサと字句解析器（裁定 2026-09-09）: MySQLサーバのソースから機械的に切り出し、libpg_queryと同じ形でwasmにする。原則は「本物から機械的に抽出したものを土台にし、手で書く部分を最小にする」。文法は`sql/sql_yacc.yy`（bison、18k行、`%expect 59`）からC++のアクション（`NEW_PTN`が923箇所）を剥がして規則・優先順位・トークンだけを残し、規則名と子要素だけの素の構文木（CST）を組む汎用アクションを機械生成してbisonに通す。字句解析器は`sql/sql_lex.cc`の`lex_one_token`（726行）と2トークン先読みの`MYSQLlex`（`WITH ROLLUP`等の合成）、`Lex_input_stream`（440行）、`find_keyword`（`sql/lex.h`の784語の表から`gen_lex_hash`が生成する完全ハッシュ）、`consume_comment`・`int_token`をそのまま使う。測ったところ、字句解析器がサーバ状態を読むのは5点だけ — `sql_mode`（`ANSI_QUOTES`の1箇所）、`default_collation_for_utf8mb4`、接続文字集合、コメント収集の`m_parser_state`、非推奨文字集合の`push_warning`（2箇所）— なのでTHDはこの5点を持つ数十行のshimで置き換える。文字の分類（`state_map` / `ident_map`）は`strings/sql_chars.cc`が文字集合のctype表から作るもので、`strings/`ライブラリごと取り込む（PGの`src/port`をlibpg_queryが抱えるのと同じ）。バージョンコメント`/*!80000 ... */`は`MYSQL_VERSION_ID`との比較なので、版ごとにビルドすれば自然に正しい。sqlshape用のASTへの変換だけが手書きで、CSTの規則名を読む。生成手段はこの裁定でbison + emccに決まる（goyaccでpure Goに持つ案は字句解析器の書き直しを伴うので原則に反する）。残る確認はスパイク: shimの表面積が本当に5点で閉じるか、抽出した文法とlexerがemccで通るか、`mysql-test/t`（1810ファイル）の受理率
    - スパイクの結果（2026-09-09、`mysql/spike/`）: 三点とも成立。shimの表面積は5点で閉じた（警告・digest・utf8本文・オプティマイザヒントは空実装）。アクションを剥がした文法はbison 3.8で`%expect 59`のまま通り、トークン番号は本物と一致する。`mysql-test/t`の1,455ファイルを文に割った137,062文のうち、エラーを期待しない125,187文の99.89%を受理し、残りはテストハーネス側の事情（delimiter、クライアント文字集合の切り替え、パーサ深さの試験）で文法の穴は見つかっていない。MySQLがアクション内で出す構文エラー（予約関数名のテーブル名、`<=>ALL`）はCSTパーサでは通るので、意味層で拾う
    - CST→AST（裁定 2026-09-09）: 剥がしたアクションの中身が CST→AST の対応表なので、parsegenがそれを読み戻して`shapes.go`（規則×選択肢 → server が何を組んだか）を生成し、`mysqlast`が汎用に再生する。ノードは server の PT クラス（`PT_query_specification` / `PTI_where` / `Item_func_eq`）で、引数名はヘッダのコンストラクタから、by-value の struct のフィールド名は`parser_yystype.h`から取る。読めないアクション（到達範囲で約 45）は hook を手書きし、hook は規則名と右辺の記号列で結び、版が上がってずれれば init で落ちる。読めもせず hook も無い構文は`Unsupported`として明示する。mysql-test の DML/DDL の 97.7% が組める
    - アナライザーが読む木（裁定 2026-09-09）: mysqlast を直接読む（PG 側が pg_query のノードを直接読むのと同じ位置づけ）。parsegen が持つコンストラクタ引数名から PT クラスごとの型付きビュー（`AsPTQuerySpecification(v)` と引数名のアクセサ）を生成して、アナライザーはそれを通す。MySQL 専用の正規化した文モデルは作らない。版ごとの揺れは hook と引数名の照合で露見させ、吸収層が要るかは SELECT の解析を書いてから判断する
- 方言の口（裁定 2026-09-09）: vet が`analyze.Result` / `schema.Schema` / `pgparse`に直接依存しているので、ルートに`x/dialect`を切る。`Load(schemaSQL) (Analyzer, error)`、`Analyzer.Analyze(sql) (*Result, error)`、Go 型 ↔ SQL 型の対応表を方言が提供し、`schema.sql`の先頭宣言（`-- sqlshape: postgres 18` / `-- sqlshape: mysql 8.4`）で選ぶ。`Result`は方言非依存の形（型は名前と NULL 可否、出所はテーブル・列名、`facts.Facts`）に寄せる。PG 実装は`check/postgres/analyze`の薄い adapter、MySQL 実装は`mysql/`側。vet が pgparse に触る 2 箇所（ORDER BY の hazard、束縛の定数）は方言側へ。SQLite 等の第三の方言もこの口に足すだけで済む形にする（パーサは同じ手法 — 本物の文法からの機械抽出 — で、Lemon の文法なら parsegen に相当する生成器をもう 1 つ書く）
    - 方言間の線引き（裁定 2026-09-10）: 機能は方言の事実として現れるだけで、共通化しない。契約（`One`の証明・義務・失敗モード）は事実（`facts.Facts`: どの列を読む・固定する・書く、どのキーで一意になる、どの制約に当たり得るか）の上に書く。契約が必要とする事実をその方言が生産できないなら、その契約はその方言ではエラーにする（黙って通さない）。これで「PG にある機能を MySQL でどう真似るか」は問いとして消え、「この契約が要る事実は何か、その方言はそれを出せるか」に置き換わる。3 層で見る: ① SQL の意味論（文法・型・エラー・名前解決）は各 server がそのまま正で、oracle で検算する ② sqlshape の契約は事実の上 ③ Go の受け型と実行時はドライバごとの表。含意: `dialect.Result`に Facts を載せて obligation を方言非依存にするのが次の設計課題。方言にだけある機能と契約の交差（`INSERT IGNORE`は失敗モードを飲み込む、`ON DUPLICATE KEY UPDATE`は`ON CONFLICT`相当、`RETURNING`が無いので「書いた値を読む」規則が成立しない）は個別に裁定する。**ドキュメント**: 何を約束し何を約束しないかが方言ごとに変わるので、利用者向け docs は方言別の約束の表として整理し直す。実装より難しいと見ていて、契約の結線を MySQL に通すタイミングで再度整理する
    - 実施（2026-09-09、縦串）: `x/dialect`は宣言の読み取り（`Declared`、`-- sqlshape: <name> <version>`で name が postgres か登録済みの方言のもの）・`Register` / `Lookup`のレジストリ・方言非依存の`Analyzer { Analyze(sql) (*Result, error); Problems() }`と`Result { Params []Type; Columns []Column }`（`Type`は方言の綴りと Go 型名の列、`Column`は名前・型・NULL 可否）だけ。vet は宣言が postgres 以外のとき`dialect.go`の細い経路（展開 → Analyze → 解析エラー / 結果列 ↔ R / パラメータ ↔ P）を通り、PG 経路は無変更で残す。PG を`Result`に寄せる作業は、`Result`が義務・違反・基数・束縛を運べるようになってから。プレースホルダは展開器の`$n`を方言側が翻訳する（MySQL は`?`。`$`は MySQL の識別子に使えるので、引用符・識別子の中は触らない）。MySQL 実装は`mysql/check/postgres/analyze`（`Analyze(schema, sql)`、独自の`Result`）と公開パッケージ`mysql/dialect`（登録と Go 型表）、`cmd/sqlshape`が blank import する
    - 文字集合レジストリ（裁定 2026-09-09）: `strings/`の照合ライブラリを丸ごとリンクする（ctype-*.cc、collations*.cc、`uca9dump`が生成するzh/jaの重み表）。wasmは4.6MBで、名前だけの表なら0.9MBだが、手で持つ表をゼロにすることと、型層でPAD SPACE・mbmaxlen・既定照合を参照するときにレジストリがそのまま答えになることを優先した。サイズが問題になったら重み表（`uca900_data.h`とhan表、約3MB）だけを空にする
  - `sql_mode`は判定を変えるので、版と同じくschema.sqlが宣言する（`-- sqlshape: mysql 8.4 sql_mode=...`の形は未決）。字句では`ANSI_QUOTES`、文法のアクションを剥がすので`PIPES_AS_CONCAT`（`||`）の意味はCST→ASTの変換で`sql_mode`を見て決める
  - オプティマイザヒント（`/*+ ... */`）は別の文法`sql/sql_hints.yy`（725行）と`Hint_scanner`を持つ。同じ手順で後から足せる。sqlshapeの判定には効かないので初期は読み飛ばす
  - カタログ（裁定 2026-09-09、測定に基づく）: MySQLには`pg_proc`に当たる表がないが、組み込み関数の定義はソースにまとまっていて機械抽出できる。①関数表`sql/item_create.cc`の`func_array`（347エントリ）: 名前 → Itemクラス × 引数個数（固定 / 最小・最大 / 偶奇）。②キーワード構文の関数（`CAST`・`TRIM`・`DATE_ADD`等、81クラス）は`sql_yacc.yy`のアクションの`NEW_PTN Item_func_*`に書かれているので、アクションを剥がす前に「規則 → Itemクラス」として拾う。③結果型の種別はItemクラスの継承から取れる: 登録277クラスの直接の基底は`Item_int_func`（77、LONGLONG）・`Item_str_func`（53、文字列）・`Item_dec_func` / `Item_real_func`（21、DOUBLE）・`Item_json_func`（10）・`Item_bool_func`（8）・日時系（9）・空間系（33）で大半が決まる。④引数型・NULL可・正確な結果型は各クラスの`resolve_type`（257個）にあり、その多くが宣言的な呼び出し — `param_type_is_default(from, to, TYPE)`（97箇所、引数の既定型）、`set_data_type_string / longlong / double / decimal / datetime / date / time / json`、`set_nullable`（82）、`aggregate_type`（引数型の合成）— で書かれているので、本体を字句的に読んで表にできる。⑤宣言的でない残り（`unsigned_flag`の伝播、長さ計算、引数で結果型が変わるもの）と④の検算は、本物のmysqld（配布tarballを`--initialize`で起こす。embedded-postgresの役）に①②から生成した`SELECT f(?, ...)`をプリペアドステートメントで問い、結果メタデータで埋める。「何が存在し何個の引数を取るか」を手で列挙しないのが要点で、抽出した表が計測の入力になる。型規則（暗黙変換・数値の昇格・文字列と数値の比較）は`Item_func::aggregate_type`と`item_cmpfunc.cc`の比較種別決定（`get_cmp_type`相当）を読んで移し、mysqldとの差分テストで検算する
    - 型規則の実施（2026-09-10）: `mysql/check/postgres/analyze/types.go`が server の規則をそのまま持つ — `Item_num_op::set_numeric_type`（REAL > DECIMAL > INT の昇格、`numeric_context_result_type`で日時は整数・文字列は実数扱い、`result_precision`の unsigned 伝播）、`Item_func_num1`（1 引数の数値関数、`Item_func_int_val`は小数を scale 0 に）、`Item::aggregate_type`（NULL を飛ばして`field_type_merge`で畳み、符号が混じる整数は一段広げる）。`field_types_merge_rules`（30×30）と`field_types_result_type`は手で写さず parsegen が`sql/field.cc`から抽出して catalog に生成する（`catalog.Merge` / `catalog.ResultKind`）。関数は catalog の Item クラスで判定: family が結果種別を決め、facts（`set_data_type_*`、`set_nullable`、`unsigned_flag=true`、`param_type_is_default(from,to,TYPE)`、`param_type_uses_non_param`）が型・NULL 可否・プレースホルダの型を補う。ヘッダにインラインで書かれた`resolve_type`（109 個）も parsegen が読むようにした（facts を持つクラス 144 → 323）。NULL 可否の既定は「引数のどれかが NULL 可なら NULL 可」（`Item_func::fix_fields`）、集約は「行が無ければ NULL」で COUNT / BIT_* / 窓関数だけ例外。読めないもの（`Item_temporal_hybrid_func`の ADDTIME / DATE_ADD、サブクエリ、ユーザー変数）は未型付けで通す
    - mysqld オラクル（2026-09-10、実施）: `mysql/check/postgres/oracle`。PATH の`mysqld`（`nix-shell -p mysql84`。配布 tarball の bin/ でも同じ）を版ごとに一度`--initialize-insecure`して`~/.cache/sqlshape/mysqld-<version>/template`に置き、Start ごとに一時ディレクトリへ複製して unix socket・networking off で起こす（約 2 秒）。`Describe(sql)`は SELECT を全パラメータ NULL で実行して結果セットのメタデータ（型・unsigned・NULL 可否・長さ・precision）を読む。書き込み文は prepare だけ。プレースホルダの型は COM_STMT_PREPARE が全部 VARCHAR で返すので検算できない。`analyze`の`TestOracle`が`analyzeCases` / `errorCases`の全文を server に問い、列名・型・NULL 可否・エラー番号の一致をゲートにする（mysqld が無ければ skip）。初回の突き合わせで直ったもの: 文字列関数は strict mode で常に NULL 可（`Item_str_func::fix_fields`）、JSON 関数と型キャストも NULL 可、FLOOR / CEILING は桁が収まれば bigint、文字列リテラルの列名は値そのもの、`IN (subquery)`は NULL 可、`x.*`の不明テーブルは 1051。版の差（文法は 8.4.6、oracle は 8.4.11）は同じ LTS 系列なので許容し、`Oracle.Version`に記録する
    - 関数表の差分テスト（2026-09-10、実施）: `analyze`の`TestProbe`が registry の 347 関数 × 代表引数型（int / bigint unsigned / decimal / double / varchar / varbinary / datetime / date / time / json / NULL 可 int、文字列先頭＋整数、整数先頭＋文字列、末尾 NULL）× 引数個数で 5,267 文を生成し、server と analyzer の答えを`testdata/probe.golden`に 1 文 1 行で残す（`=` 一致 / `?` 未型付け / `!` 不一致 / `x` server が拒否 / `X` analyzer が拒否）。比較は型名と符号と NULL 可否で、文字列は「文字列 / バイナリ列」の 2 クラス（長さを計算しないので VARCHAR と TEXT の境目は判定できず、Go 側の受け型は同じ）。到達点: 一致 4,994、未型付け 29、不一致 10（REGEXP_REPLACE の NULL 可否 9、QUOTE の照合 1）、server 拒否 234（引数型の検査はしない）。この過程で直したもの: 文字列関数の結果照合（Item の既定はバイナリ、`agg_arg_charsets_*`があれば引数から、明示の charset があればそれ）、`Item_geometry_func`の NULL 可、コンストラクタと`fix_fields`に書かれた`set_nullable` / `unsigned_flag`（parsegen がクラス本体も読む）、`fix_fields`の連鎖（`Item_str_func::fix_fields`に届くかで strict の NULL 可を決める）、`return Base::resolve_type()`形の継承、`set_data_type_*`は最後の文が勝つ、NULLIF / UNIX_TIMESTAMP / FROM_UNIXTIME / ST_X 系の instantiator、schema ローダーの BINARY(n) / VARBINARY(n)（parsegen が同じ引数個数の別コンストラクタで名前付けしていた）。golden の変更はレビュー対象
  - カタログの検算（裁定 2026-09-09）: mysqld オラクル（配布 tarball を`--initialize`で起こす）はカタログ生成のときに手元でだけ使い、生成物をコミットする。CI は検算しない（PG の`catalog/gen`と同じ扱い。tarball は 500MB 級で CI に載せる価値がない）。tarball は`~/.cache/sqlshape/mysql-<version>`に置く
  - 回帰コーパスはMySQL自身の`mysql-test/t/*.test`。パーサの受理率と、オラクルとの差分probeの両方に使う
  - 順序: パーサのスパイク（受理率で手段を裁定）→ 字句解析器 → CST→AST → スキーマローダー（CREATE TABLEの部分集合、`mysqldump`の正準形）→ アナライザーの型規則（mysqldをオラクルに差分テスト）→ テンプレート・vet・義務の結線（ここは共有部分なので薄い）。1.2の後
- TypeScriptフロントエンド。2つ目の言語フロントエンドで「核はSQLしか見ない」構想を検証する。言語非依存で共有するのは`check/postgres/analyze`、`check/postgres/schema`、マイグレーション一式、失敗モード・`One`の証明・境界の規則。言語依存はその言語のデファクトに合わせる: 呼び出し箇所の発見（TypeScript Compiler APIかtsc plugin、ESLintルール）、`R` / `P`相当の型解決（`query<R, P>(sql)`）、PG型とTS型の対応表（pg / postgres.jsが実際に返す型）、ランタイム。最初の設計論点はテンプレート構文で、Goの`text/template`を持ち込まず、`sql\`...\``のタグ付きテンプレートと`${}`の断片で「有限の展開集合」を保つ方法（断片をリテラルの選択肢に限る、など）を決める。MySQLと同じく、`cmd/sqlshape/internal/vet`のPG固有型の切り出しが前提。横展開の見立て: 検査の大半（列と型の存在、失敗モード、`One`の証明、境界の規約、インジェクション保証、マイグレーション）はSQL側だけで閉じていてホスト言語の型を見ない。言語ごとに必須なのは「lintの口」「テンプレート定数の取り出し」「ランタイム」の3つで、`R` / `P`の型照合は静的型がある言語で足す層（動的型の言語では実行時の行マッピング検査か、型ヒントがあれば静的に）
- 2-way SQL構文（`/*{{.X}}*/'lit'`）。psqlでそのまま流せるテンプレート。expandとRenderの前段で同じ変換を入れる
- ORMからの移行支援。ORMが発行したSQLを観測して`Query[R, P]`に起こす
