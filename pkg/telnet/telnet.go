package telnet

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"time"
)

// Player struct represents the player information
type Player struct {
	ID       int
	Name     string
	Position struct {
		X float64
		Y float64
		Z float64
	}
	Health  int
	Deaths  int
	Zombies int
	Players int
	Score   int
	Level   int
	PltfmID string
	CrossID string
	IP      string
	Ping    int
}

type Env struct {
	ServerAddr string `default:"localhost:8081"`
	TelnetPass string
}

var trimRe1 = regexp.MustCompile(`[0-9]\. `)

// parsePlayerInfo parses a player information line into a Player struct
func parsePlayerInfo(line string) (Player, error) {
	var player Player

	// Remove leading "0. "
	line = trimRe1.ReplaceAllString(line, "")

	// Split by comma, respecting commas inside ()
	parts := splitWithCommas(line)

	// Parse each key-value pair
	for i, part := range parts {
		if i == 1 {
			player.Name = part
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return player, fmt.Errorf("invalid key-value pair: '%s'", part)
		}

		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		switch key {
		case "id":
			fmt.Sscanf(value, "%d", &player.ID)
		case "pos":
			fmt.Sscanf(value, "(%f, %f, %f)", &player.Position.X, &player.Position.Y, &player.Position.Z)
		case "health":
			fmt.Sscanf(value, "%d", &player.Health)
		case "deaths":
			fmt.Sscanf(value, "%d", &player.Deaths)
		case "zombies":
			fmt.Sscanf(value, "%d", &player.Zombies)
		case "players":
			fmt.Sscanf(value, "%d", &player.Players)
		case "score":
			fmt.Sscanf(value, "%d", &player.Score)
		case "level":
			fmt.Sscanf(value, "%d", &player.Level)
		case "pltfmid":
			player.PltfmID = value
		case "crossid":
			player.CrossID = value
		case "ip":
			player.IP = value
		case "ping":
			fmt.Sscanf(value, "%d", &player.Ping)
		}
	}

	return player, nil
}

// splitWithCommas splits a string by commas, respecting commas inside ()
func splitWithCommas(line string) []string {
	var parts []string
	var buffer strings.Builder
	var inside bool

	for _, char := range line {
		switch char {
		case ',':
			if inside {
				buffer.WriteRune(char)
			} else {
				parts = append(parts, buffer.String())
				buffer.Reset()
			}
		case '(':
			inside = true
			buffer.WriteRune(char)
		case ')':
			inside = false
			buffer.WriteRune(char)
		default:
			buffer.WriteRune(char)
		}
	}

	// Append last part
	parts = append(parts, buffer.String())

	// Trim spaces from parts
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	return parts
}

type Telnet7days struct {
	Env
	r    *bufio.Reader
	w    *bufio.Writer
	conn net.Conn
}

func (t *Telnet7days) close() error {
	// Send "exit" command to logout
	fmt.Fprintf(t.w, "exit\n")
	t.w.Flush()
	// Close the connection
	err := t.conn.Close()
	if err != nil {
		return fmt.Errorf("Failed to close connection: %v", err)
	}
	t.r = nil
	t.w = nil
	return nil
}
func (t *Telnet7days) connect() error {
	// Connect to the server
	var err error
	t.conn, err = net.DialTimeout("tcp", t.ServerAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("Failed to connect to server: %v", err)
	}
	t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// Create a telnet reader and writer
	t.r = bufio.NewReader(t.conn)
	t.w = bufio.NewWriter(t.conn)
	_, err = t.r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("Failed to read initial response: %v", err)
	}
	fmt.Fprintf(t.w, "%s\n", t.TelnetPass)
	t.w.Flush()

	// Read initial response after login
	loginResp, err := t.r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("Failed to read initial response: %v", err)
	}

	// Check if login was successful
	if !strings.Contains(loginResp, "Logon successful.") {
		log.Fatal("Login failed. Check your password.")
	}
	return nil
}

func (t *Telnet7days) exec(cmd string) error {
	// Send "lp" command to get player information
	fmt.Fprintf(t.w, "%s\n", cmd)
	t.w.Flush()
	// Read response with player information
	for {
		line, err := t.r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("Error reading cmd:'%s' init information: %v", cmd, err)
		}

		//log.Printf("line:'%s'", line)
		// Check if the response contains the command we executed
		if strings.Contains(line, fmt.Sprintf("INF Executing command '%s' by Telnet", cmd)) {
			break
		}
	}
	return nil
}

func (t *Telnet7days) GetPlayers() ([]Player, error) {
	if err := t.connect(); err != nil {
		return nil, err
	}
	defer t.close()

	if err := t.exec("lp"); err != nil {
		return nil, err
	}
	var players []Player
	for {
		line, err := t.r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("Error reading player data information: %v", err)
		}
		if strings.Contains(line, "Total of ") {
			break
		}
		log.Printf("line:'%s'", line)
		player, err := parsePlayerInfo(line)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse player information: %v", err)
		}
		players = append(players, player)

	}
	return players, nil
}
func (t *Telnet7days) GetTime() (GameTime, error) {
	res := GameTime{}
	if err := t.connect(); err != nil {
		return res, err
	}
	defer t.close()
	if err := t.exec("gt"); err != nil {
		return res, err
	}
	line, err := t.r.ReadString('\n')
	log.Printf("line:'%s'", line)
	if err != nil {
		return res, err
	}
	if !strings.HasPrefix(line, "Day ") {
		return res, fmt.Errorf("Failed to parse time: %s", line)
	}
	return parseGameTime(line)
}

type GameTime struct {
	Days    int `json:"days"`
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
}

func parseGameTime(timeStr string) (GameTime, error) {
	var gameTime GameTime

	// Split by comma and trim spaces
	parts := strings.Split(timeStr, ",")

	if len(parts) != 2 {
		return gameTime, fmt.Errorf("invalid time format: %s", timeStr)
	}

	// Parse days
	_, err := fmt.Sscanf(parts[0], "Day %d", &gameTime.Days)
	if err != nil {
		return gameTime, fmt.Errorf("failed to parse days: %v", err)
	}

	// Parse hours and minutes
	_, err = fmt.Sscanf(parts[1], "%d:%d", &gameTime.Hours, &gameTime.Minutes)
	if err != nil {
		return gameTime, fmt.Errorf("failed to parse hours and minutes: %v", err)
	}

	return gameTime, nil
}

/*
gt
2024-06-30T09:55:59 17446.408 INF Executing command 'gt' by Telnet from 10.8.0.1:52594
Day 17, 15:27
*/

/*
func getPlayers(e Env) []Player {
	// 7 Days to Die server telnet address and port

	// Connect to the server
	conn, err := net.DialTimeout("tcp", e.ServerAddr, 10*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer conn.Close()

	// Create a telnet reader and writer
	telnetReader := bufio.NewReader(conn)
	telnetWriter := bufio.NewWriter(conn)
	_, err = telnetReader.ReadString('\n')
	if err != nil {
		log.Fatalf("Failed to read initial response: %v", err)
	}
	fmt.Fprintf(telnetWriter, "%s\n", e.TelnetPass)
	telnetWriter.Flush()

	// Read initial response after login
	loginResp, err := telnetReader.ReadString('\n')
	if err != nil {
		log.Fatalf("Failed to read initial response: %v", err)
	}

	// Check if login was successful
	if !strings.Contains(loginResp, "Logon successful.") {
		log.Fatal("Login failed. Check your password.")
	}

	// Send "lp" command to get player information
	fmt.Fprintf(telnetWriter, "lp\n")
	telnetWriter.Flush()

	// Read response with player information
	var players []Player
	for {
		line, err := telnetReader.ReadString('\n')
		if err != nil {
			log.Fatalf("Error reading player information: %v", err)
		}

		if strings.Contains(line, "INF Executing command 'lp' by Telnet") {
			break
		}
	}
	for {
		line, err := telnetReader.ReadString('\n')
		if strings.Contains(line, "Total of ") {
			break
		}
		log.Printf("line:'%s'", line)
		player, err := parsePlayerInfo(line)
		if err != nil {
			log.Fatalf("Failed to parse player information: %v", err)
		}
		players = append(players, player)

	}

	// Send "exit" command to logout
	fmt.Fprintf(telnetWriter, "exit\n")
	telnetWriter.Flush()

	log.Printf("players:%s", jsonDump(players))
}
*/
