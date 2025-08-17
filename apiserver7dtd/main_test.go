package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	yaml "github.com/oasdiff/yaml3"
)

func TestMain(m *testing.M) {
	// 認証をテストでは無効化
	_ = os.Setenv("ALLOW_NO_AUTH", "true")

	// 念のためダミー値も入れておく（使われないが将来のため）
	_ = os.Setenv("AUTH_BEARER_TOKEN", "test-bearer")
	_ = os.Setenv("API_KEY", "test-apikey")

	os.Exit(m.Run())
}

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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := yaml.NewDecoder(bytes.NewBuffer(body)).Decode(&doc); err != nil {
		t.Fatalf("yaml decode: %v,body:%s", err, string(body))
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
