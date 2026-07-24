# aft-ops — AFT Operations Toolkit

AFT (AWS Control Tower Account Factory for Terraform) が展開するリソース、特にアカウント別
customizations パイプライン（数百件規模）の状態確認・Release change 操作を行う Go 製 CLI/TUI。

既存の Bash ツールセットのゼロベースリファクタリング後継。OSS 公開を視野に開発中。

## 必読ドキュメント

- `docs/requirements.md` — 要件定義 v1（確定済み）
- `docs/design.md` — 設計書 v1（確定済み。アーキテクチャ・コマンド体系・フェーズ計画）

> 開発者ローカルの作業状態・組織固有値は git 追跡外に保持している:
> `docs/draft_progress.md`（進捗・次アクション）と `.claude/CLAUDE.local.md`（AWS 実値）。
> どちらも `.gitignore` 済みで、公開ドキュメントには載せない。

## アーキテクチャ原則（変更時は design.md も更新）

- **コア層は UI 非依存**: CLI (cobra) / TUI (Bubble Tea) は `internal/core` のサービスだけを呼ぶ。
  ガード・計測・キャッシュはコア層に置き、UI に書かない
- **AWS SDK の型を core の公開 API に露出させない**（`internal/core/model` に正規化）
- **出力境界**: stdout=データ / stderr=進捗・診断。JSON は `schema_version` 付き。
  exit code: 0=正常 / 1=ドメイン失敗 / 2=ツールエラー / 130=中断
- **実行ステータスはキャッシュしない**（鮮度優先）。キャッシュはアカウント map と
  パイプライン存在一覧のみ。キャッシュはプロファイル毎にディレクトリ分離
- 設定は YAML 単一フォーマット・自前マージ（viper 不採用）。優先順位: flag > env(AFT_OPS_*) > file > default
- コード・コメント・CLI ヘルプは英語（OSS 前提）

## 開発コマンド

```bash
go build -o aft-ops ./cmd/aft-ops
go test ./...
go vet ./...
```

## AWS 環境

- 対象は AFT 管理アカウント（Control Tower Account Factory for Terraform の管理アカウント）。
  region は設定 / 環境変数 / フラグで指定（既定値の想定は `docs/design.md` 参照）。
- 認証は AWS SDK 標準のクレデンシャルチェーンに委譲。ツールは認証情報を保持しない。
- **書き込みを伴う動作確認（Release change / StartPipelineExecution 等）は必ずユーザーに確認してから**。
- 開発者ローカルの実アカウント ID・profile 名・org 区別などの固有値は `.claude/CLAUDE.local.md`
  （git 追跡外）に集約している。

## 未解決事項

- 公開設計上の TODO は `docs/requirements.md` / `docs/design.md` の「未決事項」節を参照。
- ローカル検証が必要な項目（実スキーマ確認 D3 等）は `docs/draft_progress.md`（git 追跡外）を参照。

@.claude/CLAUDE.local.md
