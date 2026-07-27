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
│   │   └── logs/                # CodeBuild ログ取得・terraform ログ抽出
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
対象決定（名前/アカウント指定・--status Failed・--file/stdin）
→ 対象の status を再取得（下記）
→ dry-run 表示（対象一覧 + 件数）
→ 確認プロンプト（--yes でスキップ、件数 > limit なら拒否）
→ Batch Engine で StartPipelineExecution
→ 結果レポート（成功/失敗/skip 件数、失敗理由）
```

- 既定の安全ガード: `max_release_targets: 50`（設定で変更可）。超過時は `--force-limit` 相当の明示が必要
- 冪等性: 実行中 (InProgress) のパイプラインは既定でスキップ（`--include-in-progress` で上書き）
- **release は status キャッシュに依存しない**: status は「`--status` がどれを選ぶか」と
  「InProgress スキップがどれを飛ばすか」の 2 つを決めるため、`status_ttl`（既定 10m）だけ
  古いデータで書き込みを判断させない。`--status` 指定時は全件を強制再取得、明示ターゲット時は
  その対象だけ再取得してから確認に進む。TUI の Release 画面も confirm 前に対象を再取得する

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

セッション内メモリ memo（TUI 起動中のみ・ディスクに書かない）:

| データ | ソース | ポリシー |
|---|---|---|
| 実行履歴（executions 画面） | ListPipelineExecutions | 15m（`executions_ttl`、0 で無効）。先頭実行が in-flight なら常に再取得。`r` で強制 |
| アクション実行（actions 画面 / `v`） | ListActionExecutions | 終端 execution のみ無期限（不変）。in-flight は毎回 |
| build ログ（log 画面） | BatchGetBuilds + GetLogEvents | 完了 build のみ無期限（不変）。実行中は毎回 |

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
    --sort last-update|status|account  # 既定 last-update（status は重大度順: Failed → … → Succeeded）
    --order asc|desc             # 既定 desc（未実行=時間なしは常に末尾）
    --refresh                    # 全 status を強制再取得（inventory/accounts も）
    --watch [--interval 30s]     # 定期再取得（既定間隔は tui.poll_interval）。table 出力専用
aft-ops pipeline refresh <target...>  # 指定パイプラインの status だけ再取得しキャッシュ更新
aft-ops pipeline show <target>   # F2: 詳細（ステージ/実行履歴）
aft-ops pipeline executions <target>  # F2: 実行履歴一覧（alias: execs）
    [--limit 25] [--actions]     # --actions は各実行のアクション（CodeBuild id 付き）も展開
aft-ops pipeline logs <target>   # F2: CodeBuild/terraform ログ
    [--execution <id>] [--build <id>] [--raw|--summary]
    # 既定（フラグ無し）= 現在の state の失敗アクション 1 本
    # --execution = その実行の build を全部（global/account の 2 本）。
    #   `──── <stage> / <action> ────` で区切る。単一 build のときは区切り無し
    # --build = 指定 1 本のみ
aft-ops pipeline release [targets...]   # F3: Release change
    --status Failed              # フィルタ結果を対象に（対象決定時に status を強制再取得）
    --file targets.txt | -       # 明示リスト（stdin 可）
    --dry-run / --yes
    --concurrency N --chunk-size N --chunk-pause 30s

aft-ops account list
aft-ops cache status|clear|refresh
aft-ops metrics show [--last N]
aft-ops version / completion
```

- `<target>` はパイプライン名・アカウント ID・アカウント名のいずれでも解決（F7）
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

- 一覧キー（実装済み）: `/` フィルタ・`f` ステータス切替・`s` ソートキー巡回
  (last-update→status→account)・`o` 昇降順トグル・`l`/`enter` 実行履歴画面・`v` ログ直行・
  `space` 選択トグル・`x` 一括 release・`r` 選択行のみ再取得・`R` 全件再取得・`q` 終了。
  既定ソートは last-update 降順（CLI と同一のコア `model.SortSummaries`）。
  release 対象に選んだ行は**行全体をハイライト**（マーカー列は持たない）、header に `[N selected]`
- **行スタイリング（`internal/tui/statuscolor.go`）**: bubbles の table はセル値を
  「エスケープ列を可視幅として数える」ヘルパーで切り詰めるため、行データに色を埋めると壊れる
  （`\x1b[31mFailed\x1b[…`）。そのため **描画済みの view に対して後段でスタイルを当てる**。
  対象は 2 つ:
  - **STATUS 列（一覧 / Executions / Actions 共通）**: `Failed` の文字だけを赤にする
  - **選択行（一覧のみ）**: 行全体を反転気味のハイライト地に。行の同定は **ACCOUNT ID セル**
    （行内で一意かつ切り詰められない唯一の列）で行う

  行のベーススタイルと `Failed` の色は**別スパンとして描画**する（それぞれが自前で
  エスケープを開いて閉じるので、色のリセットがハイライトを行末まで落とさない）。
  既にスタイル済みの行（header・下罫線・**カーソル行**）は後段処理の対象外 —
  カーソル行は行全体が 1 つのスタイルで包まれており、内側の色のリセットが
  ハイライトを壊すため。カーソル行が伝えるべき状態は**ハイライト自体**に載せる
  （`cursorTint`）: Failed なら赤地、選択済みなら下線（背景はカーソルが使っているため）
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
  表示する。verdict 中の add/change/destroy 数値は 0 以外を緑/黄/赤（terraform の plan 色）で
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
  実行中 build は毎回再取得。ディスクには書かない（ディスクキャッシュは §4.1 の
  アカウント map・パイプライン一覧のみのまま）
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
  executions_ttl: 15m  # 実行履歴（TUI executions 画面）のセッション内 memo TTL（0 で無効化）

release:
  max_targets: 50
  skip_in_progress: true

tui:
  poll_interval: 30s   # TUI の in-flight 自動再取得間隔 / `pipeline list --watch` の既定間隔

metrics:
  enabled: true
  keep_runs: 100       # 保持する実行ごとの JSONL 件数（0 で無制限）
```

- プロファイルは AWS SDK 標準のクレデンシャルチェーンに委譲（ツールは認証情報を保持しない）
- 読み取り/書き込みでプロファイルを分けたい場合に備え `write_profile`（任意、未指定なら `profile` を使用）を用意
- **接続先の明示（取り違え防止）**: AWS に触れるコマンドは `aws: account <id> · region <r> ·
  profile <p>` を stderr に 1 行出す。profile 未設定時は「環境のクレデンシャルチェーンを
  使っている」旨を併記する（cache scope は profile+region 由来なので、profile 無指定だと
  日によって別アカウントを指しうる）。identity は cache scope に記録し、**記録と異なる
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
| CLI | golden file test（table/json 出力の回帰） |
| TUI | `teatest`（bubbletea 公式テストユーティリティ）でキー操作→画面遷移 |
| E2E | 検証用の AFT 環境で read 系 + release の疎通確認。本番相当環境では read 系のみ手動確認 |
| demo fixture | 同梱 fixture を `internal/demo` のフェイク経由でコアサービスに流し、在庫フィルタ・ステータス分布・アクション順・ログ抽出・in-flight の完了・release を検証（fixture の腐敗を録画ではなくビルドで検出する） |

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
| ~~D3~~ | ~~`aft-request-metadata` テーブルのスキーマ確認~~ | **解決済**: 実テーブルで検証し `core/account` を実スキーマに追従済み |
| D4 | TUI のログ画面で CloudWatch Logs Live Tail を使うか | Phase 2 で判断（ポーリングで十分な可能性） |
| D5 | 設定実装 | **解決済**: YAML 単一フォーマット・`yaml.v3` + 自前マージ（viper は依存過多のため不採用） |
