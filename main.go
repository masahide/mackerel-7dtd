package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/mackerelio/mackerel-client-go"
	"github.com/masahide/mackerel-7dtd/pkg/telnet"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkMetric "go.opentelemetry.io/otel/sdk/metric"
)

const (
	stateDirName  = "sdtd-monitor"
	stateFileName = "sdtd-monitor"
)

type env struct {
	Debug          bool   `envconfig:"DEBUG" default:"false"`
	MackerelHostID string `envconfig:"MACKEREL_HOST_ID"`
	MackerelAPIKey string `envconfig:"MACKEREL_API_KEY"`
	telnet.Env
	// PlayersAPIURL    string `envconfig:"PLAYERS_API_URL" default:""`
	// PlayersAPISecret string `envconfig:"PLAYERS_API_SECRET" default:""`
	// PlayersAPIUser   string `envconfig:"PLAYERS_API_USER" default:""`
}

type MetricDetail struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	IsStacked   bool   `json:"isStacked"` // 対応するメトリックを積み上げ表示するかどうか。偽なら折れ線で表示されます。
}

type MetricDef struct {
	Name        string         `json:"name"` // メトリック名の最後の . より前の部分。 custom. ではじまる必要があります。また、ワイルドカード#, * を使用することもできます。
	DisplayName string         `json:"displayName"`
	Unit        string         `json:"unit"` // "float", "integer", "percentage", "seconds", "milliseconds", "bytes", "bytes/sec", "bits/sec", "iops"
	Metrics     []MetricDetail `json:"metrics"`
}

type MetricValue struct {
	HostID string  `json:"hostId"`
	Name   string  `json:"name"`
	Time   int64   `json:"time"`
	Value  float64 `json:"value"`
}

/*
// Player 構造体を更新して、新しいフィールドを含む
type Player struct {
	SteamID         string   `json:"steamid"`
	CrossplatformID string   `json:"crossplatformid"`
	EntityID        int      `json:"entityid"`
	IP              string   `json:"ip"`
	Name            string   `json:"name"`
	Online          bool     `json:"online"`
	Position        Position `json:"position"`
	Level           float64  `json:"level"`
	Health          float64  `json:"health"`
	Stamina         float64  `json:"stamina"`
	ZombieKills     float64  `json:"zombiekills"`
	PlayerKills     float64  `json:"playerkills"`
	PlayerDeaths    float64  `json:"playerdeaths"`
	Score           float64  `json:"score"`
	TotalPlayTime   int      `json:"totalplaytime"`
	LastOnline      string   `json:"lastonline"`
	Ping            int      `json:"ping"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}
*/

type mackerelAPI struct {
	env
	mkr       *mackerel.Client
	steamIDs  []string
	stateFile string
	t         *telnet.Telnet7days
}

func jsonDump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func reqDump(req *http.Request) string {
	reqDump, err := httputil.DumpRequest(req, true)
	if err != nil {
		log.Printf("Error dumping request.  err:%s", err)
		return ""
	}
	return string(reqDump)
}
func respDump(resp *http.Response) string {
	respDump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		log.Printf("Error dumping response.  err:%s", err)
		return ""
	}
	return string(respDump)
}

/*
func (m *mackerelAPI) getPlayersOnline() []Player {
	res := []Player{}
	req, err := http.NewRequest(http.MethodGet, m.PlayersAPIURL, nil)
	if err != nil {
		log.Fatalf("Error creating request.  err:%s", err)
		return res
	}
	// ヘッダーを追加
	req.Header.Add("X-SDTD-API-TOKENNAME", m.PlayersAPIUser)
	req.Header.Add("X-SDTD-API-SECRET", m.PlayersAPISecret)
	// HTTPクライアントを作成
	client := &http.Client{}
	// リクエストを送信
	resp, err := client.Do(req)
	if err != nil {
		if m.Debug {
			log.Printf("REQUEST:\n%s", reqDump(req))
		}
		log.Fatalf("Error sending request  err:%s", err)
		return res
	}
	defer resp.Body.Close()

	// レスポンスを読み込む
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if m.Debug {
			log.Printf("RESPONSE:\n%s", respDump(resp))
		}
		log.Fatalf("Error reading response body: %s", err)
		return res
	}
	// JSONをパース
	err = json.Unmarshal(body, &res)
	err = json.Unmarshal(body, &res)
	if err != nil {
		if m.Debug {
			log.Printf("RESPONSE:\n%s", respDump(resp))
		}
		log.Fatalf("Error parsing JSON:%s ", err)
	}
	return res
}
*/

func trimSteam(steamID string) string {
	return strings.TrimPrefix(steamID, "Steam_")
}

func (m *mackerelAPI) createMetrics(players []telnet.Player, now time.Time) []*mackerel.MetricValue {
	res := make([]*mackerel.MetricValue, 0, len(players)*4)
	for _, player := range players {
		id := trimSteam(player.PltfmID)
		res = append(res, &mackerel.MetricValue{
			Name:  "custom.player.level." + id,
			Time:  now.Unix(),
			Value: player.Level,
		})
		res = append(res, &mackerel.MetricValue{
			Name:  "custom.player.x." + id,
			Time:  now.Unix(),
			Value: player.Position.X,
		})
		res = append(res, &mackerel.MetricValue{
			Name:  "custom.player.y." + id,
			Time:  now.Unix(),
			Value: player.Position.Y,
		})
		/*
			res = append(res, &mackerel.MetricValue{
				Name:  "custom.player.totalplaytime." + id,
				Time:  now.Unix(),
				Value: float64(player.TotalPlayTime),
			})
		*/
	}
	return res
}

func (m *mackerelAPI) postGraphDef(data []MetricDef) {
	url := "https://api.mackerelio.com/api/v0/graph-defs/create"
	m.post(url, data)
}

/*
func (m *mackerelAPI) postMetrics(data []MetricValue) {
	url := "https://api.mackerelio.com/api/v0/tsdb"
	m.post(url, m.setHostID(data))
}
*/

func (m *mackerelAPI) post(url string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Fatalf("Error marshaling metrics: %v", err)
	}
	if m.Debug {
		log.Printf("Posting metrics to url:%s: %s", url, jsonData)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Fatalf("Error creating request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", m.MackerelAPIKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("REQUEST:\n%s", reqDump(req))
		log.Fatalf("Error posting metrics to Mackerel: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("REQUEST:\n%s", reqDump(req))
		log.Printf("RESPONSE:\n%s", respDump(resp))
		log.Fatalf("Received non-200 response: %d", resp.StatusCode)
	}

	log.Println("Metrics posted successfully")
}

func getSteamIDs(players []telnet.Player) []string {
	ids := make([]string, len(players))
	for i, player := range players {
		ids[i] = trimSteam(player.PltfmID)
	}
	return ids
}

func compeareSteamIDs(steamIDs1, steamIDs2 []string) bool {
	if len(steamIDs1) != len(steamIDs2) {
		return false
	}
	for i := range steamIDs1 {
		if steamIDs1[i] != steamIDs2[i] {
			return false
		}
	}
	return true
}

func normalizeDisplayName(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

func makeDef(players []telnet.Player) []MetricDef {
	metricDefs := make([]MetricDef, 0, len(players)*4)
	for _, player := range players {
		id := trimSteam(player.PltfmID)
		metricDefs = append(metricDefs, MetricDef{
			Name:        "custom.player.level",
			DisplayName: "レベル",
			Unit:        "integer",
			Metrics: []MetricDetail{
				{
					Name:        "custom.player.level." + id,
					DisplayName: normalizeDisplayName(player.Name),
					IsStacked:   false,
				},
			},
		})
		metricDefs = append(metricDefs, MetricDef{
			Name:        "custom.player.x",
			DisplayName: "位置X",
			Unit:        "float",
			Metrics: []MetricDetail{
				{
					Name:        "custom.player.x." + id,
					DisplayName: normalizeDisplayName(player.Name),
					IsStacked:   false,
				},
			},
		})
		metricDefs = append(metricDefs, MetricDef{
			Name:        "custom.player.y",
			DisplayName: "位置Y",
			Unit:        "float",
			Metrics: []MetricDetail{
				{
					Name:        "custom.player.y." + id,
					DisplayName: normalizeDisplayName(player.Name),
					IsStacked:   false,
				},
			},
		})
		metricDefs = append(metricDefs, MetricDef{
			Name:        "custom.player.totalplaytime",
			DisplayName: "プレイ時間",
			Unit:        "seconds",
			Metrics: []MetricDetail{
				{
					Name:        "custom.player.totalplaytime." + id,
					DisplayName: normalizeDisplayName(player.Name),
					IsStacked:   false,
				},
			},
		})
	}
	return metricDefs
}

func (m *mackerelAPI) job() []telnet.Player {

	players, err := m.t.GetPlayers()
	if err != nil {
		log.Printf("Error getting players: %s", err)
		return nil
	}
	ids := getSteamIDs(players)
	if len(ids) == 0 {
		if m.Debug {
			log.Println("No players online")
		}
		return []telnet.Player{}
	}
	if !compeareSteamIDs(m.steamIDs, ids) {
		m.postGraphDef(makeDef(players))
		m.steamIDs = ids
		if err := saveState(m.stateFile, m.steamIDs); err != nil {
			log.Println(err)
		}
	}
	metrics := m.createMetrics(players, time.Now())
	if m.Debug {
		log.Println(jsonDump(metrics))
		return players
	}
	err = m.mkr.PostHostMetricValuesByHostID(m.MackerelHostID, metrics)
	if err != nil {
		log.Println(err)
	}
	return players
}
func readState(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}
func saveState(file string, v any) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	e := env{}
	if err := envconfig.Process("", &e); err != nil {
		log.Fatal(err)
	}
	tmpDir := os.TempDir()
	uid := os.Getuid()
	dir := filepath.Join(tmpDir, fmt.Sprintf("%s_%d", stateDirName, uid))
	fpath := filepath.Join(dir, stateFileName)
	mkr := &mackerelAPI{e, mackerel.NewClient(e.MackerelAPIKey), []string{}, fpath,
		&telnet.Telnet7days{
			Env: e.Env,
		},
	}
	os.MkdirAll(dir, 0755)
	if err := readState(fpath, &mkr.steamIDs); err != nil {
		mkr.steamIDs = []string{}
		saveState(fpath, mkr.steamIDs)
		log.Printf("Create State file: %s", fpath)
	}
	players := mkr.job()
	putOtelMetrics(players)
}

func setupMeter() (metric.Meter, func()) {
	//endpoint := "https://otlp-gateway-prod-ap-southeast-0.grafana.net/otlp/v1/metrics"
	//authHeader := "Basic " + os.Getenv("OTEL_AUTH_BASIC") // <- 事前に base64 を環境で用意

	exp, err := otlpmetrichttp.New(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	//
	reader := sdkMetric.NewPeriodicReader(exp, sdkMetric.WithInterval(24*time.Hour))
	mp := sdkMetric.NewMeterProvider(sdkMetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	return mp.Meter("sdtd"), func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			log.Fatalf("shutdown: %v", err)
		}
	}
}

func putOtelMetrics(players []telnet.Player) {
	meter, shutdown := setupMeter()
	defer shutdown()

	// ObservableGauge を登録：収集タイミング毎にコールバックで現在値を返す
	levelGauge, _ := meter.Float64ObservableGauge("sdtd.player.level")
	posXGauge, _ := meter.Float64ObservableGauge("sdtd.player.pos_x")
	posYGauge, _ := meter.Float64ObservableGauge("sdtd.player.pos_y")

	serverAttr := attribute.String("server", "my7dtd")

	_, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		for _, p := range players {
			steam := strings.TrimPrefix(p.PltfmID, "Steam_")
			attrs := []attribute.KeyValue{
				serverAttr,
				attribute.String("steam_id", steam),
				attribute.String("name", p.Name),
			}
			o.ObserveFloat64(levelGauge, float64(p.Level), metric.WithAttributeSet(attribute.NewSet(attrs...)))
			o.ObserveFloat64(posXGauge, p.Position.X, metric.WithAttributeSet(attribute.NewSet(attrs...)))
			o.ObserveFloat64(posYGauge, p.Position.Y, metric.WithAttributeSet(attribute.NewSet(attrs...)))
		}
		return nil
	}, levelGauge, posXGauge, posYGauge)
	if err != nil {
		log.Fatal(err)
	}
}
