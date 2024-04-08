7Days to Die サーバーのログインユーザー情報をmackerelに投稿する
==============================================================

setup
-------

- `go build`で作成した`mackerel-7dtd`を`/usr/local/bin/mackerel-7dtd`に配置
- `/etc/cron.d`に以下のcronファイルを設置。
```bash
MACKEREL_HOST_ID=xxxxxxxx
MACKEREL_API_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
PLAYERS_API_URL=http://xxx.xxx.xxx.xxx:8080/api/GetPlayersOnline
PLAYERS_API_USER=admin_username
PLAYERS_API_SECRET=xxxxxxxxxxxxxxxxxxx

* * * * * root /usr/local/bin/mackerel-7dtd
```
