package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- テスト用フェイクランナー ----

type fakeRunner struct {
	out   string
	code  int
	err   error
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, command string) (ExecResult, error) {
	f.calls = append(f.calls, command)
	res := ExecResult{
		Command:    command,
		ExitCode:   f.code,
		Output:     f.out,
		StartedAt:  time.Now().Add(-10 * time.Millisecond),
		FinishedAt: time.Now(),
		DurationMs: 10,
	}
	return res, f.err
}

func withRunner(r CommandRunner, fn func()) {
	prev := cmdRunner
	cmdRunner = r
	defer func() { cmdRunner = prev }()
	fn()
}

// ---- ヘルパ ----

func do(ts *httptest.Server, method, path string, body []byte) (*http.Response, map[string]any, error) {
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m, nil
}

// ---- テスト本体 ----

func TestServerStart_ReturnsExecResult(t *testing.T) {
	// appCfg の StartCmd は何でも OK（fake が使われる）
	cfg, _ := loadConfigFromEnv()
	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	f := &fakeRunner{out: "hello\nworld\n", code: 0}
	withRunner(f, func() {
		resp, m, err := do(ts, http.MethodPost, "/server/start", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status want 202 got %d", resp.StatusCode)
		}
		exec := m["exec"].(map[string]any)
		if exec["exitCode"].(float64) != 0 {
			t.Fatalf("exitCode want 0 got %v", exec["exitCode"])
		}
		if exec["output"].(string) != "hello\nworld\n" {
			t.Fatalf("output mismatch: %q", exec["output"])
		}
		if len(f.calls) != 1 {
			t.Fatalf("runner called %d times", len(f.calls))
		}
	})
}

func TestServerStop_CommandErrorIncludesOutput(t *testing.T) {
	cfg, _ := loadConfigFromEnv()
	ts := httptest.NewServer(buildRoutes(cfg))
	defer ts.Close()

	f := &fakeRunner{out: "oops: permission denied\n", code: 1, err: errors.New("exit status 1")}
	withRunner(f, func() {
		resp, m, err := do(ts, http.MethodPost, "/server/stop", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status want 409 got %d", resp.StatusCode)
		}
		er := m["error"].(map[string]any)
		details := er["details"].(map[string]any)
		exec := details["exec"].(map[string]any)
		if exec["exitCode"].(float64) != 1 {
			t.Fatalf("exitCode want 1 got %v", exec["exitCode"])
		}
		if exec["output"].(string) != "oops: permission denied\n" {
			t.Fatalf("output mismatch: %q", exec["output"])
		}
	})
}

func TestShellRunner_CombinedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test is *nix specific")
	}
	// 実プロセスで stdout / stderr を同時に出す
	res, err := ShellRunner{}.Run(context.Background(), `echo out; echo err 1>&2`)
	if err != nil && res.ExitCode == 0 {
		t.Fatalf("unexpected error: %v", err)
	}
	// CombinedOutput なので out / err の両方が含まれるはず
	if !(strings.Contains(res.Output, "out") && strings.Contains(res.Output, "err")) {
		t.Fatalf("combined output missing: %q", res.Output)
	}
}
