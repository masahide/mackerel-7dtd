package main

import (
	"github.com/masahide/mackerel-7dtd/pkg/telnet"
)

type telnetDiscordbot struct {
	env
	t *telnet.Telnet7days
}

func (d *telnetDiscordbot) GetStatus() (GameStatus, error) {
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
