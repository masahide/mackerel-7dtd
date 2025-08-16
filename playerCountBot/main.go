package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kelseyhightower/envconfig"
	"github.com/masahide/mackerel-7dtd/pkg/telnet"
)

type env struct {
	telnet.Env
	// Discord
	DiscordToken    string `envconfig:"DISCORD_TOKEN"`
	DiscordServerID string `envconfig:"DISCORD_SERVER_ID"`
	// 7Days To Die server
	GetStatsURL string `envconfig:"GET_STATS_URL"`
	APIUser     string `envconfig:"API_USER"`
	APISecret   string `envconfig:"API_SECRET"`
}

type discordbot struct {
	env
	s *discordgo.Session
	t *telnet.Telnet7days
}

type GameTime struct {
	Days    int `json:"days"`
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
}

type GameStatus struct {
	GameTime GameTime `json:"gametime"`
	Players  int      `json:"players"`
	Hostiles int      `json:"hostiles"`
	Animals  int      `json:"animals"`
}

func (d *discordbot) restAPIgetGameStatus() (GameStatus, error) {
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
		t:   &telnet.Telnet7days{Env: e.Env},
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

func (d *discordbot) telnetGetStatus() (GameStatus, error) {
	day, err := d.t.GetTime()
	if err != nil {
		return GameStatus{}, err
	}
	players, err := d.t.GetPlayers()
	if err != nil {
		return GameStatus{}, err
	}

	return GameStatus{
		GameTime: GameTime{Days: day.Days, Hours: day.Hours, Minutes: day.Minutes},
		Players:  len(players), Hostiles: 0, Animals: 0,
	}, nil
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
	stats, err := map[bool]func() (GameStatus, error){
		true:  d.telnetGetStatus,
		false: d.restAPIgetGameStatus,
	}[len(d.ServerAddr) > 0]()

	d.updateStatus(stats, err)
}
