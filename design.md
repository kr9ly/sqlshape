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
別ディレクトリの命令的ステップとして持ち、sqlshape は無視する。

## 書き味（Go）

テンプレート構文は text/template を借用（`text/template/parse` を流用、実行器は自前）。
規則は 3 つ: `{{.X}}` が式位置なら バインド、`if` 条件なら制御、両方なら省略可能。

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
    ORDER BY {{switch .Sort}}
      {{case "newest"}} o.created_at DESC
      {{case "total"}}  o.total DESC
    {{end}}
    LIMIT {{.Limit}}
`)

for row, err := range listOrders.Run(ctx, db, ListOrdersParams{Status: &s}) { ... }
```

- 基本形は `iter.Seq2[R, error]`。`Collect` / `First` はヘルパー
- 1 対多は SQL 側で `array_agg(row(...))` に書かせ、PG の複合型からネスト構造体を導出
- 可変長 IN は `= ANY($1)` に統一
- 展開形にハッシュを振り、検査器が通した集合に無い形が実行時に出たら panic

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
- カタログ生成も初日。`pg_type.dat` / `pg_proc.dat` / `pg_operator.dat` / `pg_cast.dat` /
  `pg_aggregate.dat` を Go のテーブルに吐く
- マニュアル §10（型変換）を先に、構文カバレッジは後に。演算子解決・関数解決・暗黙キャスト・
  多相型・UNION/CASE の統一が本体。JOIN / サブクエリ / CTE / 集合演算 / ウィンドウは
  スコープと列可視性の問題で型推論より単純

細部で踏みやすいもの:

- `$n` の型推論。PG は `unknown` から文脈解決し、`SELECT $1` は「型を決定できない」になる。
  鏡写しにしないと偽陰性
- typmod（`varchar(20)`、`numeric(10,2)`）の伝播規則が関数ごとに違う
- 集合返却関数と LATERAL、RETURNING、`INSERT ... ON CONFLICT` の列可視性

スコープ外: PL/pgSQL、ルール、トリガー、照合順序、`.dat` に無い拡張の関数。
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

## 位置づけ: Fat Database の復権、現代のツールチェーン付き

DB 中心設計（Koppelaars "Fat Database"、PL/SQL 中心の基幹系）が退潮したのは思想の誤りではなく、
git・CI・型検査・テスト自動化がアプリ側にだけ来て DB 側が手作業と無型のまま残ったから。
sqlshape + schema.sql + sqldef はその差を埋める。加えて LLM の時代には、SQL が学習データに濃い言語で
あること、書いた SQL が全部静的検査されるオラクルがあることで、エージェントの試行ループが DB 側ロジック
でも回る。層を減らして機械検査の表面を増やす方針に合う。

業務系に向く理由: 中身が関係データ上の CRUD・集計・不変条件で、DB 側ロジックが最も効く領域そのもの。

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

欠けている 1 点は「テンプレートの出力集合を有限として列挙し全形を検査する」。
