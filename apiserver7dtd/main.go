package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"
)

// --- 7dtd REST API レスポンス用（最小限） ---
type apiGameTime struct {
	Days    int `json:"days"`
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
}

type apiServerStatsData struct {
	GameTime apiGameTime `json:"gameTime"`
	Players  int         `json:"players"`
	Hostiles int         `json:"hostiles"`
	Animals  *int        `json:"animals"`
}
type apiServerStatsResp struct {
	Data apiServerStatsData `json:"data"`
	Meta struct {
		ServerTime string `json:"serverTime"`
	} `json:"meta"`
}

type apiPlayer struct {
	EntityID int      `json:"entityId"`
	Name     string   `json:"name"`
	Online   bool     `json:"online"`
	IP       string   `json:"ip"`
	Ping     *int     `json:"ping"`
	Level    *int     `json:"level"`
	Health   *float64 `json:"health"`
	Stamina  *float64 `json:"stamina"`
	Score    *int     `json:"score"`
	Deaths   *int     `json:"deaths"`
	Position *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"position"`
	PlatformID *struct {
		PlatformID     string `json:"platformId"`
		UserID         string `json:"userId"`
		CombinedString string `json:"combinedString"`
	} `json:"platformId"`
	CrossplatformID *struct {
		PlatformID     string `json:"platformId"`
		UserID         string `json:"userId"`
		CombinedString string `json:"combinedString"`
	} `json:"crossplatformId"`
	Kills *struct {
		Zombies *int `json:"zombies"`
		Players *int `json:"players"`
	} `json:"kills"`
	Banned *struct {
		BanActive bool    `json:"banActive"`
		Reason    *string `json:"reason"`
		Until     *string `json:"until"`
	} `json:"banned"`
}

type apiPlayersResp struct {
	Data struct {
		Players []apiPlayer `json:"players"`
	} `json:"data"`
	Meta struct {
		ServerTime string `json:"serverTime"`
	} `json:"meta"`
}

type apiHostile struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Position struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"position"`
}
type apiHostilesResp struct {
	Data []apiHostile `json:"data"`
	Meta struct {
		ServerTime string `json:"serverTime"`
	} `json:"meta"`
}

// --- ソース計測 ---
type sourceProbe struct {
	Name      string
	OK        bool
	LatencyMs int64
	ErrMsg    string
}

// --- 静的ドキュメント（任意）：OpenAPI / docs ---
//
//go:embed openapi.yaml
var docsFS embed.FS

// =====================
// 設定: envconfig 対応
// =====================
type Config struct {
	APIAddr string `envconfig:"API_ADDR" default:":8088"`

	// 例: "https://ops.example.com,https://ops2.example.com"
	OpenAPIServers []string `envconfig:"OPENAPI_SERVERS"`
	// 単一の公開URL。OpenAPIServers が空のときに使用。
	// 例: "https://ops.example.com"
	PublicBaseURL string `envconfig:"PUBLIC_BASE_URL"`
	// サーバーの ReadHeaderTimeout
	ReadHeaderTimeout time.Duration `envconfig:"READ_HEADER_TIMEOUT" default:"5s"`
	// 全体のフェイルセーフ・タイムアウト（ミドルウェア）
	GlobalTimeout time.Duration `envconfig:"GLOBAL_TIMEOUT" default:"30s"`

	// 実行する Linux コマンド（sh -c で実行）
	StartCmd  string `envconfig:"START_CMD" default:"ssh 7dtd01 docker compose -f /home/7dtd/docker-compose.yml up -d"`
	StopCmd   string `envconfig:"STOP_CMD" default:"/usr/bin/systemctl stop 7dtd.service"`
	StatusCmd string `envconfig:"STATUS_CMD" default:"ssh 7dtd01 'docker compose -f /home/7dtd/docker-compose.yml ps"`
	// Dockerログ取得用ベースコマンド（tailは付けない）。
	// 例: ssh 7dtd01 'docker compose -f /home/7dtd/docker-compose.yml logs'
	LogsCmd            string `envconfig:"LOGS_CMD" default:"ssh 7dtd01 'docker compose -f /home/7dtd/docker-compose.yml logs'"`
	ComposeServiceName string `envconfig:"COMPOSE_SERVICE" default:"7dtdserver"`

	APIBaseURL string `envconfig:"API_BASE_URL"  default:"http://127.0.0.1:8088/api"`
	APIUser    string `envconfig:"API_USER"  default:""`
	APISecret  string `envconfig:"API_SECRET" default:""`

	AuthBearerToken string `envconfig:"AUTH_BEARER_TOKEN"`             // 例: 長いランダム文字列
	APIKey          string `envconfig:"API_KEY"`                       // 例: 代替のAPIキー(任意)
	AllowNoAuth     bool   `envconfig:"ALLOW_NO_AUTH" default:"false"` // 一時無効化用
}

// グローバル設定（テスト互換のため維持）
var appCfg = Config{
	/*
		APIAddr:            ":8088",
		ReadHeaderTimeout:  5 * time.Second,
		GlobalTimeout:      30 * time.Second,
		StartCmd:           "ssh 7dtd01 docker compose -f /home/7dtd/docker-compose.yml up -d",
		StopCmd:            "/usr/bin/systemctl stop 7dtd.service",
		StatusCmd:          "ssh 7dtd01 docker compose -f /home/7dtd/docker-compose.yml ps",
		ComposeServiceName: "7dtdserver",
	*/
}

// 環境変数から読み込む（prefix=OPSA）
func loadConfigFromEnv() (Config, error) {
	cfg := appCfg // 既定値をベースに
	if err := envconfig.Process("OPSA", &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// =====================
// ミドルウェア薄層
// =====================
type Middleware func(http.Handler) http.Handler

func chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				log.Printf("[PANIC] %v", rec)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &respWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, ww.status, time.Since(start))
	})
}

type respWriter struct {
	http.ResponseWriter
	status int
}

func (w *respWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func timeoutMW(d time.Duration) Middleware {
	if d <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, http.StatusText(http.StatusGatewayTimeout))
	}
}

// =====================
/* コマンド実行ランナー（テスト差し替え可能） */
// =====================

type ExecResult struct {
	Command    string    `json:"command"`
	ExitCode   int       `json:"exitCode"`
	Output     string    `json:"output"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	DurationMs int64     `json:"durationMs"`
}

// --- Response DTOs (OpenAPI準拠) ---
type OperationResult struct {
	Status string     `json:"status"`
	Note   *string    `json:"note,omitempty"`
	Exec   ExecResult `json:"exec"`
}

type RestartExec struct {
	Stop  ExecResult `json:"stop"`
	Start ExecResult `json:"start"`
}

type RestartOperationResult struct {
	Status string      `json:"status"`
	Exec   RestartExec `json:"exec"`
}

// --- Common/Error/Health DTOs ---
type HealthResponse struct {
	OK bool `json:"ok"`
}

type ErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// --- Logs DTOs (exec.output omitted as lines are returned in data) ---
type ExecMeta struct {
	Command    string    `json:"command"`
	ExitCode   int       `json:"exitCode"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	DurationMs int64     `json:"durationMs"`
}
type ServerLogsData struct {
	Lines []string `json:"lines"`
}
type ServerLogsMeta struct {
	Exec ExecMeta `json:"exec"`
}
type ServerLogsResponse struct {
	Data ServerLogsData `json:"data"`
	Meta ServerLogsMeta `json:"meta"`
}

// --- Summary DTOs ---
type SummaryGameTime struct {
	Days    int `json:"days"`
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
}
type SummaryStats struct {
	GameTime      SummaryGameTime `json:"gameTime"`
	PlayersOnline int             `json:"playersOnline"`
	Hostiles      int             `json:"hostiles"`
	Animals       *int            `json:"animals"`
}
type SummaryPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}
type SummaryID struct {
	PlatformID     string `json:"platformId"`
	UserID         string `json:"userId"`
	CombinedString string `json:"combinedString"`
}
type SummaryKills struct {
	Zombies *int `json:"zombies"`
	Players *int `json:"players"`
}
type SummaryBanned struct {
	BanActive bool    `json:"banActive"`
	Reason    *string `json:"reason"`
	Until     *string `json:"until"`
}
type SummaryPlayer struct {
	EntityID        int              `json:"entityId"`
	Name            string           `json:"name"`
	PlatformID      *SummaryID       `json:"platformId,omitempty"`
	CrossplatformID *SummaryID       `json:"crossplatformId,omitempty"`
	Online          bool             `json:"online"`
	IP              string           `json:"ip,omitempty"`
	Ping            *int             `json:"ping,omitempty"`
	Position        *SummaryPosition `json:"position,omitempty"`
	Level           *int             `json:"level,omitempty"`
	Health          *float64         `json:"health,omitempty"`
	Stamina         *float64         `json:"stamina,omitempty"`
	Score           *int             `json:"score,omitempty"`
	Deaths          *int             `json:"deaths,omitempty"`
	Kills           *SummaryKills    `json:"kills,omitempty"`
	Banned          *SummaryBanned   `json:"banned,omitempty"`
}
type SummaryHostile struct {
	ID       int              `json:"id"`
	Name     string           `json:"name"`
	Position *SummaryPosition `json:"position,omitempty"`
}
type SummarySource struct {
	Name      string  `json:"name"`
	OK        bool    `json:"ok"`
	LatencyMs *int64  `json:"latencyMs,omitempty"`
	Error     *string `json:"error,omitempty"`
}
type ServerSummaryData struct {
	Status   ServerStatus     `json:"status"`
	Stats    SummaryStats     `json:"stats"`
	Players  []SummaryPlayer  `json:"players"`
	Hostiles []SummaryHostile `json:"hostiles"`
}
type ServerSummaryMeta struct {
	ServerTime string          `json:"serverTime"`
	Partial    bool            `json:"partial"`
	Sources    []SummarySource `json:"sources,omitempty"`
}
type ServerSummaryResponse struct {
	Data ServerSummaryData `json:"data"`
	Meta ServerSummaryMeta `json:"meta"`
}

type CommandRunner interface {
	Run(ctx context.Context, command string) (ExecResult, error)
}

// 既定ランナー：sh -c で実行し CombinedOutput（stdout+stderr）を返す
type ShellRunner struct{}

func (ShellRunner) Run(ctx context.Context, command string) (ExecResult, error) {
	res := ExecResult{
		Command:   command,
		StartedAt: time.Now(),
	}
	defer func() {
		res.FinishedAt = time.Now()
		res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	}()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	out, err := cmd.CombinedOutput() // ← 2>&1 相当（結合出力）
	res.Output = string(out)
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		res.ExitCode = -1
	}
	return res, err
}

// グローバルに差し替え可能（テストで fake に入れ替える）
var cmdRunner CommandRunner = ShellRunner{}

// =====================
// 共通ヘルパー
// =====================
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func qBool(r *http.Request, key string, def bool) (bool, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def, nil
	}
	switch s {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	default:
		return false, errors.New("invalid bool for " + key)
	}
}

func qInt(r *http.Request, key string, def, min, max int) (int, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if v < min || v > max {
		return 0, errors.New("out of range for " + key)
	}
	return v, nil
}

// =====================
// ドメイン層（ダミー＋コマンド実行）
// =====================

type ServerStatus struct {
	ServiceName   string     `json:"serviceName"`
	State         string     `json:"state"` // enum: stopped|starting|running|stopping|failed|unknown
	Pid           *int       `json:"pid,omitempty"`
	UptimeSeconds *int       `json:"uptimeSeconds,omitempty"`
	LastStartedAt *time.Time `json:"lastStartedAt,omitempty"`
	Notes         string     `json:"notes,omitempty"`
}

func startServer(ctx context.Context) (ExecResult, error) {
	return cmdRunner.Run(ctx, appCfg.StartCmd)
}
func stopServer(ctx context.Context) (ExecResult, error) {
	return cmdRunner.Run(ctx, appCfg.StopCmd)
}

func getStatus(ctx context.Context) ServerStatus {
	// ssh + docker compose ps（stderr含む結合出力）
	res, _ := cmdRunner.Run(ctx, appCfg.StatusCmd)
	state, note := parseComposePS(res.Output, appCfg.ComposeServiceName)
	return ServerStatus{
		ServiceName: appCfg.ComposeServiceName,
		State:       state,
		Notes:       note,
	}
}

// =====================
// ハンドラ実装
// =====================

func health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{OK: true})
}

func serverStatus(w http.ResponseWriter, r *http.Request) {
	st := getStatus(r.Context())
	writeJSON(w, http.StatusOK, st)
}

// 直近ログ取得: LOGS_CMD を実行し、末尾 lines 件を返す
func serverLogs(w http.ResponseWriter, r *http.Request) {
	lines, err := qInt(r, "lines", 20, 1, 2000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: ErrorDetail{Code: "INVALID_PARAM", Message: err.Error()}})
		return
	}
	// tail -n は呼び出し側で付与する
	cmd := fmt.Sprintf("%s | tail -n %d'", strings.TrimRight(appCfg.LogsCmd, "'"), lines)
	res, runErr := cmdRunner.Run(r.Context(), cmd)
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: ErrorDetail{Code: "COMMAND_FAILED", Message: runErr.Error(), Details: map[string]any{"exec": res}}})
		return
	}
	// 出力を行単位に分割
	out := strings.Split(res.Output, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	// meta.exec からは output を省略（data.lines に格納済みのため冗長）
	resp := ServerLogsResponse{
		Data: ServerLogsData{Lines: out},
		Meta: ServerLogsMeta{Exec: ExecMeta{
			Command:    res.Command,
			ExitCode:   res.ExitCode,
			StartedAt:  res.StartedAt,
			FinishedAt: res.FinishedAt,
			DurationMs: res.DurationMs,
		}},
	}
	writeJSON(w, http.StatusOK, resp)
}

// =====================
// ルーティング/起動
// =====================

// 既存テスト互換のため routes() を残す（appCfg を使用）
func routes() http.Handler {
	return buildRoutes(appCfg)
}

func buildRoutes(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /server/status", serverStatus)
	mux.HandleFunc("GET /server/summary", serverSummaryHandler(cfg))
	mux.HandleFunc("GET /server/logs", serverLogs)
	mux.HandleFunc("POST /server/start", serverStart)
	mux.HandleFunc("POST /server/stop", serverStop)
	mux.HandleFunc("POST /server/restart", serverRestart)

	// OpenAPI の配信：servers を cfg / リクエストから解決して上書き
	mux.HandleFunc("GET /docs/openapi.yaml", openapiYAMLHandler(cfg))

	return chain(mux,
		recoverMW,
		logMW,
		authMW(cfg.AuthBearerToken, cfg.APIKey, cfg.AllowNoAuth),
		timeoutMW(cfg.GlobalTimeout),
	)
}

// OpenAPI servers 差し替えハンドラ（envconfig 経由の cfg を使用）
func openapiYAMLHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := docsFS.ReadFile("openapi.yaml")
		if err != nil {
			http.Error(w, fmt.Sprintf("openapi not found: %v", err), http.StatusInternalServerError)
			return
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			http.Error(w, fmt.Sprintf("openapi yaml parse error: %v", err), http.StatusInternalServerError)
			return
		}
		srvs := resolveOpenAPIServersFromCfg(cfg, r)
		servers := make([]map[string]any, 0, len(srvs))
		for _, u := range srvs {
			if u == "" {
				continue
			}
			servers = append(servers, map[string]any{"url": u})
		}
		if len(servers) > 0 {
			doc["servers"] = servers
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			http.Error(w, fmt.Sprintf("openapi yaml marshal error: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

func resolveOpenAPIServersFromCfg(cfg Config, r *http.Request) []string {
	if len(cfg.OpenAPIServers) > 0 {
		var out []string
		for _, s := range cfg.OpenAPIServers {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if strings.TrimSpace(cfg.PublicBaseURL) != "" {
		return []string{strings.TrimSpace(cfg.PublicBaseURL)}
	}
	scheme := "http"
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	return []string{scheme + "://" + host}
}

func main() {
	var err error
	appCfg, err = loadConfigFromEnv()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	srv := &http.Server{
		Addr:              appCfg.APIAddr,
		Handler:           buildRoutes(appCfg),
		ReadHeaderTimeout: appCfg.ReadHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

// "Up" 等の判定に使う
var (
	upWord         = regexp.MustCompile(`(?i)\bUp\b`)
	exitedWord     = regexp.MustCompile(`(?i)\bExited\b`)
	pausedWord     = regexp.MustCompile(`(?i)\bPaused\b`)
	restartingWord = regexp.MustCompile(`(?i)\bRestarting\b`)
)

// docker compose ps の出力から、対象サービス行を見つけて state/notes を返す
func parseComposePS(output, service string) (state string, notes string) {
	log.Printf("output: %s", output)
	if strings.TrimSpace(output) == "" || strings.TrimSpace(service) == "" {
		return "unknown", ""
	}
	lines := strings.Split(output, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// warning やヘッダをスキップ
		if strings.HasPrefix(line, "time=") || strings.HasPrefix(line, "NAME ") {
			continue
		}
		if !strings.Contains(line, service) {
			continue
		}
		// 見つかったサービス行で状態判定
		switch {
		case upWord.MatchString(line):
			return "running", line
		case restartingWord.MatchString(line):
			return "starting", line // 見せ方は運用に応じて
		case pausedWord.MatchString(line):
			return "stopped", line
		case exitedWord.MatchString(line):
			return "stopped", line
		default:
			return "unknown", line
		}
	}
	return "unknown", ""
}

// --- docker compose 出力判定用 正規表現 ---
var (
	reWordStarted  = regexp.MustCompile(`(?i)\bStarted\b`)
	reWordRunning  = regexp.MustCompile(`(?i)\bRunning\b`)
	reWordStopping = regexp.MustCompile(`(?i)\bStopping\b`)
	reWordStopped  = regexp.MustCompile(`(?i)\bStopped\b`)
	reWordRemoved  = regexp.MustCompile(`(?i)\bRemoved\b`)
)

// 先頭の warning 行: time="... level=warning ..." は無視したい
func isWarningHeader(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "time=")
}

// "docker compose up -d" の出力から起動結果を判定
// - "Started" を含む行があれば => "started"
// - （Started がなく）"Running" のみあれば => "already_running"
// - それ以外 => "starting"
func detectStartStatus(output string) (status string, notes string) {
	if strings.TrimSpace(output) == "" {
		return "starting", ""
	}
	var startedLines, runningLines []string
	for _, raw := range strings.Split(output, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || isWarningHeader(l) {
			continue
		}
		if reWordStarted.MatchString(l) {
			startedLines = append(startedLines, l)
		} else if reWordRunning.MatchString(l) {
			runningLines = append(runningLines, l)
		}
	}
	switch {
	case len(startedLines) > 0:
		return "started", strings.Join(startedLines, "\n")
	case len(runningLines) > 0:
		return "already_running", strings.Join(runningLines, "\n")
	default:
		return "starting", ""
	}
}

// "docker compose down" の出力から停止結果を判定
// - "Removed" or "Stopped" を含む行があれば => "stopped"
// - 有効な行が何も無ければ（warning だけ等） => "already_stopped"
// - それ以外 => "stopping"
func detectStopStatus(output string) (status string, notes string) {
	if strings.TrimSpace(output) == "" {
		return "already_stopped", ""
	}
	var stoppedOrRemoved []string
	anyNonWarning := false
	for _, raw := range strings.Split(output, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || isWarningHeader(l) {
			continue
		}
		anyNonWarning = true
		if reWordRemoved.MatchString(l) || reWordStopped.MatchString(l) {
			stoppedOrRemoved = append(stoppedOrRemoved, l)
		}
	}
	switch {
	case len(stoppedOrRemoved) > 0:
		return "stopped", strings.Join(stoppedOrRemoved, "\n")
	case !anyNonWarning:
		return "already_stopped", ""
	default:
		return "stopping", ""
	}
}

// 再起動は stop → start を順に実行し、それぞれの出力を返す
type RestartResult struct {
	Stop  ExecResult `json:"stop"`
	Start ExecResult `json:"start"`
}

func restartServer(ctx context.Context) (RestartResult, error) {
	stopRes, _ := stopServer(ctx) // down は既に止まっていてもOK
	time.Sleep(5 * time.Second)   // 少し待つ（必要に応じて調整）
	startRes, startErr := startServer(ctx)
	return RestartResult{Stop: stopRes, Start: startRes}, startErr
}

func serverStart(w http.ResponseWriter, r *http.Request) {
	res, err := startServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: ErrorDetail{Code: "COMMAND_FAILED", Message: err.Error(), Details: map[string]any{"exec": res}}})
		return
	}
	st, note := detectStartStatus(res.Output)
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	payload := OperationResult{
		Status: st,
		Note:   notePtr,
		Exec:   res,
	}
	writeJSON(w, http.StatusOK, payload)
}

func serverStop(w http.ResponseWriter, r *http.Request) {
	res, err := stopServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: ErrorDetail{Code: "COMMAND_FAILED", Message: err.Error(), Details: map[string]any{"exec": res}}})
		return
	}
	st, note := detectStopStatus(res.Output)
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	payload := OperationResult{
		Status: st,
		Note:   notePtr,
		Exec:   res,
	}
	writeJSON(w, http.StatusOK, payload)
}

func serverRestart(w http.ResponseWriter, r *http.Request) {
	res, err := restartServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: ErrorDetail{Code: "COMMAND_FAILED", Message: err.Error(), Details: map[string]any{"execStop": res.Stop, "execStart": res.Start}}})
		return
	}
	startStatus, _ := detectStartStatus(res.Start.Output)
	status := "restarted"
	if startStatus == "starting" {
		status = "restarting"
	}
	payload := RestartOperationResult{
		Status: status,
		Exec: RestartExec{
			Stop:  res.Stop,
			Start: res.Start,
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

// --- 簡易HTTP GET（ヘッダ付き） ---
func httpJSONGet(ctx context.Context, url, user, secret string, v any) (latencyMs int64, _err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if user != "" {
		req.Header.Set("X-SDTD-API-TOKENNAME", user)
	}
	if secret != "" {
		req.Header.Set("X-SDTD-API-SECRET", secret)
	}
	client := &http.Client{}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return latency, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return latency, fmt.Errorf("upstream %s status=%d body=%s", url, resp.StatusCode, string(b))
	}
	return latency, json.NewDecoder(resp.Body).Decode(v)
}

// --- IPマスク（例: 203.0.113.*） ---
func maskIP(ip string) string {
	if ip == "" {
		return ""
	}
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return fmt.Sprintf("%s.%s.%s.*", parts[0], parts[1], parts[2])
	}
	// IPv6や不正値は全面マスク
	return "***"
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func serverSummaryHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// OpenAPI 既定に合わせたクエリ既定値
		includePositions, err := qBool(r, "includePositions", true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		maskIPs, err := qBool(r, "maskIPs", true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limitHostiles, err := qInt(r, "limitHostiles", 200, 0, 2000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		timeoutSec, err := qInt(r, "timeoutSeconds", 5, 1, 15)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		verbose, err := qBool(r, "verbose", false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		if timeoutSec > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
			defer cancel()
		}

		// ← ここがポイント：cfg を使う（appCfg を使わない）
		base := strings.TrimRight(cfg.APIBaseURL, "/")
		urlStats := base + "/serverstats"
		urlPlayers := base + "/player"
		urlHostiles := base + "/hostile"

		var (
			stats     apiServerStatsResp
			players   apiPlayersResp
			hostiles  apiHostilesResp
			pStats    = sourceProbe{Name: "serverstats"}
			pPlayers  = sourceProbe{Name: "player"}
			pHostiles = sourceProbe{Name: "hostile"}
		)

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			lat, err := httpJSONGet(ctx, urlStats, cfg.APIUser, cfg.APISecret, &stats)
			pStats.LatencyMs = lat
			if err != nil {
				pStats.OK = false
				pStats.ErrMsg = err.Error()
				return
			}
			pStats.OK = true
		}()
		go func() {
			defer wg.Done()
			lat, err := httpJSONGet(ctx, urlPlayers, cfg.APIUser, cfg.APISecret, &players)
			pPlayers.LatencyMs = lat
			if err != nil {
				pPlayers.OK = false
				pPlayers.ErrMsg = err.Error()
				return
			}
			pPlayers.OK = true
		}()
		go func() {
			defer wg.Done()
			lat, err := httpJSONGet(ctx, urlHostiles, cfg.APIUser, cfg.APISecret, &hostiles)
			pHostiles.LatencyMs = lat
			if err != nil {
				pHostiles.OK = false
				pHostiles.ErrMsg = err.Error()
				return
			}
			pHostiles.OK = true
		}()
		wg.Wait()

		if !(pStats.OK || pPlayers.OK || pHostiles.OK) {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: ErrorDetail{
				Code:    "UPSTREAM_FAILED",
				Message: "all upstream sources failed",
				Details: map[string]any{"sources": []sourceProbe{pStats, pPlayers, pHostiles}},
			}})
			return
		}

		// compose の状態（ここは既存実装でOK）
		st := getStatus(ctx)

		animalsPtr := (*int)(nil)
		if pStats.OK {
			animalsPtr = stats.Data.Animals
		}
		statsObj := SummaryStats{
			GameTime: SummaryGameTime{
				Days: stats.Data.GameTime.Days, Hours: stats.Data.GameTime.Hours, Minutes: stats.Data.GameTime.Minutes,
			},
			PlayersOnline: stats.Data.Players,
			Hostiles:      stats.Data.Hostiles,
			Animals:       animalsPtr,
		}

		outPlayers := make([]SummaryPlayer, 0, len(players.Data.Players))
		if pPlayers.OK {
			for _, p := range players.Data.Players {
				ip := p.IP
				if maskIPs && ip != "" {
					ip = maskIP(ip)
				}

				var pos *SummaryPosition
				if includePositions && p.Position != nil {
					pos = &SummaryPosition{X: p.Position.X, Y: p.Position.Y, Z: p.Position.Z}
				}

				var platformID *SummaryID
				if p.PlatformID != nil {
					platformID = &SummaryID{PlatformID: p.PlatformID.PlatformID, UserID: p.PlatformID.UserID, CombinedString: p.PlatformID.CombinedString}
				}
				var crossID *SummaryID
				if p.CrossplatformID != nil {
					crossID = &SummaryID{PlatformID: p.CrossplatformID.PlatformID, UserID: p.CrossplatformID.UserID, CombinedString: p.CrossplatformID.CombinedString}
				}
				var kills *SummaryKills
				if p.Kills != nil {
					kills = &SummaryKills{Zombies: p.Kills.Zombies, Players: p.Kills.Players}
				}
				var banned *SummaryBanned
				if p.Banned != nil {
					banned = &SummaryBanned{BanActive: p.Banned.BanActive, Reason: p.Banned.Reason, Until: p.Banned.Until}
				}

				outPlayers = append(outPlayers, SummaryPlayer{
					EntityID:        p.EntityID,
					Name:            p.Name,
					PlatformID:      platformID,
					CrossplatformID: crossID,
					Online:          p.Online,
					IP:              ip,
					Ping:            p.Ping,
					Position:        pos,
					Level:           p.Level,
					Health:          p.Health,
					Stamina:         p.Stamina,
					Score:           p.Score,
					Deaths:          p.Deaths,
					Kills:           kills,
					Banned:          banned,
				})
			}
		}

		outHostiles := make([]SummaryHostile, 0, len(hostiles.Data))
		if pHostiles.OK {
			for i, h := range hostiles.Data {
				if i >= limitHostiles {
					break
				}
				var pos *SummaryPosition
				if includePositions {
					pos = &SummaryPosition{X: h.Position.X, Y: h.Position.Y, Z: h.Position.Z}
				}
				outHostiles = append(outHostiles, SummaryHostile{ID: h.ID, Name: h.Name, Position: pos})
			}
		}

		serverTime := stats.Meta.ServerTime
		if serverTime == "" {
			if players.Meta.ServerTime != "" {
				serverTime = players.Meta.ServerTime
			} else if hostiles.Meta.ServerTime != "" {
				serverTime = hostiles.Meta.ServerTime
			} else {
				serverTime = time.Now().UTC().Format(time.RFC3339)
			}
		}
		partial := !(pStats.OK && pPlayers.OK && pHostiles.OK)

		summary := ServerSummaryResponse{
			Data: ServerSummaryData{
				Status:   st,
				Stats:    statsObj,
				Players:  outPlayers,
				Hostiles: outHostiles,
			},
			Meta: ServerSummaryMeta{
				ServerTime: serverTime,
				Partial:    partial,
			},
		}
		if verbose {
			srcs := make([]SummarySource, 0, 3)
			if true {
				var lat *int64
				if pStats.LatencyMs > 0 {
					l := pStats.LatencyMs
					lat = &l
				}
				var er *string
				if pStats.ErrMsg != "" {
					e := pStats.ErrMsg
					er = &e
				}
				srcs = append(srcs, SummarySource{Name: pStats.Name, OK: pStats.OK, LatencyMs: lat, Error: er})
			}
			if true {
				var lat *int64
				if pPlayers.LatencyMs > 0 {
					l := pPlayers.LatencyMs
					lat = &l
				}
				var er *string
				if pPlayers.ErrMsg != "" {
					e := pPlayers.ErrMsg
					er = &e
				}
				srcs = append(srcs, SummarySource{Name: pPlayers.Name, OK: pPlayers.OK, LatencyMs: lat, Error: er})
			}
			if true {
				var lat *int64
				if pHostiles.LatencyMs > 0 {
					l := pHostiles.LatencyMs
					lat = &l
				}
				var er *string
				if pHostiles.ErrMsg != "" {
					e := pHostiles.ErrMsg
					er = &e
				}
				srcs = append(srcs, SummarySource{Name: pHostiles.Name, OK: pHostiles.OK, LatencyMs: lat, Error: er})
			}
			summary.Meta.Sources = srcs
		}

		writeJSON(w, http.StatusOK, summary)
	}
}

func authMW(bearerToken, apiKey string, allowNoAuth bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// docs は常に無認可でOK
			if strings.HasPrefix(r.URL.Path, "/docs/") {
				next.ServeHTTP(w, r)
				return
			}
			// （任意）/health を無認証にしたい場合はここでバイパス
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			if allowNoAuth {
				next.ServeHTTP(w, r)
				return
			}

			ok := false
			if bearerToken != "" {
				if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
					tok := strings.TrimPrefix(v, "Bearer ")
					if subtle.ConstantTimeCompare([]byte(tok), []byte(bearerToken)) == 1 {
						ok = true
					}
				}
			}
			if !ok && apiKey != "" {
				if v := r.Header.Get("X-API-Key"); subtle.ConstantTimeCompare([]byte(v), []byte(apiKey)) == 1 {
					ok = true
				}
			}

			if !ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				if bearerToken != "" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="7dtd-ops"`)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    "UNAUTHORIZED",
						"message": "missing or invalid credentials",
					},
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
