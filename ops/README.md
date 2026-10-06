# SUZUME 更新運用コア（未有効化）

2026-10-06 16:56 UTC の継続調査: PR #5 はmain `0a37ddd629802abf729d610f96876d6cc5831b57` にマージ済み。
`runtime_freeze.py` に、観測済みの固定ホスト/root/container/image/Composeのみを対象とする
復旧資産準備adapterを追加した。現在のコンテナを `--pause=false` で新しいimageに固定し、
起動時update/MOD update/monitor/backupを新image側だけで無効にする。元のゲームは停止・起動せず、
ネットワークなし・capability追加なし・read-onlyの検査用コンテナで静的runtimeを比較する。
ServerFiles/MOD/buildの固定tarとimage exportのhashを検証する。中断/既存IDは再実行せずstate照合を要求する。
この追加はまだ本番で実行しておらず、模擬5テストのみ実行済み。
`prepared` は固定資産の準備で、`verified:false / runtimeRestored:false / productionEnabled:false` を維持する。

実際のentrypointは `/home/sdtdserver/openvpn.sh`（VPN起動後にuser.shをexec）。
固定起動に必要な `START_MODE=1 / UPDATE_MODS=NO` 等の根拠を現物から確認した。
対象全体は約82GB（うち既存backups約63GB）、空き約919GB。
停止後の全scopeコピー・展開・比較・固定3.3起動確認は暫定20～40分を見込むが、I/O性能に依存する。
直前に人数を2ソースで0確認してから停止する。停止後の原本と固定3.3資産は破壊せず保全する。
ゲーム通信だけのIPv4/IPv6保守遮断（TCP26900、UDP26900-26902、eth0/tun0両経路）と
再作成後の起動前適用は新規network security変更であり、別承認を得るまで適用・有効化しない。

本番の操作adapterは未接続です。`suzume_update.py` は固定対象検証、永続保守予約、6工程と成功確認後の解除、隔離ファイル復元のコアです。SSH、SteamCMD、Docker、firewallを呼ぶ運用adapterや起動可能な本番hook CLIは提供していません。`suzume-profile.observed.json` は読み取り時の構成記録であり、有効な配置設定ではありません。

現在の本番APIは commit `01a037b7f4dc69f25661826741f871548f9636a6`、`OPSA_UPDATE_ENABLED=false` のままです。この変更を本番へ配置・有効化していません。

## 根拠と既存方式

2026-10-06 14:39 UTC の読み取りで、ホスト `7dtd01`、Compose `/home/masahide/work/7dtd/docker-compose.yml`、service `7dtdserver`、ゲーム V3.3.0 b18 / Steam build 25661908、オンライン1人を確認しました。画像・コンテナ・Composeの識別子は構成記録にあります。

同日の実更新記録では、`docker compose -f ... stop -t 120 7dtdserver`、元コンテナの exit=0/OOMなしを確認、`ServerFiles / 7DaysToDie / LGSM-Config / log / docker-compose.yml / Dockerfile`（存在する場合 `.env / docker-compose.override.yml`）を GNU tar の ACL/xattr/numeric-owner付きで保存、`tar --compare`、SHA-256、元コンテナの `compose start` を実施していました。約19GBのtarは元ファイルとの比較済みですが、全体復元・旧ゲーム起動の証拠はありません。

実コンテナの `install.sh` と `scripts/server_update.sh` を読み取りました。`START_MODE=3 / VERSION=stable` はLinuxGSM updateとMODインストールを起動時に実行します。`UPDATE_MODS=YES / CPM_UPDATE=YES`、5分ごとのLinuxGSM monitor、毎日05時のbackupもあります。これを固定対象の更新・復旧に流用すると最新版追従や自動再起動が介入します。[Docker元ソース](https://github.com/masahide/Docker-7DaysToDie/tree/64bb7343ab7fc25ec22dd0c98a2a0c7d1ffcc33d) と現在のinstalled scriptは一部異なり、現物確認を優先しました。

TrailWatch `scripts/release_remote.py` の排他はMOD DLLの `.release.lock` です。人数確認はreceiver側の最初のSSH hopにあり、Docker stop/upまでの参加競合を閉じていません。ゲーム全体の共通保守予約、VPN経路とDocker公開ポート両方の参加遮断は既存手順で確認できませんでした。

## 実装と正確な検証範囲

`Workflow` は `preflight / stop / check_stopped / backup / apply / start` と `finish` を実装しています。外部adapterが満たす契約は以下です。

| 工程 | 必須の条件 |
|---|---|
| preflight | 読み取りのみ。固定payload、容量、完全復旧の証拠、共通排他・参加遮断の準備を確認。現在のsnapshotでは未達 |
| stop | 永続予約→参加と外部操作を遮断→版と人数を再取得。0人が不明・bool・負数・版変化なら停止しない。予約は残して人による回復を要求 |
| check_stopped | 予約・参加遮断の維持と、ゲーム/セーブ書き込みプロセスの不在を確認 |
| backup | 静止状態を再確認。完全scopeの復元証拠がある場合だけAPIの `{backupId,verified:true}` を返す |
| apply | バックアップを再検証し、事前取得したSHA-256・Steam build・全MODファイルhashが一致する固定payloadを適用。latest取得禁止 |
| start | 起動時update/monitor/MOD更新を抑えた固定構成で起動。参加遮断は維持 |
| finish | 管理APIが期待版を確認した後に、版・0人・MOD/runtimeを再確認して遮断解除。失敗時は保守予約が残る |

`Reservation` はLinux flockによる各操作の排他とfsync/atomic renameの永続予約を組み合わせます。プロセスが消えても予約は残ります。重複stop、他job、順序違い、途中工程の不明な再実行、構成対象の変更を拒否します。外部のstart/restart/deploy/cron/manual運用も同じ予約を同じlock下で確認する必要があります。未参加のlegacy操作をこのライブラリだけで制御できるとは主張しません。

`ArchiveStore` はゲームrootの全指定scope（上記に `backups` を追加）を保存・元ファイル比較し、Docker save形式のimage ID/config/layer hash・tar可読性を検証します。新しい私有ディレクトリにのみ展開し、GNU tarで内容・uid/gid・mode・ACL・xattrを比較します。既存/liveの復元先は受け取りません。危険なパス、重複、device、外へ出るlink、linkの配下にあるmemberを拒否します。失敗や未知のDocker archive形式は成功扱いしません。

**このファイル復元の結果はAPI用 `verified:true` ではありません。** 外部OpenVPN bind、コンテナの書き込みlayer/LinuxGSMの現物状態、コンテナ再構成、ゲーム起動・MOD互換性をまだ復元検証していません。返り値は `filesystemRestored:true / runtimeRestored:false`、保管evidenceは `verified:false` です。APIへそのまま渡せば拒否されます。本番19GBバックアップは読み出し・展開していません。生成した小さいfixtureだけで検証します。

`FrozenTarget` は凍結したServerFiles tarのSHA-256、appmanifestの正確なSteam build、MOD全ファイルhashを照合し、新しい隔離copyにのみ展開できます。ゲーム互換性をファイルhashだけで検証済みとは扱いません。現在のpayload/hash/MOD承認一覧は未準備です。

管理APIへ後方互換の `OPSA_UPDATE_FINISH_CMD` を追加しました。設定時だけ phase/steps に `releasing` が加わり、APIの期待版確認後に呼びます。解除失敗は `FINISH_FAILED`、`recoveryRequired:true` です。hookには `OPSA_UPDATE_JOB_ID / OPSA_UPDATE_CURRENT_VERSION / OPSA_UPDATE_TARGET_VERSION` をtrusted環境値で渡します。クライアントからhook名・パス・コマンドを渡す仕様はありません。通常のHTTP契約、認証、二重要求対策は維持します。

## 有効化に残る作業・承認

1. 対象版/buildとMOD互換性を承認し、隔離環境でSteamCMDから取得・検証した固定payloadとimage/runtimeを凍結する。stable起動更新やlatest MOD取得は採用しない。
2. 全bind、外部OpenVPN設定、コンテナruntime状態・image/Composeの完全バックアップと復旧手順を実装し、ネットワークなしの隔離コピーで復旧検証する。保存機密は私有ファイルに置き、出力しない。旧セーブを実運用へ戻す自動rollbackは実装しない。
3. ゲームTCP26900・UDP26900-26902のIPv4/IPv6とVPN tun0経路を、ゲーム開始前から完了まで遮断する方式を実装・隔離検証する。8080-8082管理通信、SSH、VPN接続自体は維持する。**新規network security規則は別承認が必要で、現在は禁止されている。**
4. 管理API、TrailWatchリリース、監視bot、自動monitor/backup、手動運用を共通予約へ参加させる。再起動後も予約を引き継ぎ、未参加操作を止める。別更新セッションのcheckoutは変更していない。
5. root限定の固定adapter/CLIとSSHの固定配送を配置し、状態DIRと7hookを設定する。新規資格情報・権限の付与が必要なら別承認を得る。設定だけで準備完了を宣言しない。
6. プレイヤー退出と明示された保守時間の承認を得て、停止・静止・バックアップ・固定起動・参加遮断解除の実機確認を行う。その後に `UPDATE_ENABLED=true` とする。人数0を自動で待って停止することはしない。現在版と同じ固定対象は `ALREADY_CURRENT` で実行不可。

| 検証 | ゲーム停止/影響 |
|---|---|
| source/mocks、小容量fixtureのGNU tar復元・ACL/xattr・flock・hash検証 | なし。GitHub Linux CIで実施 |
| 全バックアップの隔離展開・比較 | 停止不要だが約19GB以上のI/Oあり。プレイ中の本番では未実施 |
| 新規参加遮断/再作成後の遮断確認 | 接続に影響。現在不許可 |
| clean stop・書き込み不在・起動時update抑止・本番runtime/MOD確認 | 停止・再起動を伴う。別保守承認待ち |
| 旧世界でのゲーム起動/完全復旧 | 隔離ホスト・通信遮断が必要。本番復旧は停止とセーブ退行を伴い別承認待ち |

最小の次の一手は、本番に触れず完全runtimeコピー・固定payloadの取得と検証方式を隔離環境で完成させることです。本番への参加制限設定と停止試験は、プレイ終了後の明示された保守時間と別承認があるまで待ちます。

## 検証コマンド

`python3 -m unittest discover -s ops -v`、`go test ./...`、`go vet ./...`、`go build ./...`。

Linux CIは `acl` を用意し、所有者・権限・ACL・xattr・hardlink/symlinkを含む小容量fixtureを本当に展開・比較します。WindowsローカルではPOSIX/GNU tarの試験をskipし、hash/危険archiveのportable試験だけを実行します。Docker/SSH/SteamCMDを呼ぶ試験はありません。
