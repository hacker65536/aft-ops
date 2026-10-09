# AFT Operations Toolkit (`aft-ops`) — 設計書 v1

- Status: Draft v1
- Date: 2026-07-24
- 前提: [requirements.md](requirements.md) v1

---

## 1. 全体アーキテクチャ

### 1.1 レイヤ構成

UI（CLI / TUI）とコアロジックを完全分離する。TUI と CLI は同一のコア層 API のみを呼び出し、コア層は UI を一切知らない。

```
┌──────────────────┐   ┌──────────────────┐
│  CLI (cobra)     │   │  TUI (Bubble Tea)│      interfaces 層
│  internal/cli    │   │  internal/tui    │
└────────┬─────────┘   └────────┬─────────┘
         └──────────┬───────────┘
┌────────────────────────────────────────┐
│  Core Services                         │      core 層（UI 非依存）
│  pipeline / account / logs / release   │
├────────────────────────────────────────┤
│  Batch Engine │ Cache │ Metrics        │      基盤層
├────────────────────────────────────────┤
│  AWS Adapter (SDK for Go v2)           │      adapter 層
│  codepipeline / codebuild / cwlogs /   │
│  dynamodb / organizations / ssm        │
└────────────────────────────────────────┘
```

### 1.2 依存の向き

- interfaces → core → 基盤 → adapter の一方向のみ
- core 層は AWS SDK の型を公開 API に露出させない（自前のドメインモデルに変換）
- adapter は interface として定義し、テストではモック実装を注入

### 1.3 主要ライブラリ

| 用途 | ライブラリ | 選定理由 |
|---|---|---|
| CLI | `spf13/cobra` | サブコマンド体系・補完生成の事実上の標準 |
| TUI | `charmbracelet/bubbletea` + `bubbles` + `lipgloss` | Go TUI の標準。table/list/viewport 部品が揃う |
| AWS | `aws-sdk-go-v2` | middleware で計測を差し込める。adaptive retry 内蔵 |
| レート制御 | `golang.org/x/time/rate` | token bucket。適応制御の土台 |
| 設定 | 自前実装（`gopkg.in/yaml.v3` + env + flag） | 要件は「設定ファイルで flag の既定値を省略できること」のみ（多フォーマット対応は不要と確認済み）。単純マージなら自前が最小依存で、優先順位・strict 検証を完全制御できる。viper は依存が重くキー大小文字非区別、koanf は多フォーマット等が要る場合の対案 |
| テーブル出力 | `charmbracelet/lipgloss/table` または自前 | TUI と描画系統を統一 |

## 2. プロジェクト構成

新規リポジトリ `aft-ops` として開始（既存の Bash ツールセットは参照資料として残し、移行完了後にアーカイブ）。

```
aft-ops/
├── cmd/aft-ops/main.go          # エントリポイント（薄く保つ）
├── internal/
│   ├── cli/                     # cobra コマンド定義
│   │   ├── root.go
│   │   ├── pipeline.go          # pipeline list/show/release/logs
│   │   ├── account.go           # account list
│   │   ├── cache.go             # cache status/clear/refresh
│   │   └── metrics.go           # metrics show
│   ├── tui/                     # Bubble Tea アプリ
│   │   ├── app.go               # ルートモデル・画面遷移
│   │   ├── pipelinelist.go      # 一覧画面
│   │   ├── executions.go        # 実行履歴画面（1 パイプラインの実行一覧）
│   │   ├── actions.go           # アクション一覧画面（1 実行のアクション + インライン詳細）
│   │   ├── pipelinelog.go       # ログ閲覧画面
│   │   └── release.go           # 一括 release（confirm → run → results）
│   ├── core/
│   │   ├── model/               # ドメインモデル（Pipeline, Account, Execution, StageState...）
│   │   ├── pipeline/            # 一覧・詳細・release のサービス
│   │   ├── account/             # アカウント解決サービス
│   │   ├── logs/                # CodeBuild ログ取得・terraform ログ抽出・結論の判定
│   │   └── result/              # 実行・build ごとの terraform 結果（一覧 / Actions 共通。§4.6）
│   ├── batch/                   # 逐次バッチエンジン
│   ├── cache/                   # TTL ファイルキャッシュ
│   ├── metrics/                 # API 計測（SDK middleware + 集計）
│   ├── awsx/                    # AWS クライアント生成・レートリミッタ・リトライ
│   ├── config/                  # 設定の読込・マージ
│   ├── demo/                    # fixture 駆動のフェイク AWS クライアント（--demo）
│   └── output/                  # table/json レンダラ、exit code 規約
├── docs/
│   └── demo/                    # fixture・VHS tape・録画スクリプト・生成 GIF
├── .goreleaser.yaml
└── .github/workflows/           # CI (test/lint) + release
```

- `internal/` 配下に置き、ライブラリとしての外部公開は当面しない（OSS 公開＝バイナリ・コード公開であり、Go API の互換性保証はしない）
- コード・コメント・CLI ヘルプは英語（OSS 前提）。docs は日英併記を Phase 4 で整備

## 3. ドメインモデル（core/model）

```go
type Account struct {
    ID    string // "123456789012"
    Name  string
    Email string
}

type Pipeline struct {
    Name      string        // "123456789012-customizations-pipeline"
    AccountID string        // 名前から導出
    Account   *Account      // 解決済みの場合のみ
    Latest    *Execution    // 最新実行
}

type Execution struct {
    ID         string
    Status     Status        // Succeeded/Failed/InProgress/Stopped/...
    StartTime  time.Time
    EndTime    *time.Time
    SourceRevisions []Revision
}

type StageState struct {
    Name    string
    Status  Status
    Actions []ActionState // 失敗アクション特定用。CodeBuild の buildId を保持
}
```

- `Status` は自前 enum。AWS SDK の文字列を正規化して保持
- パイプライン種別判定: `^(\d{12})-customizations-pipeline$` にマッチ → account pipeline。それ以外は共通系（Phase 3）

## 4. 主要フロー設計

### 4.1 状態一覧（F1）

```
1. cache から account map / pipeline 一覧を取得（miss なら API → cache 保存）
   - pipeline 一覧: ListPipelines（ページネーション、名前 regex でフィルタ）
2. status cache を参照し、TTL 内のエントリはキャッシュから供給。
   TTL 超・未取得・実行中(InProgress/Stopping)のパイプラインだけ Batch Engine で
   ListPipelineExecutions(maxResults=1) を再取得（GetPipelineState は詳細画面のみ）
3. 取得結果を status cache にマージ保存（全件パス時は消滅パイプラインを prune）。
   Account 解決を合成して PipelineSummary[] + StatusStats を返す
4. renderer が table / json で出力。status の鮮度（refetched/from-cache 件数・最古経過・
   refetch 失敗件数）を stderr に明示。件数はコアが返す `model.StatusStats` をそのまま使い、
   表示層でタイムスタンプから推測しない
```

- **最新実行ステータスは per-entry（パイプライン名ごと）で短 TTL キャッシュする**（既定 `status_ttl: 10m`）。
  短時間の連続 `pl list` で全件 fan-out を避ける。当初の「ステータスは一切キャッシュしない」方針からの
  意図的な見直し。鮮度は次の 3 点で担保する:
  - 短い既定 TTL（設定・環境変数で変更可、`0` で無効化＝毎回 fan-out）
  - **実行中(InProgress/Stopping)エントリは TTL 内でも常に再取得**（変化が速いため）
  - stderr に鮮度を常時表示。fetch エラー時は既知のキャッシュ値を保持し silent blank を避ける
- 特定パイプラインだけの再取得は `pipeline refresh <target...>`（該当エントリのみ更新）。
  `pipeline release` は起動したパイプラインの status cache を無効化し、次回 list で最新化する
- 一覧の応答目標: 数百件で 10〜20 秒程度（並列度 10、レート制御下）。キャッシュヒット時はほぼ即時。実測して調整（U2）

### 4.2 詳細確認（F2）

```
GetPipelineState + ListPipelineExecutions(履歴 N 件)
→ 失敗ステージ/アクションを特定
→ アクションの CodeBuild buildId → BatchGetBuilds → CloudWatch Logs
→ terraform ログ抽出
```

terraform ログ抽出（`core/logs`）:
- CodeBuild ログから terraform セクション（`Terraform will perform...` / `Error:` / `Apply complete!` 等のマーカー）を抽出
- 出力モード: `--raw`（全文）/ 既定（terraform 部分）/ `--summary`（plan 結果サマリ・エラー行のみ）
- `--summary` の JSON 出力が AI 連携の主要境界面

### 4.3 Release change（F3 + F5）

```
対象決定（名前・--account・--status Failed・--file/stdin）
→ --expect による件数アサーション（指定時）
→ 対象の status を再取得（下記）
→ dry-run 表示（対象一覧 + 件数）
→ 書き込みクレデンシャルの解決 + アカウント検証（読み取り側と不一致なら拒否）
→ 確認プロンプト（--yes でスキップ、件数 > limit なら拒否）
→ Batch Engine で StartPipelineExecution
→ 結果レポート（成功/失敗/skip 件数、失敗理由）
```

- **対象の広がりは構文に出す**（§8.1 の解決規約）。引数・`--file` の各行は 1 本を完全一致で
  指すのみで、部分一致は解決しない。グループを撃つには `--account` を明示する。
  確認プロンプトは対象一覧を出しているが、`--yes` では消えるうえ習慣化もするので、
  「人が見ている」ことを前提にしない防御をコマンドラインの形として持たせる
- `--expect N`: 対象が N 件に解決しなければ exit 2。`max_targets` が「上限」であるのに対し
  こちらは「この件数のはず」という宣言で、無人の `--yes` 実行がアカウント増加に伴って
  黙って広がるのを止める。アサーションの対象は**選択された集合**であり、
  InProgress スキップ後に実際に起動した本数ではない
- 既定の安全ガード: `release.max_targets: 50`（設定で変更可）。超過時は `--max-targets N` での明示が必要
- 実行中 (InProgress) のスキップは `release.skip_in_progress`（既定 true）。`--include-in-progress` で 1 回だけ上書きする
- 冪等性: 実行中 (InProgress) のパイプラインは既定でスキップ（`--include-in-progress` で上書き）
- **release は status キャッシュに依存しない**: status は「`--status` がどれを選ぶか」と
  「InProgress スキップがどれを飛ばすか」の 2 つを決めるため、`status_ttl`（既定 10m）だけ
  古いデータで書き込みを判断させない。`--status` 指定時は全件を強制再取得、明示ターゲット時は
  その対象だけ再取得してから確認に進む。TUI の Release 画面も confirm 前に対象を再取得する
- **書き込み先は読み取り先と同一アカウントでなければ拒否**（§10 の 1 run = 1 account）。
  検証は確認プロンプトの**前**に行うので、`aws (write):` 行を見てから y/N を答えられる。
  write profile の identity を検証できなかった場合も拒否する（`GetCallerIdentity` は
  IAM 権限を要さないため、失敗はクレデンシャル自体が壊れていることを意味する）

**語彙: `release` は UI 層の語、`execution` はドメインの語。** 「Release change」は
AWS コンソールのボタン名であって API 名ではない（API は `StartPipelineExecution` のみ）。
そのためユーザーが触れる面 — CLI の `pipeline release`、TUI の release 画面、
設定キー `release.*`、demo fixture の `release` — はコンソールに合わせて `release` を使い、
UI を知らないはずのコア層（`internal/core`）は `StartExecution` /
`StartExecutionRequest` / `model.StartExecutionResult` と API 側に揃える。
`release` は常に**動詞**として使い、名詞には `execution` を使う（「3 件の release」ではなく
「3 件の execution」）。これは §3 の「コア層は UI 非依存」の語彙面での帰結であり、
`pipeline executions`（読み取り）と 1 文字違いの書き込みコマンドを作らないための制約でもある。

### 4.4 Trigger ドリフト検出（F10・read-only）

```
1. pipeline 一覧（§4.1 と同じ inventory キャッシュ）+ account map
2. trigger cache を参照し、TTL 内のエントリはキャッシュから供給。
   残りだけ Batch Engine で GetPipeline → triggers を正規化
3. 期待値 = TriggerPolicy.Expect(account_customizations_name) を毎回その場で計算し、
   観測値と突き合わせて state（ok/missing/drift/unknown/fetch-error）と reason slug を出す
4. renderer が table / json で出力。件数と鮮度を stderr に出す（§4.1 と同じ FetchStats）
```

AFT の customizations パイプラインのテンプレートには **`trigger` ブロックが無く、両ソース
アクションが `DetectChanges = false`** である。つまり実環境に付いている push trigger は
すべて out-of-band であり、`aft-create-pipeline` が再実行される（AFT アップグレード時・
CodeConnections 作り直し時にフリート全台で起きる）と消える。それを検知するのがこのフロー。

**なぜ trigger が統制の一部なのか**は requirements.md の F10 に書いた。要点だけ言うと、
AFT には plan → 承認 → apply のゲートが無く（上流 issue #153 が 2022 年から open）、
それを customizations リポジトリ側の CI で補完すると **merge が apply の承認点**になる。
その承認を実際の apply に繋いでいるのが push trigger であり、消えたときに起きるのは
失敗ではなく**無反応**（merge しても何も走らず、CI は成功したまま）なので、
検知する仕組みが無いと気づけない。

設計上の要点:

- **期待値はアカウントごとに設定しない。** AFT が `aft-request-metadata` に記録している
  `account_customizations_name` から `trigger.file_path_includes` /
  `trigger.file_path_excludes` で導出する。数百件のフリートが数行の設定で覆え、
  かつ期待値が AFT の記録からずれようがない。
  実測（193 本）で `filePaths` と `account_customizations_name` の突合はズレ 0・重複 0
- **既定の期待値は「アカウントディレクトリ全体を include し、ドキュメントを exclude する」。**
  ビルドすべきファイル種別を列挙する形にはしない。CodePipeline の `*` は `/` を跨がないため、
  `{customizations_name}/terraform/*.tf` のような狭いパターンは、そのアカウントが
  ローカルモジュールのディレクトリを持った瞬間に発火しなくなる — しかも**無言で**である
  （マッチしない trigger は「誰も push していないリポジトリ」と区別がつかない）。
  exclude 側で削るほうが安全で、精度も落ちない: CodePipeline は push に含まれる
  **各ファイルを独立に評価する**ので、`.tf` と `README` を含むコミットは発火し、
  ドキュメントだけのコミットは発火しない
- **excludes は期待値の一部であって例外ではない。** includes は一致するが excludes を
  持たないパイプラインは、ドキュメント push でビルドが走る別物なので `drift`
  （reason `file_path_excludes`）として報告する。パターンの**順序は判定に含めない** —
  CodePipeline 側が順序を評価しないため、設定を並べ替えただけで drift にはしない
- **期待値を導出できないパイプラインは `unknown` であって `ok` ではない。** 「判定できなかった」
  と「正しかった」は逆の答えで、混ぜるとレポートが自分の入力の欠落を健康証明として出す
- **キャッシュするのは観測した trigger だけで、判定結果はしない。** policy を変えたり
  accounts を `--refresh` したりしたときに、パイプライン定義を取り直さずに再判定できる
- **並列度はコア層で 3 に制限する**（`triggerConcurrencyCap`）。`GetPipeline` は読み取り系の
  中では重く、実測で **6 並列だと 195 本中 11 本が `ThrottlingException`（SDK 内部リトライ込み）・
  3 並列では 0 件**だった。`batch.concurrency` は `ListPipelineExecutions` に対して調整された
  値なので、そのまま使うとこのコマンドだけが黙って壊れる。`--concurrency` を明示的に
  渡したときだけ上限を外す（操作者が意図してその数を要求している）
- **判定は read-only のまま。** 期待値に揃える書き込みは `pipeline triggers fix`（§4.7・F14）が
  別コマンドとして担う。当初は「管理主体が二重になる」として書き込みを範囲外にしていたが、
  AFT 側は trigger を宣言しておらず二重になる相手が無いこと、消失がフリート全台で起き
  手作業でしか戻せないことから改めた（requirements F14）

### 4.5 Global 適用の収束実行（F11 + F12）

```
--global-ref <sha> と対象リストを受け取る
→ 対象の最新 execution を取得（ソースリビジョン + ステージ別の成否）
→ 収束済み/未収束を判定し、未収束だけを実行対象にする
→ --expect / dry-run / 確認プロンプト（4.3 と同じガード）
→ チャンクごとに StartPipelineExecution（§5 の逐次バッチエンジンをそのまま使う）
    先頭チャンク: 完了を待って結果を確認し、判断を人に返す
    以降        : 投入しつつ結果を回収。検知したら未投入のチャンクを止める
→ 報告（global の到達率 / account 層で落ちたもの / 起動できなかったもの）
```

#### 永続的な実行台帳を持たない

この機能を「対象リストを順に消化するジョブ」として実装すると、どこまで進んだかを
ローカルに永続化する必要が生じ、中断・再開・単体再実行がそれぞれ別の機構になる。
採らない。

**パイプラインの実行結果がソースリビジョンを保持している**ため、「そのアカウントが
どのコミットまで適用したか」は毎回 AWS 側から読める。つまり **AWS 側が台帳である**。
これを使って操作を収束（reconcile）として定義すると、次が同じ 1 つの仕組みに畳まれる:

| 操作 | 表現 |
|---|---|
| 通常実行 | `release --global-ref <sha> --file targets.txt` |
| 中断からの再開 | **同じコマンドをもう一度実行する** |
| 特定アカウントの再実行 | 同じコマンドを `--account` で絞る |
| 完了確認 | 同じコマンドを `--dry-run` で実行し、対象が 0 件になることを見る |

副次的な利点として、待機中に別経路（コンソール・trigger・他の運用者）でパイプラインが
起動された場合、台帳方式は実態とずれるが収束方式は毎回 AWS を見るため自動的に追従する。

処理中の一次キャッシュは §7 の既存機構をそのまま使う。**持たないのは永続化された
実行状態**であって、キャッシュではない。

#### 収束の判定はリビジョン × ステージ

リビジョンだけで判定してはならない。パイプラインは Source が成功した時点で
リビジョンを記録するため、**その後の apply が失敗しても新しいリビジョンが付く**。
さらに global ステージが成功して account ステージで落ちた場合、execution 全体の
ステータスは失敗だが **global は適用済み**である。

| global リビジョン | Global ステージ | Account ステージ | 判定 |
|---|---|---|---|
| target | 成功 | 成功 | **収束済み** |
| target | 成功 | 失敗 | global は達成。account 層の問題として別に扱う |
| target | 失敗 | — | 未収束 → 実行対象 |
| target 以外 | — | — | 未収束 → 実行対象 |

2 行目を「完了」にも「未収束」にも倒さないことが重要で、倒すと **global の再適用を
無駄に繰り返す**か、**account 層の失敗を見落とす**かのどちらかになる。この区別が
あるからこそ「global はフリート全体に行き渡った。ただし N アカウントで account 層が
落ちている」という、F11 の課題にそのまま対応する報告ができる。

ステージ別の成否は失敗した execution についてだけ引けばよいので、追加の API コストは
失敗件数に比例するだけで済む。

#### 監視は待ってから少数回

execution の所要時間には下限がある（実測では数分を下回らない）。**その間のポーリングは
全て無駄**なので、連続ポーリングはしない。

```
投入 → 実測の最小所要時間まで待つ（この間は一切問い合わせない）
     → 数回の確認で残りを回収する
```

確認回数は「完了確認だけなら 1 回・途中経過も見たいなら 2 回」で足りる規模である
（具体的な待機時間と回数は計測記録に基づいて既定値を置く）。API コストは対象件数 ×
確認回数にすぎず、状態一覧の取得より軽い。**§5 の rps とは別枠**で扱う必要はない。

失敗したものについては CodeBuild の build を辿ってログを取得する。失敗件数分だけの
追加コストであり、終端状態のログは不変なので §7 の原則どおりキャッシュできる。

#### カナリアゲートと全体時間のトレードオフ

「チャンクの結果を確認してから次のチャンク」を全チャンクに適用すると、総所要時間は

```
チャンク数 × execution の所要時間
```

に支配され、アカウント数に比例して跳ねる。数百アカウント規模では時間単位になり、
実運用に耐えない。

そこで **先頭のチャンクだけ完全にゲートし、以降は投入しつつ結果を回収する**構成を既定とする。

```
チャンク 1（カナリア）: 完了を待って全結果を確認 → 人が続行を判断
チャンク 2 以降  : chunk_pause 間隔で投入。結果は遅れて届く
                   失敗を検知したら「まだ投入していないチャンク」を止める
```

この構成では、検知した時点で `execution の所要時間 ÷ chunk_pause` チャンク分が既に投入済みで
あり、**それらは止められない**。これは避けようのない帰結なので、ガードではなく
**設計上の既知の範囲としてドキュメントと実行時出力の両方に明示する**（§6 の
「silent failure 禁止」と同じ考え方 — 止められない範囲を黙って持たない）。

全チャンクをゲートする運用が必要な場合は明示のフラグで選べるようにし、その際は所要時間が
チャンク数に比例することを実行前に提示する。

#### 実行順は人間が決める

カナリアを先に置くには対象の順序制御が要る。危険度の自動判定（環境名からの推測等）は
持たない — 組織ごとに命名規約が違ううえ、誤った推測で prod を先頭に置く事故が
自動化の利得を上回る。**対象リストの記載順をそのまま実行順とする**モードを持ち、
順序の責任を人間に残す。

#### F11（(b) の検出）は terraform を実行しない

各アカウントの「account 層の未適用コミット」は、パイプラインが記録している
account 層のソースリビジョンと、リポジトリ側の当該アカウント配下の最新コミットの
比較で求まる。**terraform も、各アカウントへの assume も要らない。**
AFT のビルド環境を再現せずに済む範囲に機能を閉じるための境界がここである。

F10（trigger ドリフト）とは因果でつながっている。trigger を失ったアカウントは
merge しても発火しないため未適用コミットが積み上がり続ける。両者は同じ画面で
並べて意味を持つ。

### 4.6 一覧の terraform 結果列（F13・read-only）

一覧に各パイプラインの**最新実行**の terraform 結果を global / account の 2 列で出す。
これまで Actions 画面（§9.1）だけが持っていた「ログから結論を取り出す」処理を core に
引き上げ、一覧・Actions 画面・CLI の 3 つが同じ部品を使う。

```
ACCOUNT NAME     ACCOUNT ID    STATUS      GLOBAL     ACCOUNT    LAST UPDATE
app-staging      123456789012  Succeeded   +0 ~1 -0   ·          2026-10-07 10:44
payments-prod    111122223333  Failed      ✗ error    —          2026-10-07 10:40
search-dev       444455556666  InProgress  ·          running    2026-10-07 10:57
data-lake-prod   777788889999  Succeeded   …          …          2026-10-07 10:35
```

#### 判定（`core/logs`）と表示の分離

`logs.Verdict`（結論 1 行の文字列を返す）を、構造化した結果を返す `logs.ParseVerdict` に
置き換える。判定規則（`Error:` 優先 → `Apply complete!` / `Destroy complete!` / `No changes.`
→ `Plan:`）は現行のまま変えない。記号にするか 1 行の文章で出すかは表示層が決める。

```go
type TerraformResult struct {
    Kind    ResultKind // applied | no_changes | error | failed | plan | running | not_run | unknown
    Add, Change, Destroy int
    Line    string     // 判定の根拠になった行（Actions 画面・JSON はこれを出す）
}
```

| Kind | 条件 | 一覧の表示 | 色 |
|---|---|---|---|
| （未取得） | 取得待ち・取得中 | `…` | グレー |
| `applied` | `Apply complete!` / `Destroy complete!`（後者は destroy 数のみ） | `+0 ~1 -0` | 0 以外の数を緑 / 黄 / 赤 |
| `no_changes` | `No changes.`、または apply の 3 数がすべて 0 | `·` | グレー |
| `error` | `Error:` 行あり | `✗ error` | 赤 |
| `failed` | アクションは Failed だが `Error:` 行なし（terraform 以前・以後で落ちた） | `✗ failed` | 赤 |
| `plan` | `Plan:` 行しかない（apply に至らなかった） | `plan +1 ~0 -0` | グレー |
| `running` | build が終わっていない | `running` | 黄 |
| `not_run` | 最新実行にそのステージのアクションが無い（前段の失敗等） | `—` | グレー |
| `unknown` | 結論行が見つからない、またはログ取得に失敗 | `?` | グレー |

- `no_changes` に「apply の 3 数がすべて 0」を含めるのは、全行が `+0 ~0 -0` だと
  **変更のあった行が埋もれる**ため。数字が出ている行＝何かが起きた行、にする
- global / account の振り分けはステージ名（AFT が定義する `Global-Customizations` /
  `Account-Customizations`）で行う。どちらにも当たらないステージは列に出さない

#### 取得（`core/result`）

```
ForExecution(pipeline, exec) → {Global, Account TerraformResult}
  = 終端 execution で保存済みなら API 0 回
  = ListActionExecutions（終端 execution は session memo）
  + ForActions(その実行の global / account の build)
ForActions(actions) → build id ごとの TerraformResult
  = 実行中の build は読まずに running
  + 保存済みの build は API 0 回
  + 残りは build ごとに GetLogEvents（既定の場所の末尾 300 行）→ ParseVerdict
```

- **ログは末尾から読む**: `StartFromHead=false`・`Limit=300` の 1 回で結論行はほぼ取れる
  （既定の 1 ページは最大 1MB / 10,000 行。全 pipeline × 2 本を毎回それだけ落とさない）。結論行が無かったときだけ全文を取りに行く（`logs.Service.FetchBuild`。
  session memo に入るので、続けてログ画面を開くと即表示になる）
- **BatchGetBuilds を呼ばない**: ログの場所は CodeBuild の既定（group `/aws/codebuild/<project>`・
  stream `<build id の uuid>`）から組み立てる（`logs.DefaultLocation`）。実環境の AFT で
  実物を確認済み。既定の場所が無い（`ResourceNotFoundException`）ときだけ BatchGetBuilds で引き直す
  - 理由は実測: 当初は実行単位で BatchGetBuilds を呼んでいたが、**CodeBuild がこの呼び出しを
    throttle した**（約 200 件の初回で、BatchGetBuilds の試行の約 6 回に 1 回）。adaptive retry がクライアント側の流量制御を
    入れ、`locate` が最大 42 秒止まり、worker が詰まって全体が律速された（結果取得 79 秒）
- **アクションが Failed なのに `Error:` 行が無い**ものは `failed` にする（terraform の前後、
  helper script やコンテナで落ちた）。apply の件数が読めていても残したうえで `failed` に
  する — 失敗した実行の結果列が「きれいな apply」に見えてはいけない
- 保存（§7）は 2 段: build id ごとの結論（Actions 画面と共有）と、終端 execution ごとの
  2 層の結論（一覧の再起動直後を API 0 回にする）。**読めなかった結論・結論行の無い結論
  （`unknown`）・実行中のものは保存しない**。`unknown` を外すのは、build 完了直後は CloudWatch
  への取り込みが遅れて末尾が欠けていることがあり、保存すると `?` が固定されてしまうため
- **トレードオフ**: Actions 画面はこれまで全文を取得しており、それがログ画面の先読みを
  兼ねていた。末尾だけを読む方式ではこの先読みが無くなり、ログ画面を初めて開くと
  全文の取得を待つ。ログ画面を開くのは一部の行だけなので、全行の全文を取るより
  安く済む方を採る

#### 遅延取得（TUI 一覧）

- status が揃った時点で一覧を描画し、結果列は `…` のまま出す。その後バックグラウンドで
  結果を取得し、**1 行終わるごとにその行を更新する**（全件終了を待ってまとめて反映しない）
- 取得順は投入時に決める: Failed の行 → 現在のソート順で上から（＝見えている行から）。
  スクロールに応じた並べ替えはしない（v1）
- 対象は「結果が未知の行」だけ。キャッシュで埋まる行は API を呼ばずに即座に埋まる
- 再取得・自動ポーリング（§9.1）で行の最新 execution id が変わったら、その行を再投入する。
  同じ execution のまま終端状態に変わった行（`running` だった行）も再投入する
- 手動の `r`（選択行の再取得）は結果も読み直す。読めなかった行の再試行手段を兼ねる
- header に進捗（`results ⠋ 120/200`、完了後は `results ✓ 200/200`）と、読めなかった行の件数（`results: N unreadable`）を出す。
  読めなかった行は `?`
- 一度に走る取得は 1 本だけ。取得中に状態が変わった行は、その取得が終わったときに拾う

#### レート制御 — バッチエンジンの変更を伴う

現行の `batch.Run` は **Run ごとに** token bucket を持ち、**item 単位**で入場を制御している
（§5）。既存の fan-out は 1 item = API 1 回なので RPS と呼び出し数が一致していたが、
結果取得は 1 item = API 2〜3 回で、しかも status のポーリングと**同時に走る**。
このままでは実際の呼び出し数が設定 RPS の数倍になる。

- **token bucket は API 呼び出し単位・AWS サービスごと・プロセスで 1 組**（`awsx.Limits` /
  `awsx.RateLimit`。Finalize step の Retry の後ろに置くので、計測（§6）と同じく試行ごとに
  1 トークン）。read / write の両 client が同じ `Limits` を持つ
  - **サービスごとに分けるのは、AWS のクォータがサービスごとに別枠だから**。当初は 1 本の
    bucket を全サービスで共有していたが、実測（実環境・約 200 件・初回）で約 1,000 回の呼び出しが
    ちょうど 8 回/秒で進み、throttle 0 件・p50 40〜150ms だった。律速は AWS ではなく自前の
    bucket で、ログ読み取りが CodePipeline の呼び出しの後ろに並んでいた
  - 既定は `batch.rps: 8`（各サービス）と `batch.service_rps.logs: 16`。結果取得は 1 pipeline
    あたり CodePipeline 1 回に対して Logs 2 回なので、Logs を倍にすると両者が同じ速さで進む
    （GetLogEvents 自体のクォータの範囲内）。`--rps` は全サービスを一括で上書きする
  - 「20 並列で throttle」の実績は CodePipeline のもの。CodePipeline の既定は据え置く
- CodeBuild の BatchGetBuilds は毎秒 4 回弱でも throttle された（上記）。結果取得では呼ばない
  ので既定は据え置くが、多数の build を引く用途を足すときは低い値を設定すること
- `batch.Config.RPS` は CLI / TUI からは 0（item 単位の制限なし）で使う。item でも
  制限すると、**キャッシュで済む item まで使わないトークンを待つ**ことになる
  （demo では 42 件の結果がすべて保存済みでも 5 秒かかった）。並列度・チャンク・進捗だけを受け持つ
- demo モードには SDK client が無いので、fake の各呼び出し（`demo.Env.tick`）が同じ `Limits` を
  サービス名つきで待つ
- `batch.Each` を足した（item ごとの結果通知。`batch.Run` は `onProgress` で件数しか運ばない）

#### コスト見積もり（数百件・RPS 8）

| | 1 pipeline あたり | 全体（約 200 件） |
|---|---|---|
| 素朴に実装（全文取得・BatchGetBuilds 個別） | 約 7 回 | 約 1,500 回・3 分強 |
| 本設計・保存なし（初回）・単一 bucket | 4 回（ListActionExecutions 1・BatchGetBuilds 1・GetLogEvents 2） | 約 850 回・実測 107 秒（status 込み 134 秒） |
| サービス別 bucket（BatchGetBuilds あり） | 同上 | 実測 79 秒（CodeBuild の throttle で詰まる。status 込み 107 秒） |
| サービス別 bucket・BatchGetBuilds なし（採用） | 3 回（ListActionExecutions 1・GetLogEvents 2） | **実測 27 秒**（CodePipeline 8 回/秒が律速。status 込み 56.5 秒・throttle 0） |
| 本設計・2 回目以降 | 新しく実行されたものだけ | 数回〜数十回 |

素朴な実装の回数は計測記録の実績（1 build あたり GetLogEvents 約 2 ページ）から見積もった。
demo fixture（42 件）の実測: 保存なし 26 秒（単一 bucket）→ 11.8 秒（サービス別）、
結果取得だけなら約 21 秒 → 5.5 秒。全件保存済みは 0.4 秒。
さらに縮めるなら `batch.service_rps.codepipeline` を `metrics show` の throttle 率を見ながら上げる。
初回の待ち時間は一覧の表示を止めない（遅延取得）ので、体感は「列が順に埋まっていく」になる。

#### CLI

- `pipeline list --results` で GLOBAL / ACCOUNT 列を追加する。指定しなければ取得しない
  （`pipeline list` の応答時間を変えない）
- JSON は各行に `results.global` / `results.account`（`kind` / `add` / `change` / `destroy` /
  `line`）を追加する。追加なので `schema_version` は据え置き
- フィルタ（`--status` / `--account`）の後に読む。表示しない行のログは読まない
- 読めなかった行は表の `?` と、stderr の `results: N unreadable (first: …)` で示す。JSON は
  その行に `results_error` を持つ
- `pipeline executions --actions` には出さない（いまはログを読んでいない。足すとコストだけが増える）

#### 範囲外（v1）

- 結果列での filter / sort（「変更があった行だけ」等）。必要になったら第 2 段階で足す
- 最新以外の実行の結果を一覧に出すこと

### 4.7 Trigger の設定（F14）

```
対象決定（引数・--account・--file・--state。既定 --state missing,drift）
→ 対象の trigger を強制再取得し、§4.4 と同じ期待値で判定（キャッシュは使わない）
→ 各対象を fixable / refused / no-op に振り分け
→ --expect による件数アサーション（fixable の件数に対して）
→ plan 表示（パイプラインごとに現在値 → 期待値。refused は理由付きで併記）
→ dry-run ならここで終了
→ 書き込みクレデンシャルの解決 + アカウント検証
→ 確認プロンプト（--yes でスキップ、件数 > limit なら拒否）
→ Batch Engine（既定 並列度 1）で 1 本ずつ:
     GetPipeline（直前の再取得）
     → trigger が plan 時点から変わっていれば skip（changed_since_plan）
     → 最新 execution が InProgress / Stopping なら skip
     → 変更前の trigger をバックアップ
     → 宣言の Triggers だけを期待値で置き換えて UpdatePipeline
     → trigger キャッシュを書き込み後の値で更新
→ 結果レポート（updated / skipped / refused / failed）
```

設計上の要点:

- **語彙: CLI は `fix`、core は API 側に揃えて `UpdateTriggers`。** `apply` は AFT の文脈では
  terraform apply と読まれるので使わない。`fix` は「F10 の判定を是正する」ことを指し、
  `pipeline triggers`（判定）のサブコマンドとして置くことで、同じ期待値・同じ対象選択を
  共有していることを構文に出す
- **期待値は §4.4 と共有する。** `TriggerPolicy.Expect` と `ClassifyTrigger` をそのまま使い、
  書き込み用の期待値を別に持たない。判定と是正の基準がずれると「fix したのに drift」になる
- **書き換えてよいのは、期待値の形で表現できる差分だけ。** `ClassifyTrigger` の reason で振り分ける:

  | reason | 扱い |
  |---|---|
  | `no_trigger` / `branches` / `file_paths` / `file_path_excludes` | fixable（書き換える） |
  | `multiple_triggers` / `source_action` / `provider_type` / `pull_request_filter` / `extra_filters` | **refused**（1 つでも含めば書き込まない） |

  refused になるのは、期待値に無いソースアクションの trigger（例: global-customizations 側）や
  PR・タグのフィルタなど、誰かが意図して付けた可能性があるものである。期待値で置き換えると
  それを黙って消すことになるので、書き込まずに理由を報告し、人が判断する。
  refused は exit 1（ドメイン上の失敗）として扱い、無人実行でも見落とされないようにする
- **変更は Triggers だけ。** `GetPipeline` が返した SDK の宣言（`PipelineDeclaration`）の
  `Triggers` フィールドだけを差し替えて `UpdatePipeline` に渡す。JSON を経由した組み立て直しは
  しない（未知のフィールドを落とす経路を作らない）。`UpdatePipeline` は定義全体の置換 API なので、
  この read-modify-write の形が唯一の安全な書き方になる
- **直前の再取得と、plan との突き合わせ。** 確認プロンプトを待つ間に他の運用者や
  `aft-create-pipeline` が定義を変えることがある。書き込み直前の `GetPipeline` で得た trigger が
  plan 時点の値と異なれば、その行は書き込まずに `changed_since_plan` として報告する。
  確認した内容と違うものを書かない
- **実行中は書き込まない（上書き手段なし）。** `UpdatePipeline` は実行中の execution を停止させる。
  release の `--include-in-progress` に当たるフラグは設けない — trigger の是正は急ぐ理由がなく、
  実行中の apply を止める代償に見合う場面が無い
- **`UpdatePipeline` は実行を起動しない。** version が 1 上がるだけで、新しい execution は
  作られない（実環境で確認済み）。したがって fix は apply の発火を伴わず、release とは
  影響の種類が異なる
- **変更前の trigger をバックアップする。** `~/.local/state/aft-ops/trigger-backups/<AFT 管理アカウント>/<実行時刻>/<pipeline>.json`
  に、書き込み直前の trigger 宣言を **CodePipeline API と同じ JSON 形**（`get-pipeline` の
  `.pipeline.triggers` と同じ形）で保存する。保存できなければその行は書き込まない。
  定義全体ではなく trigger だけにしたのは、(a) fix が変えるのは trigger だけで、戻すべきものも
  trigger だけであること、(b) SDK の型には API の JSON 形で定義全体を書き出す手段が無く、
  自前で変換すると「戻すための定義」自体が壊れうること、による。trigger が無かった場合は
  空配列を保存する（そのまま戻すと「trigger 無し」に戻る）。戻すときは
  `get-pipeline` の `.pipeline.triggers` をこのファイルの `triggers` で置き換えて
  `update-pipeline` に渡す。ツールに rollback コマンドは持たせない
  （戻す判断は個別であり、fix の逆操作として自動化すると「何に戻すか」が曖昧になる）
- **既定の並列度は 1。** `UpdatePipeline` の throttling 上限は未計測で、`GetPipeline` ですら
  3 並列が上限だった（§4.4）。`--concurrency` を明示したときだけ上げる。
  計測（§6）で上限が分かれば既定を見直す
- **安全ガードは release と同じ構成**（§4.3）: `--dry-run` / `--yes` / `--expect N` /
  `trigger.max_targets`（既定 50。超過は `--max-targets N` で明示）/ 書き込み先アカウントの検証を
  確認プロンプトの前に行う
- **権限**: `codepipeline:UpdatePipeline` と、パイプラインのサービスロールに対する
  `iam:PassRole`（`UpdatePipeline` は `roleArn` を含む定義全体を送るため）。読み取り側は
  §4.4 と同じ `GetPipeline` / `ListPipelineExecutions`
- **範囲外（v1）**: TUI からの fix、rollback コマンド、期待値に無い trigger の自動削除

## 5. 逐次バッチエンジン（internal/batch）

要件 F4 の中核。「チャンク逐次 × チャンク内並列」+ レート制御 + 計測。

```go
type Config struct {
    Concurrency int           // チャンク内並列度（既定: 10）
    ChunkSize   int           // 0 = チャンク分割なし（連続 stream）
    ChunkPause  time.Duration // チャンク間の待機
    RPS         float64       // API 呼び出しの token bucket 上限
}

// Run は items を処理し、部分失敗を per-item で返す（silent failure 禁止）
func Run[T, R any](ctx context.Context, cfg Config, items []T,
    fn func(context.Context, T) (R, error)) []Result[R]
```

設計ポイント:
- **worker pool + rate.Limiter の二段制御**: 並列度（同時実行数）と RPS（毎秒呼び出し数）を独立に制御。
  （F13 で token bucket を API 呼び出し単位・AWS サービスごとに移した。§4.6「レート制御」）実測で throttle が出ない範囲から既定は Concurrency=10 / RPS=8 程度で開始し、計測結果で調整
- **リトライ**: SDK v2 の `retry.AddWithMaxAttempts` + adaptive mode を基本とし、Throttling は指数バックオフ + ジッタ。リトライ発生は metrics に記録
- **キャンセル**: ctx キャンセル（Ctrl-C）で新規投入を止め、実行中のみ完走して部分結果を返す
- **進捗通知**: `chan Progress` を公開し、CLI はプログレスバー、TUI は画面更新に利用

## 6. レート計測（internal/metrics）

「実装しながら高精度に分析したい」（要件 F4）に対応する一級機能。

- AWS SDK v2 の **middleware** で全 API 呼び出しをフック: サービス/オペレーション/所要時間/成否/Throttling 有無を記録
  （Deserialize step の**先頭**に置く。操作ごとの deserializer より外側でないと、HTTP 400 の
  ThrottlingException がエラーに見えず throttle が 0 件と記録される。F13 の実測でこの不具合が
  見つかり修正した。それ以前の記録の throttle 0 件は、試行回数＝呼び出し回数のときだけ信用できる）
- 実行ごとに `~/.local/state/aft-ops/metrics/<timestamp>.jsonl` に追記
- `aft-ops metrics show [--last N]`: オペレーション別の呼び出し数・p50/p99/max・throttle 率を集計表示。
  レイテンシは平均ではなく**パーセンタイル**（nearest-rank）。並列度・RPS の調整は throttle と
  向き合う作業であり、それはテールに出る。平均はテールに引きずられて不穏に見える一方で
  どこまで悪化したかは隠す。nearest-rank にしているのは、返る値が必ず実測されたレイテンシに
  なるため（サンプルが少ない run では p99 が max に一致する。それが正直な答え）。
  **集計対象から自分自身の run を除く**（実行中の run は最新ファイルだが 1 回も API を
  呼んでいないので、既定の `--last 1` がそれを拾うと空の表になる）
- 将来: 計測結果から推奨 Concurrency/RPS を提示（自動チューニング）

## 7. キャッシュ（internal/cache）

> **キャッシュ原則**: 終端状態（Succeeded/Failed/Stopped/…）のデータは不変なので積極的に
> キャッシュする。in-flight（InProgress/Stopping）のものだけ常に再取得する。可変な一覧系は
> TTL + 明示 refresh（`--refresh` / TUI の `r`）で制御する。AFT のインフラパイプラインは
> アプリ CI/CD と違い大半の時間 idle であり、毎回取得は鮮度の利得に対してオーバーヘッドが
> 大きい、という判断（当初の「実行ステータスは一切キャッシュしない」方針からの見直し）。

ディスクキャッシュ（プロセスをまたいで有効）:

| データ | ソース | 既定 TTL |
|---|---|---|
| account map（ID⇔名前⇔email） | 下記 7.1 | 24h |
| pipeline 存在一覧 | ListPipelines | 6h |
| 実行ステータス（per-entry） | ListPipelineExecutions | 10m（`status_ttl`）。実行中は常に再取得 |
| pipeline trigger（per-entry） | GetPipeline | 1h（`trigger_ttl`）。定義は書き換えられたときしか変わらない |
| terraform 結果（build id ごと / 終端 execution ごと。1 ファイル `terraform-results`） | BatchGetBuilds + GetLogEvents → `ParseVerdict` | 完了 build・終端 execution のみ・TTL なし（不変）。読めなかったものは保存しない。`results_max_age`（既定 30 日）を過ぎたものは書き込み時に削除。ログ本文は保存しない（§4.6） |

セッション内メモリ memo（TUI 起動中のみ・ディスクに書かない）:

| データ | ソース | ポリシー |
|---|---|---|
| 実行履歴（executions 画面） | ListPipelineExecutions | 15m（`executions_ttl`、0 で無効）。先頭実行が in-flight なら常に再取得。`r` で強制 |
| アクション実行（actions 画面 / `v`） | ListActionExecutions | 終端 execution のみ無期限（不変）。in-flight は毎回 |
| build ログ（log 画面） | BatchGetBuilds + GetLogEvents | 完了 build のみ無期限（不変）。実行中は毎回。全文はここだけで取得する（結果列は末尾 1 ページ。§4.6） |

- 保存先: `~/.cache/aft-ops/<org-id or profile>/` （プロファイル毎に分離し、業務/PoC org の取り違えを構造的に防止）
- 形式: JSON + メタデータ（取得時刻・スキーマバージョン・取得元プロファイル）
- 出力時に stale 情報を明示（`cached 3h ago` 等）。`--refresh` を全読み取りコマンドでサポート
  （キャッシュを読まずに取り直す、が唯一の要求なので `--no-cache` は設けない。
  「取り直した結果を書かない」用途は無く、2 つあるとどちらを使うのか毎回考えることになる）
- `aft-ops cache status | clear | refresh`

### 7.1 アカウント情報のソース

AFT 管理アカウントは Organizations の管理アカウントではないため、`organizations:ListAccounts` が呼べない可能性がある。ソースを pluggable にする:

1. **aft-dynamodb**（既定）: AFT の `aft-request-metadata` テーブルから取得（AFT 管理アカウント内で完結）
2. **organizations**: delegated admin 等で呼べる環境向け
3. **static**: CSV/JSON ファイル指定（フォールバック・オフライン用）

設定 `account_source: aft-dynamodb | organizations | static` で切替。

## 8. CLI 設計（internal/cli）

### 8.1 コマンド体系

```
aft-ops                          # 引数なし → TUI 起動
aft-ops tui                      # 明示的 TUI 起動

aft-ops pipeline list            # F1: 状態一覧（alias: pl ls）。status は TTL 内キャッシュ供給
    --status Failed,InProgress   # ステータスフィルタ
    --account <name|id|部分一致>
    --sort last-update|status|account  # 既定 last-update（status は重大度順: Failed → … → Succeeded）
    --order asc|desc             # 既定 desc（未実行=時間なしは常に末尾）
    --refresh                    # 全 status を強制再取得（inventory/accounts も）
    --watch [--interval 30s]     # 定期再取得（既定間隔は tui.poll_interval）。table 出力専用
    --results                    # F13: 最新実行の terraform 結果（GLOBAL / ACCOUNT 列）も取得（§4.6）
aft-ops pipeline refresh [target...]  # 指定パイプラインの status だけ再取得しキャッシュ更新
    --account <name|id|部分一致>  # グループ指定
aft-ops pipeline show <target>   # F2: 詳細（ステージ/実行履歴）
aft-ops pipeline executions <target>  # F2: 実行履歴一覧（alias: execs）
    [--limit 25] [--actions]     # --actions は各実行のアクション（CodeBuild id 付き）も展開
aft-ops pipeline triggers        # F10: trigger ドリフト検出（alias: trig）。read-only
    --account <name|id|部分一致>
    --state ok|missing|drift|unknown|fetch-error  # カンマ区切り。未知の値は exit 2
    --fail-on-drift              # ok 以外が 1 件でもあれば exit 1（監視ジョブ向け）
aft-ops pipeline triggers fix [target...]  # F14: trigger を期待値に揃える（§4.7）。書き込み
    --state missing,drift        # 既定。対象の選択（unknown / fetch-error は常に対象外）
    --account <name|id|部分一致>
    --file targets.txt | -
    --expect N                   # 書き込み対象（fixable）が N 件でなければ exit 2
    --max-targets N
    --dry-run / --yes
aft-ops pipeline logs <target>   # F2: CodeBuild/terraform ログ
    [--execution <id>] [--build <id>] [--raw|--summary]
    # 既定（フラグ無し）= 現在の state の失敗アクション 1 本
    # --execution = その実行の build を全部（global/account の 2 本）。
    #   `──── <stage> / <action> ────` で区切る。単一 build のときは区切り無し
    # --build = 指定 1 本のみ
aft-ops pipeline release [targets...]   # F3: Release change
    --account <name|id|部分一致>  # グループ指定（複数対象を明示的に要求する唯一の位置指定手段）
    --status Failed              # フィルタ結果を対象に（対象決定時に status を強制再取得）
    --file targets.txt | -       # 明示リスト（stdin 可）
    --expect N                   # 対象が N 件でなければ exit 2
    --dry-run / --yes
    --concurrency N              # グローバルフラグ。chunk_size / chunk_pause は
                                 # 設定ファイルか env（AFT_OPS_BATCH_CHUNK_SIZE 等）で指定

aft-ops account list
aft-ops cache status|clear|refresh
aft-ops metrics show [--last N]
aft-ops version / completion
```

- **`<target>` の解決規約**: パイプライン名・アカウント ID・アカウント名のいずれかに
  **完全一致**（大小文字不問・前後空白は除去）した 1 本のみ（F7）。部分一致は解決しない。
  - 理由は 2 つ。(a) 今日 1 本を指す断片は次のアカウントが払い出された瞬間に 3 本を指す。
    単数形の引数が黙って集合に化けると、runbook や CI に残ったコマンド行から
    blast radius が読めなくなる。(b) `show` は曖昧一致を拒否するのに `release` は
    全マッチを黙って採用する、という**読み取り系と書き込み系の非対称**があった。
    引数の意味はサブコマンドをまたいで同一であるべき
  - 集合の選択は常にフラグ側（`--account` / `--status`）。両者は積で交わり（`list` と同じ）、
    名前指定した対象はその上に和で乗る
  - 解決に失敗したときは近傍候補（`--account` が選ぶ集合と同一）を最大 10 件列挙し、
    書き込み系ではグループ指定の方法（`--account <query>`）も示す。
    完全一致が複数ある場合（アカウント名の重複）だけは「曖昧」として別の文言を出す
- `pipeline refresh` / `pipeline release` は引数を取らず `--account` だけでも起動できる。
  名前指定なし・フラグなしの起動は exit 2（無選択は空結果ではなく呼び出し側の誤り）
- `--status` は列挙値を検証し、未知の値は exit 2 で拒否する（大文字小文字は不問）。
  受け付ける値は CodePipeline の各ステータスに加えて、取得できなかった行の表示名である
  `fetch-error`、および実行履歴なしを選ぶ `Unknown`。
  **検証は対象決定・status の fan-out より前**に行う: `release --status` は対象決定時に
  全件を強制再取得するため、後で弾いたのでは「API コストだけ払って no-op」になる。
  タイポが「対象ゼロ＝全部直っている」と読めてしまうのを防ぐのが主目的（`--sort` / `--order` /
  `--output` と同じ扱い）

### 8.2 出力規約（AI フレンドリー境界）

- 全コマンド共通 `--output table|json`（既定: TTY なら table、パイプなら json も検討 → 混乱を避け**既定は常に table、json は明示**とする）
- JSON はスキーマに `schema_version` を含め、後方互換を維持
- **プロバイダ生の値を JSON 消費者に押し付けない**: `revisions[].summary` は
  CodeConnections では `{"ProviderType":"GitHub","CommitMessage":"…"}` という
  「JSON 文字列の中の JSON」で降ってくる。table / TUI は `UnwrapProviderSummary` で
  展開しているので、**JSON の消費者だけが二重パースを強いられる**のは境界の設計として
  一貫しない。`revisions[].message` に展開済みの値を併記する（`summary` は
  `ProviderType` を持つ唯一の場所なので残す。追加であって置換ではないため
  `schema_version` は据え置き）
- 装飾（色・スピナー）は TTY 検知で自動 off、`--no-color` あり
- stderr = 進捗・診断、stdout = データ。パイプ処理を壊さない

### 8.3 exit code 規約

| code | 意味 |
|---|---|
| 0 | 正常（対象すべて成功/取得完了） |
| 1 | ドメイン上の失敗あり（例: Failed パイプラインが存在、release の一部失敗） |
| 2 | ツールエラー（設定不正・認証失敗・API エラー）および**要求の拒否**（フラグの排他違反、`max_targets` 等の安全ガード） |
| 130 | ユーザー中断 |

※ 安全ガードによる拒否を 1 ではなく 2 に割り当てるのは、1 が「一部は実行された上での失敗」を意味するため。
`release` が 1 を返したときは既に起動したパイプラインがあり得るが、ガード拒否では**1 本も起動していない**。
呼び出し側がこの 2 つを区別できるよう、拒否は「呼び出し方を直す必要がある」側（2）に置く。

※ `pipeline list` で「Failed が存在したら exit 1」は `--fail-on-error` フラグでオプトイン（既定は 0。監視スクリプト用途向け）。

## 9. TUI 設計（internal/tui）

### 9.1 画面構成

CodePipeline の実データモデル（pipeline → executions → action executions → build log）に
沿った 4 階層のドリルダウン。移動キーは vim 準拠: `h` 戻る / `l`（または `enter`）進む /
`j`/`k` 上下 / `v` は全階層から「最も見たいログ」への直行ショートカット。
各画面のヘッダ左端に階層インジケータ `••••`（現在位置=白 / 他=グレー）を表示し、
`v` 直行後でも現在の深さが一目で分かる。

```
[Pipeline List] ──l/enter──▶ [Executions] ──l/enter──▶ [Actions] ──l/enter──▶ [Log View]
  N rows                       recent runs (25)           per-action runs        terraform log
  / filter  f status              of one pipeline           of one execution     m mode switch
  s sort key / o order          id/status/duration        stage/action/status    j/k scroll
  r/R refresh                     /revision                 inline summary/error
  space multi-select           r refresh                  r refresh
  q quit
      │                            │                          ▲
      └────────── v ───────────────┴────── v ─────────────────┘   （一覧 / Executions とも、その実行の全 build）

[Pipeline List] ──x──▶ [Release]  (confirm → run → results)
```

- 一覧の結果列（F13・実装済み）: GLOBAL / ACCOUNT 列を STATUS と LAST UPDATE の間に置き、
  status 表示後に行ごとに遅延取得して埋める。記号・色・取得順・再投入の規則は §4.6。
  色付けは STATUS と同じく描画済みの view への後段処理（`styleTableCells` が複数列を扱う）。
  一覧のヘッダは「変化する情報が先・固定の文脈が後」の順（`[N selected]` → results 表示 →
  `[status: …]` → 件数 → `[account region]` → `[sort: …]`）。幅が足りない端末で切れるのは
  末尾なので、切れるのはソート順であって結果の完了表示ではない。接続先アカウントは
  取り違え防止のためソート順より前に残す
- 一覧キー（実装済み）: `/` フィルタ・`f` ステータス切替・`s` ソートキー巡回
  (last-update→status→account)・`o` 昇降順トグル・`l`/`enter` 実行履歴画面・`v` ログ直行・
  `space` 選択トグル・`x` 一括 release・`r` 選択行のみ再取得・`R` 全件再取得・`q` 終了。
  既定ソートは last-update 降順（CLI と同一のコア `model.SortSummaries`）。
  release 対象に選んだ行は**行全体をハイライト**（マーカー列は持たない）、header に `[N selected]`
- **行スタイリング（`internal/tui/statuscolor.go`）**: bubbles の table はセル値を
  「エスケープ列を可視幅として数える」ヘルパーで切り詰めるため、行データに色を埋めると壊れる
  （`\x1b[31mFailed\x1b[…`）。そのため **描画済みの view に対して後段でスタイルを当てる**。
  対象は 2 つ:
  - **STATUS 列（一覧 / Executions / Actions 共通）**: `Failed` と `fetch-error` の文字だけを
    赤にする。`fetch-error` は CodePipeline のステータスではなく「状態を取得できなかった行」の
    表示名だが、CLI の table は以前から赤くしており、**取得できなかった行は失敗した行と同じだけ
    注意に値する**。TUI だけ地味に出すのは 2 つのビューの言うことが食い違うだけ
  - **選択行（一覧のみ）**: 行全体を反転気味のハイライト地に。行の同定は **ACCOUNT ID セル**
    （行内で一意かつ切り詰められない唯一の列）で行う

  **この方式は bubbles のレイアウト規約（各セルは左右 1 スペースでパディング・幅 0 の列は落ちる）に
  依存する**。`cellStart` がそれを外側から再実装しており、規約が変わっても何も検知しない
  ＝**別の列を静かに着色する**という壊れ方をする。`go.mod` の bubbles 固定はこのため。
  上げるときは `TestCellStartRendersWhereBubblesPuts`（実際に描画した行からオフセットを
  実測して `cellStart` と突き合わせる自己検証テスト）・`TestCellStartAndText`・
  `TestSyncCursorTint` の失敗をまず疑う。

  行のベーススタイルと `Failed` の色は**別スパンとして描画**する（それぞれが自前で
  エスケープを開いて閉じるので、色のリセットがハイライトを行末まで落とさない）。
  既にスタイル済みの行（header・下罫線・**カーソル行**）は後段処理の対象外 —
  カーソル行は行全体が 1 つのスタイルで包まれており、内側の色のリセットが
  ハイライトを壊すため。カーソル行が伝えるべき状態は**ハイライト自体**に載せる
  （`cursorTint`）: Failed / fetch-error なら赤地、選択済みなら下線（背景はカーソルが使っているため）。
  セルの文字列とハイライトが食い違わないよう、どちらも `rowStatus` 1 つから導く
- Executions 画面（実装済み）: `pipeline.Executions`（ListPipelineExecutions 1 ページ・新しい順）を
  テーブル表示（短縮 id / status / 開始 / 所要 / commit message）。選択実行の source revisions
  （source action 名 – 短縮 hash: commit message、AFT の 2 リポジトリ分）をテーブル下に
  インライン表示。CodeConnections (GitHub) ソースの `RevisionSummary` は JSON 文字列で
  届くため `model.Revision.Message()` が `CommitMessage` を unwrap する（マネジメント
  コンソールと同じ見せ方。CLI `pipeline show` も同ヘルパーを使用）。`l`/`enter` で選択実行の
  Actions 画面へ・`v` で選択実行のログ直行・`r` 強制再取得・`h`/`q`/`esc` で一覧へ戻る。
  履歴は `executions_ttl`（既定 15m）のセッション内 memo から供給（先頭実行が in-flight なら
  常に再取得）。Actions は終端 execution のぶんだけ無期限 memo（§7 のキャッシュ原則）
- Actions 画面（実装済み）: `pipeline.ActionExecutions`（ListActionExecutions を実行 id で
  フィルタ・時系列順に正規化）をテーブル表示（stage / action / status / 開始 / 所要）。
  選択行の summary / error はテーブル下にインライン表示（独立した action detail 画面は
  設けない — AFT パイプラインはアクション数が少なく詳細が薄いため）。ロード完了後、
  終端 CodeBuild アクションのログをバックグラウンドで遅延取得し、terraform の結論 1 行
  （`logs.Verdict`: `Error:` 優先 → `Apply complete!`/`No changes.` → `Plan:`）を summary に
  表示する。**F13 で変更（§4.6）**: 取得は `result.Service.ForActions`（一覧の結果列と同じ経路・
  同じ保存）に寄せた。一覧で取得済みの結果は API なしで即表示。summary の表示（1 行の文章）は
  変えない。末尾 1 ページだけを読むため、下記のログ画面の先読みは（結論行が末尾に無い build を
  除いて）無くなった。verdict 中の add/change/destroy 数値は 0 以外を緑/黄/赤（terraform の plan 色）で
  着色（表示層で幅クリップ後に適用）。この取得は log memo を温めるため、続けてログ画面を
  開くと即表示になる。
  build id を持つアクションで `l`/`enter`/`v` → ログ画面・`h`/`q`/`esc` で戻る
- ログ画面（実装済み）: 対象 build のログを `logs.Fetch` で 1 回取得し viewport 表示。
  `m` で terraform→raw→summary をローカル切替（再フェッチなし。描画は CLI `pipeline logs` と
  同一の `logs.Render`）。`j`/`k` スクロール・`g`/`G` 先頭/末尾・`h`/`q`/`esc` で戻る。
  **複数 build を 1 画面に持てる**（AFT の customizations 実行は global / account の
  2 回 terraform を回すため、「その実行のログ」は 2 本ある）: パイプライン順に連結し、
  各 build の前に `──── <stage> / <action> ────` の区切り行を挟む。**ラベルは stage 込み**が必須 —
  AFT の 2 本はどちらも action 名が `Apply` なので、action 名だけではどちらのログか分からない。
  build が 1 本だけの画面は区切り行を持たない代わりに、header の title に
  `<開いた文脈> · <stage> / <action>` を出す（title は幅に応じてクリップ）。検索 (`/`) は全 build を横断し、
  `[` / `]` で前後の build 先頭へジャンプ（wraparound なし）、header に `[build i/N]` を表示。
  取得は build を 1 本ずつ直列（actions 画面の verdict 先読みと同じ理由: 並列化すると
  batch エンジンのレート制御外で同時リクエストが飛ぶ）。1 本だけ取得に失敗した場合は
  その section にエラー行を出し、もう 1 本のログは表示する（全滅時のみ画面全体をエラーに）。
  less 風検索: `/` で入力 → `enter` で確定（現在位置以降の最初のマッチへジャンプ）・
  `n`/`N` で次/前のマッチへ wraparound 移動・現在マッチ行は反転表示・footer に `i/N` 表示・
  `esc` は検索クリア → 2 回目で戻る。マッチは ANSI 除去後の行に対する大小文字無視の部分一致で、
  モード切替時は新しい描画に対して再検索される。
  **完了 build のログはセッション内メモリに memoize**（`logs.Service` 保持。build ログは
  完了後は不変のため安全）: 同一セッションで同じログを再訪しても API を叩かない。
  実行中 build は毎回再取得。ログ本文はディスクに書かない（ディスクに残すのは
  そこから読んだ terraform の結論だけ。§4.6 / §7）
- `v` ログ直行（実装済み）: 「失敗した → terraform ログを見る」という最頻ケースの 1 打鍵ショートカット。
  解決不能時はエラーをその場に表示。
  - 一覧: 行が保持する最新 execution の `ActionExecutions`（ListActionExecutions 1 回）→
    **build id を持つ全アクション**をパイプライン順に連結。GetPipelineState ではなく実行単位で
    引くのは、state が返すのは**アクションごとの最新 run** で、最新実行で動かなかったステージが
    古い実行のログを混ぜてしまうため。最新 execution 不明の行（status 取得失敗）だけ
    `pipeline.Detail`（GetPipelineState 1 回）＋ `model.PipelineDetail.BuildActions()` にフォールバック
  - Executions 画面: 選択実行の `ActionExecutions` → **build id を持つ全アクション**
    （`model.LogActions`、パイプライン順）を 1 つのログ画面に連結。失敗した 1 本だけに絞らないのは、
    どちらの terraform に答えがあるか探しているのがまさにこの操作だから。CLI `pipeline logs --execution`
    も同じ `model.LogActions` を使い、区切り行のラベルは共通の `model.ActionLabel`。
    単一 build を選ぶ `model.LogAction` は残すが、現在の利用者は無い（将来の単一選択用）
- Release 画面（実装済み・`internal/tui/release.go`）: 一覧で `space` 選択 → `x` で遷移。
  confirm（対象一覧 `output.PipelineTable` 再利用 + 件数 + `max_targets` ガード判定。超過時は `y` を無効化し
  「N 件外す」表示）→ 実行中は spinner + `Done/Total/Failed` 進捗 → 結果（`output.ReleaseTable` 再利用・
  started/skipped/failed 集計）。実行は注入した `ReleaseFunc`（コア `pipeline.Release` + write client）で、
  ガード・InProgress スキップは CLI と共通のコア層。完了後に任意キーで一覧へ戻り、起動した行を
  `refreshNamesMsg` で RefreshOnly 再取得（InProgress へ更新）。TUI は stderr に書けないため cache 無効化は best-effort

- ルートモデルが画面スタックを管理（push/pop）。各画面は独立した `tea.Model`（`screen` interface）。
  ナビゲーションは `pushMsg`/`popMsg` をルートが解釈し、それ以外はスタック最上位へ委譲。
  リサイズは push/pop 時に最上位へ再配送。TUI への依存注入は `tui.Deps`（Fetch/Refresh/Detail/
  Executions/Actions/Logs/Release/ReleaseLimit/PollInterval/Account/Region）に集約。
  接続先アカウント・region は一覧ヘッダに表示（TUI は stderr のバナーを使えないため）
- ロード中はスピナー + 進捗件数（batch の Progress chan を `tea.Cmd` で購読）を表示し、
  再取得時は既存の行を表示したまま更新する。初回ロードのみ行が空（status キャッシュヒット時は
  ほぼ即時に埋まる）。行単位の逐次描画は行っていない
- **実行中パイプラインの自動ポーリング（実装済み）**: `tui.poll_interval`（既定 30s、0 で無効）
  ごとに **in-flight の行だけ** RefreshOnly で再取得する。終端状態の行は自ら変化しないので
  対象にせず、in-flight が 0 件になるとポーリング自体が止まる（tick は常に 1 本だけ）
- multi-select → 一括 release（確認ダイアログに件数・対象を明示、F5 ガードは CLI と共通のコア層で実施）

### 9.2 CLI との整合

TUI の各操作は core 層サービス呼び出しであり、CLI と完全に同じコードパスを通る（ガード・計測・キャッシュ含む）。

## 10. 設定（internal/config）

優先順位: **flag > 環境変数 (`AFT_OPS_*`) > 設定ファイル > 既定値**

- **環境変数名は YAML パスから機械的に決まる**: `AFT_OPS_` + パスの大文字スネーク。
  `cache.status_ttl` → `AFT_OPS_CACHE_STATUS_TTL`、`profile` → `AFT_OPS_PROFILE`。
  **全設定キーに例外なく対応する**（`config.EnvName` が唯一の規則で、struct を歩いて適用する）。
  - 以前は 9 キーだけを手書きで拾っており、しかも名前がキーからずれていた
    （`batch.concurrency` が `AFT_OPS_CONCURRENCY`、`cache.status_ttl` が `AFT_OPS_STATUS_TTL`、
    なのに `cache.dir` は `AFT_OPS_CACHE_DIR`）。残り 13 キーには変数が無いのに、
    上記の優先順位は全キーに env があるかのように書かれていた。規則で導出すれば
    **フィールドを足した瞬間に env が効き**、ドキュメントが保守なしで真であり続ける
  - `AFT_OPS_DEMO` / `AFT_OPS_DEMO_LATENCY` はこの規則の外（設定キーではなく
    fixture の選択なので）
- **環境変数のパース失敗は握りつぶさない**: `AFT_OPS_BATCH_RPS=x` のような値はキー名・
  実際の値・期待する形式を出して exit 2。以前は「パースできたときだけ代入」だったため、
  **指定したつもりの設定が無言で無視される**（要件 §6「silent failure 禁止」に反する）
- **検証は flag マージの後にもう一度走る**: flag は `config.Load` が返った後に載せるので、
  1 回だけでは `--concurrency 0` のような値が検証を素通りして既定値に戻っていた

```yaml
# ~/.config/aft-ops/config.yaml（--config で上書き可）
profile: my-aft-management-profile   # AFT 管理アカウント用の AWS プロファイル
write_profile: ""                    # 書き込み操作用。空なら profile と同じ（§4.3 で同一アカウントを検証）
region: ap-northeast-1
aws_config_file: ~/.aws/config-sandbox  # profile を引く shared config file（後述）
account_source: aft-dynamodb
# static_accounts_file: ~/aft-accounts.json  # account_source: static のとき必須（§7.1）
aft_metadata_table: aft-request-metadata  # account_source: aft-dynamodb のとき参照（§7.1）

batch:
  concurrency: 10
  rps: 8               # API 呼び出し / 秒（AWS サービスごと。0 で無制限）
  service_rps:         # サービス別の上書き（0 = rps を使う）。§4.6
    codepipeline: 0
    codebuild: 0
    logs: 16
  chunk_size: 0        # 0 = チャンク分割なし
  chunk_pause: 0s

cache:
  dir: ~/.cache/aft-ops
  account_ttl: 24h
  pipeline_ttl: 6h
  status_ttl: 10m      # 実行ステータスのキャッシュ TTL（0 で無効化＝毎回 fan-out）
  trigger_ttl: 1h      # pipeline trigger のキャッシュ TTL（0 で無効化＝毎回 fan-out）
  executions_ttl: 15m  # 実行履歴（TUI executions 画面）のセッション内 memo TTL（0 で無効化）
  results_max_age: 720h  # terraform 結果・終端アクションのディスクキャッシュの保持期間（§4.6）

release:
  max_targets: 50
  skip_in_progress: true

trigger:               # §4.4。アカウントごとの設定は持たず metadata から導出する
  source_action: aft-account-customizations
  branch: main
  file_path_includes:  # CodePipeline の上限: includes / excludes 各 8 本・1 本 255 文字
    - "{customizations_name}/**"
  file_path_excludes:
    - "**/*.md"
    - "**/.terraform-docs.yml"
  max_targets: 50      # §4.7 `pipeline triggers fix` の上限件数。超過は --max-targets で明示

tui:
  poll_interval: 30s   # TUI の in-flight 自動再取得間隔 / `pipeline list --watch` の既定間隔

metrics:
  enabled: true
  dir: ~/.local/state/aft-ops/metrics
  keep_runs: 100       # 保持する実行ごとの JSONL 件数（0 で無制限）
```

- プロファイルは AWS SDK 標準のクレデンシャルチェーンに委譲（ツールは認証情報を保持しない）
- 読み取り/書き込みでプロファイルを分けたい場合に備え `write_profile`
  （任意、env `AFT_OPS_WRITE_PROFILE` / flag `--write-profile`、未指定なら `profile` を使用）を用意
- **1 run = 1 account**: `write_profile` は「同一アカウントの別ロール」（ReadOnly と
  Administrator など）のための設定であり、別アカウントを指すことは想定しない。
  書き込み直前の STS 検証で読み取り側のアカウントと突き合わせ、**不一致なら exit 2 で拒否**する。
  - この不変条件を選んだのは、危険な組み合わせの入口が 1 つではないため。
    `--profile` は読み取り側だけを動かすので config の `write_profile` が残り、
    同じ事故は `AFT_OPS_WRITE_PROFILE` からも config の書き間違いからも起きる。
    フラグの上書き規則で塞ぐと最初の 1 つしか塞がらない
  - **`--profile` は `write_profile` を書き換えない**。明示的に設定された値を黙って
    別のアカウントへ向け直すのは、まさにこのガードが防ごうとしている種類の挙動になる。
    両方を動かすには `--write-profile` を渡す（拒否メッセージもこれを案内する）
  - 検証コストは 0 — 別 profile のときは元々 `GetCallerIdentity` を呼んで
    `aws (write):` 行を出していた。突き合わせていなかっただけ
  - `--dry-run` は書き込みクレデンシャルを解決しないためアカウント検証もできない。
    実行時に使われる profile 名だけを「未検証」と明示して stderr に出す
  - **必要な IAM 権限は README の Permissions 節**（read 側の API 一覧と、write 側の
    `codepipeline:StartPipelineExecution` 1 本だけのポリシー）。AFT 自身が作るロールは
    サービスロール（サービス信頼で assume 不可）か `AWSAFTExecution` / `AWSAFTService`
    （中身は `AdministratorAccess` で、AFT の自動化自身が使う ID）しかないため、
    **オペレータ用には別途最小権限のロールを用意する前提**で設計している
- **`aws_config_file`（任意、env `AFT_OPS_AWS_CONFIG_FILE` / flag `--aws-config-file`）**:
  profile を引く shared config file を固定する。組織ごとに config file を分けて
  `AWS_CONFIG_FILE` で切り替える運用では、設定ファイル側の `profile` は固定なのに
  シェル側のファイルだけが変わり、**設定した profile を定義していないファイルを引く**
  という組み合わせが起きる。profile とそれを定義するファイルを一組で持たせるための設定。
  - 未設定なら SDK の解決に委ねる（`AWS_CONFIG_FILE` があればそれ、無ければ `~/.aws/config`）。
    既定を我々が埋めてしまうと素の `AWS_CONFIG_FILE` が黙って効かなくなるため、**空のまま**にする
  - 設定した場合は `AWS_CONFIG_FILE` より**強い**（`WithSharedConfigFiles`）
  - **存在チェックは config の検証で行う**。SDK に渡すと「プロファイルが見つからない」
    という症状違いのエラーになり、診断が的外れな方向へ行くため
  - credentials file は対象外（SSO 中心の運用では出番がない）
  - **cache scope は profile+region のまま**変えない。異なる config file の同名 profile が
    別アカウントを指した場合は、後述の identity ガードが検出して当該 scope を破棄する
- **接続先の明示（取り違え防止）**: AWS に触れるコマンドは `aws: account <id> · region <r> ·
  profile <p>` を stderr に 1 行出す。profile 未設定時は「環境のクレデンシャルチェーンを
  使っている」旨を併記する（cache scope は profile+region 由来なので、profile 無指定だと
  日によって別アカウントを指しうる）。SDK 既定以外の config file を使っているときは
  `· config <path>` を併記する（`aws_config_file` 指定時と、素の `AWS_CONFIG_FILE` が
  効いているときの両方。後者こそが「意図と違うファイルで profile が解決された」という
  失敗の現場なので、自分の設定だけを出しても意味がない）。identity は cache scope に記録し、**記録と異なる
  identity を検出したら警告して当該 scope を破棄する**（別アカウントのデータを供給しない）。
  検証コスト（クレデンシャル解決 + STS で約 0.6s）とキャッシュヒット時の応答（0.02s）の
  釣り合いから、実際に `sts:GetCallerIdentity` を呼ぶのは次の場合:
  - profile 未設定（＝環境依存で最も危険なケース）
  - 記録が無い / 24h より古い
  - `--refresh`（記録もキャッシュの一種として再検証する）
  - **書き込み系（`WriteAWS`）は常に検証**し、確認プロンプトの直前に出す
  記録を再利用した場合はバナーに `(identity from cache; --refresh re-checks)` を付け、
  **その run で検証していないことを表示上も区別する**（バナーが誤った安心を与えないため）

## 10.1 デモモード（`--demo`）

`--demo <fixture.json>`（env: `AFT_OPS_DEMO`）を渡すと、AWS を一切呼ばずに
ローカルの fixture だけでツール全体が動く。認証・ネットワーク・AFT アカウントの
いずれも不要。用途は 2 つ:

1. README のショーケース GIF を、実データを一切出さずにオフライン・決定論的に録画する
2. OSS 利用者が実アカウントに向ける前に挙動を試せる「お試しモード」

### 差し替え境界は AWS SDK クライアント interface

フェイクは `internal/demo` に置き、**コア層がすでに依存している狭い interface を
そのまま実装する**:

| interface | 定義 |
|---|---|
| `pipeline.API` | ListPipelines / ListPipelineExecutions / GetPipelineState / ListActionExecutions |
| `pipeline.StartAPI` | StartPipelineExecution |
| `logs.CodeBuildAPI` | BatchGetBuilds |
| `logs.LogsAPI` | GetLogEvents |
| `account.Source` | アカウント一覧 |

したがって**アダプタ層より上（正規化・キャッシュ・バッチ・ソート・両レンダラ・TUI）は
実データと完全に同じ経路を通り**、デモ専用の分岐はどこにも入らない。分岐は
`internal/cli/app.go` の AWS クライアント生成 5 箇所だけで、そこに到達しない
`readAWSLocked` はデモ時に明示エラーを返す（「AWS を呼ばない」約束を破る経路が
静かに成立しないようにする）。SDK 型を組み立てるのは `internal/demo` の中だけなので、
「AWS SDK の型を core の公開 API に露出させない」原則も保たれる。

### 時刻は相対・状態は生きている

fixture の時刻はすべて**ロード時刻からの相対値**（`started_ago` / `took` /
`completes_in`）。絶対時刻を持つと録画のたびに「3 週間前」になるため。
`completes_in` を持つ実行は録画中に実際に終了するので、`--watch` と TUI の
ポーリングは「本当に動いているものを待って更新する」様子をそのまま撮れる。
デモの Release change も同様に fixture 上に in-flight な実行を生やす（メモリ上のみ・
ファイルは変更しない）。

キャッシュは通常どおり有効。fixture の `identity.profile` がキャッシュスコープに
なるため、実プロファイルのスコープにデモデータが混ざることはない。
metrics はデモ時に無効化する（フェイク呼び出しは SDK middleware を通らない）。

詳細と fixture のスキーマは [docs/demo/README.md](demo/README.md)。

## 11. テスト戦略

| レイヤ | 方法 |
|---|---|
| core / batch / cache | adapter interface のモックによる unit test。batch はレート・キャンセル・部分失敗を重点的に |
| awsx adapter | SDK の `smithy` middleware レベルの stub。実 API は叩かない |
| CLI | golden file test（`--demo` fixture に対してコマンドライン・exit code・stdout・stderr を 1 ファイルに記録。`go test ./internal/cli -update` で再記録） |
| TUI | 各画面の `Update` に `tea.KeyMsg` / 各種 msg を直接流し、返る model と `tea.Cmd` を検証するテーブルドリブン。`teatest` は使わない（プロセスと端末を立てないぶん速く、描画タイミングに左右されないため） |
| E2E | 検証用の AFT 環境で read 系 + release の疎通確認。本番相当環境では read 系のみ手動確認 |
| demo fixture | 同梱 fixture を `internal/demo` のフェイク経由でコアサービスに流し、在庫フィルタ・ステータス分布・アクション順・ログ抽出・in-flight の完了・release を検証（fixture の腐敗を録画ではなくビルドで検出する） |

## 12. CI / リリース

- GitHub Actions: `go test` + `golangci-lint` + `go vet`（PR 毎）
- リリース: goreleaser で darwin/linux × amd64/arm64 のバイナリ + Homebrew tap（Phase 4）
- バージョニング: SemVer。`v0.x` の間は破壊的変更可

## 13. フェーズ別実装計画（requirements §8 の具体化）

| Phase | 実装物 | 状態 |
|---|---|---|
| 1 | リポジトリ骨格 / config / awsx / cache / account 解決 / batch（最小: 並列度+RPS+リトライ） / `pipeline list` / `pipeline release`（単発+ガード） / TUI 一覧画面 / metrics（記録のみ） | **実装済** |
| 2 | `pipeline show` / `pipeline executions` / `pipeline refresh` / `pipeline logs`（terraform 抽出・summary） / batch 完全版（チャンク・進捗） / `pipeline release` バッチ / `pipeline triggers`（F10・§4.4） / TUI 詳細・ログ画面・multi-select / `metrics show` | **実装済** |
| 3 | §4.6 一覧の terraform 結果列（F13）: `ParseVerdict` / `core/result` / レート制御の移設 / 一覧・Actions・CLI の共通化 ・ §4.5 収束実行（F11 + F12）: リビジョン×ステージ判定 / チャンクゲート / 完了モニタリング / 未適用コミット検出 ・ §4.7 trigger の設定（F14）: `pipeline triggers fix` | F13 実装済。F11 / F12 / F14 は未着手 |
| 4 | account-request（DynamoDB）/ Step Functions 状態 / 共通系パイプライン（F9） | 未着手 |
| 5 | OSS 公開整備（英語 docs・goreleaser・Homebrew tap・LICENSE） | 一部先行済（goreleaser / homebrew_casks / LICENSE / CI は導入済。英語ドキュメントが残り） |

Phase 番号は requirements §8 と一対一で対応させる。片方だけを増やさないこと。

## 14. 設計上の未決事項

| # | 事項 | 状態 |
|---|---|---|
| ~~D1~~ | ~~リポジトリ~~ | **解決済**: 新規リポジトリ `aft-ops` で開始。既存 Bash ツールセットは資産として残し移行後アーカイブ |
| ~~D2~~ | ~~既定リージョン~~ | **解決済**: `ap-northeast-1` |
| ~~D3~~ | ~~`aft-request-metadata` テーブルのスキーマ確認~~ | **解決済**: 実テーブルで検証し `core/account` を実スキーマに追従済み |
| ~~D4~~ | ~~TUI のログ画面で CloudWatch Logs Live Tail を使うか~~ | **解決済**: 使わない。`core/logs` は `GetLogEvents` のページングで実装。終端 build のログは不変でキャッシュが効くため、常時接続の利得が無い |
| ~~D5~~ | ~~設定実装~~ | **解決済**: YAML 単一フォーマット・`yaml.v3` + 自前マージ（viper は依存過多のため不採用） |
| D6 | F12 のカナリアゲート以降、止められない先行投入範囲をどこまで許容するか | requirements U8。`execution の所要時間 ÷ chunk_pause` で決まる |
| D7 | F12 の判定に terraform の apply サマリ行まで含めるか | requirements U7。ステージの成否だけなら AFT の出力形式に依存しない。F13（§4.6）が `ParseVerdict` で apply サマリ行を構造化するので、含める場合はそれを使える |
