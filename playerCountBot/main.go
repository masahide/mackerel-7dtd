package main

import (
	"fmt"
	"log"
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
}

type discordbot struct {
	env
	s *discordgo.Session
	t *telnet.Telnet7days
}

/*

func (d *discordbot) getGameStatus() (GameStatus, error) {
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
*/

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

func (d *discordbot) update() {
	day, err := d.t.GetTime()
	if err != nil {
		d.s.UpdateCustomStatus("サーバ停止中")
		return
	}
	players, err := d.t.GetPlayers()
	if err != nil {
		d.s.UpdateCustomStatus("サーバ停止中")
		return
	}
	if err := d.s.GuildMemberNickname(d.DiscordServerID, "@me", fmt.Sprintf("Day%d, %02d:%02d",
		day.Days, day.Hours, day.Minutes)); err != nil {
		log.Printf("Error updating nickname: %s", err)
	}
	d.s.UpdateGameStatus(0, fmt.Sprintf("プレイヤー%d人", len(players)))
}
