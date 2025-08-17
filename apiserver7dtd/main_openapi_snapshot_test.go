// main_openapi_snapshot_test.go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/go-cmp/cmp"
)

func TestOpenAPISnapshot(t *testing.T) {
	// アプリ起動（実装依存: buildRoutes, loadConfigFromEnv は既存のを使用）
	cfg, _ := loadConfigFromEnv()
	ts := httptest.NewServer(buildRoutes(cfg))
	t.Cleanup(ts.Close)

	// 1) /docs/openapi.yaml を取得
	resp, err := http.Get(ts.URL + "/docs/openapi.yaml")
	if err != nil {
		t.Fatalf("GET /docs/openapi.yaml: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	// 2) OpenAPI 妥当性検証（kin-openapi）
	ldr := &openapi3.Loader{IsExternalRefsAllowed: true}
	doc, err := ldr.LoadFromData(body)
	if err != nil {
		t.Fatalf("openapi load: %v\n%s", err, string(body))
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi validate: %v", err)
	}

	// 3) snapshot 安定化（servers は動的なので固定化）
	doc.Servers = openapi3.Servers{&openapi3.Server{URL: "https://example.invalid"}}

	// 4) JSON に正規化してゴールデン比較
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join("testdata", "openapi.snapshot.json")
	if os.Getenv("UPDATE_SNAPSHOT") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(snapPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("snapshot updated: %s", snapPath)
		return
	}
	want, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v\nヒント: UPDATE_SNAPSHOT=1 go test -run TestOpenAPISnapshot", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Fatalf("OpenAPI snapshot mismatch (-want +got):\n%s", diff)
	}
}
