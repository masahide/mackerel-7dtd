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
}

// グローバル設定（テスト互換のため維持）
var appCfg = Config{
	APIAddr:           ":8080",
	ReadHeaderTimeout: 5 * time.Second,
	GlobalTimeout:     30 * time.Second,
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
// ドメイン層のダミー
// =====================
type ServerStatus struct {
	Running     bool   `json:"running"`
	CurrentJob  string `json:"currentJob,omitempty"`
	PlayerCount int    `json:"playerCount"`
}

func startServer(ctx context.Context) error   { return nil }
func stopServer(ctx context.Context) error    { return nil }
func restartServer(ctx context.Context) error { return nil }
func getStatus(ctx context.Context) ServerStatus {
	return ServerStatus{Running: true, CurrentJob: "job-123", PlayerCount: 12}
}
func getJob(ctx context.Context, id string) (any, int) {
	// return payload, httpStatus
	if id == "notfound" {
		return map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "job not found"}}, http.StatusNotFound
	}
	// 仕様上は Job スキーマが必要だが、ここはサンプルなので簡略
	return map[string]string{"id": id, "state": "done"}, http.StatusOK
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

type summaryOpts struct {
	MaskIPs          bool
	IncludePositions bool
	LimitHostiles    int
	TimeoutSeconds   int
	Verbose          bool
}

// =====================
// ハンドラ実装
// =====================
func health(w http.ResponseWriter, r *http.Request) {
	// OpenAPI に合わせて {"ok": true}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func serverStatus(w http.ResponseWriter, r *http.Request) {
	st := getStatus(r.Context())
	writeJSON(w, http.StatusOK, st)
}

func serverStart(w http.ResponseWriter, r *http.Request) {
	if err := startServer(r.Context()); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "CONFLICT", "message": err.Error()}})
		return
	}
	// 本来は 202 + Job を返す（ここは簡略サンプル）
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "starting"})
}

func serverStop(w http.ResponseWriter, r *http.Request) {
	if err := stopServer(r.Context()); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "CONFLICT", "message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
}

func serverRestart(w http.ResponseWriter, r *http.Request) {
	if err := restartServer(r.Context()); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "CONFLICT", "message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
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

	// オプションのタイムアウトを適用（業務処理が重い場合）
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

	// メソッド＋パスのパターン（Go1.22+）
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
		timeoutMW(cfg.GlobalTimeout), // 全体のフェイルセーフ
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
		// YAML -> map に一旦落とす
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			http.Error(w, fmt.Sprintf("openapi yaml parse error: %v", err), http.StatusInternalServerError)
			return
		}
		// servers を決定
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
		// 再度 YAML 化して返す
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
	// 1) 明示指定（カンマ区切り → envconfig は自動で []string 化）
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
	// 2) PublicBaseURL
	if strings.TrimSpace(cfg.PublicBaseURL) != "" {
		return []string{strings.TrimSpace(cfg.PublicBaseURL)}
	}
	// 3) リクエストから推定
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
