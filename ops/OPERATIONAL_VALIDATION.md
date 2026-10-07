# SUZUME 実機検証記録（2026-10-07 UTC）

管理APIは `OPSA_UPDATE_ENABLED=false`。本番用7 hookのbackendは未配置。
今回の管理者CLIは公開HTTPのupdate hookに接続しない。
稼働版と準備済み固定版はともにV3.3.0 b18 / Steam build25661908であり、
同じ版への更新は `ALREADY_CURRENT` になる。

## 固定資産

準備ID `b2684acae8b0495d8afa700c4cd28b2c`。元container/image/Composeと本番mountを維持した。
通常entrypointはゲーム・MOD更新を伴うため、復帰は検証済みLinuxGSM v24.2.1の直接startを使った。

| 資産 | 実測結果 |
|---|---|
| 固定復旧image | `sha256:efd9920b0ba84b8d9f90cdae053e2fa115acc248dc41b07f7491433f067b5673` |
| runtime-image.tar | 510,636,032 bytes / SHA256 `6b770f57ce8255688e8d680ac7e8da214271a2ef2828b7ee171cd429ef911091` |
| target-serverfiles.tar | 17,737,216,000 bytes / SHA256 `16ff81524ff08a15f80760e9ecffb8a73d4e998b6289e15f7ac01f4d4393f599` |
| ServerFiles復元 | 新規私有領域へ展開し、内容・uid/gid・mode・ACL・xattrをGNU tar比較 |
| MOD/build | 準備時の全MODファイルhashとbuild25661908を照合 |
| 隔離用image | `sha256:eb44922802c9f167474147e18cc93906b3728776d527efbcf9a1890582a8d369` |
| 隔離用image archive SHA256 | `be20491203c5ce11ffcc7f2499c5afaba80d3ff4e35c717b5930f168308afff0` |

元の固定image/archiveを保存し、隔離用はOCI configのみ変更した。全layerと静的runtimeの同一性を検証。
VOLUME/EXPOSE/Composeと `desktop.docker.io/` のbindラベルを除去し、作成後・起動前に全mountを検査する。
network=noneでEOS接続を省くため、V3.3.0 b18の実metadataで確認した
`Local / None / LAN(server-only)` のplatform.cfgを試行用read-only bindにした。
元の `Steam / EOS / Steam,XBL,PSN,LAN` は維持した。このオフライン構成はSteam/EOS/VPNの復旧証明ではない。

## 一回の限定保守と静止コピー

0人でもオンラインの `main.ttw / main.ttw.bak` は変化し、最初の比較はゲーム起動前に中断した。
その未検証tarを復旧可能なワールドと認めず、`check-start` は固定launcherの起動確認だけに使った。
その後、明示承認された一回の保守を実行した。

1. 既存認証APIの `/serverstats`・`/player` と直前console `version/lp` で正確に0人を確認。
2. 管理APIを一時抑止。永続予約・既知MOD release flockを保持し、cronを同一PID/start tokenで一時停止。
3. 固定telnet `shutdown` を一回だけ送信。pane/gameの親子関係・PID/start tokenを照合し、終了コード0・EOF・writer不在を確認。
4. ワールド/configを静止保存し、sourceとGNU tar比較。
5. 元containerで固定GSM start。期待版とAPI2系統の復帰を確認し、cron・tmux設定の継承状態・管理APIを復帰。

| 静止コピー | 証拠 |
|---|---|
| ファイル | `limited-maintenance/quiescent-world-config.tar` |
| サイズ | 1,415,331,840 bytes |
| SHA256 | `0fb1cf59cd0a9da970d5ddfdfb84c36380abd2e863daed203bfb59f371de4fe0` |
| 完了記録 | `upgrade-backups/limited-maintenance-reservation/<準備ID>.completed.json` |
| 終了・整合性 | `gameExitCode:0 / writerAbsenceVerified:true / sourceQuiescent:true` |
| 復帰 | `phase:completed / gameReturned:true / recoveryRequired:false` |

復帰後game PID88538、元cron PID48、元VPN PID10、元container・固定資産・MOD・設定を確認。
ワールドday848、管理API正常、人数2系統とも0を照合した。
通信遮断・SteamCMD取得・payload適用・Docker再起動は行っていない。
人数確認からshutdownまでの参加競合を完全に遮断したとは主張しない。
この一回の保守承認を無人更新やネットワーク変更の承認に置き換えない。

## 静止コピーの隔離復元

`rehearse-quiescent` は同一IDの正常完了・復帰記録とsize/hashを要求する。
非0終了・writer不明・未復帰・予約残存・コピー変更・曖昧なworld・外部save pathを拒否する。
ServerFilesも新規私有領域へ展開し、world/configとGNU tar比較。
全6 bindが試行領域を指すことを起動前に確認し、network=none・公開ポートなし・6GiB/2CPU・restart=noで起動。
versionだけで成功にせず、保存済みday848と0人を待つ。起動180秒・試行container停止90秒上限。
削除失敗は `cleanupRequired:true` と元エラーを永続receiptに残す。

実機の隔離復元は成功し、識別済みcontainerを削除して完了した。

| 隔離起動結果 | 証拠 |
|---|---|
| 対象 | `SUZUME3.0 / RWG / New Dedave Territory` |
| 保存main.ttw（起動前） | SHA256 `dc1c5e1a5f6649619dc063b9541d01ccb73d1b466f192b1f8ae24bdf28f007a7` |
| config | LGSM必要設定5ファイルが静止tarと一致、ServerFiles XMLを含むpayloadも比較成功 |
| 起動 | V3.3.0 b18 / `Day 848, 20:17` / 0人 |
| 個別証拠 | `sourceQuiescent:true / cleanExitVerified:true / worldConfigCopyRestored:true / serverFilesCopyRestored:true / modsHashMatched:true / savedWorldBootVerified:true` |
| 後片付け | `trialRemoved:true / cleanupRequired:false` |
| receipt | `<準備領域>/quiescent-recovery-trial/receipt.json` |

隔離container ID `123ebde8ae5c1c288fac02efa08fdddd0e10fd05c4fd974f3a0c397fc40f1758`。
GNU tar比較後のコピーとログは私有領域へ証跡として残し、本番/元固定資産へ書き戻していない。
実行後の既存認証APIのversion・人数2系統が再度正常だった。
追加照合で本番game PID88538・cron PID48・VPN PID10と元container起動日時が変わらず、
restart count0、残存試行container0、元設定・資産のhash一致を確認した。

7つの宣言MODの `Loaded Mod` 記録を確認したが、MODの完全初期化・互換性は成功扱いしない。
隔離consoleにはERR/EXCが10行あり、そのうちTrailwatch/Harmonyの
`Undefined target method` 例外を検出した。本番consoleにも同じ例外がある。
コード識別子は `TrailwatchBridge.QuestObserverInstaller.InstallPatchClass / TrailwatchBridge.QuestStartPatch`。
本番と隔離でMOD hashとゲーム版は同一である。生ログ・資格情報は出力していない。
この観測は保存worldの起動成功を否定するものではないが、MOD互換性や更新有効化の証拠には使えない。
親へこのエラーを引き継ぐ。別セッションのTrailwatch checkoutは変更していない。

## テストと限界

実装commit `a9592f588d86601d5988ed0712ebeb742dab3538` の
[Linux CI](https://github.com/masahide/mackerel-7dtd/actions/runs/37565102852) は成功。
Python81件（Linux skip0）、Go APIテスト、update race、OpenAPI生成差分・snapshotを検証。
Windowsは81件中29件がPOSIX/GNU tar対象としてskip、52件が成功。
模擬試験を本番updateや外部VPN復旧の実績に数えない。

全scopeは約82.16GB（履歴backups約62.97GB、活動資産約19.20GB）。静止tarは `7DaysToDie / LGSM-Config` のみ。
履歴backups/log、外部OpenVPN bind、元のVPN・認証・公開経路を含む独立runtime復元、
MODの実ゲーム互換性、異なる更新先への適用は未実証。
`fullWorldBackup:false / verified:false / runtimeRestored:false / productionEnabled:false` を維持する。
実測のない保守時間や完全復旧を保証しない。[次工程](RECOVERY_REHEARSAL_PLAN.md)を参照。
