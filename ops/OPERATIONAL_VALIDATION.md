# 2026-10-06〜07 の単発検証

ユーザーは保守中の通信遮断を不要とし、遮断なしでの単発検証を承認した。
iptables、IPv4/IPv6 firewall、参加遮断は変更していない。
無人の自動更新を有効にするための保守機構とは区別する。
既存Workflowのfence証明を偽装せず、`verified:true` を返さない。

## 固定資産とファイル復元

準備ID `b2684acae8b0495d8afa700c4cd28b2c`。稼働版V3.3.0 b18 / Steam build 25661908。
commitは `--pause=false` で一度だけ実行した。元のコンテナは停止・再起動せず、
別imageで起動時更新・MOD更新・monitor・backupを無効にした。
`image ls --all` により一意のラベルからimageを解決し、固定ENV・静的runtimeを照合した。

| 資産 | 実機での結果 |
|---|---|
| 固定image | `sha256:efd9920b0ba84b8d9f90cdae053e2fa115acc248dc41b07f7491433f067b5673` |
| runtime-image.tar | 510,636,032 bytes / SHA256 `6b770f57ce8255688e8d680ac7e8da214271a2ef2828b7ee171cd429ef911091` |
| target-serverfiles.tar | 17,737,216,000 bytes / SHA256 `16ff81524ff08a15f80760e9ecffb8a73d4e998b6289e15f7ac01f4d4393f599` |
| 固定ServerFilesの復元 | 別ディレクトリへ展開、内容・所有者・mode・ACL・xattrのGNU tar比較が一致 |
| MOD/build | 保存した全MODファイルhashとSteam build 25661908を照合 |
| 完全復旧証明 | 未達。`verified:false / runtimeRestored:false / productionEnabled:false` |

Docker 29.6.1/containerdのexportはOCI layoutとDocker manifestの併記だった。
image IDはOCI manifest digestでありconfig digestではない。
検証器はmanifest/config/layerのdigest・size、gzip CRC、展開後DiffID、tar可読性、
併記manifestのconfig/layer順の一致を検証する。
対応は一つのlinux/amd64 runnable OCI manifest、ローカルSHA256 blob、非圧縮/gzip layer。
multi-platform/nested index、zstd、未知mediaType、外部URL・埋込dataは拒否する。
展開後layer上限は8GiB。汎用の全OCI形式対応ツールではない。

参考: [OCI descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md)、
[image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)、
[manifest](https://github.com/opencontainers/image-spec/blob/main/manifest.md)、
[DiffID](https://github.com/opencontainers/image-spec/blob/main/config.md#layer-diffid)。

## 10月7日の隔離起動試行

`runtime_rehearsal.py` は固定imageと上記ServerFilesコピー、別のワールド/configコピーを使う。
network=none、公開ポートなし、VPN起動なし、6GiB/2CPU上限、restart=no。
本番container IDへの停止を拒否する。起動待ちは最大180秒、隔離コンテナ停止待ちは最大90秒。
成功してもオンラインコピーからの起動を完全バックアップ証明とは扱わない。

直前に既存管理経路でversion一致、`/serverstats`・`/player` の0人を確認した。
約1.4GBのワールド/configコピーの比較で中断した。
`main.ttw` と `main.ttw.bak` が稼働側で書き換わり、内容差2件・mtime差2件、比較exit=1だった。
tarの12,876 memberと全ファイル末尾までの読み取りは成功したが、
整合したワールドの復旧用コピーとは認めない。

隔離コンテナは作成されず、展開も未実施。失敗tarとreceiptは私有領域へ保全し、自動再試行しない。
本番のcontainer ID・image・Config・起動時刻・再起動回数が不変なことを読み取りで確認した。
0人だけではワールド静止を証明できない。
修正後は比較失敗を `ONLINE_COPY_NOT_MATCHED` と
`failedPhase:comparing_online_source / startAttempted:false` で保存する。
隔離起動後の後片付け失敗も `cleanupRequired:true` とエラーを永続化する。
実機に残った旧receiptの `PREPARATION_COMMAND_FAILED` は書き換えていない。

## 次段階

整合するワールドを得るにはゲームwriterの静止が必要。
停止前に最新人数を再取得し、停止範囲・固定復帰手順・保守時間を親へ報告してから進む。
[次段階の手順](RECOVERY_REHEARSAL_PLAN.md) を参照。
比較を無視したり、証明フラグを書き換えたりしない。

全scope約82.16GBのうち約62.97GBは過去backups、約19.20GBは活動資産。
完全バックアップの復元、実ゲーム起動、OpenVPN経路、MOD互換性は未確認。
以前の20〜40分という停止見込みは実測に基づかないため撤回済み。
管理APIは `OPSA_UPDATE_ENABLED=false`、追加hook未配置。
ゲーム停止・再起動・更新・API有効化・通信変更は実施していない。
