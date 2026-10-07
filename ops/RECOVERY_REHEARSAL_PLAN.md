# 整合したワールドによる次の復旧試行

2026-10-07時点の提案。以下の本番停止はまだ実施していない。
オンライン複製は `main.ttw / main.ttw.bak` の変化で不一致になった。
比較を無視した起動成功は完全復旧の証明にならない。

## 停止範囲と事前条件

最初にゲームprocessだけを正常終了し、静止ワールド/configをコピーする。
本番へpayloadを適用せず、Compose再作成やDocker restartも行わない。
既存containerとVPNが維持できることを直前に確認する。
人数取得から終了までの再参加競合は残り、通信遮断は行わない。
人数非0・不明・不一致の場合は停止に進まない。

既知MOD releaseとのflock、永続保守予約、API/手動操作との重複防止を確保する。
cronのmonitor/backup、実行中のupdate/backupとの協調が必要。
この処理を含む本番adapterは未配置であり、復帰までレビューしてから実行する。
固定プログラムと既存SSH経路を使い、公開HTTPにシェル/パスを追加しない。

## 復帰を含む手順

1. 現行container/image/Compose、固定資産、MOD/static runtime/configのhashを再確認。
   必要config、system SteamCMD、LinuxGSM v24.2.1一致、`updateonstart=off` と
   上書き設定がないことを確認する。
2. 親へ停止範囲、未測定の所要時間、復帰手順を報告し、追加保守時間の承認を受ける。
   停止直前に既存認証でversion・`/serverstats`・`/player` の正確な0人を再確認する。
   以前の0人を流用しない。
3. 正常shutdown完了とゲーム・セーブwriter不在を確認。
   GSM stopはtelnet失敗後にtmux killへ進むため、成功コードだけでclean stopと認めない。
   正常終了未確認なら適用/バックアップ証明へ進まない。
4. 私有領域へワールド/configをGNU tarで保存し、静止sourceと比較する。
   完全scopeの証明には約63GBの過去backups、その他bind、外部VPN設定、
   container private inspect、固定imageも必要。この短いコピーのみでは完了しない。
5. 元のServerFiles/configのまま固定GSM launcherを直接startして本番へ復帰する。
   観測済み候補は `docker exec --user sdtdserver --workdir /home/sdtdserver <固定元container ID> ./sdtdserver start`。
   上記hash/設定の再確認が条件。install.sh、Compose start/restartを通さない。
   version/build/MODと既存管理経路で正常稼働を確認する。
6. 静止時コピーを別ディレクトリへ展開しtarとowner/mode/ACL/xattrを比較。
   本番復帰後、固定imageとコピーのみで公開ポートなし・network=noneの隔離起動を行う。
   隔離失敗で本番ファイルを書き戻さず私有logとreceiptを保全する。

未実装・実機未確認部分があるため、停止前にcron等との協調と固定start CLIを完成させる。
既存containerが停止・失われた場合は通常restartせず、固定imageと外部VPNを使う
別の復旧手順が必要。このnetwork=none試行はVPN起動を証明しない。

## 時間と中断条件

約1.4GBのオンラインコピーは作成から比較失敗まで約46秒だったが、
正常shutdown・復帰・完全scope保存/展開の実測はない。
全scope約82GBのため停止時間を20〜40分等と保証できない。
未測定部分を親へ示し、停止時間上限と中断時復帰条件を決めてから実行する。

版/hash変化、人数非0/不明、排他喪失、強制終了、writer残存、コピー不一致、
資源不足、固定start失敗が中断条件。自動rollback/再試行/有効化は行わない。
管理画面の更新を安全に使えると報告するには、完全復旧証明、実target、
全hook・排他方針・配置のレビューまで必要。
