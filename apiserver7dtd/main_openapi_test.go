// main_openapi_test.go
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// main.go の //go:embed openapi.yaml を使う
var _ embed.FS = docsFS

// OpenAPI を読み込み、servers を baseURL に置換して Router を返す
func loadOpenAPISpecWithServer(t *testing.T, baseURL string) (*openapi3.T, routers.Router) {
	t.Helper()

	specBytes, err := docsFS.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	// ★ ここでテスト用サーバーURLに置換
	doc.Servers = openapi3.Servers{&openapi3.Server{URL: baseURL}}

	// 自己検証（OpenAPI 3.0.3 推奨）
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi validate self: %v", err)
	}

	rt, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return doc, rt
}

func newTestServer() *httptest.Server {
	return httptest.NewServer(routes())
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body io.Reader, hdr map[string]string) (*http.Request, *http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return req, resp, b
}

func validateResponseWithOpenAPI(t *testing.T, rt routers.Router, req *http.Request, resp *http.Response, body []byte) error {
	t.Helper()

	route, pathParams, err := rt.FindRoute(req)
	if err != nil {
		return fmt.Errorf("find route: %w", err)
	}

	// 認証はテストではスキップ
	opts := &openapi3filter.Options{
		AuthenticationFunc: func(ctx context.Context, in *openapi3filter.AuthenticationInput) error {
			return nil
		},
	}

	// リクエスト検証
	rin := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    opts,
	}
	if err := openapi3filter.ValidateRequest(context.Background(), rin); err != nil {
		return fmt.Errorf("request validation: %w", err)
	}

	// レスポンス検証
	rout := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: rin,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
		Options:                opts,
	}
	if err := openapi3filter.ValidateResponse(context.Background(), rout); err != nil {
		return fmt.Errorf("response validation: %w\nbody=%s", err, string(body))
	}
	return nil
}

/********** テスト **********/

func TestOpenAPI_Health(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	req, resp, body := doReq(t, ts, http.MethodGet, "/health", nil, nil)

	// /health は {"ok": true} を返す実装になっている必要あり
	if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
		t.Fatalf("/health not conforming: %v", err)
	}
	_ = json.Valid(body)
}

/********** ヘルパ **********/
type composeFakeRunner struct {
	out   string
	code  int
	err   error
	calls []string
}

func (f *composeFakeRunner) Run(_ context.Context, command string) (ExecResult, error) {
	f.calls = append(f.calls, command)
	return ExecResult{
		Command:    command,
		ExitCode:   f.code,
		Output:     f.out,
		StartedAt:  time.Now().Add(-5 * time.Millisecond),
		FinishedAt: time.Now(),
		DurationMs: 5,
	}, f.err
}

/********** テスト **********/
func TestOpenAPI_ServerStatus(t *testing.T) {
	// テスト用設定（Composeサービス名・コマンドは何でもOK：実行はフェイク）
	cfg, _ := loadConfigFromEnv()
	cfg.ComposeServiceName = "7dtdserver"
	cfg.StatusCmd = `ssh 7dtd01 "docker compose -f /home/masahide/work/7dtd/docker-compose.yml ps"`

	// appCfg を一時的に差し替え（buildRoutes は cfg を使って起動）
	prevCfg := appCfg
	appCfg = cfg
	defer func() { appCfg = prevCfg }()

	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	// OpenAPI: servers を ts.URL に置換
	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	// サンプル出力（Up）
	upOut := `time="2025-08-17T14:02:06+09:00" level=warning msg="/home/masahide/work/7dtd/docker-compose.yml: the attribute ` + "`version`" + ` is obsolete, it will be ignored, please remove it to avoid potential confusion"
NAME         IMAGE             COMMAND                  SERVICE      CREATED        STATUS        PORTS
7dtdserver   7dtd-7dtdserver   "/home/sdtdserver/op…"   7dtdserver   41 hours ago   Up 41 hours   0.0.0.0:8080-8082->8080-8082/tcp, [::]:8080-8082->8080-8082/tcp, 0.0.0.0:26900->26900/tcp, [::]:26900->26900/tcp, 0.0.0.0:26900-26902->26900-26902/udp, [::]:26900-26902->26900-26902/udp
`
	// サンプル出力（Exited）
	exitedOut := `NAME         IMAGE             COMMAND                  SERVICE      CREATED        STATUS                  PORTS
7dtdserver   7dtd-7dtdserver   "/home/sdtdserver/op…"   7dtdserver   2 hours ago    Exited (1) 1 hour ago   -
`

	t.Run("Up => running", func(t *testing.T) {
		fr := &composeFakeRunner{out: upOut, code: 0}
		withRunner(fr, func() {
			req, resp, body := doReq(t, ts, http.MethodGet, "/server/status", nil, nil)

			// OpenAPI 準拠チェック（ルート解決＋レスポンススキーマ）
			if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
				t.Fatalf("openapi validation failed: %v", err)
			}

			// 返却JSONの state を確認
			var got struct {
				ServiceName string `json:"serviceName"`
				State       string `json:"state"`
				Notes       string `json:"notes"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("json: %v", err)
			}
			if got.ServiceName != cfg.ComposeServiceName {
				t.Fatalf("serviceName want %q got %q", cfg.ComposeServiceName, got.ServiceName)
			}
			if got.State != "running" {
				t.Fatalf("state want running got %s; body=%s", got.State, string(body))
			}
		})
	})

	t.Run("Exited => stopped", func(t *testing.T) {
		fr := &composeFakeRunner{out: exitedOut, code: 0}
		withRunner(fr, func() {
			req, resp, body := doReq(t, ts, http.MethodGet, "/server/status", nil, nil)
			if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
				t.Fatalf("openapi validation failed: %v", err)
			}
			var got struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("json: %v", err)
			}
			if got.State != "stopped" {
				t.Fatalf("state want stopped got %s; body=%s", got.State, string(body))
			}
		})
	})
}

func TestOpenAPI_StartStopRestart(t *testing.T) {
	// 設定（コマンド文字列の一部にマッチさせやすいよう、簡潔な match を用意）
	cfg, _ := loadConfigFromEnv()
	cfg.ComposeServiceName = "7dtdserver"
	cfg.StartCmd = `ssh 7dtd01 docker compose -f /home/masahide/work/7dtd/docker-compose.yml up -d`
	cfg.StopCmd = `ssh 7dtd01 docker compose -f //home/masahide/work/7dtd/docker-compose.yml down`

	prev := appCfg
	appCfg = cfg
	defer func() { appCfg = prev }()

	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	// 既に起動済みのときの出力（あなたの実測値）
	upFresh := `time="2025-08-17T15:00:58+09:00" level=warning msg="/home/masahide/work/7dtd/docker-compose.yml: the attribute ` + "`version`" + ` is obsolete, it will be ignored, please remove
it to avoid potential confusion"
 Network a_my-network  Creating
 Network a_my-network  Created
 Container 7dtdserver  Creating
 Container 7dtdserver  Created
 Container 7dtdserver  Starting
 Container 7dtdserver  Started`
	upAlready := `time="2025-08-17T14:27:41+09:00" level=warning msg="/home/masahide/work/7dtd/docker-compose.yml: the attribute ` +
		"`version`" + ` is obsolete, it will be ignored, please remove it to avoid potential confusion"
 Container 7dtdserver  Running
`
	downFresh := `time="2025-08-17T15:01:06+09:00" level=warning msg="/home/masahide/work/7dtd/docker-compose.yml: the attribute ` + "`version`" + ` is obsolete, it will be ignored, please remove
it to avoid potential confusion"
 Container 7dtdserver  Stopping
 Container 7dtdserver  Stopped
 Container 7dtdserver  Removing
 Container 7dtdserver  Removed
 Network a_my-network  Removing
 Network a_my-network  Removed
`
	downAlready := `time="2025-08-17T15:01:12+09:00" level=warning msg="/home/masahide/work/7dtd/docker-compose.yml: the attribute ` + "`version`" + ` is obsolete, it will be ignored, please remove
it to avoid potential confusion"
`
	runner := &scriptedRunner{
		scripts: []scriptEntry{
			// 最初の /server/start 呼び出し → fresh start
			{match: "up -d", out: upFresh, code: 0},
			// /server/stop → down（削除のログあり）
			{match: "down", out: downFresh, code: 0},
			// /server/restart は stop→start の2回叩くので、次は already stopped → already running にしてみる
			{match: "down", out: downAlready, code: 0},
			{match: "up -d", out: upAlready, code: 0},
		},
	}

	withRunner(runner, func() {
		// --- start (fresh) ---
		{
			resp, m := doJSON(t, ts, http.MethodPost, "/server/start", []byte(`{}`))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("start: want 200 got %d", resp.StatusCode)
			}
			if s := m["status"].(string); s != "started" {
				t.Fatalf("start: status want started got %q (body=%v)", s, m)
			}
			// 参考: note に Started 行が含まれる
			if note, _ := m["note"].(string); note != "" && !strings.Contains(note, "Started") {
				t.Fatalf("start: note should contain 'Started', got %q", note)
			}
		}

		// --- stop (fresh) ---
		{
			resp, m := doJSON(t, ts, http.MethodPost, "/server/stop", []byte(`{}`))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stop: want 200 got %d", resp.StatusCode)
			}
			if s := m["status"].(string); s != "stopped" {
				t.Fatalf("stop: status want stopped got %q (body=%v)", s, m)
			}
			// 参考: note に Removed 行が含まれる
			if note, _ := m["note"].(string); note != "" && !strings.Contains(note, "Removed") {
				t.Fatalf("stop: note should contain 'Removed', got %q", note)
			}
		}

		// --- restart (down already + up already running) ---
		{
			resp, m := doJSON(t, ts, http.MethodPost, "/server/restart", []byte(`{}`))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("restart: want 200 got %d", resp.StatusCode)
			}
			if s := m["status"].(string); s != "restarted" && s != "restarting" {
				t.Fatalf("restart: status want restarted/restarting got %q (body=%v)", s, m)
			}
			// exec 内に stop/start 両方が含まれることを確認
			execMap := m["exec"].(map[string]any)
			if _, ok := execMap["stop"]; !ok {
				t.Fatalf("restart: exec.stop missing")
			}
			if _, ok := execMap["start"]; !ok {
				t.Fatalf("restart: exec.start missing")
			}
		}
	})

	// 呼び出し回数: start(1) + stop(1) + restart(stop+start=2) = 4
	if got := len(runner.calls); got != 4 {
		t.Fatalf("runner calls want 4 got %d (%v)", got, runner.calls)
	}
}

/********** フェイクリナー：コマンドに応じて出力を返す **********/
type scriptEntry struct {
	match string
	out   string
	code  int
	err   error
}

type scriptedRunner struct {
	scripts []scriptEntry
	calls   []string
}

func (s *scriptedRunner) Run(_ context.Context, command string) (ExecResult, error) {
	s.calls = append(s.calls, command)
	res := ExecResult{
		Command:   command,
		StartedAt: time.Now(),
	}
	for _, sc := range s.scripts {
		if strings.Contains(command, sc.match) {
			res.Output = sc.out
			res.ExitCode = sc.code
			res.FinishedAt = time.Now()
			res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
			return res, sc.err
		}
	}
	res.FinishedAt = time.Now()
	res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	return res, nil
}

func withRunner(r CommandRunner, fn func()) {
	prev := cmdRunner
	cmdRunner = r
	defer func() { cmdRunner = prev }()
	fn()
}

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body []byte) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func TestOpenAPI_ServerSummary(t *testing.T) {
	// ---- 上流(7dtd REST) の偽サーバを用意 ----
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/serverstats":
			io.WriteString(w, `{"data":{"gameTime":{"days":266,"hours":13,"minutes":6},"players":1,"hostiles":14,"animals":0},"meta":{"serverTime":"2025-08-17T09:52:37.5861810+09:00"}}`)
		case "/api/player":
			io.WriteString(w, `{"data":{"players":[{"entityId":64489,"name":"KenJapan","platformId":{"combinedString":"Steam_76561198261284786","platformId":"Steam","userId":"76561198261284786"},"crossplatformId":{"combinedString":"EOS_0002923a34e4408a8bca0a5fa0fa4081","platformId":"EOS","userId":"0002923a34e4408a8bca0a5fa0fa4081"},"online":true,"ip":"118.241.17.204","ping":4,"position":{"x":72.0625,"y":38.09375,"z":816.03125},"level":39,"health":108,"stamina":119.018654,"score":1298,"deaths":19,"kills":{"zombies":1645,"players":0},"banned":{"banActive":false,"reason":null,"until":null}}]},"meta":{"serverTime":"2025-08-17T09:52:37.5947430+09:00"}}`)
		case "/api/hostile":
			io.WriteString(w, `{"data":[{"id":78032,"name":"zombieFatHawaiian","position":{"x":42,"y":38,"z":806}},{"id":78033,"name":"zombieYo","position":{"x":37,"y":38,"z":807}}],"meta":{"serverTime":"2025-08-17T09:52:37.5943040+09:00"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	// ---- アプリ側サーバ（上流BASEを差し替え） ----
	cfg, _ := loadConfigFromEnv()
	cfg.APIBaseURL = up.URL + "/api"
	// compose 状態の getStatus は cmdRunner を使うが、このテストでは呼ばれて OK（デフォルトShellRunnerでも副作用なし）

	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	// OpenAPI の servers をアプリ側に合わせる
	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	// 1) 既定値（maskIPs=true, includePositions=true など）
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/server/summary", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200 got %d; body=%s", resp.StatusCode, string(body))
		}
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Fatalf("summary(default) openapi validate: %v\nbody=%s", err, string(body))
		}
		// マスクされているはず
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		players := got["data"].(map[string]any)["players"].([]any)
		if len(players) == 0 {
			t.Fatalf("players empty")
		}
		ip := players[0].(map[string]any)["ip"].(string)
		if !strings.HasSuffix(ip, ".*") {
			t.Fatalf("ip should be masked, got %q", ip)
		}
	}

	// 2) クエリ付き（位置消し/マスク無効/hostiles制限/verbose）
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/server/summary?includePositions=false&maskIPs=false&limitHostiles=1&timeoutSeconds=3&verbose=true", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200 got %d; body=%s", resp.StatusCode, string(body))
		}
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Fatalf("summary(queries) openapi validate: %v\nbody=%s", err, string(body))
		}
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		data := got["data"].(map[string]any)
		players := data["players"].([]any)
		// 位置が null
		if players[0].(map[string]any)["position"] != nil {
			t.Fatalf("player position should be null when includePositions=false")
		}
		// hostiles が 1件に制限
		hostiles := data["hostiles"].([]any)
		if len(hostiles) != 1 {
			t.Fatalf("hostiles length want 1 got %d", len(hostiles))
		}
		// sources が付く（verbose=true）
		meta := got["meta"].(map[string]any)
		if _, ok := meta["sources"]; !ok {
			t.Fatalf("meta.sources missing with verbose=true")
		}
	}
}
