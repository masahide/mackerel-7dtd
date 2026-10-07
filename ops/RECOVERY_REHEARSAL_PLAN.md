# 更新機能の有効化条件と次工程

2026-10-07 UTC。承認された一回の停止・静止コピー・元ゲーム復帰は完了した。
コピーの隔離復元は本番を再停止せず進められる。
管理画面はAPIの `canExecute / blockers / recoveryRequired` を反映し、証明が不足する更新を実行可能と表示しない。

## 既存承認内で完了できる作業

- 完了記録・SHA256・sizeを照合し、コピーだけを新規領域へ展開してGNU tarと全MOD/buildを検証。
- 全mount検査後、公開ポートなし・network=noneで保存済みday848を起動し、識別済み試行containerだけを削除。
- 本番game/cron/VPN/APIが稼働を続けた証拠を残し、PR #6のレビューと既存API契約での画面検証を進める。

同名試行領域を上書きせず、再試行前には永続receiptと実containerを確認する。
完成した静止tarを、稼働中worldとの比較結果で上書き・取り直ししない。

## 有効化までの具体的条件

| 条件 | 現状 | 必要な証拠・判断 |
|---|---|---|
| 新しい更新先 | 稼働/固定版ともV3.3.0 b18、build25661908 | 新版/build・MODを明示承認、事前取得した固定payloadをhash検証。準備と本番適用は別段階 |
| 完全backup | 静止world/configとServerFiles/imageのファイル検証まで | 履歴backups/log・全bind・外部VPN設定・container metadataを含むscopeを決定し、独立コピーで復元照合 |
| 元runtime復旧 | 元containerを維持した固定startは実証済み | container喪失時の再構築、Steam/EOS・OpenVPN・認証経路と必要設定の復元確認 |
| MOD互換性 | 固定版全ファイルhash、7MODのLoaded記録と保存world起動を確認。本番・隔離双方でTrailwatch QuestStartPatchのUndefined target method例外あり | 既存Trailwatch担当へ例外を引き継ぎ、対象methodとgame版の互換性・実ゲーム動作を検証。Local/LAN起動を完全MOD互換性証明にしない |
| 排他 | APIのlease/idempotency、限定保守予約・既知release flockを実証 | API・cron/monitor/backup・release・手動操作すべてが同じ予約を尊重するbackendと運用手順 |
| 参加競合 | 一回の保守で直前API2系統/console0人を確認 | 人数確認からshutdownまでの方針を決定。現Workflowのfence要件を満たすか、承認された代替設計を実装・模擬検証 |
| 本番hook | コアと管理者CLIのみ、全7hook backend未配置 | 固定backendを実装して失敗/中断/重複/復帰不能を検証。API自身を停止する保守coordinatorを自己実行hookに流用しない |
| 配置 | 管理APIは無効化 | レビュー済みcommit/binary、固定hook配置/実行ユーザー/権限、私有state dir、target・timeoutを具体化して親へ提出 |

ユーザーは今回通信遮断を不要とした。fence証拠を偽造して無人Workflowを通さない。
新しいnetwork/VPN経路・資格情報・権限、ゲーム再停止/更新適用、API有効化が必要な工程は、具体的対象と影響を親へ報告して承認を待つ。
今回の既存root SSH/私有CLIを使う保守に新しい資格情報・firewall変更はなかった。

## 配置レビューへ渡す内容

公開契約は `POST /server/update/plan`、`POST /server/update/jobs`、`GET /server/update/jobs/{jobId}` と `/latest`。
ジョブ入力は `planId / confirmation:"UPDATE SUZUME" / idempotencyKey` のみ。
任意shell/path/target URLをクライアントから受け付けず、既存認証を維持する。

配置案では `OPSA_UPDATE_ENABLED=false` の状態で固定hookを検査し、`OPSA_UPDATE_TARGET_VERSION / OPSA_UPDATE_STATE_DIR`、
各hookの配置・実行ユーザー・timeoutを明記する。環境変数名は `apiserver7dtd/main.go` のConfigを正とする。
任意 `OPSA_UPDATE_FINISH_CMD` はAPIの新版確認後に実行し、失敗時は予約と `recoveryRequired:true` を維持する。
既存配置済みbinaryにはこのPRのfinish拡張をまだ反映していない。PR merge/本番有効化は今回の作業に含めない。

APIのbackup receipt `{backupId,verified:true}` は完全復元証拠が成立した場合だけ返す。
今回の `sourceQuiescent:true / savedWorldBootVerified:true` は個別の事実であり、API receiptや `runtimeRestored:true` に変換しない。

## 中断・復帰

限定保守は実行済みなので `collect` を繰り返さない。private `status <準備ID>` で永続状態とworkerを読み取り、
`completed / gameReturned:true / recoveryRequired:false` と実game/cron/APIを照合する。
新たな異常では保存状態・PID/start token・固定資産を先に確認し、無関係processをkillしない。

未復帰の限定保守のprivate `recover` は同じ予約だけを対象に固定復帰する。
正常終了不明ならpayloadへ進まず、元containerの通常restartもしない。
通常entrypointには最新版/MOD取得が含まれるため、復帰は検証済み固定GSM startと設定確認が必要。
元container喪失は今回未検証の独立runtime復元に該当する。

将来のupdate失敗でも自動rollback/盲目的再試行をせず、実game状態・save進行・backup時点・復元証拠をoperatorが確認する。
保守時間は実測から範囲を提示し、約82GB全scopeの保存/展開やVPN復元の所要時間を推定だけで約束しない。
