package a4c_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// VALIDATION-ONLY ADDITION (acceptance review, 2026-10-21): two Patch 1
// invariants had no direct regression test after the direct-only
// simplification. These tests add no production behavior; they pin guarantees
// the implementation already provides:
//
//   - interrupted execution can never be accepted or integrated as success;
//   - a git commit failure during integration propagates and aborts.
//
// Committed separately from the implementation commit 559da5c.

// TestGate_InterruptedRootCannotBeAcceptedOrIntegrated: an interrupted task is
// a failed terminal outcome for finalization purposes - durable and
// inspectable, but never acceptable, and never integrable.
func TestGate_InterruptedRootCannotBeAcceptedOrIntegrated(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h)

	if err := h.store.InterruptWithResult(task.TaskID, a4c.ActivityResult{
		ID:     task.TaskID,
		Status: a4c.StatusInterrupted,
		Outputs: map[string]any{
			"interruption_reason":  "process_interrupted",
			"interruption_message": "host session terminated mid-run",
		},
	}, errors.New("host session terminated mid-run")); err != nil {
		t.Fatalf("interrupt root: %v", err)
	}

	// Recorded state stays interrupted (durable, not silently flipped).
	if got := liStatus(t, h, task.TaskID); got != a4c.StatusInterrupted {
		t.Fatalf("root status = %s, want interrupted", got)
	}

	if err := liAccept(h, task.TaskID); err == nil {
		t.Fatalf("interrupted task was accepted as success")
	}
	if err := liIntegrate(h, task.TaskID); err == nil {
		t.Fatalf("interrupted task was integrated as success")
	}

	// The graph reports the interruption rather than hiding it.
	violations, err := h.service.ValidateTaskGraph(task.TaskID)
	if err != nil || len(violations) == 0 {
		t.Fatalf("interrupted root not reported as a graph violation: %v %v", violations, err)
	}
}

// TestGate_GitCommitFailureDuringIntegrationPropagates: the integration commit
// happens in the task's worktree; if it cannot be created, integration must
// abort with an error - proceeding would merge content that was never
// committed against the approval.
func TestGate_GitCommitFailureDuringIntegrationPropagates(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h)

	// Real work in the worktree, completed and accepted.
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "feature.go"), []byte("package fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Inject a reproducible commit failure: a pre-commit hook that always
	// fails. Git worktrees share the base repository's hooks directory.
	hooksDir := filepath.Join(h.repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooksDir, "pre-commit")
	body := "#!/bin/sh\necho 'injected pre-commit failure' >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hook) })

	// Validate the injection works in this environment before asserting on
	// the service: if the platform's git does not execute hooks, this is an
	// environmental limitation, reported as a skip rather than a false pass.
	probe := filepath.Join(task.WorktreePath, "probe.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	probeCmd := exec.Command("git", "add", "probe.tmp")
	probeCmd.Dir = task.WorktreePath
	if out, err := probeCmd.CombinedOutput(); err != nil {
		t.Fatalf("probe git add: %v (%s)", err, out)
	}
	probeCommit := exec.Command("git", "commit", "-m", "probe")
	probeCommit.Dir = task.WorktreePath
	if out, err := probeCommit.CombinedOutput(); err == nil {
		t.Skipf("environment does not execute git hooks (%s); commit-failure injection unavailable", runtime.GOOS)
	} else if !strings.Contains(string(out), "injected pre-commit failure") {
		t.Skipf("git hooks not enforced here (probe output: %s)", strings.TrimSpace(string(out)))
	}
	// Unstage the probe so the worktree returns to its pre-probe shape.
	resetCmd := exec.Command("git", "reset", "-q", "HEAD")
	resetCmd.Dir = task.WorktreePath
	_, _ = resetCmd.CombinedOutput()
	_ = os.Remove(probe)

	// Integration must fail with the propagated commit failure, never
	// silently succeed.
	_, err := h.service.IntegrateTask(context.Background(), a4c.IntegrateTaskRequest{TaskID: task.TaskID})
	if err == nil {
		t.Fatalf("integration succeeded despite a failing git commit")
	}
	if !strings.Contains(err.Error(), "git commit failed during integration") {
		t.Fatalf("integration aborted for the wrong reason (want propagated commit failure), got: %v", err)
	}

	// The task must remain unintegrated and re-triable once the hook is gone.
	details, err := h.service.GetTask(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if details.State == a4c.TaskStateIntegrated {
		t.Fatalf("task marked integrated after a failed integration commit")
	}
}
