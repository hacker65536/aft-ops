# AFT Operations Toolkit — 要件定義書 v1

- Status: Draft v1
- Date: 2026-07-24
- 対象: 既存 Bash スクリプト群のゼロベースリファクタリング

---

## 1. 目的・背景

AWS 公式ソリューション [AFT (Account Factory for Terraform)](https://github.com/aws-ia/terraform-aws-control_tower_account_factory) が AFT 管理アカウントに展開するリソース群の**状態確認と運用オペレーション**を、コマンドベースで高速・安全に行うためのツール。

### 解決したい課題

- アカウント別 customizations パイプライン（CICD 本体）が数百個あり、「適用できているか / どこでこけているか」の確認が高頻度で発生する
- 失敗したパイプラインの再実行（Release change）を個別・一括で行いたい
- AFT 標準の全体/一部適用機構（Step Functions ベース）は動作が重く、**より軽量な代替手段**が欲しい
- 素朴な高並列 API 呼び出しはレートリミット (Throttling) に当たる（実績: 20 並列程度で発生）

## 2. 決定事項

| 項目 | 決定 | 補足 |
|---|---|---|
| 実装言語 | **Go** | AWS SDK for Go v2 / goroutine による並列制御 / シングルバイナリ配布 |
| UI 戦略 | **コア層 + CLI + TUI を同時並行開発** | コアロジックを UI 非依存に分離することが構造要件 |
| 配布 | **OSS 公開を視野** | 組織固有値のハードコード禁止・設定外出し・ライセンス選定 |
| スコープ | **AFT リソース全般** | 最優先はパイプライン状態確認 + Release change |
| 名称 | **AFT Operations Toolkit** | コマンド名は `aft-ops` を推奨（`aot` は AOT compilation と衝突） |

## 3. ドメイン前提（AFT の構造）

- AFT 管理アカウントに CodePipeline 群が展開される:
  - **アカウント別 customizations パイプライン** (`<account-id>-customizations-pipeline`) — アカウント毎の CICD 本体。**主対象**
  - 共通系: account-request / account-provisioning-customizations 等
- パイプライン名はアカウント ID ベース → 人間が読むには**アカウント名⇔ID の解決が必須**
- パイプライン内の CodeBuild で terraform が実行され、**terraform のログは CodeBuild ログに出る**
- 関連リソース（将来スコープ）: DynamoDB (aft-request 系テーブル)、Step Functions（プロビジョニングフロー）、SSM パラメータ

## 4. 規模・スケール要件

- 想定規模: **数百アカウント規模**（= 同数の customizations パイプライン）。今後増加見込み
- OSS として、**より大規模な環境（数百〜1000 超）** でも実用に耐える設計とする
- アカウント数に対して線形以下の操作感を目指す（キャッシュ + 並列制御で吸収）

## 5. 機能要件

### F1. パイプライン状態の一覧確認【最優先】

- アカウント別パイプライン全件の状態（Succeeded / Failed / InProgress / Stopped 等）を一括取得・一覧表示
- アカウント名・アカウント ID・最終実行時刻を付与
- フィルタ: ステータス、アカウント名/ID の部分一致 等
- 「適用できているか」の一覧判定: 最新実行が Succeeded かどうか

### F2. パイプライン詳細の確認

- 特定パイプラインの実行履歴・ステージ/アクション単位の状態 → **どこでこけているか**の特定
- 失敗アクションに紐づく **CodeBuild ログ（terraform ログ）の閲覧**
  - 一覧でざっと見るケースと、個別に深掘りするケースの両方をサポート
  - 例: terraform plan/apply の要約・エラー箇所の抽出（AI フレンドリー出力と親和）

### F3. パイプライン操作

- 単発の Release change（StartPipelineExecution）
- 実行中パイプラインの停止（StopPipelineExecution）※要否は設計時に確認
- 対象選択: 名前指定 / フィルタ結果（例: Failed 全件）/ 明示リスト（ファイル・標準入力）

### F4. 逐次バッチエンジン

数百件規模の取得・実行を支える共通基盤。AFT 標準の Step Functions ベース全体適用より**軽量・高速**な代替を目指す。

- **並列 × 直列の組み合わせ**: チャンク単位の逐次処理 + チャンク内 N 並列
- **並列度をバッチごとに制御可能**（設定 + コマンドラインで上書き）
- Throttling 検知時の指数バックオフ・リトライ
- **API 呼び出しの計測機構**: 呼び出し回数・レイテンシ・throttle 発生率を記録し、最適な並列度を実測ベースで分析できるようにする（要件 3 の「実装しながら高精度に分析したい」に対応）
- 進捗表示（N/総数 完了、失敗件数 等）

### F5. 実行系操作の安全ガード

- dry-run モード（対象一覧を表示するだけで実行しない）
- 実行前の確認プロンプト（対象件数の明示）、`--yes` での省略
- 上限件数ガード・中断 (Ctrl-C) 時の安全な停止

### F6. キャッシュ

- 変更頻度の低い情報を TTL 付きでローカルキャッシュ:
  - アカウント一覧（名前⇔ID マップ）
  - パイプライン一覧（存在情報）
- キャッシュの明示的更新 (refresh) / クリア / 状態表示
- キャッシュ利用有無を出力上で判別可能に（stale なデータで誤判断しないため）

### F7. アカウント名⇔ID 解決

- Organizations（または AFT の DynamoDB/SSM）からアカウント情報を取得し、双方向解決
- 全機能の表示・フィルタで利用（例: アカウント名でパイプラインを指定できる）

### F8. インターフェース（2 系統・同一コア）

**CLI（AI/自動化フレンドリー）**
- `コマンド → 出力` の単発実行
- 出力形式: 人間向けテーブル + 機械向け JSON（`--output json` 等）を全コマンドで一貫サポート
- exit code 規約（例: 0=正常、1=対象に失敗あり、2=ツールエラー）
- パイプ・リダイレクト前提の挙動（TTY 検知で装飾の自動 off 等）

**TUI（対話操作）**
- 一覧 → 絞り込み → 詳細（ステージ/ログ）→ 操作（Release change）のフロー
- 数百件リストの快適なナビゲーション（インクリメンタル検索・ソート）
- 実行中パイプラインの状態ウォッチ（自動更新）

### F9. AFT リソース全般（Phase 2 以降の拡張点）

初期設計で拡張点として構造を確保する（実装は後続フェーズ）:

- account-request の状態確認（DynamoDB）
- プロビジョニングフロー（Step Functions 実行状態）
- 共通系パイプラインの状態
- AFT メタデータ（SSM パラメータ）の参照

### F10. パイプライン trigger のドリフト検出【read-only】

#### 背景: AFT には plan → 承認 → apply のゲートが無い

AFT の customizations パイプラインは **Source → Global-Customizations(Apply) →
Account-Customizations(Apply)** の 3 ステージだけで、CodeBuild の buildspec は
`terraform apply -no-color --auto-approve` を直接実行する（AFT 1.21.1 のテンプレートで確認）。
plan ステージも手動承認アクションも存在しない。IaC 適用としては一般的な
「plan を人が読んでから apply」というパターンが、AFT には**構造として無い**。

これは上流でも長年の要望であり、現時点で未提供:

| issue / PR | 内容 | 状態 |
|---|---|---|
| [#153](https://github.com/aws-ia/terraform-aws-control_tower_account_factory/issues/153) | The Change Management - Approval Process（本命の tracking issue） | **2022-05 起票・open のまま** |
| [#302](https://github.com/aws-ia/terraform-aws-control_tower_account_factory/issues/302) | account-request に plan + approval ステージを | #153 の重複として close |
| [#481](https://github.com/aws-ia/terraform-aws-control_tower_account_factory/issues/481) | Add plan and approval stage for CodePipeline | 2024-08 起票・open |
| [#624](https://github.com/aws-ia/terraform-aws-control_tower_account_factory/pull/624) | apply-with-approval workflow type の PR | **unmerged で close** |
| [#636](https://github.com/aws-ia/terraform-aws-control_tower_account_factory/issues/636) | fork ベースの承認ゲート PoC | 2026-07 公開。上流は現状 community contribution を受け付けない旨が明記されている |

#### 補完策と、そこで trigger が担う役割

このゲートは customizations リポジトリ側の CI（GitHub Actions 等）で補完できる。
パイプラインの buildspec と同じ backend / provider 構成を組み立てて PR 上で
`terraform plan` を実行し、結果をレビューして approve → merge する。
**merge が apply の承認点**になり、merge をもってパイプラインが起動して apply が走る。

この形が成立する前提が **merge で当該アカウントのパイプラインが自動起動すること**、
すなわち push trigger である。ところが AFT のテンプレートは trigger を宣言せず、
両ソースアクションは変更検知を切っている（`DetectChanges` / `PollForSourceChanges` = false）。
したがって trigger は **out-of-band で付けるほかなく、`aft-create-pipeline` が再実行されると消える**
（AFT アップグレード時・ソース接続の作り直し時にフリート全台で起きる）。

trigger が消えたときに起きるのは**失敗ではなく無反応**である。PR で plan をレビューして
merge しても apply が走らず、CI は成功し、パイプラインは前回の状態のまま何も言わない。
気づく手段が無い。

なお上流の設計思想では「customizations パイプラインは手動起動が前提」とされている（#153 の議論）。
自動 trigger を足すというのは、その前提から外れて **PR の plan ゲートに承認点を移す**構成への
乗り換えを意味する。つまりこの trigger は利便性ではなく**統制の一部**であり、
その消失は統制の穴になる。ドリフト検出が要る理由はここにある。

#### 要件

- フリート全体について、各アカウントのパイプラインが**期待する push trigger を持つか**を
  read-only で判定できること。有無だけでなく**内容の一致**（ブランチ・ソースアクション・
  監視パス）まで見ること — trigger はあるが別ディレクトリを見ている状態は、無いのと同じく発火しない
- **期待値はアカウント毎の設定として持たない。** AFT のメタデータから導出する。
  数百件を設定ファイルに書かせない、かつ期待値が AFT 自身の記録からずれないこと
- **判定できなかったものを「正常」として報告しないこと**（§6「silent failure 禁止」の一形態）
- 定期実行で異常を検知できること（exit code でドリフトを表明する）
- **trigger の設定（書き込み）は本ツールの範囲外。** 恒久化はパイプラインの作られ方そのものを
  変える話（AFT 本体の fork 等）であり、外部からの reconcile ではない。両方を持つと
  trigger の管理主体が二重になる

## 6. 非機能要件

| 項目 | 要件 |
|---|---|
| 性能 | 数百件規模の状態一覧をキャッシュ併用で実用時間内に取得（目標値は計測機構導入後に設定） |
| レート制御 | Throttling を前提とした適応的制御。既定並列度は保守的に（実測ベースで throttle が出ない範囲）、計測に基づき調整可能 |
| 信頼性 | リトライ・部分失敗の明示（どのアカウントの取得に失敗したか）。silent failure 禁止 |
| 出力境界 | 人間向け（テーブル/TUI）と機械向け（JSON）の明確な分離。JSON スキーマの安定性 |
| 設定 | AWS プロファイル・リージョン・並列度・TTL 等を設定ファイル + 環境変数 + フラグで指定（優先順位を規定） |
| 移植性 | macOS / Linux をサポート（開発環境は macOS/zsh）。シングルバイナリ |
| OSS 品質 | 組織固有値のハードコード禁止、README/使用例整備、ライセンス（MIT 想定）、CI（テスト・リリース） |
| セキュリティ | 認証情報を扱わない（AWS SDK の標準クレデンシャルチェーンに委譲）。キャッシュに機微情報を含める場合の扱いを設計時に規定 |

## 7. 環境・制約

- 実行環境: 運用者のローカル端末（macOS / Linux）。AWS SSO プロファイル等による認証
- 対象環境: AFT 管理アカウント（Control Tower Account Factory for Terraform の管理アカウント）
- 書き込み操作（Release change 等）には、対象 AFT 管理アカウントで StartPipelineExecution 等を
  実行できる権限を持つプロファイルを使用する。プロファイル・リージョンは設定で指定（§6 参照）

## 8. フェーズ計画（案）

| Phase | 内容 |
|---|---|
| 1 | コア層 + CLI + TUI の骨格 / F1 状態一覧 / F3 単発 Release change / F6 キャッシュ / F7 アカウント解決 |
| 2 | F2 詳細・CodeBuild(terraform)ログ / F4 逐次バッチ + 計測機構 / F5 安全ガード / F10 trigger ドリフト検出 |
| 3 | F9 AFT リソース全般（account-request / Step Functions / 共通系パイプライン） |
| 4 | OSS 公開整備（英語ドキュメント・CI/リリース・ライセンス） |

※ Phase 1/2 の切り方は設計時に再調整。「一番すぐに欲しい機能」= F1 + 単発 Release change を最短で使える状態にすることを優先。

## 9. 未決事項

| # | 事項 | 備考 |
|---|---|---|
| ~~U1~~ | ~~書き込み操作用プロファイル/ロール~~ | **解決済**: 設定で指定する AFT 管理アカウント用プロファイルを使用（§6 / §7） |
| U2 | 性能目標の数値化 | F4 の計測機構で実測してから設定 |
| U3 | ライセンス・公開リポジトリの場所 | MIT 想定。個人 repo か org repo か |
| U4 | ドキュメント言語 | OSS なら英語基本 + 日本語併記? |
| U5 | StopPipelineExecution 等、Release change 以外の操作の要否 | 設計時に確認 |
| U6 | コマンド名 | `aft-ops` 推奨（`afto` 案もあり） |
