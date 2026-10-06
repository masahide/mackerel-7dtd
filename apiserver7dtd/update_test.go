package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type updateGame struct {
	mu            sync.Mutex
	version       string
	players       int
	statsOverride string
	listOverride  string
	commandError  bool
}

func (g *updateGame) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/command":
		if g.commandError {
			http.Error(w, "DO_NOT_LEAK_UPSTREAM_SECRET", 500)
			return
		}
		var req CommandRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Command != "version" {
			http.Error(w, "unexpected command", 400)
			return
		}
		var resp CommandResponse
		resp.Data.Result = g.version
		_ = json.NewEncoder(w).Encode(resp)
	case "/api/serverstats":
		if g.statsOverride != "" {
			_, _ = io.WriteString(w, g.statsOverride)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"players": g.players}})
	case "/api/player":
		if g.listOverride != "" {
			_, _ = io.WriteString(w, g.listOverride)
			return
		}
		players := []map[string]any{}
		for i := 0; i < g.players; i++ {
			players = append(players, map[string]any{"online": true})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"players": players}})
	default:
		http.NotFound(w, r)
	}
}

type updateFakeRunner struct {
	mu          sync.Mutex
	calls       []string
	fail        string
	nilError    bool
	backup      string
	block       string
	entered     chan struct{}
	release     chan struct{}
	game        *updateGame
	keepVersion bool
}

func (f *updateFakeRunner) Run(ctx context.Context, cmd string) (ExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	fail, block, backup, nilError, keepVersion := f.fail == cmd, f.block == cmd, f.backup, f.nilError, f.keepVersion
	f.mu.Unlock()
	if block {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-f.release:
		case <-ctx.Done():
			return ExecResult{ExitCode: -1}, ctx.Err()
		}
	}
	if fail {
		var err error
		if !nilError {
			err = errors.New("DO_NOT_LEAK_HOOK_SECRET")
		}
		return ExecResult{Command: "DO_NOT_LEAK_COMMAND", ExitCode: 7, Output: "DO_NOT_LEAK_HOOK_SECRET"}, err
	}
	if cmd == "start-hook" && !keepVersion {
		f.game.mu.Lock()
		f.game.version = "Game version: V 2.3 (b9) Compatibility Version: V 2.3"
		f.game.mu.Unlock()
	}
	if cmd == "backup-hook" {
		if backup == "" {
			backup = `{"backupId":"backup_0123456789abcdef","verified":true}`
		}
		return ExecResult{ExitCode: 0, Output: backup}, nil
	}
	return ExecResult{ExitCode: 0}, nil
}

func (f *updateFakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

type updateFixture struct {
	m      *updateManager
	server *httptest.Server
	game   *updateGame
	runner *updateFakeRunner
	cfg    Config
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	g := &updateGame{version: "Game version: V 2.2 (b3) Compatibility Version: V 2.2"}
	up := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(up.Close)
	cfg := Config{APIBaseURL: up.URL + "/api", AuthBearerToken: "update-test-token", UpdateEnabled: true,
		UpdateStateDir: t.TempDir(), UpdateTargetVersion: "Game version: V 2.3 (b9) Compatibility Version: V 2.3", UpdatePreflightCmd: "preflight-hook",
		UpdateStopCmd: "stop-hook", UpdateCheckStoppedCmd: "stopped-hook", UpdateBackupCmd: "backup-hook",
		UpdateApplyCmd: "apply-hook", UpdateStartCmd: "start-hook", UpdateTimeout: 5 * time.Second, UpdateVerifyTimeout: 100 * time.Millisecond}
	m := newUpdateManager(cfg)
	if m.unready != "" {
		t.Fatalf("manager unavailable: %s", m.unready)
	}
	f := &updateFakeRunner{game: g, entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.runner = f
	ts := httptest.NewServer(buildRoutesWithUpdates(cfg, m))
	t.Cleanup(func() { ts.Close(); m.close() })
	return &updateFixture{m: m, server: ts, game: g, runner: f, cfg: cfg}
}

func updateRequest(t *testing.T, ts *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer update-test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (f *updateFixture) plan(t *testing.T) UpdatePlan {
	t.Helper()
	status, b := updateRequest(t, f.server, "POST", "/server/update/plan", struct{}{})
	if status != 200 {
		t.Fatalf("plan status=%d body=%s", status, b)
	}
	var resp struct {
		Data UpdatePlan `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Data
}

func (f *updateFixture) submit(t *testing.T, p UpdatePlan, key string) (int, UpdateJob) {
	t.Helper()
	status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{PlanID: p.PlanID, Confirmation: updateConfirmation, IdempotencyKey: key})
	var resp struct {
		Data UpdateJob `json:"data"`
	}
	_ = json.Unmarshal(b, &resp)
	if status != 200 && status != 202 {
		t.Fatalf("submit status=%d body=%s", status, b)
	}
	return status, resp.Data
}

func (f *updateFixture) wait(t *testing.T, id string) UpdateJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, b := updateRequest(t, f.server, "GET", "/server/update/jobs/"+id, nil)
		if status != 200 {
			t.Fatalf("poll %d %s", status, b)
		}
		var resp struct {
			Data UpdateJob `json:"data"`
		}
		_ = json.Unmarshal(b, &resp)
		if resp.Data.Status != "queued" && resp.Data.Status != "running" {
			return resp.Data
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
	return UpdateJob{}
}

func TestUpdateSuccessAndDurableIdempotency(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	if !p.CanExecute || p.OnlinePlayers != 0 || len(p.Blockers) != 0 || len(p.Steps) != 7 {
		t.Fatalf("bad plan: %+v", p)
	}
	status, accepted := f.submit(t, p, "request_0123456789abcdef")
	if status != 202 {
		t.Fatal(status)
	}
	job := f.wait(t, accepted.JobID)
	if job.Status != "succeeded" || job.ResultVersion != "Game version: V 2.3 (b9) Compatibility Version: V 2.3" || job.BackupID == "" || job.RecoveryRequired {
		t.Fatalf("job=%+v", job)
	}
	want := []string{"preflight-hook", "preflight-hook", "stop-hook", "stopped-hook", "backup-hook", "apply-hook", "start-hook"}
	if strings.Join(f.runner.commands(), ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v", f.runner.commands())
	}
	f.m.close()
	restarted := newUpdateManager(f.cfg)
	defer restarted.close()
	restarted.runner = f.runner
	ts := httptest.NewServer(buildRoutesWithUpdates(f.cfg, restarted))
	defer ts.Close()
	status, b := updateRequest(t, ts, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "request_0123456789abcdef"})
	if status != 200 || !bytes.Contains(b, []byte(job.JobID)) || len(f.runner.commands()) != len(want) {
		t.Fatalf("replay %d %s calls=%v", status, b, f.runner.commands())
	}
	status, b = updateRequest(t, ts, "GET", "/server/update/jobs/latest", nil)
	if status != 200 || !bytes.Contains(b, []byte(job.JobID)) {
		t.Fatalf("latest %d %s", status, b)
	}
}

func TestUpdateFailureStagesNeverContinue(t *testing.T) {
	cases := []struct {
		cmd, code, phase string
		recovery         bool
	}{
		{"preflight-hook", "PRECHECK_FAILED", "checking", false},
		{"stop-hook", "STOP_FAILED", "stopping", true},
		{"stopped-hook", "STOP_NOT_VERIFIED", "checking_stopped", true},
		{"backup-hook", "BACKUP_FAILED", "backing_up", true},
		{"apply-hook", "UPDATE_FAILED", "updating", true},
		{"start-hook", "START_FAILED", "starting", true},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			f := newUpdateFixture(t)
			p := f.plan(t)
			f.runner.fail = tc.cmd
			f.runner.nilError = true // Nonzero exit alone must fail.
			_, accepted := f.submit(t, p, "failure_0123456789abcdef")
			job := f.wait(t, accepted.JobID)
			if job.Status != "failed" || job.Error == nil || job.Error.Code != tc.code || job.Phase != tc.phase || job.RecoveryRequired != tc.recovery {
				t.Fatalf("job=%+v", job)
			}
			calls := f.runner.commands()
			if calls[len(calls)-1] != tc.cmd {
				t.Fatalf("continued after failure: %v", calls)
			}
			b, _ := json.Marshal(job)
			if bytes.Contains(b, []byte("DO_NOT_LEAK")) {
				t.Fatalf("secret leaked: %s", b)
			}
			if tc.recovery {
				status, _ := updateRequest(t, f.server, "GET", "/server/start", nil)
				if status != 409 {
					t.Fatalf("recovery did not block start: %d", status)
				}
			}
		})
	}
}

func TestUpdateBackupMustBeVerified(t *testing.T) {
	for _, b := range []string{`{}`, `{"backupId":"backup_0123456789abcdef","verified":false}`, `{"backupId":"/arbitrary/path","verified":true}`, `{"backupId":"backup_0123456789abcdef","verified":true} {}`} {
		t.Run(b, func(t *testing.T) {
			f := newUpdateFixture(t)
			p := f.plan(t)
			f.runner.backup = b
			_, accepted := f.submit(t, p, "backup_0123456789abcdef")
			job := f.wait(t, accepted.JobID)
			if job.Error == nil || job.Error.Code != "BACKUP_NOT_VERIFIED" || !job.RecoveryRequired {
				t.Fatalf("%+v", job)
			}
			if strings.Contains(strings.Join(f.runner.commands(), ","), "apply-hook") {
				t.Fatal("updated without verified backup")
			}
		})
	}
}

func TestUpdateFreshChecks(t *testing.T) {
	for _, kind := range []string{"players", "version", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			p := f.plan(t)
			f.game.mu.Lock()
			code := "PLAYERS_ONLINE"
			switch kind {
			case "players":
				f.game.players = 1
			case "version":
				f.game.version = "Game version: V 2.4 (b1) Compatibility Version: V 2.4"
				code = "VERSION_CHANGED"
			case "unknown":
				f.game.statsOverride = `{"data":{}}`
				code = "PRECHECK_FAILED"
			}
			f.game.mu.Unlock()
			_, accepted := f.submit(t, p, "fresh_0123456789abcdef")
			job := f.wait(t, accepted.JobID)
			if job.Error == nil || job.Error.Code != code || job.RecoveryRequired {
				t.Fatalf("%+v", job)
			}
			if len(f.runner.commands()) != 2 {
				t.Fatalf("destructive call after changed precheck: %v", f.runner.commands())
			}
		})
	}
}

func TestUpdatePlanFailClosedOnUnknownPlayers(t *testing.T) {
	cases := []struct{ stats, list string }{
		{`{}`, ""}, {`{"data":{}}`, ""}, {`{"data":{"players":-1}}`, ""},
		{"", `{}`}, {"", `{"data":{"players":null}}`}, {"", `{"data":{"players":[{}]}}`},
		{`{"data":{"players":1}}`, ""},
	}
	for i, tc := range cases {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			f := newUpdateFixture(t)
			f.game.statsOverride = tc.stats
			f.game.listOverride = tc.list
			status, b := updateRequest(t, f.server, "POST", "/server/update/plan", struct{}{})
			if status != 502 || !bytes.Contains(b, []byte("PRECHECK_FAILED")) {
				t.Fatalf("%d %s", status, b)
			}
			if len(f.runner.commands()) != 1 {
				t.Fatal("unexpected mutation")
			}
		})
	}
}

func TestUpdatePlanBlockersAndValidation(t *testing.T) {
	f := newUpdateFixture(t)
	f.game.players = 1
	p := f.plan(t)
	if p.CanExecute || len(p.Blockers) != 1 || p.Blockers[0] != "PLAYERS_ONLINE" {
		t.Fatalf("%+v", p)
	}
	request := UpdateJobRequest{p.PlanID, updateConfirmation, "valid_0123456789abcdef"}
	status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", request)
	if status != 409 || !bytes.Contains(b, []byte("PLAN_BLOCKED")) {
		t.Fatalf("%d %s", status, b)
	}
	request.Confirmation = "yes"
	status, _ = updateRequest(t, f.server, "POST", "/server/update/jobs", request)
	if status != 400 {
		t.Fatal(status)
	}
	status, _ = updateRequest(t, f.server, "POST", "/server/update/plan", map[string]string{"command": "rm -rf /"})
	if status != 400 {
		t.Fatal(status)
	}
	f.game.mu.Lock()
	f.game.players = 0
	f.game.version = "Game version: V 2.3 (b9) Compatibility Version: V 2.3"
	f.game.mu.Unlock()
	p = f.plan(t)
	if p.CanExecute || p.Blockers[0] != "ALREADY_CURRENT" {
		t.Fatalf("%+v", p)
	}
}

func TestUpdateConcurrentDuplicateAndLegacyLock(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	f.runner.block = "apply-hook"
	_, accepted := f.submit(t, p, "parallel_0123456789abcdef")
	select {
	case <-f.runner.entered:
	case <-time.After(time.Second):
		t.Fatal("hook did not enter")
	}
	for _, path := range []string{"/server/start", "/server/stop", "/server/restart", "/server/command?command=version"} {
		status, _ := updateRequest(t, f.server, "GET", path, nil)
		if status != 409 {
			t.Fatalf("%s did not lock: %d", path, status)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, job := f.submit(t, p, "parallel_0123456789abcdef")
			if status != 200 || job.JobID != accepted.JobID {
				t.Errorf("duplicate created another job")
			}
		}()
	}
	wg.Wait()
	status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{"different", updateConfirmation, "parallel_0123456789abcdef"})
	if status != 409 || !bytes.Contains(b, []byte("IDEMPOTENCY_CONFLICT")) {
		t.Fatalf("%d %s", status, b)
	}
	status, _ = updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "different_0123456789abcdef"})
	if status != 409 {
		t.Fatal(status)
	}
	close(f.runner.release)
	job := f.wait(t, accepted.JobID)
	if job.Status != "succeeded" {
		t.Fatalf("%+v", job)
	}
	if len(f.runner.commands()) != 7 {
		t.Fatalf("duplicated hooks: %v", f.runner.commands())
	}
	status, b = updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "different_0123456789abcdef"})
	if status != 409 || !bytes.Contains(b, []byte("PLAN_BLOCKED")) {
		t.Fatalf("reused plan: %d %s", status, b)
	}
}

func TestUpdateInterruptedJobAndExclusiveStateOwner(t *testing.T) {
	f := newUpdateFixture(t)
	second := newUpdateManager(f.cfg)
	defer second.close()
	if second.unready != "UPDATE_STATE_UNAVAILABLE" {
		t.Fatal("state directory shared by two API processes")
	}
	now := time.Now().UTC()
	f.m.mu.Lock()
	f.m.journal.Jobs["test-job"] = UpdateJob{JobID: "test-job", Status: "running", Phase: "updating", CreatedAt: now, UpdatedAt: now}
	f.m.journal.Latest = "test-job"
	if err := f.m.saveLocked(); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Unlock()
	f.m.close()
	restarted := newUpdateManager(f.cfg)
	defer restarted.close()
	if restarted.unready != "" || !restarted.recovery {
		t.Fatalf("unready=%s recovery=%v", restarted.unready, restarted.recovery)
	}
	job := restarted.journal.Jobs["test-job"]
	if job.Status != "interrupted" || !job.RecoveryRequired || job.Error.Code != "UPDATE_INTERRUPTED" {
		t.Fatalf("%+v", job)
	}
	ts := httptest.NewServer(buildRoutesWithUpdates(f.cfg, restarted))
	defer ts.Close()
	status, _ := updateRequest(t, ts, "GET", "/server/command?command=shutdown", nil)
	if status != 409 {
		t.Fatal(status)
	}
	status, b := updateRequest(t, ts, "GET", "/server/update/jobs/latest", nil)
	if status != 200 || !bytes.Contains(b, []byte("interrupted")) {
		t.Fatalf("%d %s", status, b)
	}
}

func TestUpdateVerificationAndCancellation(t *testing.T) {
	t.Run("wrong version", func(t *testing.T) {
		f := newUpdateFixture(t)
		p := f.plan(t)
		f.runner.keepVersion = true
		_, accepted := f.submit(t, p, "verify_0123456789abcdef")
		job := f.wait(t, accepted.JobID)
		if job.Status != "failed" || job.Error.Code != "VERIFY_FAILED" || job.ResultVersion != "Game version: V 2.2 (b3) Compatibility Version: V 2.2" || !job.RecoveryRequired {
			t.Fatalf("%+v", job)
		}
	})
	t.Run("process shutdown", func(t *testing.T) {
		f := newUpdateFixture(t)
		p := f.plan(t)
		f.runner.block = "apply-hook"
		_, accepted := f.submit(t, p, "cancel_0123456789abcdef")
		select {
		case <-f.runner.entered:
		case <-time.After(time.Second):
			t.Fatal("hook did not enter")
		}
		f.m.close()
		job := f.wait(t, accepted.JobID)
		if job.Status != "interrupted" || job.Error.Code != "UPDATE_INTERRUPTED" || !job.RecoveryRequired {
			t.Fatalf("%+v", job)
		}
	})
}

func TestUpdatePersistentWriteFailureDoesNotStopServer(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	if err := os.Rename(f.m.statePath(), filepath.Join(f.cfg.UpdateStateDir, "saved-journal.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.m.statePath(), 0700); err != nil {
		t.Fatal(err)
	}
	status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "persist_0123456789abcdef"})
	if status != 503 || !bytes.Contains(b, []byte("UPDATE_STATE_UNAVAILABLE")) {
		t.Fatalf("%d %s", status, b)
	}
	if len(f.runner.commands()) != 1 {
		t.Fatal("executed despite failed durable acceptance")
	}
	status, b = updateRequest(t, f.server, "GET", "/server/update/jobs/latest", nil)
	if status != 200 || !bytes.Contains(b, []byte("interrupted")) || !bytes.Contains(b, []byte("UPDATE_STATE_UNAVAILABLE")) {
		t.Fatalf("ambiguous acceptance left an unpollable queued job: %d %s", status, b)
	}
}

func TestUpdateAuthAndDisabledConfiguration(t *testing.T) {
	f := newUpdateFixture(t)
	req, _ := http.NewRequest("POST", f.server.URL+"/server/update/plan", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || len(f.runner.commands()) != 0 {
		t.Fatal("unauthenticated preflight executed")
	}
	for _, change := range []func(*Config){func(c *Config) { c.UpdateEnabled = false }, func(c *Config) { c.AllowNoAuth = true }, func(c *Config) { c.UpdateApplyCmd = "" }, func(c *Config) { c.UpdateStateDir = "relative" }} {
		cfg := f.cfg
		cfg.UpdateStateDir = t.TempDir()
		change(&cfg)
		m := newUpdateManager(cfg)
		m.close()
		want := "UPDATE_UNAVAILABLE"
		if !filepath.IsAbs(cfg.UpdateStateDir) {
			want = "UPDATE_STATE_UNAVAILABLE"
		}
		if m.unready != want {
			t.Fatalf("unsafe config accepted: %s", m.unready)
		}
	}
}

func TestUpdateDisabledStillOwnsStateAndRecoveryFence(t *testing.T) {
	f := newUpdateFixture(t)
	now := time.Now().UTC()
	f.m.mu.Lock()
	request := UpdateJobRequest{PlanID: "old-plan", Confirmation: updateConfirmation, IdempotencyKey: "disabled_0123456789abcdef"}
	f.m.journal.Jobs["old-job"] = UpdateJob{JobID: "old-job", PlanID: request.PlanID, Status: "running", Phase: "updating", CreatedAt: now, UpdatedAt: now}
	f.m.journal.Keys[request.IdempotencyKey] = updateKey{Request: request, JobID: "old-job"}
	f.m.journal.Latest = "old-job"
	if err := f.m.saveLocked(); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Unlock()
	cfg := f.cfg
	cfg.UpdateEnabled = false
	second := newUpdateManager(cfg)
	if second.unready != "UPDATE_STATE_UNAVAILABLE" {
		t.Fatal("disabled second process bypassed ownership")
	}
	ts2 := httptest.NewServer(buildRoutesWithUpdates(cfg, second))
	status, _ := updateRequest(t, ts2, "GET", "/server/start", nil)
	if status != 503 {
		t.Fatalf("disabled second process bypassed mutation lock: %d", status)
	}
	ts2.Close()
	second.close()
	f.m.close()
	disabled := newUpdateManager(cfg)
	defer disabled.close()
	if disabled.unready != "UPDATE_UNAVAILABLE" || !disabled.recovery {
		t.Fatalf("disabled lost recovery: %s %v", disabled.unready, disabled.recovery)
	}
	ts := httptest.NewServer(buildRoutesWithUpdates(cfg, disabled))
	defer ts.Close()
	for _, path := range []string{"/server/start", "/server/stop", "/server/restart", "/server/command?command=shutdown"} {
		status, _ := updateRequest(t, ts, "GET", path, nil)
		if status != 409 {
			t.Fatalf("disabled recovery bypassed %s: %d", path, status)
		}
	}
	status, b := updateRequest(t, ts, "POST", "/server/update/jobs", request)
	if status != 200 || !bytes.Contains(b, []byte("interrupted")) || !bytes.Contains(b, []byte("old-job")) {
		t.Fatalf("disabled replay: %d %s", status, b)
	}
	status, _ = updateRequest(t, ts, "POST", "/server/update/plan", struct{}{})
	if status != 503 {
		t.Fatal("disabled manager accepted a new plan")
	}
}

func TestUpdateRestartInvalidatesUnusedPlan(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	f.m.close()
	cfg := f.cfg
	cfg.APIBaseURL = "http://different-server.invalid/api"
	cfg.UpdateApplyCmd = "different-hook"
	m := newUpdateManager(cfg)
	defer m.close()
	m.runner = f.runner
	ts := httptest.NewServer(buildRoutesWithUpdates(cfg, m))
	defer ts.Close()
	status, b := updateRequest(t, ts, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "restart_0123456789abcdef"})
	if status != 404 || !bytes.Contains(b, []byte("PLAN_NOT_FOUND")) || len(f.runner.commands()) != 1 {
		t.Fatalf("old confirmation survived changed config: %d %s", status, b)
	}
}

func TestUpdateParallelInitialAcceptance(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	f.runner.block = "apply-hook"
	start := make(chan struct{})
	results := make(chan struct {
		status int
		job    UpdateJob
	}, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, job := f.submit(t, p, "initial_0123456789abcdef")
			results <- struct {
				status int
				job    UpdateJob
			}{status, job}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	firstID := ""
	acceptedCount := 0
	for result := range results {
		if result.status == 202 {
			acceptedCount++
		}
		if firstID == "" {
			firstID = result.job.JobID
		}
		if result.job.JobID != firstID {
			t.Fatal("parallel initial requests created different jobs")
		}
	}
	if acceptedCount != 1 {
		t.Fatalf("202 count=%d", acceptedCount)
	}
	close(f.runner.release)
	if job := f.wait(t, firstID); job.Status != "succeeded" || len(f.runner.commands()) != 7 {
		t.Fatalf("duplicate execution: %+v %v", job, f.runner.commands())
	}
}

func TestUpdateLostAcceptanceResponseReplaysSameJobAfterRestart(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	base := buildRoutesWithUpdates(f.cfg, f.m)
	var lost atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/server/update/jobs" && lost.CompareAndSwap(false, true) {
			recorder := httptest.NewRecorder()
			base.ServeHTTP(recorder, r)
			if recorder.Code != 202 {
				t.Errorf("acceptance=%d", recorder.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()
	request := UpdateJobRequest{p.PlanID, updateConfirmation, "lost_0123456789abcdef"}
	body, _ := json.Marshal(request)
	req, _ := http.NewRequest("POST", ts.URL+"/server/update/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer update-test-token")
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("mock did not lose the response")
	}
	status, b := updateRequest(t, ts, "POST", "/server/update/jobs", request)
	if status != 200 {
		t.Fatalf("retry %d %s", status, b)
	}
	var replay struct {
		Data UpdateJob `json:"data"`
	}
	_ = json.Unmarshal(b, &replay)
	job := f.wait(t, replay.Data.JobID)
	if job.Status != "succeeded" || len(f.runner.commands()) != 7 {
		t.Fatalf("%+v %v", job, f.runner.commands())
	}
	f.m.close()
	m := newUpdateManager(f.cfg)
	defer m.close()
	m.runner = f.runner
	ts2 := httptest.NewServer(buildRoutesWithUpdates(f.cfg, m))
	defer ts2.Close()
	status, b = updateRequest(t, ts2, "POST", "/server/update/jobs", request)
	if status != 200 || !bytes.Contains(b, []byte(job.JobID)) || len(f.runner.commands()) != 7 {
		t.Fatalf("restart replay %d %s", status, b)
	}
}

func TestUpdateVersionLineNormalization(t *testing.T) {
	old := "Game version: V 2.2 (b3) Compatibility Version: V 2.2"
	newVersion := "Game version: V 2.3 (b9) Compatibility Version: V 2.3"
	f := newUpdateFixture(t)
	f.game.version = old + "\r\nMod B: 1\r\nMod A: 1\r\n"
	f.m.cfg.UpdateTargetVersion = newVersion
	p := f.plan(t)
	if p.CurrentVersion != old || p.TargetVersion != newVersion {
		t.Fatalf("%+v", p)
	}
	f.game.mu.Lock()
	f.game.version = "\n" + old + "\nMod A: 1\nMod B: 1\n"
	f.game.mu.Unlock()
	f.runner.keepVersion = true
	f.runner.block = "start-hook"
	_, accepted := f.submit(t, p, "mods_0123456789abcdef")
	select {
	case <-f.runner.entered:
	case <-time.After(time.Second):
		t.Fatal("start did not enter")
	}
	f.game.mu.Lock()
	f.game.version = newVersion + "\nMod C: 2\nMod A: 1\n"
	f.game.mu.Unlock()
	close(f.runner.release)
	job := f.wait(t, accepted.JobID)
	if job.Status != "succeeded" || job.ResultVersion != newVersion {
		t.Fatalf("Mod ordering affected game verification: %+v", job)
	}
	for _, value := range []string{old + "\n" + old, "Game version: unknown", "Unknown command version"} {
		if _, err := normalizeUpdateVersion(value); err == nil {
			t.Fatalf("ambiguous version accepted: %q", value)
		}
	}
	if actual, err := normalizeUpdateVersion("\tGame  version:\tV 2.3  (b9) Compatibility Version: V 2.3 \r\nMod A: 1"); err != nil || actual != newVersion {
		t.Fatalf("normalization %q %v", actual, err)
	}
}

func TestUpdateExpiryAndReplayAfterExpiry(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	f.m.mu.Lock()
	expired := p
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	f.m.journal.Plans[p.PlanID] = expired
	f.m.mu.Unlock()
	status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "expiry_0123456789abcdef"})
	if status != 409 || !bytes.Contains(b, []byte("PLAN_EXPIRED")) {
		t.Fatalf("%d %s", status, b)
	}
	f.m.mu.Lock()
	f.m.journal.Plans[p.PlanID] = p
	f.m.mu.Unlock()
	_, accepted := f.submit(t, p, "expiry_0123456789abcdef")
	f.wait(t, accepted.JobID)
	f.m.mu.Lock()
	f.m.journal.Plans[p.PlanID] = expired
	f.m.mu.Unlock()
	status, job := f.submit(t, p, "expiry_0123456789abcdef")
	if status != 200 || job.JobID != accepted.JobID || len(f.runner.commands()) != 7 {
		t.Fatal("expired replay executed again")
	}
}

func TestUpdateMalformedVersionAndSecretError(t *testing.T) {
	for _, kind := range []string{"malformed", "upstream_error"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			if kind == "malformed" {
				f.game.version = "Unknown command: version"
			} else {
				f.game.commandError = true
			}
			status, b := updateRequest(t, f.server, "POST", "/server/update/plan", struct{}{})
			if status != 502 || bytes.Contains(b, []byte("DO_NOT_LEAK")) {
				t.Fatalf("%d %s", status, b)
			}
		})
	}
}

func TestUpdateRequestCancellationDoesNotCancelAcceptedJob(t *testing.T) {
	f := newUpdateFixture(t)
	p := f.plan(t)
	f.runner.block = "apply-hook"
	body, _ := json.Marshal(UpdateJobRequest{p.PlanID, updateConfirmation, "detached_0123456789abcdef"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.server.URL+"/server/update/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer update-test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Data UpdateJob `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatal(resp.StatusCode)
	}
	cancel()
	select {
	case <-f.runner.entered:
	case <-time.After(time.Second):
		t.Fatal("hook did not enter")
	}
	close(f.runner.release)
	if final := f.wait(t, job.Data.JobID); final.Status != "succeeded" {
		t.Fatalf("%+v", final)
	}
}

func TestUpdateTimeoutAndTargetConfigurationChange(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newUpdateFixture(t)
		p := f.plan(t)
		f.m.cfg.UpdateTimeout = 500 * time.Millisecond
		f.runner.block = "apply-hook"
		_, accepted := f.submit(t, p, "timeout_0123456789abcdef")
		job := f.wait(t, accepted.JobID)
		if job.Status != "failed" || job.Error.Code != "UPDATE_TIMEOUT" || !job.RecoveryRequired {
			t.Fatalf("%+v", job)
		}
	})
	t.Run("target", func(t *testing.T) {
		f := newUpdateFixture(t)
		p := f.plan(t)
		f.m.cfg.UpdateTargetVersion = "Game version: V 2.4 (b1) Compatibility Version: V 2.4"
		status, b := updateRequest(t, f.server, "POST", "/server/update/jobs", UpdateJobRequest{p.PlanID, updateConfirmation, "target_0123456789abcdef"})
		if status != 409 || !bytes.Contains(b, []byte("VERSION_CHANGED")) || len(f.runner.commands()) != 1 {
			t.Fatalf("%d %s", status, b)
		}
	})
}

func TestUpdateCorruptJournalFailsClosed(t *testing.T) {
	f := newUpdateFixture(t)
	f.m.close()
	if err := os.WriteFile(f.m.statePath(), []byte(`{"plans":{},"jobs":{},"keys":{"valid_0123456789abcdef":{"jobId":"missing"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	m := newUpdateManager(f.cfg)
	defer m.close()
	if m.unready != "UPDATE_STATE_UNAVAILABLE" {
		t.Fatal("corrupt key/job references accepted")
	}
	ts := httptest.NewServer(buildRoutesWithUpdates(f.cfg, m))
	defer ts.Close()
	status, _ := updateRequest(t, ts, "GET", "/server/start", nil)
	if status != 503 {
		t.Fatalf("corrupt state allowed mutation: %d", status)
	}
}

func TestUpdateOpenAPIContract(t *testing.T) {
	f := newUpdateFixture(t)
	_, rt := loadOpenAPISpecWithServer(t, f.server.URL)
	headers := map[string]string{"Authorization": "Bearer update-test-token", "Content-Type": "application/json"}
	req, resp, b := doReq(t, f.server, "POST", "/server/update/plan", strings.NewReader(`{}`), headers)
	if err := validateResponseWithOpenAPI(t, rt, req, resp, b); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Data UpdatePlan `json:"data"`
	}
	_ = json.Unmarshal(b, &plan)
	body, _ := json.Marshal(UpdateJobRequest{plan.Data.PlanID, updateConfirmation, "openapi_0123456789abcdef"})
	req, resp, b = doReq(t, f.server, "POST", "/server/update/jobs", bytes.NewReader(body), headers)
	if err := validateResponseWithOpenAPI(t, rt, req, resp, b); err != nil {
		t.Fatal(err)
	}
	var job struct {
		Data UpdateJob `json:"data"`
	}
	_ = json.Unmarshal(b, &job)
	f.wait(t, job.Data.JobID)
	for _, path := range []string{"/server/update/jobs/" + job.Data.JobID, "/server/update/jobs/latest", "/server/update/jobs/missing"} {
		req, resp, b = doReq(t, f.server, "GET", path, nil, headers)
		if err := validateResponseWithOpenAPI(t, rt, req, resp, b); err != nil {
			t.Fatal(err)
		}
	}
}
