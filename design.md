# sqlshape — 設計メモ

2026-09-04 の議論から。

## 動機

RDBMS を使うアプリケーションに本当に必要なのは 4 つ:

1. SQL の構文チェック
2. 型安全で、変な制約のない値のバインド
3. 戻り値の自動パースと値へのマッピング
4. （あれば）アプリケーションスキーマから構築できるマイグレーション

既存ツールはどれも 2〜3 個を満たして残りを落とす。理由は構造的で、
1〜3 は「SQL が正」、4 は「アプリの型定義が正」で真実の源が割れている。
両方やろうとすると SQL を DSL に翻訳させることになり（jOOQ / Kysely / Drizzle）、
それが要件 2 の「変な制約」の正体。

sqlc は「1 クエリ = 1 関数、クエリ全文がコード生成時に固定」という前提のため、
動的 SQL（任意フィルタ・並び順切替）、可変長 IN、1 対多のネスト、戻り型の命名、
COALESCE/CASE の型推論で詰まる。

## 中心となる転換

### 検査器を自作せず DB エンジン本体を検査器にする

DB を Postgres に割り切る。方言パーサーと型推論器の再実装をやめ、
実 PG にスキーマを流して全クエリを `PREPARE` → `Describe` で聞く。
パラメータ型・結果列の型・列の由来テーブルが返る。DDL の意味論も再実装不要。
副産物で `EXPLAIN` による seq scan / インデックス欠落の lint も同じ基盤で出せる。

MySQL 等は別インターフェースとして割り切る。共通化しようとした瞬間に sqlc に戻る。

### クエリを「有限の出力集合を持つテンプレート」にする

文字列結合が危ないのは結合そのものではなく、出力集合が無限で不透明だから。
テンプレートに `if` / `switch` / `range` を持たせれば出力集合は有限（range は
0・1・2 回展開で代表）。全展開形を PREPARE すれば動的 SQL がそのまま検査できる。

- 2-way SQL（Doma / uroboroSQL / DBFlute）は同じ形を 15 年前から実務で回しているが、
  検査するのは既定の 1 形だけ。全列挙が欠けている部分
- 注入安全は自動で付く。差し込めるのはバインド値と型検査済み断片だけ
- 分岐ごとに結果型が出るので、投影が分岐で変わるクエリも sum 型で返せる
- 出力集合が有限なので prepared statement キャッシュが有界になる

分岐爆発（if が 12 個超など）時の退避路: 各 if ブロックを「外側で true」1 本 +
全 false 1 本だけ PREPARE。独立な AND 述語ならこれで型検査は完備。
非独立な組み合わせだけ全列挙に戻す。

### コード生成ではなく vet

構造体と SQL を自分で書き、検査器が合っているかだけ見る。

- 生成物なし。gopls のリネーム・参照検索がそのまま効く
- 既存コードに後付け可能。定数 SQL 文字列はディレクティブ 0 個のテンプレート
- 定数に畳めない引数は「未検査クエリ」として警告 → 検査カバレッジが数値になる
- 言語依存部分はフロントエンドの薄い一枚（呼び出し箇所の発見、型のフィールド解決、
  PG 型 → 言語型の対応表、実行時ライブラリ）。核は SQL しか見ない

### スキーマは宣言 1 ファイル、マイグレーションは書かない

`schema.sql` を真実の源にする。DB の実状態は sqldef が寄せ、コードの実状態は
sqlshape が寄せる。検査器はマイグレーション再生すら不要で `schema.sql` を
空の PG に流すだけ。

「マイグレーションに書いてあるスキーマしか使えない」は制約として検査するのではなく、
制約以外の状態が作れなくなる。要件 4 は解決ではなく消滅する。

同じ基盤でほぼ無償で出るもの:

- マイグレーション PR の影響分析（この DROP はどの呼び出し箇所を壊すか）
- 死んだスキーマの検出（どのクエリからも参照されない列・テーブル・インデックス）
- ローリングデプロイの世代跨ぎ検査（HEAD と HEAD~1 の schema.sql で 2 回回す）
- ドリフト検出（再生結果 vs 本番 `pg_dump --schema-only`）
- sqldef の DROP ゲート: 参照クエリ 0 件なら CI 自動許可、1 件以上なら人間レビュー

sqldef で表現できないもの（データマイグレーション / 順序依存の複合変更）は
当面は別ディレクトリの命令的ステップとして持つ。いずれ sqlshape 側で引き取る
（「マイグレーション: sqldef の先」参照）。

## 書き味（Go）

テンプレート構文は text/template を借用（`text/template/parse` を流用、実行器は自前）。
規則は 3 つ: `{{.X}}` が式位置なら バインド、`if` 条件なら制御、両方なら省略可能。
分岐は `if` / `else if` / `with` / `range` のみ（`switch` は text/template に無いので `else if eq` で書く）。

```go
type OrderSummary struct {
    ID       int64           `col:"id"`
    Total    decimal.Decimal `col:"total"`
    UserName string          `col:"user_name"`
}

var listOrders = sqlshape.Query[OrderSummary, ListOrdersParams](`
    SELECT o.id, o.total, u.name AS user_name
    FROM orders o JOIN users u ON u.id = o.user_id
    WHERE true
      {{if .Status}}  AND o.status = {{.Status}}        {{end}}
      {{if .UserIDs}} AND o.user_id = ANY({{.UserIDs}}) {{end}}
    ORDER BY {{if eq .Sort "newest"}} o.created_at DESC
             {{else if eq .Sort "total"}} o.total DESC
             {{else}} o.id {{end}}
    LIMIT {{.Limit}}
`)

for row, err := range listOrders.Run(ctx, db, ListOrdersParams{Status: &s}) { ... }
```

- 基本形は `iter.Seq2[R, error]`。`Collect` / `First` はヘルパー
- 1 対多は SQL 側で `array_agg(row(...))` に書かせ、PG の複合型からネスト構造体を導出
- 可変長 IN は `= ANY($1)` に統一
- 実行時は各レンダリングを分岐シグネチャで静的展開形と突き合わせ、検査器が見ていない SQL は error で拒む（「進捗」参照）

## アーキテクチャ

- **analyzer**: `go/analysis`。`go vet -vettool` / golangci-lint / gopls に一度に乗る
- **frontend**: `sqlshape.Query[R, P](literal)` を拾い、`go/types` で R・P のフィールド解決
- **expander**: テンプレート → 全展開形（`{{.X}}` → `$n`）
- **prober**: 生成カタログ + 自前アナライザー（pure Go）。embedded-postgres は差分テストのオラクル
- **matcher**: 結果列 ↔ R フィールド（名前 + `col` タグ）、パラメータ ↔ P フィールドの型照合
- **runtime**: pgx 直結。テンプレート実行、`$n` 引数列、名前ベースの行マッパー、
  展開形ごとの statement キャッシュ
- 補助: pg_query_go（libpg_query）で断片の切り出し・バインド位置の式文脈特定

### prober の方式候補

当初案は embedded-postgres（実バイナリ、起動 1 秒弱）に schema.sql を流して PREPARE / Describe。
PGlite は JS ホスト前提で Go からは使いづらい。DB を起動しない代替は以下。prober は interface にして差し替え可能にする。

1. **パーサー抽出**: libpg_query（pg_query_go）。構文は本物だが raw parse まで。
   `parse_analyze` はカタログ依存で抽出されていない
2. **生成カタログ + 型推論の仕様実装**: PG ソースの `pg_proc.dat` / `pg_operator.dat` /
   `pg_type.dat` / `pg_cast.dat` から静的カタログを生成し、ユーザーテーブルは schema.sql を
   pg_query で解析して足す。型推論はマニュアル 10 章「型変換」が仕様として書き切っている
   （§10.2 演算子解決、§10.3 関数解決、§10.5 UNION/CASE、多相型）ので仕様どおりに実装する。
   sqlc の穴は仕様を全部実装しなかったことから来ていて方式の限界ではない。
   pure Go・起動ゼロだが、数千〜1 万行と PG バージョン追従の永続コスト
3. **アナライザーごと抽出 + 偽カタログ**: libpg_query の手法を `analyze.c` / `parse_*.c` まで
   広げ、syscache の裏をメモリ上の偽カタログにする。忠実度は本物だが syscache / relcache /
   MemoryContext の絡みが深く研究課題に近い
4. **PG を wasm に**: PGlite の WASI ビルドが成立すれば wazero でプロセス内・ミリ秒起動。
   実用段階かは未確認

どの方式でも nullability は PG が答えないので自前。

### 裁定: 2 を本線、embedded PG はオラクル

2 に寄せる積極的理由:

- nullability が同じ木の走査に載る。embedded PG 方式では「型は PG、null は自前」で
  解析器が 2 つになるが、自前で式の木を歩くなら NOT NULL / JOIN 種別 / COALESCE の伝播は
  数百行の追加
- 参照の全数が取れる。Describe は結果列の由来しか返さず WHERE / JOIN 条件の列は見えない。
  自前解析なら DROP ゲートと死んだスキーマの検出が完全になる

1 週間で終わらせる条件:

- 初日にオラクルを作る。本物の PG を差分テストの正解役に置き、クエリ生成 → 自前推論と
  PREPARE / Describe の突き合わせ → 不一致を fixture 化、のループで収束させる
- カタログ生成も初日。実装は `.dat` の Perl 形式を解析するのではなく、**オラクル PG から
  pg_type / pg_proc / pg_operator / pg_cast / pg_aggregate を COPY で dump** して TSV を embed する
  （`go run ./internal/catalog/gen`）。デフォルト値が解決済み・OID 確定・パーサ不要で、拡張の
  `pg_proc` 取り込みと同じ経路になる。PG 17.5 で 469 型 / 3319 関数 / 799 演算子 / 229 キャスト /
  157 集約、TSV 合計 230KB、ロード 10ms
- マニュアル §10（型変換）を先に、構文カバレッジは後に。演算子解決・関数解決・暗黙キャスト・
  多相型・UNION/CASE の統一が本体。JOIN / サブクエリ / CTE / 集合演算 / ウィンドウは
  スコープと列可視性の問題で型推論より単純

細部で踏みやすいもの:

- `$n` の型推論。PG は `unknown` から文脈解決する。オラクル実測（PG 17）: `SELECT $1` は
  エラーではなく `$1 text` / 結果 `text` に落ちる（unknown → text の最終規則）。鏡写しにしないと偽陰性
- typmod（`varchar(20)`、`numeric(10,2)`）の伝播規則が関数ごとに違う
- 集合返却関数と LATERAL、RETURNING、`INSERT ... ON CONFLICT` の列可視性

オラクルの初回実測で分かった限界（internal/oracle/testdata/queries/*.golden が正）:

- Describe の列由来（TableOID / attnum）は**ビューで止まる**。`order_summary.id` の attnotnull は false で
  基表の NOT NULL が見えない。nullability は自前解析でビュー定義を辿るしかなく、オラクルは
  ビュー越しの null 判定の正解を持たない
- `array_agg(row(...))` は `record[]`（匿名複合型）で返る。ネスト構造体の導出は自前の行型推論が必要。
  名前付き複合型（`CREATE TYPE`）にキャストさせれば PG 側でも型が付く
- **ドメイン型の列は Describe に基底型で出る**（`email` ドメイン → `text`、`yen` → `bigint`）。
  RowDescription は基底型 OID を送る仕様なので、オラクルからはドメインが見えない。式の型検査では
  ドメインとして扱い、オラクル照合の直前で基底型に潰す（`Types.BaseOf`）。ドメインの NOT NULL も
  attnotnull には現れない
- **列由来はサブクエリと CTE は透過し、ビューで止まる**。`markTargetListOrigins` が解析時に走り、
  ビューはまだ RTE_RELATION のため。アナライザーは同じ規則を鏡写しにしている
- **select_common_type の typmod**: 入力に untyped literal / `$n` が 1 つでもあると typmod は落ちる
  （`coalesce($1, total)` は `numeric`、`CASE ... THEN total ELSE total` は `numeric(12,2)`）
- **`format_type(oid, -1)` は TYPEMOD_GIVEN 扱い**: typmod 無しの `bpchar` は `bpchar`、`bit` は `"bit"` と出る
- **`text[] || 'x'` は array_cat に解決される**（anycompatiblearray || anycompatiblearray）。多相型の
  整合検査（check_generic_type_consistency）を候補選別に入れないと array_append 側に誤解決する。
  `array || 'literal'` が実行時に "malformed array literal" になる有名な罠の正体
- `numeric` の typmod は列直参照なら残る（`numeric(12,2)`）が集約（`sum`）や `$n` では落ちる。
  typmod 伝播規則が関数ごとに違うことの実例

スコープ外: PL/pgSQL、ルール、拡張の関数（下記の dump 経路は未実装）。
拡張は本物の PG から `pg_proc` を dump して同じ形式に落とす経路を残す。

## 副産物: 型安全なストアドプロシージャ

`schema.sql` の `CREATE FUNCTION` / `CREATE PROCEDURE` はカタログの `pg_proc` 行として取り込まれるので、
`SELECT * FROM f($1, $2)` / `CALL p(...)` は組み込み関数と同じ経路で引数型・戻り列・オーバーロード・
多相の解決が通る。特別対応なし。

ストアドが避けられてきた理由がこの構成で全部消える:

- バージョン管理されない → schema.sql が git にある
- デプロイが手作業 → sqldef が `CREATE OR REPLACE FUNCTION` の差分を当てる
- 呼び出し側が型なしで壊れる → シグネチャ変更で全呼び出し箇所が lint で赤くなる

踏み込める範囲:

- `LANGUAGE sql` の `BEGIN ATOMIC` 本体（PG14+）は素の SQL なので自前アナライザーで本体も検査できる。
  関数越しのテーブル依存も追えて DROP ゲート / 死んだスキーマ検出が効く
- PL/pgSQL 本体はスコープ外。シグネチャだけ信用する（PG 自身も実行時まで検証しない）。plpgsql_check の位置

注意点:

- 関数戻り値の nullability は原理的に不明。`STRICT` と NOT NULL ドメインから拾い、他は nullable 扱いで
  `-- sqlshape: not null` 注釈で上書き
- `RETURNS record` を OUT なしで宣言した関数は呼び出し側の列定義リストが必須。そこも検査対象
- psqldef が `CREATE FUNCTION` の差分をどこまで扱えるか未確認。扱えなければテーブルは sqldef、
  関数は `CREATE OR REPLACE` で全量再適用、の分担で済む

## 副産物: ビュー / マテリアライズドビューを DB の公開 API にする

ビュー本体は素の SQL なので自前アナライザーが全部見られる。列型も nullability も本体から推論できる
（本物の PG は `pg_attribute` でビュー列を常に nullable と報告するので、ここは PG より良くなる）。

- **ビュー = スキーマに住む名前付き型付きクエリ。** sqlc に無かった「クエリ断片の再利用」「CTE の共通化」は
  ビューが答え。断片の型付けという新しい仕組みが要らない
- **依存グラフ。** テーブル → ビュー → ビュー → クエリの参照を自前カタログで辿れる。DROP ゲートが
  ビュー越しに効く、未使用ビューの検出、ビュー差し替え時の列型変化が呼び出し側の lint エラーとして出る
- **public / private 境界。** lint ルール「アプリコードはビューと関数だけ参照可、テーブル直参照は禁止」を
  選べる。テーブルは private、ビュー・関数が public API。テーブルのリネーム・分割・正規化変更をビューを
  固定したまま裏で行える。書き込みは `INSTEAD OF` トリガー付き更新可能ビューか関数経由。
  段階導入: 最初は警告のみで、直参照件数がカバレッジとして減っていく
- **不変条件。** CHECK は 1 行・サブクエリ不可。トリガー関数なら行・テーブルを跨ぐ不変条件を経路非依存で
  置ける。違反は独自 SQLSTATE で投げ、`-- sqlshape: error XX001 = SubscriptionLimitExceeded` の対応から
  実行時ライブラリが Go の型付きエラーに写す。注意: 行を跨ぐ検査は READ COMMITTED で競合に抜けられる
  （部分ユニークインデックス / FOR UPDATE / SERIALIZABLE で守る）

マテリアライズドビュー:

- 型付けはビューと同じ
- lint: `REFRESH ... CONCURRENTLY` に必要なユニークインデックスの有無、依存元テーブル一覧
  （「このテーブルに書いたらこの MV が古くなる」表）
- runtime: 型付きの `Refresh(ctx, OrderStatsMV)` ハンドル
- リフレッシュのタイミングは業務判断。ツールは依存情報まで

psqldef の `CREATE VIEW` 差分は対応があるはず。MV / 関数は対応が薄ければ全量再適用の分担。

## ORM の代替としての読み方

ORM の目的を分けると、読み側はビュー、書き側は関数、動的絞り込みはテンプレートに散る。

- **読み: ビューが集約の形を返す。** `order_detail` を `array_agg(row(...))` で定義すれば集約ルートが 1 行で返る。
  N+1 は構造的に起きない、遅延ロードは不要（形を事前に決めるから）、identity map も不要。
  ネストの型は複合型から導出。DDD 的には 集約 = 読みのビュー + 書きの関数 の対
- **書き: 複合型を受ける関数。** `CREATE TYPE order_input AS (...)` + `save_order(order_input, order_item_input[])`。
  unit of work / dirty checking / cascade save の置き換え。複合型はカタログにいるので引数も型検査
- **動的絞り込み: テンプレートがビューの上に載る。** `FROM order_detail WHERE {{if .Status}}...`
- 捨てるもの: DB 可搬性、クラスからのスキーマ生成、SQL を隠すこと

注意:

- ビューの増殖。ビューは集約・ドメインの形の単位、画面ごとの投影はテンプレート側で列を選ぶ
- プランナーの inline 可否。GROUP BY / DISTINCT / ウィンドウを含むビューは述語が押し込めない場合がある。
  EXPLAIN ベースの lint で「このビューへの述語は押し込まれない」を警告できる
- LEFT JOIN のネスト側の nullability は JOIN 種別から正しく nullable にする

性能の落とし穴:

- ネスト側の巨大化。子が 1 万行ある集約を `array_agg` で畳むと行が肥大化する。ビュー側でネストに
  `LIMIT` を入れるか、大きくなり得る子は別クエリに分ける。lint で「ネスト内に上限なし」を警告可
- prepared statement の generic plan。同じ文 5 回で generic に切り替わり、偏った列の `$1` で遅い値が出る。
  展開形ごとの statement キャッシュはこれに当たりやすい。runtime に「この文は毎回 custom plan」フラグ
  （`plan_cache_mode = force_custom_plan` 相当）
- lint の EXPLAIN は embedded PG 上で統計が本番と違う。プラン系の警告は統計非依存（構造的に押し込めない等）に限定

## 値集合の共有: enum / CHECK / lookup テーブル

「DB 側の enum とアプリ側の enum の範囲は同じか」を静的に検査する。真実の源は DB 側。

### バインディングは宣言ではなく使用から推論する

- matcher が列 ↔ R フィールド、パラメータ ↔ P フィールドを照合する時点で、列が値集合を持つ型
  （PG enum / CHECK IN 付きドメイン / lookup テーブルへの FK）で、Go 側が named type
  （`type OrderStatus string`）なら、その対を「Go 型 ⇔ 値集合」のバインディングとして記録する
- 同じ Go 型が別の値集合にも当たったら意味の混線として報告
- 明示登録 API は置かない。vet モードの思想（生成物なし・後付け可）に揃える。未使用の型は
  検査対象から外れるが、使っていないなら問題も起きない

### 集合の比較

- Go 側 = `go/types` でその named type の typed const 全部。パッケージ跨ぎは go/analysis の Fact で
  定義側パッケージから export
- DB 側 = カタログの `pg_enum` ラベル、CHECK の IN リスト、lookup テーブルの固定値
- 両方向で報告する。**DB にあって Go に無い**（読み取りで未知値、switch が抜ける）と
  **Go にあって DB に無い**（書き込みで実行時エラー）。前者のほうが深刻
- 検査できない形は「検査できない」と明示する（黙って素通しにしない）:
  enum 列を素の `string` で受けている、`smallint` に意味を持たせている列（DB 側に情報が無い）

### 値集合の 3 つの置き場と、lookup テーブルを本命にする理由

| | PG enum | text + CHECK IN | lookup テーブル + FK |
|---|---|---|---|
| 集合の取得 | pg_enum | CHECK 式を解析 | テーブルの固定値（データ） |
| 値の削除 | 不可（DROP VALUE 無し） | CHECK 差し替え | DELETE（FK で参照中なら弾かれる） |
| 属性（表示名・並び順・有効フラグ） | 無理 | 無理 | 列を足すだけ |
| 分析側からの見え方 | ラベル文字列 | 文字列 | JOIN で名前解決できる |
| 検査の前提 | カタログのみ | カタログのみ | **固定値データがスキーマの一部である必要** |

lookup テーブルが最も表現力があり、enum 固有の落とし穴も無いが、値集合が「データ」なので
schema.sql の再生だけでは検査器が集合を知れない。**固定値テーブルの宣言的データマイグレーション**
（後述）があれば、lookup テーブルの値は schema.sql と同格の真実になり、enum と同じ精度で検査できる。
3 つのどれでも集合が引けるようにしておき、推奨は lookup テーブル。

### enum の落とし穴と対応

静的に止める（analyzer）:

- Go の零値: `type OrderStatus string` の零値 `""` はラベルに無い。P の非ポインタ enum フィールドは
  「未設定だと実行時エラー」を警告。ポインタか定数初期化を促す
- NULL 可の enum 列を非ポインタで受けている（nullability 伝播で出る）
- `ORDER BY status` / `status < 'x'`: enum の比較は**宣言順**でラベルの字句順ではない。
  `ADD VALUE BEFORE/AFTER` 後は特に直感と離れる。info 級で出す
- text 型パラメータとの比較: P のフィールドが素の `string` だと runtime が text で送り
  `operator does not exist: order_status = text` になり得る。上の「string で受けている」警告の実利
- `OrderStatus("typo")`: string リテラルからの変換式がラベル外なら警告。閉集合の保証にはならないが
  静的に見える範囲は止める
- switch の網羅性: DB 起点の集合で exhaustive 検査。値追加時のアプリの取りこぼしを見つける

runtime で吸収する:

- pgx v5 は `[]OrderStatus` ⇔ `order_status[]` に `LoadType` 登録が要る（スカラは text フォールバックで
  通るが配列は通らない）。`= ANY($1)` に統一する方針なので確実に踏む。runtime が接続時に検査で見た
  enum 型を全部 LoadType する。OID は実行時にしか決まらないので runtime の仕事
- 未知ラベルの受信（DB に値が足されアプリが古い）は行マッパーで型付きエラーにする。
  panic / error / 素通しを設定で選ぶ

schema.sql の差分で止める（マイグレーション側、後述）:

- 値の削除: PG に `DROP VALUE` は無い。新型作成 → `ALTER COLUMN TYPE ... USING` → 旧型 drop が必要
- `RENAME VALUE`: schema.sql 上は削除 + 追加と区別がつかない。意図宣言が要る
- `ADD VALUE` のトランザクション制約: 同一トランザクション内で追加した値は使えない
  （`unsafe use of new value`）。データ移行と同時にやると踏む

文書にしかならない:

- デプロイ順序。値追加は DB → アプリ、削除はアプリ → DB。静的検査は「両方の最新」しか見ない

## 解釈の共有: アプリが値に載せている意味を DB 側の情報で検査する

値集合の照合を一般化する。アプリは DB の値に「意味」を載せて扱うが、その意味の大半は
カタログ（型・ドメイン・FK・制約・DEFAULT）に既に書いてあり、PG 自身は検査しない。
sqlshape は使用箇所から Go の named type と DB 側の名目的な型を結び、両側を突き合わせる。

骨格は一つ: **使用箇所で Go named type ⇔ DB の名目型（enum / ドメイン / FK 先 / lookup）をバインドし、
集合・制約・単位を照合する。** enum で作る仕組みがそのまま ID と単位に伸びる。

### 名目型の照合（PG より厳しい型検査）

- **ID の名目型は FK グラフから導出する。** `orders.user_id` と `products.id` は PG には同じ bigint だが、
  FK があるので「この列の同一性ドメインは `users.id`」と分かる。Go 側 `type UserID int64` を
  使用箇所からバインドすれば、`WHERE o.product_id = $1` に UserID を渡す誤りが出る。
  `JOIN users u ON u.id = o.product_id` のような同型だが意味違いの JOIN 条件も FK グラフと突き合わせて
  警告できる。ORM がモデルの参照型で持っていた安全性を生 SQL で取り戻す
- **ドメインを不透明な単位型として扱う。** `CREATE DOMAIN yen AS bigint` は PG では演算で bigint に
  戻るので `price_yen + weight_g` が通る。sqlshape はドメイン同士の演算・比較を名目的に検査して止める。
  Go 側 `type Yen int64` とのバインディングは ID と同じ仕組み。素の int64 で受けていたら
  「検査できない」を明示する
- **値集合**（前節）は名目型照合の一種。enum / CHECK / lookup の集合を Go の typed const と突き合わせる

### カーディナリティの裏付け

- `One` / `First` 相当を使う展開形で、WHERE がユニーク制約（PK / UNIQUE / 部分ユニーク）の全列を
  等値で束縛しているかを判定する。証明できれば「最大 1 行」が静的に確定し、`One` は証明できなければ
  エラー。「1 件返るはず」というアプリの解釈を DB の制約で裏付ける
- ORDER BY 無しの結果を順序依存で消費している（`First`、`LIMIT`）は警告
- `array_agg` のネスト側が 1:1（FK 側に UNIQUE）なのに slice で受けている、1:N なのに単体で受けている

### 表現の忠実さ

- **数値**: `numeric(p,s)` を float64 で受ける（精度損失）、bigint を int32 で受ける、smallint 列に
  int を送る。PG 型 → Go 型対応表を「許容」と「情報が落ちる」に分け、後者を警告
- **時刻**: `timestamp`（tz 無し）を `time.Time` で受けるとどの tz で解釈するかがアプリの暗黙になる。
  `timestamptz` を推し、`timestamp` 列は info。`date` を `time.Time` で受けると tz で日付がずれる
- **文字列**: `varchar(n)` の長さ、`citext` の大文字小文字非区別、照合順序。アプリ側で再実装している
  比較・ソートと DB の解釈が割れやすい。ORDER BY 付きの結果をアプリ側で再ソートしている等は
  静的に見えにくいので、まず対応表の info に留める

### 既定値と生成値の所有者

- P のフィールドが非ポインタで、対応する列に DEFAULT / `GENERATED` がある → Go の零値が常に送られ、
  DB の既定値は一度も効かない。「既定値はどちらが持つか」の解釈が割れている典型。警告
- identity 列・`DEFAULT now()` の列への明示挿入も同類（アプリと DB の両方が「生成する側」だと思っている）

### 失敗モードの共有（制約 → 型付きエラー）

- INSERT / UPDATE / DELETE の各展開形が違反し得る制約（UNIQUE、FK、CHECK、NOT NULL、独自 SQLSTATE）は
  カタログから静的に列挙できる
- runtime が制約名 → Go の型付きエラーに写す（不変条件の節の SQLSTATE 対応を全制約に一般化）
- analyzer は「この文は `orders_email_key` 違反を返し得るが呼び出し側で判別していない」を出せる。
  失敗の解釈もアプリの独自実装ではなくカタログから来る

### 行の所属（テナント / 論理削除）

- `tenant_id` を持つテーブルは必ず tenant_id で絞る、`deleted_at IS NULL` を通す、という
  「行がどう見えるべきか」の解釈。正道はビュー（semantic layer）で吸収し、テーブル直参照禁止 lint で守る
- 補助として列ポリシー lint（「この列を持つテーブルは WHERE でこの列を等値束縛すること」）。
  マイクロサービス × 単一 DB のスキーマ境界と同じ枠

### 解釈の文書共有

- `COMMENT ON COLUMN / TABLE / TYPE` を Go 側の doc コメント・gopls hover に流す。検査ではないが
  「解釈の源は DB」を日常の導線にする

### 位置づけ

ORM は「解釈をアプリ言語のモデルクラスに閉じ込める」ことで型安全を得ていた。ここでは解釈を DB の
カタログに置き、アプリ側はそれを**参照して照合される側**になる。同じ照合が分析基盤・別サービス・psql
にも届くので、解釈が 1 か所にしか無い状態を保てる。SQL を一級市民にする、の実体はこれ。

## 分析基盤との共有: ビューを semantic layer にする

アプリ側の「解釈」（有効注文の条件、コード値の名前解決、税抜→税込）が ORM のモデルクラスにあると
分析側（Aurora → Athena / Redshift）からは見えず、dbt の staging モデルで再実装して静かにずれる。
解釈がビューにあれば SQL テキストとして持ち出せる。

本線: Athena federated query（Aurora PG コネクタ）を分析専用リーダー（カスタムエンドポイント）に繋ぐ。
ビューは PG のまま見えるので transpile も方言サブセットの lint も不要。アプリ側はアプリの都合だけで
ビューを設計してよい。注意はリーダー上の長時間クエリの recovery conflict キャンセルと、Lambda コネクタ側の
上限（15 分・同時実行・部分的な述語プッシュダウン）。

退避路: スキャン量がコネクタの上限を超えたら S3 エクスポート / zero-ETL + schema.sql のビュー定義を
sqlglot で transpile して再作成。Redshift は PG 系でほぼそのまま。ここまで来たときだけ
「持ち出すビューは方言サブセットのみ」の lint が要る。

意味を所有するアプリチームが schema.sql に一度だけ書き、分析側は消費者に回る。
「テーブルは private、ビューは public API」の消費者に分析基盤が加わる。テーブル変更で分析側が静かに
壊れる問題がビュー固定で消える。

層の分かれ方は自然に出る: 集約ビュー（`array_agg(row(...))` 入り、アプリが読む）は flat な意味層のビューの
上に組む方が書きやすいので、分析側が読める flat 層は制約なしでも生まれる。分析側が集約ビューを読む理由はない。

LLM: 分析用エージェントに schema.sql の意味層を読ませれば、アプリの解釈を持った状態でクエリを書く。

## マイクロサービス × 単一 DB（DB 層のモジュラーモノリス）

「DB 共有」がアンチパターンだった理由は全部「テーブルを直接共有した」ことから来る（他サービスの変更で壊れる、
所有権がない、誰が何を読むか分からない）。private スキーマ + 公開ビュー・関数 + GRANT で解決し、
sqlshape が「サービス B のコードは `a_api.*` と `b_private.*` 以外を参照禁止」を lint で執行する。
サービス境界がコンパイル時に検査される。

戻ってくるもの:

- トランザクション。A と B の書き込み関数を 1 トランザクションで。saga / 補償 / 結果整合性が不要
- JOIN。B が `a_api.order_summary` と自テーブルを JOIN。API 合成・BFF 結合・CDC + Kafka の読みモデル同期が消える
- 契約テスト = lint。A が `a_api` を変える PR で B の lint を A の新 schema.sql に対して回す。消費者駆動契約が機械化

失うもの: ストアの独立スケール、ポリグロット永続化、障害範囲（DB 停止で全停止）、ノイジーネイバー。

緩和: PG は縦に遠くまで行く、読みはサービスごとのリーダーで分離。決定的なのは、スキーマ境界が最初から
切れていれば後で本当に分けるときその境界で切れること（`a_private` を論理レプリケーションで別インスタンスへ、
`a_api` を FDW か API 呼び出しに置換）。境界のない共有 DB の分離が地獄だったのは境界がなかったから。

## マイグレーション: sqldef の先

最初は sqldef でよい。ただし diff ツール一般の限界が 2 つあり、片方は sqlshape だけが埋められる。

- **変更の意味を知らない。** before/after の diff からは「rename か drop + add か」「enum 値の削除を
  どう実現するか」「NOT NULL 追加時の既存行はどうするか」が決まらない。Prisma も Atlas も同じ壁に
  当たって、対話で聞くかマイグレーションファイルを手で書かせるかに落ちている
- **消費者を知らない。** 列を落として誰が壊れるか分からない

後者は sqlshape が唯一持てる情報。「全消費者を知っている diff ツール」というだけで既存のどれとも
違う位置に立つ。DROP・型変更の影響分析、ビュー経由なら無変更で済む判定、非推奨期間の消費者ゼロ確認。

### 意図の宣言（前者を埋める）

diff が判断できない部分を schema.sql の隣に宣言として置く:

```sql
-- @migrate rename orders.state -> orders.status
-- @migrate enum order_status: drop 'canceled' using 'cancelled'
-- @migrate backfill orders.status = 'pending' where status is null
```

- 検査器が宣言と diff の整合を確認する。宣言した rename が diff に無ければエラー、diff に説明の
  つかない drop があればエラー
- 実行手順（enum の新型作成 → USING キャスト → 旧型 drop、ADD VALUE のトランザクション分離、
  backfill のバッチ化）はパターンとして生成する。手書きの UP / DOWN を消す
- backfill の式は普通の SQL なので、アナライザーで型検査できる。宣言的データマイグレーションを
  型付きにする鍵で、既存ツールには無い

### 固定値テーブルの宣言的データマイグレーション（本命）

lookup テーブル（`order_statuses(code, label, sort_order, active)` のような固定値テーブル）の中身を
schema.sql と同格の宣言にする:

```sql
-- @data order_statuses (code, label, sort_order)
--   ('pending',   '保留',   10),
--   ('paid',      '支払済', 20),
--   ('shipped',   '発送済', 30);
```

- 適用は宣言と実テーブルの差分から生成する。追加は INSERT、変更は UPDATE、削除は DELETE
  （FK 参照が残っていれば DB が弾く。事前に参照件数を出して止められる）
- **検査器はこの宣言を値集合として読める**。lookup テーブルが enum と同じ精度で「値集合の共有」
  検査に乗る。enum の落とし穴（DROP VALUE 不在・宣言順比較・LoadType）は構造的に無い
- ビュー（semantic layer）から JOIN で名前解決できるので、分析側にも同じ集合が届く
- 固定値テーブルとマスタデータ（運用で増える）の境界は宣言の有無で決まる。宣言したテーブルは
  宣言以外の行が「ドリフト」として検出される

### 進め方

sqldef の実行系は書き直さない。「sqldef の出力を sqlshape が検査して危険なら止める」ラッパーから
始め、意図宣言 → 固定値データ → 手順生成の順で diff の判断部分を引き取る。
アナライザー本体（型検査・スコープ・nullability）が無いと影響分析も backfill の検査も成立しないので、
依存は明確に「アナライザー → マイグレーション」。

## 訴求: ORM が原理的にできないこと

「不得手」ではなく「原理的に不可能」に絞る。

- 発行される SQL をコードレビューで読む（実行時組み立て。ここでは全展開形が diff に出る）
- 方言の全構文を使う（DSL が表現できない瞬間に生 SQL へ落ち、無検査になる。ここでは生 SQL が主対象）
- 生 SQL の逃げ道を型検査する（ORM の逃げ道は定義上 ORM の外）
- クエリが触る列を全数把握する（モデル単位では分かるがクエリ単位・生 SQL 込みでは不可。DROP 影響分析が成立しない）
- 意味を他の消費者と共有する（モデルクラスの解釈はアプリ言語に閉じる。ビューなら分析基盤・別サービス・psql に届く）
- アクセス経路に依らず不変条件を守る（ORM のバリデーションはその ORM を通った書き込みにしか効かない）
- クエリの集合を有限に固定する（遅延ロードはコードパスで発行数が変わる。ここでは展開形集合がビルド時に確定）
- スキーマを固定 API の裏で変更する（モデル 1 対 1 テーブルの前提。ビュー固定ならアプリ無変更）
- 使われていないスキーマを見つける（生 SQL 込みの全数参照解析が前提）
- DB 側の部品を一級で使う（ビュー・関数・複合型・ドメイン・enum・lookup テーブル）
- 値集合をアプリと DB で照合する（enum / CHECK / lookup テーブルの範囲一致、switch の網羅性）
- ID と単位を名目型で検査する（FK グラフ由来の ID 型、ドメインの不透明化。PG 自身より厳しい）
- 「1 件返る」をユニーク制約で証明する（`One` の静的裏付け）
- 文が返し得る制約違反を列挙し、型付きエラーとして呼び出し側に要求する

逆に ORM ができてこれができないこと（正面から「捨てた」と書く）:

- DB 可搬性
- identity map と変更追跡による「取ってきたオブジェクトを書き換えて save」
- クラスからのスキーマ生成

## 位置づけ: Fat Database の復権、現代のツールチェーン付き

DB 中心設計（Koppelaars "Fat Database"、PL/SQL 中心の基幹系）が退潮したのは思想の誤りではなく、
git・CI・型検査・テスト自動化がアプリ側にだけ来て DB 側が手作業と無型のまま残ったから。
sqlshape + schema.sql + sqldef はその差を埋める。加えて LLM の時代には、SQL が学習データに濃い言語で
あること、書いた SQL が全部静的検査されるオラクルがあることで、エージェントの試行ループが DB 側ロジック
でも回る。層を減らして機械検査の表面を増やす方針に合う。

業務系に向く理由: 中身が関係データ上の CRUD・集計・不変条件で、DB 側ロジックが最も効く領域そのもの。

Supabase との関係: LLM に見せる表面が「PG のスキーマと SQL」だけ、という強みは同じ。Supabase が
エンタープライズで詰まる点（認可が RLS 一枚、ロジックの置き場が Edge Functions かトリガーで型検査の外、
クライアント直結で監査・外部連携を挟めない、ベンダー結合）を Go のアプリ層で受け、その Go から叩く SQL は
全部 lint 済み。DB 側の規律は Supabase と同じなので、**Supabase からの出口**にもなる — PG・ビュー・関数を
そのまま残して前に Go の層を足すだけで移行できる。スキーマ資産を捨てない移行先。

置く / 置かないの線引き:

- 置く: 往復の多いトランザクション、重い集約の読み取り、行・テーブルを跨ぐ不変条件、公開 API としてのビュー
- 置かない: 外部 API 呼び出し、頻繫に変わるビジネスルール（PL/pgSQL 本体は lint の外）、重い計算
  （DB は水平に増やしにくい）

リスク:

- lint の忠実度が製品の全て。「通ったのに実行時に落ちた」が一度出ると信頼が崩れる。差分テストを最初に
- PG 限定。日本の業務系は MySQL / Oracle / SQL Server が多い。対象は新規か PG 移行済み
- DB 側ロジックのテスト作法（embedded PG 上で Go テストから叩く、pgTAP 相当）を同梱しないと、
  置いた途端に検査の薄い場所になる
- 運用側のトリガーへの恐怖。schema.sql に全部あって lint が依存を列挙する、で「見えない魔法」ではなくする

## MVP

1. analyzer が `sqlshape.Query[R, P](literal)` を拾いリテラルを取り出す
2. text/template/parse で if / switch を全展開、`{{.X}}` を `$n` に置換
3. 生成カタログ + schema.sql で各展開形を解析し、パラメータ型・結果列型・null 許容を取得（正解は embedded PG の PREPARE / Describe と差分テスト）
4. R と結果列、P とパラメータを照合し diagnostics
5. nullability は NOT NULL / JOIN 種別 / COALESCE の伝播で推論し、`col:"name,nullable"` で上書き可

一番面倒なのは PG 型 → Go 型の対応表と、typmod / 多相型の解決。

## 進捗（2026-09-05）

- runtime の穴 2 件: ① ネスト行 — analyzer が `row(...)` / 行全体参照 / 複合型列の形を `Column.Fields` に
  出し、vet は受け側の struct を位置で照合（名前付き複合型は名前と順序、匿名 record は位置のみ）、runtime は
  `CompositeIndexScanner` / `ArraySetter` を実装した位置ベースの dest で pgx のバイナリ複合デコードに乗せる。
  ユーザー型（enum・複合型・配列）は接続の TypeMap に無いと復号できないので、Run が結果列の未知 OID を
  見つけたら pg_type で名前を引いて `LoadTypes` → 再実行（接続ごとに一度）。匿名 record の中の enum は
  スキャン前に見えないので `LoadUserTypes(ctx, conn)`（pool の AfterConnect 用）で全ユーザー型を一括登録。
  ② 未検査展開形 — 「vet が通した集合を埋め込む」のは生成物が要るので採らず、runtime が `Render` ごとに
  分岐シグネチャ（`if@POS:then` 等、静的 expander と同一書式）を組み、同じ署名の静的展開形と SQL を
  バイト一致で照合する。expander と evaluator という 2 つのテンプレート実装の乖離を実行時に検出できる。
  range 3 回以上は静的な双子が無いので照合をスキップ
- 失敗モードの共有（analyze/violation.go + vet/expect.go + runtime `ConstraintError`）: 各 DML 展開形が
  違反し得る制約を列挙する — INSERT は全ユニーク鍵、UPDATE は SET 列に触れる鍵、DELETE / 鍵の更新は
  NO ACTION / RESTRICT で参照している側の FK、書き込む列を参照する CHECK、ドメイン CHECK、値が NULL になり
  得るときの NOT NULL（裸の `$n` ならパラメータ番号を持たせ、Go 型が非 nullable なら vet が落とす）。
  ON CONFLICT はアービタの鍵を吸収。無名制約は schema 側で PG と同じ命名（`users_pkey` /
  `orders_total_check` / `yen_check`）を付ける — 実行時エラーの制約名と一致させるため。
  契約はテンプレート内の `-- sqlshape: expect a, b, t.col` 行（言語非依存の置き場）。vet は
  「違反し得るのに未宣言」と「宣言したが違反し得ない」を両方向で報告。runtime は class 23 の PgError を
  `ConstraintError{Code, Constraint, Table, Column}` に写し `Violates(err, key)` で判別。
  NOT NULL 列を省いたデフォルト無し INSERT は「常に失敗」の Note。未対応: トリガーの独自 SQLSTATE
  （`-- sqlshape: error XX001 = Name`）、EXCLUDE 制約、DEFAULT 式が CHECK を破るケース
- カーディナリティの裏付け（analyze/card.go + `sqlshape.One`）: FROM の各葉について「ユニーク制約の全列が
  既知値〔リテラル・`$n`・外側参照・非相関スカラサブクエリ・それらのキャスト/演算〕と等値束縛されている」
  を関数従属の不動点で証明する。single になった葉の全列は既知になり JOIN の等値で他の葉へ伝播。
  LEFT/RIGHT JOIN の ON は null 側だけを縛る（左側の列を既知にしない）、FULL JOIN は不可、
  部分ユニークインデックスは述語が WHERE に構造一致で再現されているときだけ使う。ビュー・サブクエリ・CTE は
  外から束縛された出力列を種にして定義クエリを再帰的に証明。集約 + GROUP BY 無し・定数 LIMIT 0/1・FROM 無し・
  1 行 VALUES・UPDATE/DELETE の WHERE も同じ枝。`Result.AtMostOne` / `ManyRowsWhy`、vet は `One` 宣言の
  各展開形で証明できなければ理由つきで報告。runtime は `Single.Get`（ErrNoRows）/ `Find` / `Exec`、
  2 行目が来たら `ErrManyRows`（スキーマが証明を裏切った検知）。未対応: 関数呼び出しを既知値に含める
  （volatility 判定が要る）、GROUP BY 列がすべて既知のケース、`First` / LIMIT の ORDER BY 無し警告
- ドメインの不透明化（analyze/domain.go）: PG が基底型で解決する演算・比較・CASE/COALESCE/UNION・代入を
  名目的に検査し、`Result.Notes` として返す（PG は通す文なので `Error` とは別レーン。golden 側は
  Notes ゼロを断言）。リテラル・定数・`$n` は単位を継承、`yen + yen` / `yen * n` / `yen / n` / `-yen` /
  `abs` `max` `round` 等の単位保存関数 / `coalesce(yen, 0)` は結果も `yen` のまま流れ、`yen / yen` は無次元、
  `sum(yen)` は PG どおり numeric（単位喪失は黙認）。`::bigint` で明示的に落とす、`::yen` で主張する。
  代入はドメイン列だけを守る（素の列は単位を宣言していないので何でも受ける）。vet は Notes を
  テンプレート位置に写して診断
- 解釈の共有（vet/binding.go）: enum / ドメイン / キー同一性〔単一列 PK、単一列 FK を根まで辿る〕への
  バインディングを使用箇所から推論し、衝突を報告。enum はラベル集合と typed const を両方向で diff
  （パッケージ跨ぎは ConstSetFact / BindingFact）、`T("typo")` 変換と switch の網羅性も検査。
  アナライザー側の変更は `Result.ParamSources`（パラメータが比較・代入された列）の追加だけ。
  `-strict` で「無名型で受けていて検査できない」を報告。想定どおり明示登録 API は不要だった
- runtime（ルートパッケージ）: 自前の template 評価器で `{{.X}}` → `$n` + 引数列（静的 expander と同じ
  番号付け規則）、`Run` = `iter.Seq2[R, error]` / `Collect` / `First` / `Exec`、名前ベースの行マッパー
  （col/db タグ・snake_case、過不足はエラー）、named string 型とその slice は string / []string に正規化して
  pgx に渡す（未登録 enum 配列 OID でも text 形式で通る）。実 PG で round trip 確認。展開形ごとの statement
  キャッシュは pgx の自動 prepared statement キャッシュに乗る。未実装: array_agg(row) のネスト構造体、
  未検査展開形の実行時 panic（vet が通した集合を埋め込む経路が要る）
- expand / vet / cmd/sqlshape で縦に通った: `go run ./cmd/sqlshape ./examples/...` が実 Go コードに対して
  型・列・nullability の指摘を出す。診断はテンプレート内の位置に写す（raw string 前提）
- oracle / catalog / schema / analyze の 4 パッケージが動作。analyze はオラクル golden 57 本
  （SELECT / JOIN / CTE / 集合演算 / VALUES / 関数 in FROM / DML + RETURNING / エラー 10 種）と一致
- 未実装: GROUP BY 妥当性検査（42803）、照合順序、range 型の subtype、ROWS FROM、データ変更 CTE、
  nullability の精緻化（現状は NOT NULL + 外部結合 + 主要な式規則のみ）

## 実装状況の棚卸し（2026-09-05）

本文に書いた機能を、実装済み（✅）・部分（🔶）・未着手（⬜）・方針転換（↪）で仕分ける。
各項の根拠は上の各節。未着手のうち「別マイルストーン」は Komado ゴール `sqlshape/migration` /
`sqlshape/example-app` 側。

### 検査器の核（internal/analyze）

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | §10 型変換（演算子・関数・多相・キャスト・select_common_type）、スコープ、DML + RETURNING、`$n` 推論、リテラル検証 | golden 57 本でオラクル一致 |
| ✅ | 生成カタログ（COPY dump → TSV embed）、schema.sql の取り込み | |
| 🔶 | nullability | NOT NULL / JOIN 種別 / 主要な式規則 + null 除外述語（`IS NOT NULL`、strict 演算子での比較、内部結合の ON）で結果列を not null に。外部結合の null 側に述語が掛かれば基表の NOT NULL 列も戻る。残: CASE 全分岐、非 strict 関数の個別規則 |
| ✅ | `-- sqlshape: not null` 注釈 | schema.sql では CREATE FUNCTION 直前のコメントで戻り値を NOT NULL に、テンプレートでは `-- sqlshape: not null col, col` で結果列を上書き。STRICT なユーザー関数も引数から伝播 |
| ✅ | GROUP BY 妥当性（42803） | analyze/grouping.go: deparse 一致・集約引数・PK による関数従属、GROUPING SETS は未検査。golden 5 本 |
| ✅ | ROWS FROM、データ変更 CTE | ROWS FROM は関数ごとの列を横並び（列定義リスト・WITH ORDINALITY・別名込み）、データ変更 CTE は RETURNING 列を CTE の列にし失敗モードも合算 |
| ✅ | range の subtype | pg_range を dump し anyrange ⇔ anyelement ⇔ anymultirange を解決（lower/upper/range_agg/multirange） |
| ✅ | 照合順序 | analyze/collation.go: 式に照合と導出の強さ（none / implicit / conflict / explicit）を持たせ parse_collate.c の merge 規則で合成。明示 COLLATE 同士の衝突と、UNION / INTERSECT / EXCEPT（ALL 以外）での暗黙衝突は PG と同じ 42P21 の Error（golden）。暗黙の非既定照合同士の衝突は PG が Describe を通して実行時に落ちるので、比較演算子・LIKE / 正規表現・lower / upper / min / max・GREATEST / LEAST・ORDER BY・GROUP BY・DISTINCT に届いたとき Note。照合名の存在は検証しない |
| ✅ | `CALL procedure(...)` | 引数は関数と同じ経路（OUT も渡す）、INOUT / OUT が結果行、関数の CALL / プロシージャの SELECT は 42809。オラクルは拡張プロトコルの Parse で CALL を Describe できた |
| ✅ | `LANGUAGE sql` / `BEGIN ATOMIC` 関数本体の検査と、関数越しのテーブル依存 | analyze/function.go: 引数を名前と `$n` で見せて各文を解析（同名列が優先、PG と同じ）、最終文の形を RETURNS と照合（代入キャスト許容、42P13）。vet はスキーマ読込時に全関数を検査。`FunctionResult.Relations` が関数越しの依存 |
| ↪ | EXPLAIN 系 lint | lint 時に PG を起動しない方針を維持し、統計非依存の構造的 advisory に置換（`-strict`）: ① 述語列に先頭一致するインデックス / 鍵が無いテーブル述語（schema が全インデックスを保持）② ビューへ押し込めない述語（LIMIT/OFFSET・集合演算・ウィンドウ関数・GROUP BY ビューの非グループ列）。「ネスト内に上限なし」は構造的に決められないので不採用。EXPLAIN 本体はオラクル上でも統計が違うため採らない |
| ⬜ | 拡張の `pg_proc` を dump して取り込む経路 | 生成器は同形式なので経路は開いている |
| ✅ | 述語ポリシー | schema.sql でテーブル直前に `-- sqlshape: visible where deleted_at IS NULL`。全 select レベルで葉ごとに述語の構造一致（WHERE / その葉を縛る ON）を要求、ビュー本体は AnalyzeView でスキーマ問題として一度だけ報告、テンプレートは `-- sqlshape: unfiltered t` で明示的に外す。vet フラグ案は棄却（ポリシーはスキーマの知識） |

### テンプレートと展開（internal/expand）

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | if / else / with / range 0・1・2、`{{.X}}` → `$n`、同一パス同一番号、位置写像、256 上限 | |
| ↪ | `{{switch .Sort}} {{case ...}}` | text/template に無く、前処理で書き換えると位置写像が崩れる。`{{if eq .Sort "x"}} ... {{else if eq .Sort "y"}} ... {{end}}` で書く方針に決め、書き味の例を直した |
| ✅ | 分岐爆発の退避路 | 全組合せが 256 を超えたら「全 off・全 on・各分岐単独 on」の疎な集合に自動で落とす（`Result.Sparse`）。依存する断片は単独形でエラーになるのが再構成の合図。runtime の展開形照合はこのとき無効、`-strict` で疎検査を通知。「非独立な組み合わせだけ全列挙に戻す」自動判定は棄却（独立性の判定が要る） |
| ↪ | 分岐ごとに結果型が変わるクエリを sum 型で返す | Go に sum 型が無いので「省略可能な投影」に読み替え: ある展開形にだけ無い列は nullable フィールド（ポインタ / slice）で受け、その分岐では nil のまま。全展開形で無い列は従来どおりエラー。runtime の行マッパーも nullable フィールドの欠落を許す。タグ付き union の生成は棄却（生成物が要る） |
| ✅ | interpreted string リテラルの位置写像 | エスケープを復号しながら源位置を辿る |

### 照合（internal/vet）

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | 結果列 ↔ R、`$n` ↔ P、方向つき精度損失、nullable ラッパ、schema.sql 探索、分岐名つき診断 | |
| ✅ | ネスト行（`array_agg(row(...))` / `array_agg(t)` / 複合型列 ↔ struct の位置照合） | |
| ✅ | 未検査クエリ | 非定数引数はエラー、`-coverage` でパッケージごとに「N of M checked」を報告 |
| ✅ | enum の零値警告（P の非ポインタ enum フィールド） | `-strict` |
| ✅ | enum の宣言順比較 info（`ORDER BY status`、`status < 'x'`） | analyzer の advisory Note、`-strict` |
| 🔶 | 表現の忠実さ: `timestamp`（tz 無し）、`date` の tz | `-strict`。`varchar(n)` 長さは静的に見えないので対象外、citext は string で受ける |
| ✅ | 既定値と生成値の所有者（P 非ポインタ ⇔ DEFAULT / identity 列） | `-strict`。GENERATED / identity ALWAYS への明示挿入は PG 自身のエラー |
| 🔶 | LIMIT の ORDER BY 無し警告 | analyzer の advisory Note、`-strict`。`First` は呼び出し箇所なので vet からは見えない。`array_agg` ネスト側の 1:1 / 1:N 突合は未 |
| ✅ | 行の所属（列ポリシー lint） | `-require-columns=tenant_id`: その列を持つテーブルへの全文が等値で固定（INSERT は代入）していることを `Result.Fixed`（全 select レベルの等値束縛）で検査。`deleted_at IS NULL` のような述語ポリシーは未 |
| ✅ | テーブル直参照禁止 lint（`-no-tables`）、サービス境界（`-schemas=a_api,b_private`） | analyzer が `Result.Relations`（直接参照した関係、ビューは展開しない）を出す。DROP 影響分析 / 死んだスキーマ検出の土台 |
| ✅ | `COMMENT ON` を Go doc / gopls hover へ | `-sync-comments`: 列 / テーブルの COMMENT を、doc コメントの無い受け側フィールド / 型への SuggestedFix として提案（gopls の quick fix で適用 → hover に出る） |
| 🔶 | MV: REFRESH CONCURRENTLY に要るユニークインデックス | `-strict` のスキーマ advisory。依存元テーブル一覧は `Result.Relations` を MV 定義に掛ければ出るが出力先が未定 |
| 🔶 | 値集合の CHECK IN / lookup テーブル対応 | `CHECK (col IN (...))` / `= ANY(ARRAY[...])` を列の値集合として enum と同じ両方向 diff・変換・switch 検査に載せた。lookup は `@data` 宣言（マイグレーション側）待ち |

### 解釈の共有

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | enum バインド + 両方向 diff + `T("typo")` + switch 網羅性（パッケージ跨ぎは Fact） | |
| ✅ | キー同一性（単一列 PK / FK を根まで）、ドメインバインド、`-strict` の無名型報告 | |
| ✅ | ドメインの不透明化（演算・比較・CASE/COALESCE/UNION・代入） | |
| ✅ | カーディナリティ: `One` のユニーク鍵証明（JOIN・ビュー・サブクエリ・CTE 越し） | |
| ✅ | 失敗モード: 違反し得る制約の列挙 + `-- sqlshape: expect` + `ConstraintError` | |
| ✅ | トリガーの独自 SQLSTATE（`-- sqlshape: error XX001 = Name`）→ 型付きエラー | schema が CREATE TRIGGER を取り込み、トリガー関数の directive を DML の失敗モードに載せる。expect 行にはコードを書く（名前は診断文のみ）。runtime は expect 行にあるコードの PgError を ConstraintError に写す |
| ✅ | 複合キー（複数列 PK / FK）の同一性 | 複合 FK は位置で参照先の列へ、複合 PK の各列はそれぞれ同一性 |
| ✅ | `One` の既知値に関数呼び出しを含める、GROUP BY 列がすべて既知のケース | 解析時に関数呼び出しごとの volatility を記録し、stable / immutable かつ引数が既知なら既知。GROUP BY は全グループ式が固定されていれば 1 グループ（ビュー `order_stats WHERE user_id = $1` が One になる） |

### runtime

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | Run / Collect / First / Exec、Render、名前ベース行マッパー、named string の正規化 | |
| ✅ | ネスト行の位置スキャン、ユーザー型の遅延 `LoadTypes`、`LoadUserTypes` | |
| ✅ | `ConstraintError` / `Violates`、`Single.Get` / `Find` / `ErrManyRows` | |
| ↪ | 未検査展開形の実行時 panic | 「vet が通した集合の埋め込み」は生成物が要るので採らず、分岐シグネチャで静的展開形と SQL をバイト一致照合し error にする |
| ✅ | 未知 enum ラベル受信の型付きエラー | Go の enum 型が `Known() bool`（`Labelled`）を実装していれば行マッパーが検証し `*UnknownLabelError`。実装しなければ素通し。panic モードは置かない |
| ✅ | 毎回 custom plan フラグ | `Stmt.Unprepared()` / `Single.Unprepared()`: pgx の `QueryExecModeExec` で prepared statement を作らず実行。展開形ごとのキャッシュ制御は pgx に委ねたまま |
| ✅ | MV の型付き `Refresh` ハンドル | `sqlshape.MatView("name").Refresh / RefreshConcurrently`、vet が名前と種別を検査 |

### スキーマ / マイグレーション（ゴール `sqlshape/migration`）

| 状態 | 項目 | 備考 |
|---|---|---|
| ✅ | schema.sql → テーブル / ビュー / MV / enum / ドメイン / 複合型 / 関数 / 制約 / ユニークインデックス / COMMENT | 無名制約は PG と同じ命名 |
| ⬜ | 参照の全数解析: DROP 影響分析、死んだスキーマ検出、HEAD~1 との世代跨ぎ検査、ドリフト検出、sqldef DROP ゲート | analyzer は列参照を全部見ているが、集計して出す層が無い |
| ⬜ | 意図宣言 `@migrate`（rename / enum 値の削除 / backfill）と diff の整合検査、手順生成 | |
| ⬜ | 固定値テーブルの `@data` 宣言 → 差分適用 + ドリフト検出、値集合としての読み取り | 本命 |
| ⬜ | 再生の外（CREATE EXTENSION、ロール、search_path、PG 版）を schema.sql に書かせて検査 | |
| ⬜ | psqldef の CREATE FUNCTION / MV 差分の対応確認 | 対応が薄ければ全量再適用の分担 |

### 周辺・同梱物

| 状態 | 項目 | 備考 |
|---|---|---|
| ⬜ | example プロジェクト（ビュー / 関数 / 複合型 / enum / lookup を含む schema.sql、読み・書き・動的絞り込みの 3 形） | ゴール `sqlshape/example-app` |
| ✅ | DB 側ロジックのテスト作法 | `pgtest.Start(ctx, schemaSQL)` — オラクルと同じ embedded PG に schema.sql を流した `*pgx.Conn` を返す |
| 🔶 | 言語非依存化 | analyze / expand / schema は Go 非依存。vet / runtime が Go フロントエンド。契約（expect 行）は SQL 側に置く方針で揃えた |

### 本文と実装のズレ（本文側を直すか判断が要るもの）

- 書き味の例と「展開形にハッシュ…panic」の記述は 2026-09-05 に本文側を実装に合わせて修正済み
- prober は方式 2（生成カタログ + §10 仕様実装）で確定、embedded PG はオラクル。カタログは `.dat` 解析ではなく COPY dump
- 「runtime が接続時に検査で見た enum 型を全部 LoadType」は、結果列の未知 OID を見つけたときの遅延ロード +
  `LoadUserTypes` の一括登録に置き換えた（runtime は検査結果を持たないため）

## 未解決

- **nullability** の推論精度。NOT NULL 制約 + JOIN 種別 + COALESCE の規則で 9 割を拾い、
  残りは注釈で逃がす。オラクル（実 PG）は null 許容を返さないので、ここだけ差分テストが効かない
- 制御用とバインド用で同じ変数を使うときの型付け（pointer か Optional か）
- range の 0・1・2 展開で代表できない構造（再帰的 CTE の動的生成）は対象外
- 再生の外にあるもの（CREATE EXTENSION、ロール、search_path、PG バージョン）は
  schema.sql 側に書かせて検査対象に入れる

## 既存との位置関係

- **SafeQL**（TS）: lint + 実 DB + 宣言型照合まで同じ。動的テンプレートなし、TS 限定
- **sqlvet**（Go）: 構文 + 列存在の検査。実 DB なし、型照合なし、メンテ停止
- **safesql**（Go, Stripe）: SQL 引数の定数性検査のみ
- **Doma / uroboroSQL / DBFlute**（Java）: 2-way SQL。既定 1 形しか検査しない
- **sqlc**: 静的ファイル前提のコード生成。動的 SQL で詰まる
- **sqldef / Atlas**: 宣言スキーマ側単独。クエリ検査と結線した製品はない
- **PostgREST / Supabase**: Schema Isolation（テーブルは private、`api` スキーマのビューと関数だけ公開）を
  標準作法として推奨し、Supabase の規模で実運用実績がある。ただしアプリ層を捨てて DB を直接 API にする形。
  sqlshape はアプリ層を Go で残しつつ同じ規律を持ち込み、呼び出し側まで型検査する
- ORM 系: モデルがテーブルと 1 対 1 の前提なのでビュー主役は構造的に無理。Rails Scenic のように
  ビューをマイグレーション管理する補助はあるが推奨作法ではない

欠けている 1 点は「テンプレートの出力集合を有限として列挙し全形を検査する」。
