# SUZUME 更新 API

この実装は更新計画、明示確認、永続的な非同期ジョブ、進捗・結果照会を提供する。
既定では無効。本番の SteamCMD / コンテナ更新コマンドは含めていない。

調査基準は main `a0e3a0143678a6e172b2623e2d05d29a63d52489`。
`main.go` の起動・状態取得は SSH + Docker Compose、停止の既定値は systemd で、
この組み合わせを更新用に流用する根拠は確認できなかった。
README にも Compose 定義、SteamCMD の配置、セーブの実パス、更新・復元手順はない。
読み取りで確認した TrailWatch の `docs/deployment.md` / `docs/integrations.md` にも
固定された status/logs/restart/start だけが記載され、更新手順はない。
そのため更新専用の管理者固定 hook を必須にし、実パス・コマンドを推測していない。

## HTTP 契約

既存の Bearer / X-API-Key 認証を使用する。`ALLOW_NO_AUTH=true` では更新機能を有効化できない。
POST は `Content-Type: application/json` 必須。未知の JSON フィールド、クエリ、2 KiB を超える入力を拒否する。
クライアントは対象版、コマンド、パスを指定できない。

| 操作 | 入力 | 成功 |
|---|---|---|
| `POST /server/update/plan` | `{}` | `200 {data: UpdatePlan}` |
| `POST /server/update/jobs` | `{planId, confirmation:"UPDATE SUZUME", idempotencyKey}` | 新規 `202`、同一要求の再送 `200`、ともに `{data: UpdateJob}` |
| `GET /server/update/jobs/{jobId}` | パスの ID | `200 {data: UpdateJob}` |
| `GET /server/update/jobs/latest` | なし | 最後のジョブ。未作成 `404` |

計画は5分間有効で、API再起動時には未使用計画を失効する。ジョブと冪等キーは保持する。
`currentVersion` / `targetVersion` / `resultVersion` は正規化した `Game version: V ...` の1行。
管理者設定の `UPDATE_TARGET_VERSION` は version 結果全文またはその1行を受け付ける。
build と Compatibility Version を含む行を厳密比較し、空白、CRLF/LF、MOD行の順序を除外する。
MOD互換性は運用hookで別途検証する。複数のGame version行や不明な版は拒否する。
API は Steam 最新版を調べず、管理者が承認した固定対象への更新だけを扱う。

`UpdatePlan` は `planId, createdAt, expiresAt, currentVersion, targetVersion, onlinePlayers,
canExecute, blockers, confirmation, steps`。
`blockers` / `steps` は `string[]`、版は `string`、人数は `number` で null を返さない。
不明な情報は `502 PRECHECK_FAILED`。blockers は `PLAYERS_ONLINE` / `ALREADY_CURRENT`。

`UpdateJob` は `jobId, planId, status, phase, createdAt, updatedAt, currentVersion, targetVersion,
recoveryRequired`、任意で `finishedAt, resultVersion, backupId, error`。
日時は RFC3339 UTC。`error` は `{code,message,details?}`、hook の失敗では details に
`exitCode` / `timedOut` を含む。コマンド、stdout/stderr、資格情報を返さない。

status は `queued / running / succeeded / failed / interrupted`。
phase は `queued / checking / stopping / checking_stopped / backing_up / updating / starting /
verifying / completed`。工程を表し、時間やダウンロードの割合ではない。
照会を2～5秒間隔で行い、終端状態で止める。`failed` を成功として扱わない。

`202` の `Location` が照会先。HTTP 接続や通常の API タイムアウトから独立してジョブを継続する。
サーバーが停止中も API のジョブ照会は使用できる設置構成が必要。
受付応答が不明な場合は**同じ要求と同じキー**を再送する。
キーは `[A-Za-z0-9_-]{16,128}`、UUID などを使用し、対象ごとに生成して保持する。
同一キー・同一要求は期限後や API 再起動後も元のジョブを返す。新規更新を無効化しても、正常に読み込めた過去の受付要求は同じジョブを返す。
異なる内容でのキー再使用は `409 IDEMPOTENCY_CONFLICT`。
別キーでも同じ計画から2回のジョブは作られない。再試行には新しい計画と再確認が必要。

HTTP エラーは既存 `ErrorResponse`。

| HTTP | code |
|---|---|
| 400 | `INVALID_REQUEST`, `CONFIRMATION_REQUIRED` |
| 401 | `UNAUTHORIZED` |
| 404 | `PLAN_NOT_FOUND`, `JOB_NOT_FOUND` |
| 409 | `UPDATE_BUSY`, `PLAN_EXPIRED`, `PLAN_BLOCKED`, `IDEMPOTENCY_CONFLICT`, `VERSION_CHANGED`（設定対象の変更） |
| 502 | `PRECHECK_FAILED`（計画作成） |
| 503 | `UPDATE_UNAVAILABLE`, `UPDATE_STATE_UNAVAILABLE` |

受付後の人数増加、版の変更、確認不能はジョブの `error.code` に `PLAYERS_ONLINE`,
`VERSION_CHANGED`, `PRECHECK_FAILED` を記録する。HTTP 受付が `202` でも更新成功ではない。
その他は `STOP_FAILED`, `STOP_NOT_VERIFIED`, `BACKUP_FAILED`, `BACKUP_NOT_VERIFIED`,
`UPDATE_FAILED`, `START_FAILED`, `VERIFY_FAILED`, `UPDATE_TIMEOUT`, `UPDATE_INTERRUPTED`,
`UPDATE_STATE_UNAVAILABLE`, `INTERNAL_ERROR`。失敗工程は phase に残る。

## 安全条件と hook 契約

計画時と実行直前にゲームの `/api/command`（固定 `version`）、`/api/serverstats`、
`/api/player` を照会する。2つの人数ソースが一致し、オンライン人数0のときだけ停止へ進む。
人数フィールド欠落・null・不一致・接続失敗を0として扱わない。現在版が計画から変わった場合も停止しない。

すべての hook は管理者が設定する固定コマンドで、既存実行方式に合わせて Linux `sh -c` で実行する。
入力を連結しない。終了コード0だけを成功とする。バックアップ以外の出力は結果に使用しない。
出力収集は2,049 bytesまで。詳細ログは hook が保護された運用ログに記録する。
認証情報をコマンドに埋め込まず、管理されたファイル・環境・SSH鍵で供給する。

| 環境変数（すべて `OPSA_` 接頭辞） | 必須の動作 |
|---|---|
| `UPDATE_PREFLIGHT_CMD` | 読み取り専用。固定対象の取得可能性、容量、完全バックアップと復元手順、権限、外部操作の排他を確認。計画時15秒以内、ジョブ時はジョブ期限以内で終了。 |
| `UPDATE_STOP_CMD` | 新規接続を閉じ、人数を再確認し、不明/非ゼロなら更新を許可しない。セーブを完了して同じ対象を正常停止。後続hookまで維持できる運用側の保守フェンスも必要。 |
| `UPDATE_CHECK_STOPPED_CMD` | 対象ゲームプロセスが停止し、セーブ等の書き込みがないことを検証。Composeの短いコマンド成功だけで判断しない。 |
| `UPDATE_BACKUP_CMD` | 停止後の整合した saves/worlds、設定、MOD、元の実行ファイル/イメージ・Compose識別情報を保全し、復元可能性と整合性を検証。stdout は下記JSONのみ。 |
| `UPDATE_APPLY_CMD` | 確認済みの実運用方式で固定対象に更新。MODと互換性、固定Steam build/branchやimage digestを遵守。最新への無条件追従は不可。失敗を必ず非ゼロで返す。 |
| `UPDATE_START_CMD` | 同じ対象を起動。ゲームAPIで期待版/人数を確認できる構成を維持。保守フェンス解除は運用側の成功確認に合わせて実施。 |

バックアップ stdout の例（実際のファイルパスは返さない）:

```json
{"backupId":"backup_0123456789abcdef","verified":true}
```

backupId は16～128文字の英数字、`_`、`-`。`verified:true` は運用hookの検証済み宣言。
Go側はその構造とフラグを確認するが、バックアップファイルの実内容を検証する実装ではない。
復元のリハーサルと保全対象確認なしで有効化してはいけない。
バックアップ失敗・未検証時には更新コマンドを呼ばない。

API内では plan確認、start/stop/restart/command、更新ジョブが同じ操作ロックを使用する。
`UPDATE_ENABLED=false` は新規更新だけを無効化する。`UPDATE_STATE_DIR` が設定されていれば、
無効化時や設定不備時も状態の読込・OS所有権・復旧ロックを維持し、過去の保守状態を迂回できない。
保守中に状態ディレクトリの設定を外したり別の場所へ変更してはいけない。
同じ永続ディレクトリを2つの API プロセスが使用することも OS ファイルロックで拒否する。
TrailWatch、手動SSH、別ディレクトリを使うAPI、他の自動更新とはこのロックを共有できない。
本番配置前にそれらも保守フェンスを尊重する仕組みを用意し、更新中の再起動や新規参加を防ぐ必要がある。
この条件が未確認の間は機能を無効に保つ。

## 状態の保存と復旧

`UPDATE_STATE_DIR` はゲームの更新・再起動に巻き込まれない絶対パス。Linuxでは運用APIユーザーだけが
書き込める永続ディレクトリにする。journal は一時ファイルへの書き込み、fsync、rename、directory fsync
の順で保存する。受付・工程を保存できなければ次の副作用に進まない。
過去ジョブ・キーは削除しない（10,000ジョブで新規受付を拒否し、管理者の保管作業を要求）。
有効な未使用計画は最大128件。期限切れ計画は新計画作成時に整理される。
Windows実装はテスト用で、ディレクトリfsyncは行わない。本番対象はLinux。

停止を試みた後の失敗は `recoveryRequired:true` として、更新と既存変更操作を拒否し続ける。
失敗した SSH / シェルの終了やタイムアウトは、リモート処理が確実に終了した証拠にはならない。
API再起動時の queued/running ジョブは interrupted に変更し、勝手に再開・再起動・復元しない。
成功時のみ期待版を resultVersion に記録して succeeded にする。

運用者はAPIを停止し、リモートの残存処理、ゲーム状態、保守フェンス、バックアップを確認する。
必要なら互換性を保った元の実行ファイル/イメージ・MOD・設定・セーブを一式復元する。
新しいゲーム版で書き込まれたセーブを旧版へ単独で戻す操作は自動化していない。
復旧後に正常版、保存状態、人数APIを検証してから、保管した journal.json の該当終端ジョブの
`recoveryRequired` を offline で false に変更する。status/error/ID/keys は保全する。
APIを再起動し latest を照会して確認する。journal を削除すると冪等性を失うため、削除による解除は禁止。
owner.lock は OS 管理のリースで、ファイル削除による解除は禁止。
復旧確認をAPIから無条件に解除するエンドポイントは公開していない。

## 配置前に必要な確認・承認

1. 実行中の別更新セッションを完了させ、稼働版と更新方式、対象ホスト/サービス、セーブ・MOD・バイナリの実配置を確定する。
2. 運用hook、完全バックアップ/復元、外部排他、新規接続防止を実資料に沿って用意し、模擬・復元環境で検証する。
3. `UPDATE_ENABLED=false` のまま API バイナリを作成・レビューする。本番配置は別途承認を取得する。
4. API専用の永続ディレクトリ、実行ユーザー・SSH権限、EnvironmentFile、固定対象版を承認のうえ設定する。
   `UPDATE_TIMEOUT` は既定60分、`UPDATE_VERIFY_TIMEOUT` は既定5分。
5. サイトの専用サーバー側proxyから既存認証を付けて接続する。秘密をブラウザへ渡さない。
   本番更新APIの配置・到達性・認証を確認できるまで、404/503等を実行不可として表示する。
6. 承認後に `UPDATE_ENABLED=true`。最初の実更新にも計画画面で明示確認が必要。

今回の作業では本番の資格情報・権限・ネットワーク・サービス設定・ゲームサーバーを変更していない。

## 検証

`go test ./...`, `go vet ./...`, `go build ./...`。
`update_test.go` は専用のHTTPゲームモックと偽コマンドだけを使い、実サーバー・SSH・SteamCMDを呼ばない。
成功、各工程失敗、未検証バックアップ、人数不明/変化、版変化、確認不足、同時重複・キー競合・計画再使用、
既存操作との排他、プロセス排他、途中終了、永続書込み失敗、起動後版不一致、認証と未設定拒否、OpenAPI整合を検証する。

今回のWindows環境では上記3コマンドとLinux amd64 API/テストバイナリのクロスコンパイルが成功。
更新関連の22テスト関数とそのケースを実行した。ローカル `go test -race` は gcc 不在により実行できない。
Linuxでのrace検査はCIに追加したが、pushしていないためまだ実行していない。
oapi-codegen は既存の OpenAPI 3.1 使用に対する警告を出すが、生成とスキーマ検証・スナップショットテストは成功する。
