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

func TestOpenAPI_ServerStatus(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	req, resp, body := doReq(t, ts, http.MethodGet, "/server/status", nil, nil)
	if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
		// 現実装が OpenAPI の ServerStatus スキーマに未対応ならここで不一致が出ます
		t.Errorf("/server/status not conforming: %v", err)
	}
}

func TestOpenAPI_StartStopRestart(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	// POST /server/start
	{
		req, resp, body := doReq(t, ts, http.MethodPost, "/server/start",
			bytes.NewReader([]byte(`{}`)),
			map[string]string{"Content-Type": "application/json"},
		)
		// 202 + Job スキーマが期待
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/server/start not conforming: %v", err)
		}
	}
	// POST /server/stop
	{
		req, resp, body := doReq(t, ts, http.MethodPost, "/server/stop",
			bytes.NewReader([]byte(`{}`)),
			map[string]string{"Content-Type": "application/json"},
		)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/server/stop not conforming: %v", err)
		}
	}
	// POST /server/restart
	{
		req, resp, body := doReq(t, ts, http.MethodPost, "/server/restart",
			bytes.NewReader([]byte(`{}`)),
			map[string]string{"Content-Type": "application/json"},
		)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/server/restart not conforming: %v", err)
		}
	}
}

func TestOpenAPI_JobByID(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	// 200
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/jobs/abc123", nil, nil)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/jobs/{id} 200 not conforming: %v", err)
		}
	}
	// 404（ErrorResponse スキーマが要求されます）
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/jobs/notfound", nil, nil)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/jobs/{id} 404 not conforming: %v", err)
		}
	}
}

func TestOpenAPI_ServerSummary(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	_, rt := loadOpenAPISpecWithServer(t, ts.URL)

	// デフォルト
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/server/summary", nil, nil)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/server/summary default not conforming: %v", err)
		}
	}
	// クエリあり
	{
		req, resp, body := doReq(t, ts, http.MethodGet, "/server/summary?includePositions=true&maskIPs=true&limitHostiles=50&timeoutSeconds=3&verbose=false", nil, nil)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, body); err != nil {
			t.Errorf("/server/summary with queries not conforming: %v", err)
		}
	}
}
