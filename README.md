# mackerel-7dtd

7 Days to Die のプレイヤー情報を取得して Mackerel へメトリックを送信するツールと、7dtd 運用 API、Discord bot を含む Go リポジトリです。

## 構成

| パス | 役割 | 実行形態の例 |
| --- | --- | --- |
| `.` | プレイヤー情報を Mackerel へ送信 | `/etc/cron.d/mackerel-7dtd` から毎分実行 |
| `apiserver7dtd/` | 7 Days to Die の運用 API | `apiserver7dtd.service`。デフォルトで TCP `8088` |
| `playerCountBot/` | Discord bot。ゲーム時刻、プレイヤー数などを表示 | `playerCountBot.service` |
| `pkg/telnet/` | 7 Days to Die の Telnet クライアントとパーサー | 上記アプリから利用 |

systemd の unit ファイルはデプロイ先の設定として管理しており、このリポジトリには含めていません。

## ビルドとテスト

```sh
go build ./...
go test ./...
go vet ./...
```

個別にバイナリを作成する場合:

```sh
go build -o mackerel-7dtd .
go build -o apiserver7dtd/apiserver7dtd ./apiserver7dtd
go build -o /tmp/playerCountBot ./playerCountBot
```

## Mackerel 収集ツール

ビルドしたルートバイナリを `/usr/local/bin/mackerel-7dtd` に配置し、cron から実行します。実サーバーでは `/etc/cron.d/mackerel-7dtd` を使用しています。

```cron
MACKEREL_HOST_ID=<Mackerel の host ID>
MACKEREL_API_KEY=<Mackerel API key>

PLAYERS_API_URL=http://<7dtd API host>:8080/api/GetPlayersOnline
PLAYERS_API_USER=<API user>
PLAYERS_API_SECRET=<API secret>

* * * * * root /usr/local/bin/mackerel-7dtd 2>&1 | logger
```

プレイヤー情報の取得方法は次の2種類です。

- REST API モード: `SERVERADDR` を設定しない場合。`PLAYERS_API_URL`、`PLAYERS_API_USER`、`PLAYERS_API_SECRET` を使用します。
- Telnet モード: `SERVERADDR` を設定した場合。`SERVERADDR` と `TELNETPASS` を使用します。

実際に使用するモードに必要な設定だけを指定してください。`MACKEREL_API_KEY`、`PLAYERS_API_SECRET`、`TELNETPASS` などの実値はリポジトリへ保存しないでください。

## `apiserver7dtd`

7 Days to Die の状態取得・操作用 HTTP API です。デフォルトでは `:8088` で待ち受けます。

```sh
go build -o /usr/local/bin/apiserver7dtd ./apiserver7dtd
```

設定は `OPSA_` 接頭辞付きの環境変数で読み込みます。認証情報などの秘密値は `EnvironmentFile` などのデプロイ先設定で管理してください。

実サーバーでは、次の unit として稼働しています。

```text
apiserver7dtd.service
ExecStart=/usr/local/bin/apiserver7dtd
```

## `playerCountBot`

Discord 上に 7 Days to Die のゲーム時刻、プレイヤー数などを表示する bot です。

```sh
go build -o /usr/local/bin/playerCountBot ./playerCountBot
```

主な設定は `DISCORD_TOKEN`、`DISCORD_SERVER_ID`、`GET_STATS_URL`、`GET_PLAYERS_URL`、`GET_ZOMBIES_URL` などです。実サーバーでは `playerCountBot.service` として稼働し、設定は `/etc/default/playerCountBot` から読み込みます。

## 設定値の扱い

API key、token、password、secret、`OTEL_EXPORTER_OTLP_HEADERS` などの認証情報は、README、ソース、commit に実値を書かないでください。環境変数、`EnvironmentFile`、または secrets manager から実行時に渡します。
