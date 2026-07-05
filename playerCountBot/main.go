package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/kelseyhightower/envconfig"
	"github.com/masahide/mackerel-7dtd/pkg/telnet"
)

type env struct {
	telnet.Env
	DiscordToken    string `envconfig:"DISCORD_TOKEN"`
	DiscordServerID string `envconfig:"DISCORD_SERVER_ID"`
	// 7Days To Die server
	GetStatsURL     string `envconfig:"GET_STATS_URL"`
	APIUser         string `envconfig:"API_USER"`
	APISecret       string `envconfig:"API_SECRET"`
	GetPlayersURL   string `envconfig:"GET_PLAYERS_URL"`
	GetZombiesURL   string `envconfig:"GET_ZOMBIES_URL"`
	StatusChannelID string `envconfig:"STATUS_CHANNEL_ID"`
}

type GameStatusProvider interface {
	GetStatus() (GameStatus, error)
}

type discordbot struct {
	s *discordgo.Session
	env
	GameStatusProvider

	lastTopic   string
	lastTopicAt time.Time
	bioMinStep  time.Duration
}

type GameTime struct {
	Days    int `json:"days"`
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
}

type Player struct {
	Name string `json:"name"`
}

type GameStatus struct {
	GameTime GameTime `json:"gametime"`
	Players  int      `json:"players"`
	Hostiles int      `json:"hostiles"`
	Animals  int      `json:"animals"`
	Online   []Player `json:"online,omitempty"` // ここにオンライン名を入れる
}

type restAPIDiscordbot struct {
	env
}

func (d *restAPIDiscordbot) GetStatus() (GameStatus, error) {
	res := GameStatus{}
	req, err := http.NewRequest(http.MethodGet, d.GetStatsURL, nil)
	if err != nil {
		log.Printf("Error creating request.  err:%s", err)
		return res, err
	}
	req.Header.Add("X-SDTD-API-TOKENNAME", d.APIUser)
	req.Header.Add("X-SDTD-API-SECRET", d.APISecret)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error sending request  err:%s", err)
		return res, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response body: %s", err)
		return res, err
	}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Printf("Error parsing JSON:%s ", err)
	}
	return res, err
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	e := env{}
	envconfig.Process("", &e)
	dg, err := discordgo.New("Bot " + e.DiscordToken)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}

	d := &discordbot{
		env: e,
		s:   dg,
		GameStatusProvider: map[bool]GameStatusProvider{
			true:  &telnetDiscordbot{env: e, t: &telnet.Telnet7days{Env: e.Env}},
			false: &restAPIDiscordbot{env: e},
		}[len(e.ServerAddr) > 0],
		bioMinStep: 60 * time.Second, // 最短でも60秒間隔
	}
	dg.AddHandler(d.ready)
	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	defer dg.Close()
	select {}
}

func (d *discordbot) ready(s *discordgo.Session, event *discordgo.Ready) {
	d.s = s
	d.update()
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for range ticker.C {
			d.update()
		}
	}()
}

func (d *discordbot) updateStatus(stats GameStatus, err error) {
	if err != nil {
		log.Printf("Error getting game status: %s", err)
		d.s.UpdateCustomStatus("サーバ停止中")
		return
	}

	if err := d.s.GuildMemberNickname(d.DiscordServerID, "@me", fmt.Sprintf("Day%d, %02d:%02d",
		stats.GameTime.Days, stats.GameTime.Hours, stats.GameTime.Minutes)); err != nil {
		log.Printf("Error updating nickname: %s", err)
	}
	d.s.UpdateGameStatus(0, fmt.Sprintf("プレイヤー%d人", stats.Players))
}

func (d *discordbot) update() {
	stats, err := d.GetStatus()
	d.updateStatus(stats, err)
	// プレイヤー名 & ゾンビ集計を取得してチャンネルトピックへ
	names, err := d.fetchOnlineNames()
	if err != nil {
		log.Printf("fetchOnlineNames error: %v", err)
	}
	var ztotal int
	var zmap map[string]int
	if len(d.GetZombiesURL) > 0 {
		ztotal, zmap, err = d.fetchZombies()
		if err != nil {
			log.Printf("fetchZombies error: %v", err)
		}
	}
	d.updateChannelTopic(names, stats.Players, stats.GameTime.Days, stats.GameTime.Hours, ztotal, zmap)

}

type playersAPIResponse struct {
	Data struct {
		Players []struct {
			Name   string `json:"name"`
			Online bool   `json:"online"`
		} `json:"players"`
	} `json:"data"`
}

func (d *discordbot) fetchOnlineNames() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, d.GetPlayersURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("X-SDTD-API-TOKENNAME", d.APIUser)
	req.Header.Add("X-SDTD-API-SECRET", d.APISecret)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var res playersAPIResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(res.Data.Players))
	seen := make(map[string]struct{}, len(res.Data.Players))
	for _, p := range res.Data.Players {
		if !p.Online {
			continue
		}
		n := strings.TrimSpace(p.Name)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	// 表示を安定させるためにソート
	sort.Strings(names)
	return names, nil
}

// ★ ゾンビAPIの実際のスキーマに合わせた型
type zombiesAPIResponse struct {
	Data []struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		Position struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		} `json:"position"`
	} `json:"data"`
	// meta.serverTime は必要なら後で使えます
}

// ★ ゾンビ取得＆集計（総数と種別別カウントを返す）
func (d *discordbot) fetchZombies() (total int, byType map[string]int, err error) {
	req, err := http.NewRequest(http.MethodGet, d.GetZombiesURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Add("X-SDTD-API-TOKENNAME", d.APIUser)
	req.Header.Add("X-SDTD-API-SECRET", d.APISecret)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}

	var res zombiesAPIResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, nil, err
	}

	byType = make(map[string]int, 32)
	for _, z := range res.Data {
		kind := prettifyZombieKind(z.Name) // 例: zombieSoldierFeral → Soldier (Feral)
		byType[kind]++
		total++
	}
	return total, byType, nil
}

// ★ 表示用に軽く整形（不要ならそのまま name を返してOK）
func prettifyZombieKind(name string) string {
	n := name
	if strings.HasPrefix(n, "zombie") {
		n = n[len("zombie"):] // 先頭の "zombie" を落とす
	}

	// よくある接尾辞を括弧に寄せる
	// 例: SoldierFeral → Soldier (Feral)
	suffixes := []string{"Radiated", "Feral", "Charged", "Mutated"}
	var suffix string
	for _, sfx := range suffixes {
		if strings.HasSuffix(n, sfx) {
			suffix = sfx
			n = strings.TrimSuffix(n, sfx)
			break
		}
	}

	// CamelCase をスペース区切りに（TomClark → Tom Clark）
	label := insertSpaces(n)
	if suffix != "" {
		label = fmt.Sprintf("%s (%s)", label, suffix)
	}
	return label
}

func insertSpaces(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if i > 0 && unicode.IsUpper(r[i]) &&
			(unicode.IsLower(r[i-1]) || (i+1 < len(r) && unicode.IsLower(r[i+1]))) {
			b.WriteRune(' ')
		}
		b.WriteRune(r[i])
	}
	return b.String()
}

// 旧: updateChannelTopic(playerNames []string, playerCount int, day int, zombieTotal int, zombieByType map[string]int)
// 新: hour を追加
func (d *discordbot) updateChannelTopic(playerNames []string, playerCount int, day int, hour int, zombieTotal int, zombieByType map[string]int) {
	// レート/ノイズ対策
	if time.Since(d.lastTopicAt) < 60*time.Second && d.lastTopic != "" {
		return
	}

	// ★ 1行目：ゲーム内日付時刻＋ブラッドムーン表記
	headerLine := formatInGameHeader(day, hour)

	// 2行目：プレイヤー
	playerLine := "🎮プレイヤーが誰もいません"
	if playerCount > 0 && len(playerNames) > 0 {
		if len(playerNames) > 20 {
			playerNames = playerNames[:20]
		}
		joined := joinWithLimit(playerNames, 950)
		playerLine = fmt.Sprintf("🎮:%d人(%s)", playerCount, joined)
	} else if playerCount > 0 {
		playerLine = fmt.Sprintf("🎮:%d人", playerCount)
	}

	zombieLine := ""
	if len(d.GetZombiesURL) > 0 {
		zombieLine = "🧟: 0体"
		if zombieTotal > 0 && len(zombieByType) > 0 {
			type kv struct {
				Name  string
				Count int
			}
			kvs := make([]kv, 0, len(zombieByType))
			for k, v := range zombieByType {
				kvs = append(kvs, kv{Name: k, Count: v})
			}
			sort.Slice(kvs, func(i, j int) bool {
				if kvs[i].Count == kvs[j].Count {
					return kvs[i].Name < kvs[j].Name
				}
				return kvs[i].Count > kvs[j].Count
			})
			if len(kvs) > 15 {
				kvs = kvs[:15]
			}
			parts := make([]string, 0, len(kvs))
			for _, x := range kvs {
				parts = append(parts, fmt.Sprintf("%s x%d", x.Name, x.Count))
			}
			joined := joinWithLimit(parts, 950)
			zombieLine = fmt.Sprintf("🧟:%d体[%s]", zombieTotal, joined)
		}

	}

	topic := "[Login](https://sc.suzu.me.uk/157.7.208.157:26900)\n[map](https://7w.suzu.me.uk/)\n"
	topic = topic + headerLine + "\n" + playerLine
	if zombieLine != "" {
		topic += "\n" + zombieLine
	}
	if topic == d.lastTopic {
		return
	}
	if d.lastTopic != topic {
		if _, err := d.s.ChannelEditComplex(d.StatusChannelID, &discordgo.ChannelEdit{Topic: topic}); err != nil {
			log.Printf("failed to update topic: %v", err)
			return
		}
	}
	d.lastTopic = topic
	d.lastTopicAt = time.Now()
}

func joinWithLimit(items []string, limit int) string {
	var b strings.Builder
	for i, s := range items {
		if i > 0 {
			if b.Len()+2 > limit {
				b.WriteString("…")
				break
			}
			b.WriteString(", ")
		}
		if b.Len()+len([]rune(s)) > limit {
			b.WriteString("…")
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

func bloodMoonTag(day int) string {
	if day > 0 && day%7 == 0 {
		return "[🔴BloodMoon🧟‍♀️]"
	}
	// 次のBloodMoon（7の倍数日）
	var next int
	if day <= 0 {
		next = 7
	} else {
		next = day + (7 - (day % 7))
	}
	diff := next - day
	return fmt.Sprintf("[%d日後BloodMoon(%d)]", diff, next)
}

func formatInGameHeader(day, hour int) string {
	return fmt.Sprintf("%d日%d時 %s ", day, hour, bloodMoonTag(day))
}
