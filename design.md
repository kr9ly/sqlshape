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
- **prober**: embedded-postgres に `schema.sql` を流し、各展開形を PREPARE / Describe
- **matcher**: 結果列 ↔ R フィールド（名前 + `col` タグ）、パラメータ ↔ P フィールドの型照合
- **runtime**: pgx 直結。テンプレート実行、`$n` 引数列、名前ベースの行マッパー、
  展開形ごとの statement キャッシュ
- 補助: pg_query_go（libpg_query）で断片の切り出し・バインド位置の式文脈特定

PGlite は JS ホスト前提で Go からは使いづらい。embedded-postgres（実バイナリ、起動 1 秒弱）で行く。

## MVP

1. analyzer が `sqlshape.Query[R, P](literal)` を拾いリテラルを取り出す
2. text/template/parse で if / switch を全展開、`{{.X}}` を `$n` に置換
3. `schema.sql` を流した embedded PG に各展開形を PREPARE、Describe で型取得
4. R と結果列、P とパラメータを照合し diagnostics
5. nullability は `col:"name,nullable"` の明示だけ。推論は後回し

一番面倒なのは PG 型 → Go 型の対応表と、Describe の OID を pgx 型マップに通す部分。

## 未解決

- **nullability**。Describe は結果列の null 許容を返さない。NOT NULL 制約 + JOIN 種別 +
  COALESCE 程度の規則で 9 割を拾い、残りは注釈で逃がす。どのアプローチでも避けられない壁
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
