// main_parser_test.go
package main

import (
	"strings"
	"testing"
)

const warnLine = `time="2025-08-17T15:01:04+09:00" level=warning msg="/home/.../docker-compose.yml: the attribute \` + "`version`" + ` is obsolete"`

const upFresh = warnLine + `
 Network a_my-network  Creating
 Container a-web-1  Starting
 Container a-web-1  Started
 Container a-nginx-1  Started
`

const upAlready = warnLine + `
 Container a-web-1  Running
 Container a-nginx-1  Running
`

const upOnlyStarting = warnLine + `
 Container a-web-1  Starting
 Container a-nginx-1  Starting
`

const upMixedRunningThenStarted = warnLine + `
 Container a-web-1  Running
 Container a-nginx-1  Running
 Container a-web-1  Started
 Container a-nginx-1  Started
`

const downFresh = warnLine + `
 Container a-nginx-1  Stopping
 Container a-nginx-1  Stopped
 Container a-nginx-1  Removing
 Container a-nginx-1  Removed
 Container a-web-1  Stopping
 Container a-web-1  Stopped
 Container a-web-1  Removing
 Container a-web-1  Removed
 Network a_my-network  Removing
 Network a_my-network  Removed
`

const downOnlyStopping = warnLine + `
 Container a-web-1  Stopping
 Container a-nginx-1  Stopping
`

func TestDetectStartStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		in               string
		wantStatus       string
		wantNoteContains string // "" のときは note が空を期待
	}{
		{"empty", "", "starting", ""},
		{"warning-only", warnLine, "starting", ""},
		{"fresh-started", upFresh, "started", "Started"},
		{"already-running", upAlready, "already_running", "Running"},
		{"only-starting", upOnlyStarting, "starting", ""},
		{"mixed-running-then-started", upMixedRunningThenStarted, "started", "Started"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			status, note := detectStartStatus(tt.in)
			if status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (input:\n%s\n)", status, tt.wantStatus, tt.in)
			}
			if tt.wantNoteContains == "" {
				if note != "" {
					t.Fatalf("note = %q, want empty", note)
				}
			} else {
				if !strings.Contains(note, tt.wantNoteContains) {
					t.Fatalf("note %q does not contain %q", note, tt.wantNoteContains)
				}
			}
		})
	}
}

func TestDetectStopStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		in               string
		wantStatus       string
		wantNoteContains string // "" のときは note が空を期待
	}{
		{"empty", "", "already_stopped", ""},
		{"warning-only", warnLine, "already_stopped", ""},
		{"fresh-down", downFresh, "stopped", "Removed"}, // Removed/Stopped いずれかを拾えればOK。より確実に Removed で確認。
		{"only-stopping", downOnlyStopping, "stopping", ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			status, note := detectStopStatus(tt.in)
			if status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (input:\n%s\n)", status, tt.wantStatus, tt.in)
			}
			if tt.wantNoteContains == "" {
				if note != "" {
					t.Fatalf("note = %q, want empty", note)
				}
			} else {
				if !strings.Contains(note, tt.wantNoteContains) {
					t.Fatalf("note %q does not contain %q", note, tt.wantNoteContains)
				}
			}
		})
	}
}
