# 2026-10-06 の単発検証

ユーザーは保守中の通信遮断を不要とし、遮断なしでの単発検証を承認した。
iptables、IPv4/IPv6 firewall、参加遮断は変更しない。
将来の無人自動更新を有効にするための検証済み保守機構とは区別する。
既存 Workflow の fence 証明を偽装したり、`verified:true` を返したりしない。

停止に進める条件は、固定3.3 b18/build 25661908・現在のMOD・runtime資産と
最新版を取得しない復帰手順を確保し、直前の `/serverstats` と `/player` が
ともに0人を示すこと。人数不明・不一致・非0人なら停止しない。
人数取得から停止までの再参加の窓は残るため、ゼロとは報告しない。
停止中の検証は必要な範囲に限定し、復帰を優先する。

準備ID `b2684acae8b0495d8afa700c4cd28b2c` の commit は再実行していない。
通常一覧では見えなかった untagged image は `docker image ls --all` で発見した。
ラベル・固定起動フラグ・静的runtimeハッシュを照合済み。
image ID は `sha256:efd9920b0ba84b8d9f90cdae053e2fa115acc248dc41b07f7491433f067b5673`。
元コンテナのイメージ・Config・起動時刻・再起動回数は同じ。
Mounts 配列の順序だけが変わるため、全要素を Destination 順で比較する。

書き出し済み `runtime-image.tar` は 510,636,032 bytes。
Docker 29.6.1/containerd image store が OCI layout と Docker manifest を出力した。
inspect ID は OCI image manifest digest で、config digest とは異なる。
現行の legacy Docker-save 検証器は config digest を image ID とみなすため
`IMAGE_ID_MISMATCH` で停止した。OCI manifest/config/blob digest・size、圧縮blob、
展開後 diffID、tar可読性を検証する実装と試験が必要。照合条件は緩めない。

現時点で固定 ServerFiles tar は未作成、全復旧・再起動は未検証。
ゲーム停止・再起動・更新・API有効化・通信変更は未実施。
全scope 82.16GBのうち62.97GBは既存バックアップ、残り約19.20GBは稼働資産。
20〜40分の停止見込みは実測に基づかないため撤回し、所要時間は未確定。
