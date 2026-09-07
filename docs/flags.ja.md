# フラグとエディタ設定

[English](flags.md)

`cmd/sqlshape`は`go vet -vettool`互換の検査器である。`sqlshape ./...`、`sqlshape vet ./...`、`go vet -vettool=$(which sqlshape) ./...`のどれでも実行でき、以下のフラグはどの形でも同じように渡せる。マイグレーションのサブコマンド（`diff`、`apply`、`verify-schema`）のフラグは別で、[migrations.ja.md](migrations.ja.md)にある。

## 検査器のフラグ

| フラグ | 既定 | 意味 |
|---|---|---|
| `-schema PATH` | パッケージから上に辿って最初に見つかる`schema.sql`か`schema/` | 検査に使うスキーマ。ディレクトリなら`*.sql`を名前順に適用する |
| `-strict` | off | 助言も報告する（[下記](#-strict)） |
| `-no-table-reads` | off | テーブルの読み取りを禁じる。SELECTも書き込み中の読み取り部分もビューを通す。INSERT / UPDATE / DELETE / MERGEの対象にはテーブルを使える |
| `-no-tables` | off | テーブルへの参照を一切禁じる。アプリケーションはビューを読み、関数を呼ぶ |
| `-schemas=a_api,b_private` | 全部 | このコードが参照してよいPostgreSQLのスキーマ。1つのデータベースを複数サービスで使うときの境界 |
| `-require-columns=tenant_id` | なし | すべての文が、その列を持つ各テーブルでその列を等値で固定しなければならない。INSERTは値を入れなければならない。その列を固定する行レベルセキュリティのポリシーがあれば満たしたことになる |
| `-raw-sql=constant` | `constant` | sqlshapeを通さないドライバ呼び出し（pgx / `database/sql`の`Query`、`Exec`など）の扱い。`constant`はSQL引数が定数であることを要求し、`forbid`は拒否し、`allow`は無視する |
| `-raw-sql-allow=pkg/...` | なし | `-raw-sql=forbid`を適用しないパッケージ（`/...`で終わる接頭辞も可） |
| `-coverage` | off | パッケージごとに、検査した`Query` / `One`の数と、検査できなかった数（テンプレートが定数でないもの）を報告する |
| `-sync-comments` | off | スキーマの`COMMENT ON`から、結果の構造体のフィールドと型にdocコメントを提案する（`-fix`で適用） |
| `-fix` | off | 提案された修正（構造体の書き換え、docコメント）をソースに適用する |

## `-strict`

`-strict`を付けると助言が加わる。ルール違反ではなく、意図してそう書いた可能性もあるが、一度見直す価値のあるもの、である。文についての助言:

- どの展開でも使われていない`P`のフィールド
- enum・ドメイン・キー列を無名のGo型（`string`、`int64`）で受けている。結びつきの検査ができない
- `timestamp`や`date`を`time.Time`で受けている。タイムゾーンや時刻の情報が無いのに補われる
- enumのパラメータがポインタでない。ゼロ値の`""`はラベルではないので、未設定のまま渡すと実行時に失敗する
- `DEFAULT`やidentityを持つ列に、NULLを表せない型のパラメータで常に値を書き込んでいる。データベース側の既定値が使われることがない
- `ORDER BY`の無い`LIMIT`。どの行が返るか決まらない
- enumの比較や`ORDER BY`。アルファベット順ではなく宣言順で並ぶ
- 先頭列にインデックスの無い条件で絞っているテーブル（全表走査になる）、プランナーがビューの中に押し込めない条件（ビューに`LIMIT` / `OFFSET`、集合演算、ウィンドウ関数がある）
- 分岐の組み合わせが256を超えて疎に検査されたテンプレート
- 一意インデックスの無いビューに対する`MatView.RefreshConcurrently`
- `-require-columns`を、`FORCE ROW LEVEL SECURITY`でないテーブルのポリシーで満たしている。所有者には効かない

スキーマについての助言:

- enumの列。seed済みlookupテーブルのほうが変更しやすく、検査は同じようにできる
- 一意インデックスの無いマテリアライズドビュー。concurrentlyにrefreshできない
- 行レベルセキュリティが有効なのにポリシーが無いテーブル。所有者以外には行が見えない
- `current_setting(name, true)`を読むポリシー。設定していないセッションではNULLになり、エラーにならずに何も見えなくなる
- 所有者にはポリシーが効かないテーブルに到達する`SECURITY DEFINER`関数
- PL/pgSQLの`EXECUTE`で実行時に組み立てた文字列を実行している。検査できない

## エディタで使う

検査器は`go vet`のツールなので、`go vet`が走る場所ならどこでも走る。一度ビルドして`-vettool`に指定する:

```
$ go install github.com/kr9ly/sqlshape/cmd/sqlshape@latest
$ go vet -vettool="$(which sqlshape)" -strict ./...
```

Go統合のあるエディタは保存時に`go vet`を走らせて診断をインラインに表示できるので、その設定に同じ`-vettool`と`-strict`を渡す。gopls自体はサードパーティのアナライザーを読み込まないため、goplsではなく`go vet`を経由する。CIでも同じコマンドを走らせればよい。golangci-lintにはモジュールプラグインとして読み込める。

### SQLから構造体を書き起こす

`type OrderRow struct{}`と`type OrderParams struct{}`を空で宣言し、クエリを書いて保存する。すべての結果列について対応するフィールドが無い、すべての`{{.X}}`について対応するパスが無い、という診断が出て、それぞれにクエリから構造体を書き換えるquick fixが付く。書き起こされる内容:

- すべての分岐の列。一部の分岐だけが選ぶ列はポインタになる
- NULLになりうる列はポインタになる
- enumやlookupの値は、モジュール内で既に結びつけられているGo型になる
- レコードと複合型はネストした構造体、その配列はスライスになる
- `COMMENT ON`があればdocコメントになる
- `P`については、パスごとにSQL側が期待する型のフィールド（`.Filter.Name`ならネストした構造体、`range`ならスライス）と、`{{if .Flag}}`で調べるだけのフィールドには`bool`

同じquick fixは、以後に不一致が出たとき（SELECTに列を足した、スキーマで型を変えた）にも付く。まだ合っているフィールドの名前・docコメント・タグ・型は保たれるので、選択肢の中から選んだ型（`numeric`に対する`decimal.Decimal`など）は残る。`numeric`はモジュールが`shopspring/decimal`を既にimportしていれば`decimal.Decimal`、していなければ`pgtype.Numeric`になる。`uuid`も同様に、使用中のuuidパッケージの型になる。

コマンドラインからは`sqlshape -fix ./...`で適用する。エディタからは診断に付いたquick fixで適用する。
