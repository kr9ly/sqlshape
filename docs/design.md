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

### PL/pgSQLの本体も解析する

決めたこと。`LANGUAGE plpgsql`の関数本体をlibpg_queryのPL/pgSQLパーサで構造に分け、埋め込まれた各SQL断片を既存のアナライザーで検査する（`internal/analyze/plpgsql.go`）。PL変数は関数パラメータと同じ経路（`funcParam`）でスコープに入れ、`plVar`印で`$n`から外し、変数と列の衝突はPostgreSQLの既定（`variable_conflict = error`）どおりエラーにする。record変数の形はそれを埋めたクエリから取り、トリガー関数は`CREATE TRIGGER`の結びつきごとに`NEW` / `OLD`をそのテーブルの行型として解析する（結びつきの無いトリガー関数は解析しない）。`RAISE`のSQLSTATEは本体から拾い、`-- sqlshape: error`注釈は名前を付ける役に退く。`EXECUTE`は定数文字列だけ検査し、それ以外は`-strict`の助言。

理由。DB側にロジックを置く方針なのに、トリガー関数と書き込み関数の大半を占めるPL/pgSQLが読めないと、関数経由の失敗モード・消費者索引・SECURITY DEFINERの到達がすべて本体の手前で止まり、READMEの主張が成り立たない。パーサが本体を構造化してくれるので、足したのはPL側の層だけで済んだ。

対象外。動的SQL（`EXECUTE`の非定数）、カーソル経由で取り出した行の型（`FETCH INTO`の先は形が不明なrecordとして扱い、フィールド参照は型不明で通す）。

### 境界の規則は義務として宣言し、文の事実で判定する

決めたこと。`visible where`・`-require-columns`・`-no-table-reads` / `-no-tables`は、表（ビュー）が参照側の文に課す**義務**の実例として一つの仕組みに載せる。宣言は`schema.sql`の`-- sqlshape: require <body> [on <kinds>]`（bodyはSQLのboolean式、`pinned(列)`、`immutable(列)`、`via view`）。アナライザーは判定せず、文が証明できること（葉・正規化述語・等値の閉包・代入集合）を`internal/facts`のデータとして出し、`internal/obligation`が「事実 ⇒ 義務」を判定して履行経路（文自身 / ビュー / ポリシー / 複合FK / waive）ごと返す。`aggregate`は義務の束を生む宣言、`context`は文脈ごとの差分、`sqlshape check`はGoの外の文への同じ判定の入口。従来の構文とフラグは略記として残す。経緯・設計・ロードマップは[obligations.md](obligations.md)。

理由。規則を一つずつ実装すると「ポリシーが満たす」「ビューが運ぶ」のような特例が規則の数だけ増える。判定を一本にすると新しい規則は宣言で済み、方言を足すときも「Factsを出すアナライザー」と「述語を落とすLowerer」だけで判定を共有できる。核（アナライザー・スキーマ・vet）はobligationを知らない。

棄てた案。文をまたぐ規則（トランザクション内の対）を扱う層（分析の単位は文、という核の裁定と衝突する）。方言をまたぐ義務DSL（SQL式と少数の構造述語で足りる）。opt-outの命名規則によるパッケージ→文脈の写像（暗黙は読めない。パッケージコメントの`// sqlshape: context`とフラグで明示する）。

### スコープ外

FETCH（カーソルの列は静的に決まらない）。EXPLAINの実行（embedded PGの統計は本番と違う。性能の助言は構造的に判定できるもの、インデックスの先頭列とビューへの述語押し込みに限る）。初期リリースではPostgreSQL以外のRDBMS（下記「検討中」のMySQL参照）。

## コード配置

| package | 役割 |
|---|---|
| `sqlshape` | 公開API: `Query[R, P]` / `One[R, P]` / `Batch` / `Copy[R]` / `MatView`、pgx上のランタイム（行マッパー、型登録、`ConstraintError`、描画のバイト比較） |
| `pgtest` | schema.sqlを適用した本物のPGをアプリケーションのテストに提供する。`Verify`がアナライザーとPGを突き合わせる |
| `internal/expand` | テンプレートの全展開。`{{.X}}` → `$n`と`P`上のパス |
| `internal/pgparse` | パーサ。libpg_queryを版ごとにWebAssemblyへビルドして埋め込み、wazeroで実行する。`Version.Parse` / `Deparse` / `SplitWithScanner` / `ParsePlPgSqlToJSON`と、最新版のprotoから生成したノード型 |
| `internal/schema` | libpg_queryでschema.sqlをカタログの上に載せる。テーブル・ビュー・enum・ドメイン・複合型・関数・制約・ポリシー・seed |
| `internal/catalog` | 埋め込みのpg_catalog（版ごとの型・関数・演算子・キャスト・集約、`data/<major>/`）と拡張のdump |
| `internal/analyze` | アナライザー本体。10章の型変換、スコープ、DML、`$n`推論、nullability、カーディナリティ、違反の列挙、PG互換のエラー。文の事実（`facts.Facts`）の生産と、義務の述語を事実の言語に落とす`Lower` |
| `internal/facts` | アナライザーと義務検査の間のデータ契約。文種・スコープの木・葉・正規化述語・等値の辺・固定列・代入集合。パーサのノードを含まず、方言を知らない |
| `internal/obligation` | 境界の規則。schema.sqlの`require` / `visible where`とvetのフラグを義務に読み、事実に対して履行を判定する（[obligations.md](obligations.md)）。`analyze`にも`vet`にも依存しない |
| `internal/vet` | `go/analysis`アナライザー。結果列 ↔ `R`、`$n` ↔ `P`、束縛、expect行、SuggestedFix。境界の規則は`obligation`に委ね、フラグを義務に展開して渡す |
| `internal/oracle` | 差分テストのオラクルとしての本物のPG（embedded-postgres）。検査時には使わない |
| `internal/dump` / `diff` / `migrate` / `consumers` / `cli` | マイグレーション側。pg_dumpによる正準形、オブジェクト単位のdiff、DDL生成と終点検証、消費者索引、サブコマンド |

## 検討中

- MySQLを、PostgreSQLとは別の実装としてサポートする。初期リリースの範囲外。方言をまたぐ共通DSLは作らない（作った瞬間にSQLをDSLに翻訳させる形に戻る）。代わりにプラットフォームごとに、そのDBの構文と型規則を実装したアナライザーと、そのDBのdumpを使うマイグレーション支援を持つ。共有するのはテンプレートの展開、`go/analysis`のフロントエンド、Go側の型照合の枠組み
  - パーサと字句解析器（裁定 2026-09-09）: MySQLサーバのソースから機械的に切り出し、libpg_queryと同じ形でwasmにする。原則は「本物から機械的に抽出したものを土台にし、手で書く部分を最小にする」。文法は`sql/sql_yacc.yy`（bison、18k行、`%expect 59`）からC++のアクション（`NEW_PTN`が923箇所）を剥がして規則・優先順位・トークンだけを残し、規則名と子要素だけの素の構文木（CST）を組む汎用アクションを機械生成してbisonに通す。字句解析器は`sql/sql_lex.cc`の`lex_one_token`（726行）と2トークン先読みの`MYSQLlex`（`WITH ROLLUP`等の合成）、`Lex_input_stream`（440行）、`find_keyword`（`sql/lex.h`の784語の表から`gen_lex_hash`が生成する完全ハッシュ）、`consume_comment`・`int_token`をそのまま使う。測ったところ、字句解析器がサーバ状態を読むのは5点だけ — `sql_mode`（`ANSI_QUOTES`の1箇所）、`default_collation_for_utf8mb4`、接続文字集合、コメント収集の`m_parser_state`、非推奨文字集合の`push_warning`（2箇所）— なのでTHDはこの5点を持つ数十行のshimで置き換える。文字の分類（`state_map` / `ident_map`）は`strings/sql_chars.cc`が文字集合のctype表から作るもので、`strings/`ライブラリごと取り込む（PGの`src/port`をlibpg_queryが抱えるのと同じ）。バージョンコメント`/*!80000 ... */`は`MYSQL_VERSION_ID`との比較なので、版ごとにビルドすれば自然に正しい。sqlshape用のASTへの変換だけが手書きで、CSTの規則名を読む。生成手段はこの裁定でbison + emccに決まる（goyaccでpure Goに持つ案は字句解析器の書き直しを伴うので原則に反する）。残る確認はスパイク: shimの表面積が本当に5点で閉じるか、抽出した文法とlexerがemccで通るか、`mysql-test/t`（1810ファイル）の受理率
  - `sql_mode`は判定を変えるので、版と同じくschema.sqlが宣言する（`-- sqlshape: mysql 8.4 sql_mode=...`の形は未決）。字句では`ANSI_QUOTES`、文法のアクションを剥がすので`PIPES_AS_CONCAT`（`||`）の意味はCST→ASTの変換で`sql_mode`を見て決める
  - オプティマイザヒント（`/*+ ... */`）は別の文法`sql/sql_hints.yy`（725行）と`Hint_scanner`を持つ。同じ手順で後から足せる。sqlshapeの判定には効かないので初期は読み飛ばす
  - カタログ: MySQLには`pg_proc`に当たるものがない（組み込み関数のシグネチャは`item_create.cc`とItemクラスのC++にしかない）。型規則も暗黙変換だらけで仕様文書はPGの§10ほど機械的でない。方針は「オラクルから測ってカタログを作る」: 本物のmysqld（配布tarballを`--initialize`で起こす。PGのembedded-postgresの役）に対して、関数名一覧×引数型の組み合わせをプリペアドステートメントのメタデータで問い、結果型の表を生成して埋め込む。PGの`.dat`由来カタログの経験則版。手で写す型規則は、測った表で差分検査する
  - 回帰コーパスはMySQL自身の`mysql-test/t/*.test`。パーサの受理率と、オラクルとの差分probeの両方に使う
  - 順序: パーサのスパイク（受理率で手段を裁定）→ 字句解析器 → CST→AST → スキーマローダー（CREATE TABLEの部分集合、`mysqldump`の正準形）→ アナライザーの型規則（mysqldをオラクルに差分テスト）→ テンプレート・vet・義務の結線（ここは共有部分なので薄い）。1.2の後
- TypeScriptフロントエンド。2つ目の言語フロントエンドで「核はSQLしか見ない」構想を検証する。言語非依存で共有するのは`internal/analyze`、`internal/schema`、マイグレーション一式、失敗モード・`One`の証明・境界の規則。言語依存はその言語のデファクトに合わせる: 呼び出し箇所の発見（TypeScript Compiler APIかtsc plugin、ESLintルール）、`R` / `P`相当の型解決（`query<R, P>(sql)`）、PG型とTS型の対応表（pg / postgres.jsが実際に返す型）、ランタイム。最初の設計論点はテンプレート構文で、Goの`text/template`を持ち込まず、`sql\`...\``のタグ付きテンプレートと`${}`の断片で「有限の展開集合」を保つ方法（断片をリテラルの選択肢に限る、など）を決める。MySQLと同じく、`internal/vet`のPG固有型の切り出しが前提。横展開の見立て: 検査の大半（列と型の存在、失敗モード、`One`の証明、境界の規約、インジェクション保証、マイグレーション）はSQL側だけで閉じていてホスト言語の型を見ない。言語ごとに必須なのは「lintの口」「テンプレート定数の取り出し」「ランタイム」の3つで、`R` / `P`の型照合は静的型がある言語で足す層（動的型の言語では実行時の行マッピング検査か、型ヒントがあれば静的に）
- 2-way SQL構文（`/*{{.X}}*/'lit'`）。psqlでそのまま流せるテンプレート。expandとRenderの前段で同じ変換を入れる
- ORMからの移行支援。ORMが発行したSQLを観測して`Query[R, P]`に起こす
- PGのバージョンを`schema.sql`が宣言し（`-- sqlshape: postgres 18`）、アナライザーはその版の規則で判定する。既定は最新のサポート版（宣言はスキーマに1行なので、新版利用者に毎回書かせない）。パーサはlibpg_queryをWebAssemblyにビルドしてwazeroで実行する（スパイクで成立: longjmpはemscriptenのエミュレーションで動き、protobuf出力はcgo版とバイト一致、速度は3.6倍遅いが解析全体では誤差）。cgoが消えて`go install`にCツールチェーンが要らなくなり、wasmモジュールは隔離されるので版ごとのパーサを同じバイナリに複数積める。宣言版より新しい構文は、その版の本物の文法が拒否する（gram.y差分の表は要らない）。ノードのGo型は最新版のpg_query.protoから生成した1組だけを持つ。protobufのフィールド番号はlibpg_queryが版ごとに振り直すので（17→18で267フィールド、enum値434が変わった）版をまたいでは読めず、木はどの版もパーサのJSON出力（フィールド名・ノード名で書かれる）をprotojsonで読む。JSON経路のほうがprotobuf経路より速い（1文215µs対273µs）。古い版で名前が違うフィールドは読む前にJSON上で最新版の形に書き換える（17→18は`returningList`→`returningClause.exprs`と、生の木には現れない3フィールドの削除だけ）。書き換え漏れはprotojsonが未知フィールドとして拒否するので黙って消えない。regressコーパス43,226文で、17の木は全文が書き換え経由で読め、18のJSON経路はprotobuf経路と全一致。カタログは版ごとの生成TSVを埋め込み、型規則の版分岐は§10実装内の分岐で差分表を持つ。オラクルとregressコーパスは版別のCIマトリクス。`dump` / `apply`は接続先の`server_version`と宣言版のずれを警告する。サポート窓はPGの5年に合わせるが、まず17と18の2版でこの構造を作り、以後は年1回のメジャーリリース直後に追従する（マイナー版は判定に影響しないので埋め込みPGを上げるだけ）。メジャー間で判定が変わるのは、構文の追加、カタログの追加・オーバーロードによる曖昧化・稀な削除、予約語の追加、ごく稀な型規則の変更
- PGの新バージョンへの追従を機械化する。回帰コーパスの取り込み → 差分 → 修正のループ。上の版指定があれば、追従は「版を1つ足す」作業になる。libpg_queryのリリースを待つ形は変わらない（18は8か月遅れた）。pg_query_goを待つ必要はなくなる
