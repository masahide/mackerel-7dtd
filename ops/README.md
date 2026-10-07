# SUZUME 更新運用コア（未有効化）

更新APIはPR #5でmainにマージ済み。実機管理APIはcommit
`01a037b7f4dc69f25661826741f871548f9636a6`、`OPSA_UPDATE_ENABLED=false`。
追加adapterは本番update hookとして配置していない。
稼働版と準備した固定対象はともにV3.3.0 b18 / build 25661908であり、
同じ版への更新は `ALREADY_CURRENT` になる。

`runtime_freeze.py` は観測済みホスト/root/container/image/Composeのみを扱う管理CLI。
稼働コンテナを停止せず、一度だけ `--pause=false` で固定imageを作り、
別imageで起動時更新・MOD更新・monitor・backupを無効にする。
別の読み取り専用コンテナで静的runtimeを比較し、image exportとServerFiles tarの
hash、Steam build、全MODファイルhashを検証する。
`stage-copy` は新規の私有ディレクトリに展開しGNU tarで比較する。
不確かな既存ディレクトリは再利用しない。

実機で固定資産の準備とServerFilesの展開比較まで完了した。
`runtime_rehearsal.py` の隔離起動は0人でもワールドが書き換わるためコピー比較で中断した。
隔離コンテナは作成されていない。[実機記録](OPERATIONAL_VALIDATION.md) と
[次段階](RECOVERY_REHEARSAL_PLAN.md) を参照。
全資産は `verified:false / runtimeRestored:false / productionEnabled:false` のまま。

## 観測した既存運用

ホスト `7dtd01`、Compose `/home/masahide/work/7dtd/docker-compose.yml`、service `7dtdserver`。
ID・構成は `suzume-profile.observed.json` にあるが有効な配置設定ではない。
entrypoint `/home/sdtdserver/openvpn.sh` はVPNとuser.shを起動する。
`START_MODE=3 / VERSION=stable / UPDATE_MODS=YES / CPM_UPDATE=YES` により
普通のDocker/Compose再起動は最新ゲーム・MODを取得する。固定復帰に流用しない。

LinuxGSM launcher/modulesはともにv24.2.1、現在configは `updateonstart=off`、
必要configとsystem SteamCMDは存在する。直接GSM startはentrypointを通らない。
stopmode=8はtelnet失敗後にtmux killへ進むため、終了コードだけでclean stopと認めない。
5分monitorと毎日05時backupも保守中に協調させる必要がある。

既知TrailWatch releaseとの共通flockはMOD DLLの `.release.lock`。
準備CLIはこれを保持する。未参加manual操作・cron・他管理経路を
すべて排除しているとは主張しない。別セッションのcheckoutは変更していない。
ユーザー指示によりfirewall/参加遮断は変更しない。

## コアの契約

`suzume_update.py` は固定対象検証、永続保守予約、段階遷移とファイル復元を提供する。
SSH・SteamCMD・Dockerを呼ぶ全7hookの本番backendは未提供。

| 段階 | 必須条件 |
|---|---|
| preflight | 固定payload、容量、完全復旧証明、共通排他、保守条件を読み取り確認 |
| stop | 予約を保持して人数を再取得。不明・不一致・非0なら停止しない |
| check_stopped | ゲーム・セーブwriterの不在、排他の維持を確認 |
| backup | 静止状態と完全scope/runtimeの復旧証明がある場合だけAPI用receiptを返す |
| apply | backupを再検証しhash・build・全MODhashが一致する固定payloadのみ適用 |
| start | 起動時update/monitor/MOD更新を抑えた固定構成で起動 |
| finish | APIの期待版確認後にruntime/MODを再確認し予約を解放。失敗時は予約が残る |

無人運用用 `Workflow` は参加遮断の証明も要求する。
遮断なしの単発検証でこの条件を偽装しない。無人更新を提供する際は、
人数取得から停止までの再参加競合を含む方針をレビューし、実装・画面と一致させる。
単発検証の承認を無人更新の承認とみなさない。

`Reservation` はLinux flockとfsync/atomic renameの永続予約を組み合わせ、
中断・対象変更・重複・段階の不確かな再実行を拒否する。
既存API側も永続idempotency、OS lease、start/stop/restart/commandとの排他を持つ。
未参加の外部操作には追加協調が必要。

`ArchiveStore` は全指定scopeを保存し、新規の別ディレクトリへ展開する。
GNU tarで内容・uid/gid・mode・ACL・xattrを比較しimage archiveも検証する。
危険なパス、重複、device、外へ出るlink、link配下memberは拒否する。
ファイル復元のみでAPI用 `verified:true` は返さない。
外部OpenVPN bind、container runtime、実ゲーム起動・MOD互換性の確認も必要。

`FrozenTarget` は固定ServerFiles tar、SHA256、正確なbuild、全MODファイルhashを照合し、
新しいコピーにのみ展開する。hash一致をゲーム互換性の証明とは扱わない。

任意hook `OPSA_UPDATE_FINISH_CMD` はAPI期待版確認後の `releasing` に実行する。
失敗時は `FINISH_FAILED / recoveryRequired:true`。
trusted ENVは `OPSA_UPDATE_JOB_ID / OPSA_UPDATE_CURRENT_VERSION / OPSA_UPDATE_TARGET_VERSION`。
クライアントから任意パス・コマンド・hook名は受け取らず既存認証/HTTP契約を維持する。

## 配置・有効化の残条件

1. 静止ワールドを含む完全scope、外部VPN、image/runtimeの復元・実起動確認。
2. API、既知release、cron/monitor/backup、manual操作の排他協調と中断時復旧。
3. 新しい実対象版/build/MODの承認・固定・互換性確認。同じ版は更新しない。
4. 固定CLI配置先、私有state dir、7hook、既存SSH権限の実行方法のレビュー。
5. 追加保守時間と停止範囲を親へ報告し、最新人数0まで停止しない。

追加資格情報・権限・ネットワーク変更、PR merge、本番有効化は行っていない。
rollbackは自動実行せず、セーブ進行と復旧条件を確認してoperatorが判断する。

## 検証

`limited_maintenance.py` は承認済みの一回限りの停止・静止コピー・固定版復帰を行う管理者 CLI。
`verify/collect/recover <準備ID>` のみを受け付け、公開 HTTP hook には接続しない。
停止前に既存 API の `/serverstats` と `/player`、直前の固定 telnet `version/lp` を照合し、
0 人を確認できない場合は停止しない。正常な game 終了コードと writer 不在を確認してから
ワールド/config をコピーする。元 container/VPN と固定 ServerFiles は保持し、直接 GSM start で戻す。
cron は PID/start token と元 crontab hash を保存して一時停止し、復帰を確認する。
永続予約と既知 release flock を保持し、途中切断後も `recover` で復帰できる。

`receiver_maintenance.py` は既存 root SSH 経路専用の固定 coordinator。
`stage` は root 所有の非公開コードと既存資格情報を SSH stdin 経由で配置し、読み取り確認だけ行う。
`collect` は管理 API を一時停止してその変更操作を抑止し、切断で終了しない固定 worker を起動する。
ゲーム・cron の復帰確認後だけ同一管理 API を再開する。不明時は API を抑止したまま予約を残す。
`status/recover` で保存結果の確認と復帰を行う。ネットワーク規則、資格情報、unit 設定は変更しない。

隔離起動は OCI config だけを変更した別イメージを使用する。元の復旧 image/archive は保持し、
全 layer と静的 runtime の同一性を検証する。継承された VOLUME/EXPOSE/Compose と
`desktop.docker.io/` の bind 情報を除き、作成後の全 mount が私有コピーと一致する場合だけ起動する。
`check-start` は固定起動確認に限り、オンラインコピーの整合性や完全復旧の証明には使わない。
network=none で EOS の初期化が失敗するため、固定版の `platform.cfg` と
V3.3.0 b18 の metadata で確認した LAN/None 設定を、試行専用ファイルへ限定して使う。
このファイルだけを read-only bind で重ね、元 ServerFiles コピー・アーカイブ・本番設定は保持する。
元の Steam/EOS/VPN 経路の復旧確認は、このオフライン起動から証明しない。

`python3 -m unittest discover -s ops -v`、`go test ./...`、`go vet ./...`、`go build ./...`。
Linux CIは実GNU tar、owner/mode/ACL/xattr、flock、復元、破損・競合・重複・中断を検証する。
WindowsではPOSIX/GNU tar部分をskipしportableなhash/archive/probeを検証する。
隔離起動mockは本番ID拒否、資源不足、起動応答喪失、後片付け失敗、
オンラインワールド変化と永続receiptも確認する。
