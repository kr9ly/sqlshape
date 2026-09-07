# フラグとエディタ設定

[English](flags.md)

`cmd/sqlshape`は`go vet -vettool`互換の検査器。`sqlshape ./...`、`sqlshape vet ./...`、
`go vet -vettool=$(which sqlshape) ./...`のどれでも走り、下のフラグはどの場合も同じように渡す。
マイグレーションのサブコマンド（`diff`、`apply`、`verify-schema`）には固有のフラグがあり、
[migrations.ja.md](migrations.ja.md) に載せてある。

## 検査器のフラグ

| フラグ | 既定 | 意味 |
|---|---|---|
| `-schema PATH` | パッケージから上に辿って最も近い`schema.sql`か`schema/` | 検査対象のスキーマ。ディレクトリなら`*.sql`を名前順に適用 |
| `-strict` | off | 助言的な指摘も報告する（[下記](#-strict)） |
| `-no-table-reads` | off | SELECTと書き込みの読み部分はビューを通す。テーブルはINSERT / UPDATE / DELETE / MERGEの対象にはなれる |
| `-no-tables` | off | テーブルへの直接参照を一切禁じる: アプリケーションコードはビューを読み関数を呼ぶ |
| `-schemas=a_api,b_private` | 全部 | このコードが参照してよいPostgreSQLスキーマ（1つのデータベース上のサービス境界） |
| `-require-columns=tenant_id` | なし | すべての文がその列を持つ各テーブルで等値による固定をしなければならない。INSERTは代入する（列を固定する行レベルセキュリティのポリシーでも満たせる） |
| `-raw-sql=constant` | `constant` | sqlshape外のドライバ呼び出し（pgx / `database/sql`の`Query`、`Exec`、…）: `constant`はSQLが定数文字列であることを求め、`forbid`は拒否し、`allow`は無視する |
| `-raw-sql-allow=pkg/...` | なし | `-raw-sql=forbid`を適用しないパッケージ（`/...`で終わる接頭辞も可） |
| `-coverage` | off | パッケージごとに検査した`Query` / `One`宣言の数と、検査できなかった数（定数でないテンプレート）を報告 |
| `-sync-comments` | off | スキーマの`COMMENT ON`から結果structのフィールドと型にdocコメントを提案する（`-fix`で適用） |
| `-fix` | off | 提案された修正（structの書き換え、docコメント）をソースに適用する |

## `-strict`

`-strict`は助言的な指摘を加える: 合法で意図的かもしれないが、一目見る価値のあるもの。文について:

- どの展開も読まない`P`のフィールド
- 無名のGo型（`string`、`int64`）で運ばれるenum・ドメイン・キー列。バインディングの検査が追えない
- `time.Time`で受けた`timestamp`や`date`（ゾーン、または時刻が捏造される）
- 非ポインタのenumパラメータ: 零値`""`はラベルでなく実行時に失敗する
- `DEFAULT`やidentityを持つ列に常に書き込む非nullableなパラメータ。データベースの既定値が適用されない
- `ORDER BY`の無い`LIMIT`: どの行が返るか未定義
- enumの比較や`ORDER BY`。アルファベット順でなく宣言順で並ぶ
- 先頭列にインデックスの無いテーブル述語（全表走査）、プランナーがビューに押し込めないビュー述語
  （ビューに`LIMIT` / `OFFSET`、集合演算、ウィンドウ関数がある）
- 分岐の組み合わせが256を超えて疎に検査されたテンプレート
- 一意インデックスの無いビューへの`MatView.RefreshConcurrently`
- `FORCE ROW LEVEL SECURITY`の無いテーブルでポリシーにより満たされた`-require-columns`（所有者には固定が効かない）

スキーマについて:

- enum列: seed済みlookupテーブルの方が変えやすく、同じように検査される
- 一意インデックスの無いマテリアライズドビュー。concurrentlyにrefreshできない
- 行レベルセキュリティが有効でポリシーの無いテーブル: 所有者以外は行が見えない
- `current_setting(name, true)`を読むポリシー: 設定しなかったセッションはNULLを得て、黙って行が見えなくなる
- 所有者を縛らないポリシーを持つテーブルに到達する`SECURITY DEFINER`関数

## エディタで使う

goplsはサードパーティのアナライザーを読めないので、検査器は`go vet`として走る。Goのエディタ統合は保存時に走らせる:

```
$ go build -o "$(go env GOPATH)/bin/sqlshape" github.com/kr9ly/sqlshape/cmd/sqlshape
$ go vet -vettool="$(go env GOPATH)/bin/sqlshape" -strict ./...
```

VS Code（Go拡張）:

```json
"go.vetOnSave": "workspace",
"go.vetFlags": ["-vettool=/path/to/sqlshape", "-strict"]
```

指摘はProblemsペインに出る。他のエディタ: 同じ`go vet`コマンドを保存時のlinterとして走らせるか、
golangci-lintに`sqlshape`をモジュールプラグインとして加える。

### SQLから文の型を書き起こす

`type OrderRow struct{}`と`type OrderParams struct{}`を空で宣言し、クエリを書いて保存する。すべての結果列が
フィールド無しとして、すべての`{{.X}}`がパス無しとして報告され、それぞれの診断にクエリからstructを書き換える
quick fixが付く:

- すべての分岐の列。一部の分岐だけが選ぶ列はポインタ
- nullableな列はポインタ
- enumとlookupの値は、モジュール内で既に束縛されているGo型
- レコードと複合型はネストしたstruct、その配列はスライス
- `COMMENT ON`からのdocコメント
- `P`について: パスごとにSQLが期待する型のフィールド（`.Filter.Name`はネストしたstruct、`range`はスライス）、
  フィールドを調べるだけの`{{if .Flag}}`ごとに`bool`

同じ修正は以後のすべての不一致（SELECTに足した列、スキーマで変えた型）に付く。まだ合っているフィールドは名前・
docコメント・タグ・型を保つので、選択肢から選んだ型（`numeric`の`decimal.Decimal`）は残る。`numeric`はモジュールが
既にimportしていれば`shopspring/decimal.Decimal`、なければ`pgtype.Numeric`。`uuid`も同様に使用中のuuidパッケージを選ぶ。

コマンドラインからは`sqlshape -fix ./...`で修正を適用し、エディタからはProblemsペインのquick fixで。
