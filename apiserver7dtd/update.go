package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const updateConfirmation = "UPDATE SUZUME"

var updateKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var updateVersionPattern = regexp.MustCompile(`^Game version: V \S+`)

// Build and compatibility information remain part of the strict comparison.
// Mod output ordering and transport line endings do not identify the game build.
func normalizeUpdateVersion(output string) (string, error) {
	version := ""
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if !strings.HasPrefix(line, "Game version:") {
			continue
		}
		if version != "" || !updateVersionPattern.MatchString(line) {
			return "", errors.New("ambiguous or invalid game version")
		}
		version = line
	}
	if version == "" || len(version) > 4096 {
		return "", errors.New("game version missing")
	}
	return version, nil
}

type UpdatePlan struct {
	PlanID         string    `json:"planId"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CurrentVersion string    `json:"currentVersion"`
	TargetVersion  string    `json:"targetVersion"`
	OnlinePlayers  int       `json:"onlinePlayers"`
	CanExecute     bool      `json:"canExecute"`
	Blockers       []string  `json:"blockers"`
	Confirmation   string    `json:"confirmation"`
	Steps          []string  `json:"steps"`
}

type UpdateJob struct {
	JobID            string       `json:"jobId"`
	PlanID           string       `json:"planId"`
	Status           string       `json:"status"`
	Phase            string       `json:"phase"`
	CreatedAt        time.Time    `json:"createdAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
	FinishedAt       *time.Time   `json:"finishedAt,omitempty"`
	CurrentVersion   string       `json:"currentVersion"`
	TargetVersion    string       `json:"targetVersion"`
	ResultVersion    string       `json:"resultVersion,omitempty"`
	BackupID         string       `json:"backupId,omitempty"`
	RecoveryRequired bool         `json:"recoveryRequired"`
	Error            *ErrorDetail `json:"error,omitempty"`
}

type UpdateJobRequest struct {
	PlanID         string `json:"planId"`
	Confirmation   string `json:"confirmation"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type updateKey struct {
	Request UpdateJobRequest `json:"request"`
	JobID   string           `json:"jobId"`
}

type updateJournal struct {
	Plans  map[string]UpdatePlan `json:"plans"`
	Jobs   map[string]UpdateJob  `json:"jobs"`
	Keys   map[string]updateKey  `json:"keys"`
	Latest string                `json:"latest"`
}

// One manager owns the journal for the lifetime of the API process. The OS lease
// prevents another API process from mutating the same state directory.
type updateManager struct {
	cfg      Config
	mu       sync.Mutex
	journal  updateJournal
	busy     bool
	recovery bool
	unready  string
	lease    *os.File
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	runner   CommandRunner
}

func newUpdateManager(cfg Config) *updateManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &updateManager{cfg: cfg, ctx: ctx, cancel: cancel, runner: cmdRunner,
		journal: updateJournal{Plans: map[string]UpdatePlan{}, Jobs: map[string]UpdateJob{}, Keys: map[string]updateKey{}}}
	if _, ok := m.runner.(ShellRunner); ok {
		m.runner = updateShellRunner{}
	}
	target, targetErr := normalizeUpdateVersion(cfg.UpdateTargetVersion)
	if targetErr == nil {
		m.cfg.UpdateTargetVersion = target
	}
	if !cfg.UpdateEnabled {
		m.unready = "UPDATE_UNAVAILABLE"
	}
	if cfg.UpdateEnabled && ((strings.TrimSpace(cfg.AuthBearerToken) == "" && strings.TrimSpace(cfg.APIKey) == "") || cfg.AllowNoAuth ||
		targetErr != nil || len(cfg.UpdateTargetVersion) > 4096 || cfg.UpdateTimeout <= 0 || cfg.UpdateVerifyTimeout <= 0) {
		m.unready = "UPDATE_UNAVAILABLE"
	}
	for _, cmd := range []string{cfg.UpdatePreflightCmd, cfg.UpdateStopCmd, cfg.UpdateCheckStoppedCmd, cfg.UpdateBackupCmd, cfg.UpdateApplyCmd, cfg.UpdateStartCmd} {
		if cfg.UpdateEnabled && strings.TrimSpace(cmd) == "" {
			m.unready = "UPDATE_UNAVAILABLE"
		}
	}
	for _, secret := range []string{cfg.APISecret, cfg.APIKey, cfg.AuthBearerToken} {
		if secret != "" && strings.Contains(cfg.UpdateTargetVersion, secret) {
			m.unready = "UPDATE_UNAVAILABLE"
		}
	}
	// Disabling new updates must not discard an existing maintenance fence or
	// permit a second process to bypass ownership of the configured journal.
	if strings.TrimSpace(cfg.UpdateStateDir) == "" {
		m.unready = "UPDATE_UNAVAILABLE"
		return m
	}
	if !filepath.IsAbs(cfg.UpdateStateDir) {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		return m
	}
	if err := os.MkdirAll(cfg.UpdateStateDir, 0700); err != nil {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		return m
	}
	lease, err := lockUpdateState(filepath.Join(cfg.UpdateStateDir, "owner.lock"))
	if err != nil {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		return m
	}
	m.lease = lease
	b, err := os.ReadFile(m.statePath())
	if err == nil {
		if err = json.Unmarshal(b, &m.journal); err != nil || m.journal.Plans == nil || m.journal.Jobs == nil || m.journal.Keys == nil {
			m.journal = updateJournal{}
			m.unready = "UPDATE_STATE_UNAVAILABLE"
			return m
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		return m
	}
	if !m.validJournal() {
		m.journal = updateJournal{}
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		return m
	}
	// A confirmation belongs to this process's fixed operation configuration.
	// Restart requires a fresh plan; accepted jobs and idempotency keys survive.
	m.journal.Plans = map[string]UpdatePlan{}
	for id, job := range m.journal.Jobs {
		if job.Status == "queued" || job.Status == "running" {
			now := time.Now().UTC()
			job.Status, job.UpdatedAt, job.FinishedAt = "interrupted", now, &now
			job.RecoveryRequired = true
			job.Error = &ErrorDetail{Code: "UPDATE_INTERRUPTED", Message: "API process ended before completion; operator recovery is required"}
			m.journal.Jobs[id] = job
		}
		if job.RecoveryRequired {
			m.recovery = true
		}
	}
	if err := m.saveLocked(); err != nil {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
	}
	return m
}

func (m *updateManager) validJournal() bool {
	if m.journal.Latest != "" {
		if _, ok := m.journal.Jobs[m.journal.Latest]; !ok {
			return false
		}
	}
	for id, job := range m.journal.Jobs {
		if id == "" || job.JobID != id {
			return false
		}
		switch job.Status {
		case "queued", "running", "succeeded", "failed", "interrupted":
		default:
			return false
		}
	}
	for key, entry := range m.journal.Keys {
		job, ok := m.journal.Jobs[entry.JobID]
		if !ok || key != entry.Request.IdempotencyKey || !updateKeyPattern.MatchString(key) ||
			entry.Request.Confirmation != updateConfirmation || job.PlanID != entry.Request.PlanID {
			return false
		}
	}
	return true
}

func (m *updateManager) close() {
	m.cancel()
	m.wg.Wait()
	if m.lease != nil {
		_ = m.lease.Close()
	}
}

func (m *updateManager) statePath() string {
	return filepath.Join(m.cfg.UpdateStateDir, "journal.json")
}

// Flush each transition before its side effect. Never resume a recorded job.
func (m *updateManager) saveLocked() error {
	b, err := json.Marshal(m.journal)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.cfg.UpdateStateDir, ".journal-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, m.statePath())
	}
	if err == nil {
		err = syncUpdateDirectory(m.cfg.UpdateStateDir)
	}
	return err
}

func updateError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorDetail{Code: code, Message: message}})
}

func (m *updateManager) available(w http.ResponseWriter) bool {
	m.mu.Lock()
	code := m.unready
	m.mu.Unlock()
	if code != "" {
		updateError(w, 503, code, "Update service is disabled or requires operator configuration")
		return false
	}
	return true
}

// Also serialize legacy game commands: even GET /command may mutate the game.
func (m *updateManager) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		if m.unready == "UPDATE_STATE_UNAVAILABLE" || (m.cfg.UpdateEnabled && m.unready != "") {
			m.mu.Unlock()
			updateError(w, 503, "UPDATE_STATE_UNAVAILABLE", "Update state must be repaired before server operations")
			return
		}
		if m.busy || m.recovery {
			m.mu.Unlock()
			updateError(w, 409, "UPDATE_BUSY", "Another operation or operator recovery holds the server lock")
			return
		}
		m.busy = true
		m.mu.Unlock()
		defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
		next(w, r)
	}
}

func decodeUpdateRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.URL.RawQuery != "" || strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		updateError(w, 400, "INVALID_REQUEST", "Use application/json without query parameters")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		updateError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return false
	}
	return true
}

func updateID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func (m *updateManager) planHandler(w http.ResponseWriter, r *http.Request) {
	if !m.available(w) {
		return
	}
	var body struct{}
	if !decodeUpdateRequest(w, r, &body) {
		return
	}
	m.mu.Lock()
	if m.busy || m.recovery {
		m.mu.Unlock()
		updateError(w, 409, "UPDATE_BUSY", "Another operation or operator recovery holds the server lock")
		return
	}
	m.busy = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := m.runHook(ctx, m.cfg.UpdatePreflightCmd); err != nil {
		updateError(w, 502, "PRECHECK_FAILED", "Operator update preflight failed")
		return
	}
	version, players, err := m.probe(ctx)
	if err != nil {
		updateError(w, 502, "PRECHECK_FAILED", "Current version and online players could not be verified")
		return
	}
	id, err := updateID()
	if err != nil {
		updateError(w, 503, "UPDATE_STATE_UNAVAILABLE", "Could not create a plan")
		return
	}
	now := time.Now().UTC()
	plan := UpdatePlan{PlanID: id, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), CurrentVersion: version,
		TargetVersion: strings.TrimSpace(m.cfg.UpdateTargetVersion), OnlinePlayers: players, CanExecute: true,
		Blockers: []string{}, Confirmation: updateConfirmation,
		Steps: []string{"checking", "stopping", "checking_stopped", "backing_up", "updating", "starting", "verifying"}}
	if players != 0 {
		plan.Blockers = append(plan.Blockers, "PLAYERS_ONLINE")
	}
	if plan.CurrentVersion == plan.TargetVersion {
		plan.Blockers = append(plan.Blockers, "ALREADY_CURRENT")
	}
	plan.CanExecute = len(plan.Blockers) == 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.journal.Plans {
		if now.After(p.ExpiresAt) {
			delete(m.journal.Plans, id)
		}
	}
	if len(m.journal.Plans) >= 128 {
		updateError(w, 503, "UPDATE_STATE_UNAVAILABLE", "Plan capacity reached; retry after plans expire")
		return
	}
	m.journal.Plans[id] = plan
	if err := m.saveLocked(); err != nil {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		updateError(w, 503, m.unready, "Could not persist the plan")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, struct {
		Data UpdatePlan `json:"data"`
	}{plan})
}

func (m *updateManager) createJobHandler(w http.ResponseWriter, r *http.Request) {
	var request UpdateJobRequest
	if !decodeUpdateRequest(w, r, &request) {
		return
	}
	if request.Confirmation != updateConfirmation {
		updateError(w, 400, "CONFIRMATION_REQUIRED", "Explicit UPDATE SUZUME confirmation is required")
		return
	}
	if !updateKeyPattern.MatchString(request.IdempotencyKey) || request.PlanID == "" {
		updateError(w, 400, "INVALID_REQUEST", "Provide planId and a 16-128 character idempotencyKey (letters, digits, _ or -)")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key, ok := m.journal.Keys[request.IdempotencyKey]; ok && m.unready != "UPDATE_STATE_UNAVAILABLE" {
		if key.Request != request {
			updateError(w, 409, "IDEMPOTENCY_CONFLICT", "The idempotency key belongs to a different request")
			return
		}
		m.writeJob(w, 200, m.journal.Jobs[key.JobID])
		return
	}
	if m.unready != "" {
		updateError(w, 503, m.unready, "Update service is disabled or requires operator configuration/state repair")
		return
	}
	if m.busy || m.recovery {
		updateError(w, 409, "UPDATE_BUSY", "Another operation or operator recovery holds the server lock")
		return
	}
	plan, ok := m.journal.Plans[request.PlanID]
	if !ok {
		updateError(w, 404, "PLAN_NOT_FOUND", "Plan not found")
		return
	}
	if time.Now().After(plan.ExpiresAt) {
		updateError(w, 409, "PLAN_EXPIRED", "Create and confirm a fresh plan")
		return
	}
	if !plan.CanExecute {
		updateError(w, 409, "PLAN_BLOCKED", "The plan is blocked by its preconditions")
		return
	}
	if plan.TargetVersion != strings.TrimSpace(m.cfg.UpdateTargetVersion) {
		updateError(w, 409, "VERSION_CHANGED", "The configured target changed; create a fresh plan")
		return
	}
	// A plan is single-use, even if the client generates another idempotency key.
	for _, job := range m.journal.Jobs {
		if job.PlanID == request.PlanID {
			updateError(w, 409, "PLAN_BLOCKED", "This plan already has a job")
			return
		}
	}
	if len(m.journal.Jobs) >= 10000 {
		updateError(w, 503, "UPDATE_STATE_UNAVAILABLE", "Operator journal archival is required")
		return
	}
	id, err := updateID()
	if err != nil {
		updateError(w, 503, "UPDATE_STATE_UNAVAILABLE", "Could not create a job")
		return
	}
	now := time.Now().UTC()
	job := UpdateJob{JobID: id, PlanID: plan.PlanID, Status: "queued", Phase: "queued", CreatedAt: now, UpdatedAt: now,
		CurrentVersion: plan.CurrentVersion, TargetVersion: plan.TargetVersion}
	m.journal.Jobs[id] = job
	m.journal.Keys[request.IdempotencyKey] = updateKey{Request: request, JobID: id}
	m.journal.Latest = id
	if err := m.saveLocked(); err != nil {
		m.unready = "UPDATE_STATE_UNAVAILABLE"
		now := time.Now().UTC()
		job.Status, job.UpdatedAt, job.FinishedAt = "interrupted", now, &now
		job.RecoveryRequired = true
		job.Error = &ErrorDetail{Code: m.unready, Message: "Job was not started because durable acceptance could not be confirmed; operator state repair is required"}
		m.journal.Jobs[id] = job
		m.recovery = true
		updateError(w, 503, m.unready, "Job persistence failed; do not submit a different key")
		return
	}
	m.busy = true
	m.wg.Add(1)
	go m.execute(job)
	w.Header().Set("Location", "/server/update/jobs/"+id)
	m.writeJob(w, 202, job)
}

func (m *updateManager) writeJob(w http.ResponseWriter, status int, job UpdateJob) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, struct {
		Data UpdateJob `json:"data"`
	}{job})
}

func (m *updateManager) jobHandler(w http.ResponseWriter, r *http.Request) {
	m.getJob(w, r.PathValue("jobId"))
}

func (m *updateManager) latestJobHandler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	id := m.journal.Latest
	m.mu.Unlock()
	m.getJob(w, id)
}

func (m *updateManager) getJob(w http.ResponseWriter, id string) {
	// Even if later writes fail, allow inspection of the last known in-memory job.
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.journal.Jobs[id]; ok {
		m.writeJob(w, 200, job)
		return
	}
	if m.unready != "" {
		updateError(w, 503, m.unready, "Update service is unavailable")
		return
	}
	updateError(w, 404, "JOB_NOT_FOUND", "Job not found")
}

func (m *updateManager) transition(job *UpdateJob, phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job.Phase, job.Status, job.UpdatedAt = phase, "running", time.Now().UTC()
	m.journal.Jobs[job.JobID] = *job
	return m.saveLocked()
}

// Never return hook commands, stdout, stderr, URL errors or credentials in the
// update API. Nonzero exit status is a failure even when a runner returns nil.
func (m *updateManager) runHook(ctx context.Context, cmd string) (string, error) {
	res, err := m.runner.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 || ctx.Err() != nil {
		return "", &updateHookError{exitCode: res.ExitCode, timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	return res.Output, nil
}

type updateHookError struct {
	exitCode int
	timedOut bool
}

func (*updateHookError) Error() string { return "operator hook failed" }

func (m *updateManager) execute(job UpdateJob) {
	defer m.wg.Done()
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.UpdateTimeout)
	defer cancel()
	mutated := false
	failure := ""
	var failureDetails map[string]any
	hook := func(command, code string) (string, bool) {
		out, err := m.runHook(ctx, command)
		if err != nil {
			failure = code
			var hookErr *updateHookError
			if errors.As(err, &hookErr) {
				failureDetails = map[string]any{"exitCode": hookErr.exitCode, "timedOut": hookErr.timedOut}
			}
			return "", false
		}
		return out, true
	}
	defer func() {
		if recover() != nil {
			failure = "INTERNAL_ERROR"
		}
		now := time.Now().UTC()
		job.UpdatedAt, job.FinishedAt = now, &now
		if failure == "" {
			job.Status, job.Phase = "succeeded", "completed"
		} else {
			job.Status = "failed"
			if ctx.Err() != nil {
				failure = "UPDATE_TIMEOUT"
				if m.ctx.Err() != nil {
					job.Status, failure = "interrupted", "UPDATE_INTERRUPTED"
				}
			}
			job.RecoveryRequired = mutated
			job.Error = &ErrorDetail{Code: failure, Message: "Update did not complete in phase " + job.Phase + "; inspect the job and operator logs", Details: failureDetails}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.journal.Jobs[job.JobID] = job
		if m.saveLocked() != nil {
			m.unready = "UPDATE_STATE_UNAVAILABLE"
			job.Status, job.RecoveryRequired = "interrupted", true
			job.Error = &ErrorDetail{Code: m.unready, Message: "Final job state could not be persisted; operator recovery is required"}
			m.journal.Jobs[job.JobID] = job
		}
		m.recovery = job.RecoveryRequired
		m.busy = false
	}()
	phase := func(name string) bool {
		if err := m.transition(&job, name); err != nil {
			failure = "UPDATE_STATE_UNAVAILABLE"
			return false
		}
		if ctx.Err() != nil {
			failure = "UPDATE_TIMEOUT"
			return false
		}
		return true
	}
	if !phase("checking") {
		return
	}
	if _, ok := hook(m.cfg.UpdatePreflightCmd, "PRECHECK_FAILED"); !ok {
		return
	}
	version, players, err := m.probe(ctx)
	if err != nil {
		failure = "PRECHECK_FAILED"
		return
	}
	if players != 0 {
		failure = "PLAYERS_ONLINE"
		return
	}
	if version != job.CurrentVersion {
		failure = "VERSION_CHANGED"
		return
	}
	if !phase("stopping") {
		return
	}
	mutated = true // A failed stop may already have stopped some processes.
	if _, ok := hook(m.cfg.UpdateStopCmd, "STOP_FAILED"); !ok {
		return
	}
	if !phase("checking_stopped") {
		return
	}
	if _, ok := hook(m.cfg.UpdateCheckStoppedCmd, "STOP_NOT_VERIFIED"); !ok {
		return
	}
	if !phase("backing_up") {
		return
	}
	out, ok := hook(m.cfg.UpdateBackupCmd, "BACKUP_FAILED")
	if !ok {
		return
	}
	var backup struct {
		BackupID string `json:"backupId"`
		Verified bool   `json:"verified"`
	}
	d := json.NewDecoder(bytes.NewBufferString(out))
	d.DisallowUnknownFields()
	if len(out) > 2048 || d.Decode(&backup) != nil || d.Decode(new(any)) != io.EOF || !backup.Verified || !updateKeyPattern.MatchString(backup.BackupID) {
		failure = "BACKUP_NOT_VERIFIED"
		return
	}
	for _, secret := range []string{m.cfg.APISecret, m.cfg.APIKey, m.cfg.AuthBearerToken} {
		if secret != "" && strings.Contains(backup.BackupID, secret) {
			failure = "BACKUP_NOT_VERIFIED"
			return
		}
	}
	job.BackupID = backup.BackupID
	if !phase("updating") {
		return
	}
	if _, ok := hook(m.cfg.UpdateApplyCmd, "UPDATE_FAILED"); !ok {
		return
	}
	if !phase("starting") {
		return
	}
	if _, ok := hook(m.cfg.UpdateStartCmd, "START_FAILED"); !ok {
		return
	}
	if !phase("verifying") {
		return
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, m.cfg.UpdateVerifyTimeout)
	defer verifyCancel()
	for {
		version, _, err := m.probe(verifyCtx)
		if err == nil {
			job.ResultVersion = version
			if version == job.TargetVersion {
				return
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-verifyCtx.Done():
			timer.Stop()
			failure = "VERIFY_FAILED"
			return
		case <-timer.C:
		}
	}
}

func (m *updateManager) probe(ctx context.Context) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := strings.TrimRight(m.cfg.APIBaseURL, "/")
	var version CommandResponse
	if _, err := httpJSONPost(ctx, base+"/command", m.cfg.APIUser, m.cfg.APISecret, CommandRequest{Command: "version"}, &version); err != nil {
		return "", 0, errors.New("version unavailable")
	}
	current, versionErr := normalizeUpdateVersion(version.Data.Result)
	if versionErr != nil {
		return "", 0, errors.New("version unavailable")
	}
	// A missing data/count/list must never decode to a trustworthy zero.
	var stats struct {
		Data *struct {
			Players *int `json:"players"`
		} `json:"data"`
	}
	var players struct {
		Data *struct {
			Players *[]struct {
				Online *bool `json:"online"`
			} `json:"players"`
		} `json:"data"`
	}
	if _, err := httpJSONGet(ctx, base+"/serverstats", m.cfg.APIUser, m.cfg.APISecret, &stats); err != nil || stats.Data == nil || stats.Data.Players == nil || *stats.Data.Players < 0 {
		return "", 0, errors.New("player count unavailable")
	}
	if _, err := httpJSONGet(ctx, base+"/player", m.cfg.APIUser, m.cfg.APISecret, &players); err != nil || players.Data == nil || players.Data.Players == nil {
		return "", 0, errors.New("player list unavailable")
	}
	online := 0
	for _, p := range *players.Data.Players {
		if p.Online == nil {
			return "", 0, errors.New("player state unavailable")
		}
		if *p.Online {
			online++
		}
	}
	if online != *stats.Data.Players {
		return "", 0, errors.New("player sources disagree")
	}
	for _, secret := range []string{m.cfg.APISecret, m.cfg.APIKey, m.cfg.AuthBearerToken} {
		if secret != "" {
			current = strings.ReplaceAll(current, secret, "[redacted]")
		}
	}
	return current, online, nil
}

// Hook output is limited to backup metadata; bulky updater logs belong in an
// operator-controlled log. WaitDelay bounds local orphaned output pipes on cancel.
type updateShellRunner struct{}

type updateOutput struct{ bytes.Buffer }

func (b *updateOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 2049 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func (updateShellRunner) Run(ctx context.Context, command string) (ExecResult, error) {
	res := ExecResult{StartedAt: time.Now().UTC(), ExitCode: -1}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 3 * time.Second
	var out updateOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	res.FinishedAt = time.Now().UTC()
	res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	res.Output = out.String()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	return res, err
}
