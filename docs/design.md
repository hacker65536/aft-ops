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
│   │   ├── pipelinedetail.go    # 詳細画面（ステージ/実行履歴）
│   │   ├── logview.go           # ログ閲覧画面
│   │   └── confirm.go           # 実行確認ダイアログ
│   ├── core/
│   │   ├── model/               # ドメインモデル（Pipeline, Account, Execution, StageState...）
│   │   ├── pipeline/            # 一覧・詳細・release のサービス
│   │   ├── account/             # アカウント解決サービス
│   │   └── logs/                # CodeBuild ログ取得・terraform ログ抽出
│   ├── batch/                   # 逐次バッチエンジン
│   ├── cache/                   # TTL ファイルキャッシュ
│   ├── metrics/                 # API 計測（SDK middleware + 集計）
│   ├── awsx/                    # AWS クライアント生成・レートリミッタ・リトライ
│   ├── config/                  # 設定の読込・マージ
│   └── output/                  # table/json レンダラ、exit code 規約
├── docs/
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
3. 取得結果を status cache にマージ保存。Account 解決を合成して PipelineSummary[] を返す
4. renderer が table / json で出力。status の鮮度（refetched/from-cache・最古経過）を stderr に明示
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
対象決定（名前/アカウント指定・--status Failed・--file/stdin）
→ dry-run 表示（対象一覧 + 件数）
→ 確認プロンプト（--yes でスキップ、件数 > limit なら拒否）
→ Batch Engine で StartPipelineExecution
→ 結果レポート（成功/失敗/skip 件数、失敗理由）
```

- 既定の安全ガード: `max_release_targets: 50`（設定で変更可）。超過時は `--force-limit` 相当の明示が必要
- 冪等性: 実行中 (InProgress) のパイプラインは既定でスキップ（`--include-in-progress` で上書き）

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
- **worker pool + rate.Limiter の二段制御**: 並列度（同時実行数）と RPS（毎秒呼び出し数）を独立に制御。実測で throttle が出ない範囲から既定は Concurrency=10 / RPS=8 程度で開始し、計測結果で調整
- **リトライ**: SDK v2 の `retry.AddWithMaxAttempts` + adaptive mode を基本とし、Throttling は指数バックオフ + ジッタ。リトライ発生は metrics に記録
- **キャンセル**: ctx キャンセル（Ctrl-C）で新規投入を止め、実行中のみ完走して部分結果を返す
- **進捗通知**: `chan Progress` を公開し、CLI はプログレスバー、TUI は画面更新に利用

## 6. レート計測（internal/metrics）

「実装しながら高精度に分析したい」（要件 F4）に対応する一級機能。

- AWS SDK v2 の **middleware** で全 API 呼び出しをフック: サービス/オペレーション/所要時間/成否/Throttling 有無を記録
- 実行ごとに `~/.local/state/aft-ops/metrics/<timestamp>.jsonl` に追記
- `aft-ops metrics show [--last N]`: オペレーション別の呼び出し数・p50/p99・throttle 率を集計表示
- 将来: 計測結果から推奨 Concurrency/RPS を提示（自動チューニング）

## 7. キャッシュ（internal/cache）

| データ | ソース | 既定 TTL |
|---|---|---|
| account map（ID⇔名前⇔email） | 下記 7.1 | 24h |
| pipeline 存在一覧 | ListPipelines | 6h |
| 実行ステータス（per-entry） | ListPipelineExecutions | 10m（`status_ttl`）。実行中は常に再取得 |

- 保存先: `~/.cache/aft-ops/<org-id or profile>/` （プロファイル毎に分離し、業務/PoC org の取り違えを構造的に防止）
- 形式: JSON + メタデータ（取得時刻・スキーマバージョン・取得元プロファイル）
- 出力時に stale 情報を明示（`cached 3h ago` 等）。`--no-cache` / `--refresh` を全読み取りコマンドでサポート
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
    --sort last-update|status|account  # 既定 last-update
    --order asc|desc             # 既定 desc（未実行=時間なしは常に末尾）
    --refresh                    # 全 status を強制再取得（inventory/accounts も）
    --watch                      # 定期再取得（簡易ウォッチ）
aft-ops pipeline refresh <target...>  # 指定パイプラインの status だけ再取得しキャッシュ更新
aft-ops pipeline show <target>   # F2: 詳細（ステージ/実行履歴）
aft-ops pipeline logs <target>   # F2: CodeBuild/terraform ログ
    [--execution <id>] [--raw|--summary]
aft-ops pipeline release [targets...]   # F3: Release change
    --status Failed              # フィルタ結果を対象に
    --file targets.txt | -       # 明示リスト（stdin 可）
    --dry-run / --yes
    --concurrency N --chunk-size N --chunk-pause 30s

aft-ops account list
aft-ops cache status|clear|refresh
aft-ops metrics show [--last N]
aft-ops version / completion
```

- `<target>` はパイプライン名・アカウント ID・アカウント名のいずれでも解決（F7）

### 8.2 出力規約（AI フレンドリー境界）

- 全コマンド共通 `--output table|json`（既定: TTY なら table、パイプなら json も検討 → 混乱を避け**既定は常に table、json は明示**とする）
- JSON はスキーマに `schema_version` を含め、後方互換を維持
- 装飾（色・スピナー）は TTY 検知で自動 off、`--no-color` あり
- stderr = 進捗・診断、stdout = データ。パイプ処理を壊さない

### 8.3 exit code 規約

| code | 意味 |
|---|---|
| 0 | 正常（対象すべて成功/取得完了） |
| 1 | ドメイン上の失敗あり（例: Failed パイプラインが存在、release の一部失敗） |
| 2 | ツールエラー（設定不正・認証失敗・API エラー） |
| 130 | ユーザー中断 |

※ `pipeline list` で「Failed が存在したら exit 1」は `--fail-on-error` フラグでオプトイン（既定は 0。監視スクリプト用途向け）。

## 9. TUI 設計（internal/tui）

### 9.1 画面構成

```
[Pipeline List]  ──enter──▶  [Pipeline Detail]  ──l──▶  [Log View]
  N rows                       stages / history           terraform log
  / filter                     failed action             m mode switch
  f status filter              ↑/↓ scroll                (future) search/follow
  s sort key / o order
  r/R refresh          ──x──▶  [Release]  (confirm → run → results)
  space multi-select             count / targets / guard
  q quit
```

- 現状（実装済み）の一覧キー: `/` フィルタ・`f` ステータス切替・`s` ソートキー巡回
  (last-update→status→account)・`o` 昇降順トグル・`enter` 詳細画面・`space` 選択トグル・
  `x` 一括 release・`r` 選択行のみ再取得・`R` 全件再取得・`q` 終了。既定ソートは last-update 降順
  （CLI と同一のコア `model.SortSummaries`）。選択行は先頭列に `✓`、header に `[N selected]`
- 詳細画面（実装済み）: `enter` で選択パイプラインの `pipeline.Detail`（ステージ/アクション + 履歴 5 件）を
  取得し viewport にスクロール表示。描画は CLI の `output.PipelineDetailText` を再利用（CLI と同一テキスト）。
  `↑/↓` スクロール・`l` ログ画面・`q`/`esc` で一覧へ戻る
- ログ画面（実装済み）: 詳細画面で `l` → 失敗 CodeBuild アクション（無ければ build id を持つ最後のアクション）の
  ログを `logs.Fetch` で 1 回取得し viewport 表示。`m` で terraform→raw→summary をローカル切替（再フェッチなし。
  描画は CLI `pipeline logs` と同一の `logs.Render`）。`↑/↓` スクロール・`q`/`esc` で戻る
- Release 画面（実装済み・`internal/tui/release.go`）: 一覧で `space` 選択 → `x` で遷移。
  confirm（対象一覧 `output.PipelineTable` 再利用 + 件数 + `max_targets` ガード判定。超過時は `y` を無効化し
  「N 件外す」表示）→ 実行中は spinner + `Done/Total/Failed` 進捗 → 結果（`output.ReleaseTable` 再利用・
  started/skipped/failed 集計）。実行は注入した `ReleaseFunc`（コア `pipeline.Release` + write client）で、
  ガード・InProgress スキップは CLI と共通のコア層。完了後に任意キーで一覧へ戻り、起動した行を
  `refreshNamesMsg` で RefreshOnly 再取得（InProgress へ更新）。TUI は stderr に書けないため cache 無効化は best-effort

- ルートモデルが画面スタックを管理（push/pop）。各画面は独立した `tea.Model`（`screen` interface）。
  ナビゲーションは `pushMsg`/`popMsg` をルートが解釈し、それ以外はスタック最上位へ委譲。
  リサイズは push/pop 時に最上位へ再配送。TUI への依存注入は `tui.Deps`（Fetch/Refresh/Detail/Logs/Release/
  ReleaseLimit）に集約
- 一覧はロード中も操作可能: キャッシュ済み情報を即表示 → バッチ取得の進捗に応じて行を逐次更新（batch の Progress chan を `tea.Cmd` で購読）
- 実行中パイプラインがある場合は自動ポーリング（間隔は設定、既定 30s）※未実装（Phase 3 候補）
- multi-select → 一括 release（確認ダイアログに件数・対象を明示、F5 ガードは CLI と共通のコア層で実施）

### 9.2 CLI との整合

TUI の各操作は core 層サービス呼び出しであり、CLI と完全に同じコードパスを通る（ガード・計測・キャッシュ含む）。

## 10. 設定（internal/config）

優先順位: **flag > 環境変数 (`AFT_OPS_*`) > 設定ファイル > 既定値**

```yaml
# ~/.config/aft-ops/config.yaml（--config で上書き可）
profile: my-aft-management-profile   # AFT 管理アカウント用の AWS プロファイル
region: ap-northeast-1
account_source: aft-dynamodb

batch:
  concurrency: 10
  rps: 8
  chunk_size: 0        # 0 = チャンク分割なし
  chunk_pause: 0s

cache:
  dir: ~/.cache/aft-ops
  account_ttl: 24h
  pipeline_ttl: 6h
  status_ttl: 10m      # 実行ステータスのキャッシュ TTL（0 で無効化＝毎回 fan-out）

release:
  max_targets: 50
  skip_in_progress: true

tui:
  poll_interval: 30s
```

- プロファイルは AWS SDK 標準のクレデンシャルチェーンに委譲（ツールは認証情報を保持しない）
- 読み取り/書き込みでプロファイルを分けたい場合に備え `write_profile`（任意、未指定なら `profile` を使用）を用意

## 11. テスト戦略

| レイヤ | 方法 |
|---|---|
| core / batch / cache | adapter interface のモックによる unit test。batch はレート・キャンセル・部分失敗を重点的に |
| awsx adapter | SDK の `smithy` middleware レベルの stub。実 API は叩かない |
| CLI | golden file test（table/json 出力の回帰） |
| TUI | `teatest`（bubbletea 公式テストユーティリティ）でキー操作→画面遷移 |
| E2E | 検証用の AFT 環境で read 系 + release の疎通確認。本番相当環境では read 系のみ手動確認 |

## 12. CI / リリース

- GitHub Actions: `go test` + `golangci-lint` + `go vet`（PR 毎）
- リリース: goreleaser で darwin/linux × amd64/arm64 のバイナリ + Homebrew tap（Phase 4）
- バージョニング: SemVer。`v0.x` の間は破壊的変更可

## 13. フェーズ別実装計画（requirements §8 の具体化）

| Phase | 実装物 |
|---|---|
| 1 | リポジトリ骨格 / config / awsx / cache / account 解決 / batch（最小: 並列度+RPS+リトライ） / `pipeline list` / `pipeline release`（単発+ガード） / TUI 一覧画面 / metrics（記録のみ） |
| 2 | `pipeline show` / `pipeline logs`（terraform 抽出・summary） / batch 完全版（チャンク・進捗） / `pipeline release` バッチ / TUI 詳細・ログ画面・multi-select / `metrics show` |
| 3 | account-request（DynamoDB）/ Step Functions 状態 / 共通系パイプライン |
| 4 | OSS 公開整備（英語 docs・goreleaser・Homebrew tap・LICENSE） |

## 14. 設計上の未決事項

| # | 事項 | 状態 |
|---|---|---|
| ~~D1~~ | ~~リポジトリ~~ | **解決済**: 新規リポジトリ `aft-ops` で開始。既存 Bash ツールセットは資産として残し移行後アーカイブ |
| ~~D2~~ | ~~既定リージョン~~ | **解決済**: `ap-northeast-1` |
| D3 | `aft-request-metadata` テーブルのスキーマ確認 | Phase 1 着手時に実環境で確認 |
| D4 | TUI のログ画面で CloudWatch Logs Live Tail を使うか | Phase 2 で判断（ポーリングで十分な可能性） |
| D5 | 設定実装 | **解決済**: YAML 単一フォーマット・`yaml.v3` + 自前マージ（viper は依存過多のため不採用） |
