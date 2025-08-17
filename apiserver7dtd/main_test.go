package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	yaml "github.com/oasdiff/yaml3"
)

/*
//********** テスト用ユーティリティ **********

func newTestServer() *httptest.Server {
	return httptest.NewServer(routes())
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, b
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("json unmarshal: %v\nbody=%s", err, string(b))
	}
}

//********** 正常系 **********

func TestHealth(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, body := doReq(t, ts, http.MethodGet, "/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status want 200 got %d", resp.StatusCode)
	}
	var m map[string]string
	mustJSON(t, body, &m)
	if m["status"] != "ok" {
		t.Fatalf("body %v", m)
	}
}

func TestServerStatus(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, body := doReq(t, ts, http.MethodGet, "/server/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status want 200 got %d", resp.StatusCode)
	}
	var st ServerStatus
	mustJSON(t, body, &st)
	if !st.Running {
		t.Fatalf("expected running=true got %v", st.Running)
	}
	if st.CurrentJob == "" {
		t.Fatalf("expected current job non-empty")
	}
}

func TestStartStopRestart(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// start
	{
		resp, body := doReq(t, ts, http.MethodPost, "/server/start", strings.NewReader(""))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("start status want 202 got %d", resp.StatusCode)
		}
		var m map[string]string
		mustJSON(t, body, &m)
		if m["status"] != "starting" {
			t.Fatalf("unexpected response: %v", m)
		}
	}
	// stop
	{
		resp, body := doReq(t, ts, http.MethodPost, "/server/stop", strings.NewReader(""))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("stop status want 202 got %d", resp.StatusCode)
		}
		var m map[string]string
		mustJSON(t, body, &m)
		if m["status"] != "stopping" {
			t.Fatalf("unexpected response: %v", m)
		}
	}
	// restart
	{
		resp, body := doReq(t, ts, http.MethodPost, "/server/restart", strings.NewReader(""))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("restart status want 202 got %d", resp.StatusCode)
		}
		var m map[string]string
		mustJSON(t, body, &m)
		if m["status"] != "restarting" {
			t.Fatalf("unexpected response: %v", m)
		}
	}
}

func TestJobByID(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// found
	{
		resp, body := doReq(t, ts, http.MethodGet, "/jobs/abc123", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status want 200 got %d", resp.StatusCode)
		}
		var m map[string]string
		mustJSON(t, body, &m)
		if m["id"] != "abc123" || m["state"] != "done" {
			t.Fatalf("unexpected: %v", m)
		}
	}
	// not found
	{
		resp, _ := doReq(t, ts, http.MethodGet, "/jobs/notfound", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status want 404 got %d", resp.StatusCode)
		}
	}
}

func TestServerSummary_Defaults(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, body := doReq(t, ts, http.MethodGet, "/server/summary", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status want 200 got %d", resp.StatusCode)
	}
	var m map[string]any
	mustJSON(t, body, &m)

	// フィールド存在チェック
	wantKeys := []string{"maskIPs", "includePositions", "limitHostiles", "timeoutSeconds", "verbose", "hostiles"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in summary: %v", k, m)
		}
	}
}

//********** クエリパラメータの正常系 & エラー系 **********

func TestServerSummary_QueryParams(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// 正常系（すべて指定）
	path := "/server/summary?maskIPs=true&includePositions=1&limitHostiles=10&timeoutSeconds=2&verbose=yes"
	resp, body := doReq(t, ts, http.MethodGet, path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status want 200 got %d", resp.StatusCode)
	}
	var m map[string]any
	mustJSON(t, body, &m)

	// 値が反映されているか（JSONは number として返るので型変換に注意）
	if v, ok := m["maskIPs"].(bool); !ok || !v {
		t.Fatalf("maskIPs not true: %#v", m["maskIPs"])
	}
	if v, ok := m["includePositions"].(bool); !ok || !v {
		t.Fatalf("includePositions not true: %#v", m["includePositions"])
	}
	if v, ok := m["limitHostiles"].(float64); !ok || int(v) != 10 {
		t.Fatalf("limitHostiles not 10: %#v", m["limitHostiles"])
	}
	if v, ok := m["timeoutSeconds"].(float64); !ok || int(v) != 2 {
		t.Fatalf("timeoutSeconds not 2: %#v", m["timeoutSeconds"])
	}
	if v, ok := m["verbose"].(bool); !ok || !v {
		t.Fatalf("verbose not true: %#v", m["verbose"])
	}
}

func TestServerSummary_QueryParamErrors(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	cases := []string{
		"/server/summary?maskIPs=maybe",         // bool パース失敗
		"/server/summary?includePositions=what", // bool パース失敗
		"/server/summary?limitHostiles=-1",      // 範囲外
		"/server/summary?limitHostiles=1000000", // 範囲外
		"/server/summary?timeoutSeconds=999999", // 範囲外
		"/server/summary?verbose=¯\\_(ツ)_/¯",    // bool パース失敗
	}

	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			resp, _ := doReq(t, ts, http.MethodGet, p, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400 got %d for %s", resp.StatusCode, p)
			}
		})
	}
}

//********** タイムアウト系（形だけ確認） **********

func TestGlobalTimeoutMiddleware(t *testing.T) {
	// ルータの全体タイムアウトは 30s。ここでは「応答が返ること」だけ軽く確認。
	ts := newTestServer()
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/server/summary?timeoutSeconds=1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status want 200 got %d", resp.StatusCode)
	}
}
*/

func TestOpenAPIYAML_EnvOverride(t *testing.T) {
	t.Setenv("OPSA_OPENAPI_SERVERS", "https://ops.example.com, https://ops2.example.com")

	// ★ env を反映した cfg を読み込み
	cfg, err := loadConfigFromEnv()
	if err != nil {
		t.Fatalf("loadConfigFromEnv: %v", err)
	}

	// ★ routes() ではなく buildRoutes(cfg) を使う
	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var doc map[string]any
	if err := yaml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}

	servers, _ := doc["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %d", len(servers))
	}
	got0 := servers[0].(map[string]any)["url"].(string)
	got1 := servers[1].(map[string]any)["url"].(string)
	if got0 != "https://ops.example.com" || got1 != "https://ops2.example.com" {
		t.Fatalf("unexpected servers: %v, %v", got0, got1)
	}
}

func TestOpenAPIYAML_DeriveFromRequest(t *testing.T) {
	ts := httptest.NewServer(routes())
	defer ts.Close()

	// ensure env is unset
	os.Unsetenv("OPSA_OPENAPI_SERVERS")
	os.Unsetenv("OPSA_PUBLIC_BASE_URL")

	req, _ := http.NewRequest("GET", ts.URL+"/docs/openapi.yaml", nil)
	// 代理環境を模擬する場合は X-Forwarded-Proto を付与
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var doc map[string]any
	if err := yaml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	servers, _ := doc["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	got := servers[0].(map[string]any)["url"].(string)
	if !strings.HasPrefix(got, "https://") {
		t.Fatalf("expected https scheme from X-Forwarded-Proto, got %s", got)
	}
}
