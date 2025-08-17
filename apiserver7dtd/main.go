package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"
)

// --- 静的ドキュメント（任意）：OpenAPI / docs ---
//
//go:embed openapi.yaml
var docsFS embed.FS

// =====================
// 設定: envconfig 対応
// =====================
type Config struct {
	// リッスンアドレス（例 :8080）
	APIAddr string `envconfig:"API_ADDR" default:":8080"`

	// /docs/openapi.yaml の servers: を上書き
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
	StartCmd   string `envconfig:"START_CMD" default:"/usr/bin/systemctl start 7dtd.service"`
	StopCmd    string `envconfig:"STOP_CMD" default:"/usr/bin/systemctl stop 7dtd.service"`
	RestartCmd string `envconfig:"RESTART_CMD" default:"/usr/bin/systemctl restart 7dtd.service"`
}

// グローバル設定（テスト互換のため維持）
var appCfg = Config{
	APIAddr:           ":8080",
	ReadHeaderTimeout: 5 * time.Second,
	GlobalTimeout:     30 * time.Second,
	StartCmd:          "/usr/bin/systemctl start 7dtd.service",
	StopCmd:           "/usr/bin/systemctl stop 7dtd.service",
	RestartCmd:        "/usr/bin/systemctl restart 7dtd.service",
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
	Running     bool   `json:"running"`
	CurrentJob  string `json:"currentJob,omitempty"`
	PlayerCount int    `json:"playerCount"`
}

func startServer(ctx context.Context) (ExecResult, error) {
	return cmdRunner.Run(ctx, appCfg.StartCmd)
}
func stopServer(ctx context.Context) (ExecResult, error) {
	return cmdRunner.Run(ctx, appCfg.StopCmd)
}
func restartServer(ctx context.Context) (ExecResult, error) {
	return cmdRunner.Run(ctx, appCfg.RestartCmd)
}

func getStatus(ctx context.Context) ServerStatus {
	return ServerStatus{Running: true, CurrentJob: "job-123", PlayerCount: 12}
}
func getJob(ctx context.Context, id string) (any, int) {
	if id == "notfound" {
		return map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "job not found"}}, http.StatusNotFound
	}
	return map[string]string{"id": id, "state": "done"}, http.StatusOK
}

type summaryOpts struct {
	MaskIPs          bool
	IncludePositions bool
	LimitHostiles    int
	TimeoutSeconds   int
	Verbose          bool
}

func getSummary(ctx context.Context, opt summaryOpts) any {
	return map[string]any{
		"maskIPs":          opt.MaskIPs,
		"includePositions": opt.IncludePositions,
		"limitHostiles":    opt.LimitHostiles,
		"timeoutSeconds":   opt.TimeoutSeconds,
		"verbose":          opt.Verbose,
		"hostiles":         3,
	}
}

// =====================
// ハンドラ実装
// =====================

func health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func serverStatus(w http.ResponseWriter, r *http.Request) {
	st := getStatus(r.Context())
	writeJSON(w, http.StatusOK, st)
}

func serverStart(w http.ResponseWriter, r *http.Request) {
	res, err := startServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "COMMAND_FAILED",
				"message": err.Error(),
				"details": map[string]any{"exec": res},
			},
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "starting",
		"exec":   res,
	})
}

func serverStop(w http.ResponseWriter, r *http.Request) {
	res, err := stopServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "COMMAND_FAILED",
				"message": err.Error(),
				"details": map[string]any{"exec": res},
			},
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "stopping",
		"exec":   res,
	})
}

func serverRestart(w http.ResponseWriter, r *http.Request) {
	res, err := restartServer(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "COMMAND_FAILED",
				"message": err.Error(),
				"details": map[string]any{"exec": res},
			},
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "restarting",
		"exec":   res,
	})
}

func jobByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	payload, code := getJob(r.Context(), id)
	writeJSON(w, code, payload)
}

func serverSummary(w http.ResponseWriter, r *http.Request) {
	maskIPs, err := qBool(r, "maskIPs", false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	includePositions, err := qBool(r, "includePositions", false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limitHostiles, err := qInt(r, "limitHostiles", 100, 0, 10_000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	timeoutSec, err := qInt(r, "timeoutSeconds", 5, 0, 300)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	verbose, err := qBool(r, "verbose", false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	opt := summaryOpts{
		MaskIPs:          maskIPs,
		IncludePositions: includePositions,
		LimitHostiles:    limitHostiles,
		TimeoutSeconds:   timeoutSec,
		Verbose:          verbose,
	}

	ctx := r.Context()
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	result := getSummary(ctx, opt)
	writeJSON(w, http.StatusOK, result)
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
	mux.HandleFunc("POST /server/start", serverStart)
	mux.HandleFunc("POST /server/stop", serverStop)
	mux.HandleFunc("POST /server/restart", serverRestart)
	mux.HandleFunc("GET /jobs/{id}", jobByID)
	mux.HandleFunc("GET /server/summary", serverSummary)

	// OpenAPI の配信：servers を cfg / リクエストから解決して上書き
	mux.HandleFunc("GET /docs/openapi.yaml", openapiYAMLHandler(cfg))

	return chain(mux,
		recoverMW,
		logMW,
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
