package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type updateMetadataRunner struct {
	base     *updateFakeRunner
	mu       sync.Mutex
	metadata []updateHookMetadata
}

func (r *updateMetadataRunner) Run(ctx context.Context, cmd string) (ExecResult, error) {
	r.mu.Lock()
	metadata, _ := ctx.Value(updateHookContextKey{}).(updateHookMetadata)
	r.metadata = append(r.metadata, metadata)
	r.mu.Unlock()
	return r.base.Run(ctx, cmd)
}

func TestUpdateFinishAfterVerification(t *testing.T) {
	for _, failure := range []string{"", "finish-hook", "wrong-version"} {
		t.Run(failure, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.m.cfg.UpdateFinishCmd = "finish-hook"
			if failure == "wrong-version" {
				f.runner.keepVersion = true
			} else {
				f.runner.fail = failure
			}
			p := f.plan(t)
			if len(p.Steps) != 8 || p.Steps[7] != "releasing" {
				t.Fatal(p.Steps)
			}
			_, accepted := f.submit(t, p, "finish_0123456789abcdef")
			job := f.wait(t, accepted.JobID)
			calls := f.runner.commands()
			switch failure {
			case "":
				if job.Status != "succeeded" || calls[len(calls)-1] != "finish-hook" || job.RecoveryRequired {
					t.Fatalf("job=%+v calls=%v", job, calls)
				}
			case "finish-hook":
				if job.Status != "failed" || job.Phase != "releasing" || !job.RecoveryRequired || job.Error.Code != "FINISH_FAILED" {
					t.Fatalf("job=%+v", job)
				}
			case "wrong-version":
				if strings.Contains(strings.Join(calls, ","), "finish-hook") || job.Error.Code != "VERIFY_FAILED" || !job.RecoveryRequired {
					t.Fatalf("job=%+v calls=%v", job, calls)
				}
			}
		})
	}
}

func TestUpdateTrustedHookMetadata(t *testing.T) {
	f := newUpdateFixture(t)
	runner := &updateMetadataRunner{base: f.runner}
	f.m.runner = runner
	p := f.plan(t)
	_, accepted := f.submit(t, p, "metadata_0123456789abcdef")
	job := f.wait(t, accepted.JobID)
	if job.Status != "succeeded" {
		t.Fatal(job)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for i, value := range runner.metadata {
		if value.TargetVersion != p.TargetVersion {
			t.Fatal(value)
		}
		if i == 0 {
			if value.JobID != "" || value.CurrentVersion != "" {
				t.Fatal(value)
			}
		} else if value.JobID != job.JobID || value.CurrentVersion != job.CurrentVersion {
			t.Fatal(value)
		}
	}
}

func TestUpdateHookEnvironmentOverridesInheritedMetadata(t *testing.T) {
	t.Setenv("OPSA_UPDATE_JOB_ID", "untrusted-inherited-id")
	t.Setenv("OPSA_UPDATE_TARGET_VERSION", "untrusted-inherited-target")
	ctx := context.WithValue(context.Background(), updateHookContextKey{}, updateHookMetadata{"trusted-job", "current", "target"})
	for _, current := range []context.Context{ctx, context.Background()} {
		values := map[string][]string{}
		for _, item := range updateHookEnvironment(current) {
			key, value, _ := strings.Cut(item, "=")
			values[key] = append(values[key], value)
		}
		for _, key := range []string{"OPSA_UPDATE_JOB_ID", "OPSA_UPDATE_CURRENT_VERSION", "OPSA_UPDATE_TARGET_VERSION"} {
			if len(values[key]) != 1 || strings.Contains(values[key][0], "untrusted") {
				t.Fatal(values[key])
			}
			if current == context.Background() && values[key][0] != "" {
				t.Fatal(values[key])
			}
		}
	}
}
