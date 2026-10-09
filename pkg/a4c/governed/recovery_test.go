package governed_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/a4c/pkg/a4c/governed"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type testRig struct {
	repoDir     string
	service     a4c.TeamService
	manager     *governed.Manager
	taskID      string
	dispatchDir string
}

func setupTestRig(t *testing.T) *testRig {
	t.Helper()

	repoDir := t.TempDir()

	// Initialize git repo
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Governed Tester")
	runGit(t, repoDir, "config", "user.email", "governed@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Governed Workspace\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/test\n\ngo 1.23.0\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nfunc Answer() int {\n\treturn 42\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fail()\n\t}\n}\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "chore: initial commit")

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")
	structDir := filepath.Join(dotDir, "structural")
	dispatchDir := filepath.Join(dotDir, "yawr-dispatch")

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	lifecycleStore, err := a4c.NewDurableLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatalf("create lifecycle store: %v", err)
	}
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("create roster: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	wtMgr, err := a4c.NewWorktreeManager(repoDir, wtDir)
	if err != nil {
		t.Fatalf("create worktrees: %v", err)
	}
	structStore, err := a4c.NewDurableStructuralStore(structDir)
	if err != nil {
		t.Fatalf("create structural store: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_git_inspect", "governed_run_tests",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr, structStore)

	ctx := context.Background()
	taskRes, err := svc.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective: "Test Governed YAWR Execution",
		Strategy:  "team",
	})
	if err != nil {
		t.Fatalf("begin task: %v", err)
	}

	// Assign activity to Bob (coder) so task is Running and active agent is coder
	_, err = svc.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskRes.TaskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Implement feature",
	})
	if err != nil {
		t.Fatalf("assign activity: %v", err)
	}

	mgr, err := governed.NewManager(svc, dispatchDir)
	if err != nil {
		t.Fatalf("create governed manager: %v", err)
	}

	return &testRig{
		repoDir:     repoDir,
		service:     svc,
		manager:     mgr,
		taskID:      taskRes.TaskID,
		dispatchDir: dispatchDir,
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\nOutput: %s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// 1. Repeated identical patch invocation (idempotent replay, Replayed == true)
func TestGoverned_IdempotentReplay(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	patch := `diff --git a/feature.go b/feature.go
new file mode 100644
--- /dev/null
+++ b/feature.go
@@ -0,0 +1,5 @@
+package main
+
+func Feature() string {
+	return "yawr-governed"
+}
`
	key := "idemp-patch-001"

	// First invocation -> executes patch
	res1, err := rig.manager.ApplyPatch(ctx, rig.taskID, patch, "", key)
	if err != nil {
		t.Fatalf("first apply_patch failed: %v", err)
	}
	if res1.Status != tool.InvocationStatusCompleted {
		t.Fatalf("expected status completed, got %s: %s", res1.Status, res1.ErrorMessage)
	}
	if res1.Replayed {
		t.Errorf("expected res1.Replayed to be false, got true")
	}
	if res1.OccurrenceID == "" {
		t.Errorf("expected non-empty OccurrenceID")
	}

	// Verify file was actually created in worktree
	wtPath, err := rig.service.GetWorktreePath(rig.taskID)
	if err != nil {
		t.Fatalf("get worktree path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "feature.go")); os.IsNotExist(err) {
		t.Fatalf("feature.go was not created in worktree")
	}

	// Second invocation with identical request and key -> returns replayed result without duplicate execution
	res2, err := rig.manager.ApplyPatch(ctx, rig.taskID, patch, "", key)
	if err != nil {
		t.Fatalf("second apply_patch failed: %v", err)
	}
	if res2.Status != tool.InvocationStatusCompleted {
		t.Fatalf("expected status completed, got %s", res2.Status)
	}
	if !res2.Replayed {
		t.Errorf("expected res2.Replayed to be true, got false")
	}
	if res2.OccurrenceID != res1.OccurrenceID {
		t.Errorf("expected same OccurrenceID %s, got %s", res1.OccurrenceID, res2.OccurrenceID)
	}
}

// 2. Reused idempotency key with conflicting patch (ErrIdempotencyConflict)
func TestGoverned_IdempotencyConflict(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	patchA := `diff --git a/a.txt b/a.txt
new file mode 100644
--- /dev/null
+++ b/a.txt
@@ -0,0 +1 @@
+patch-a
`
	patchB := `diff --git a/b.txt b/b.txt
new file mode 100644
--- /dev/null
+++ b/b.txt
@@ -0,0 +1 @@
+patch-b
`
	key := "conflict-key-999"

	resA, err := rig.manager.ApplyPatch(ctx, rig.taskID, patchA, "", key)
	if err != nil || resA.Status != tool.InvocationStatusCompleted {
		t.Fatalf("patchA failed: %v (%+v)", err, resA)
	}

	// Reusing key with different patch must return ErrIdempotencyConflict
	resB, err := rig.manager.ApplyPatch(ctx, rig.taskID, patchB, "", key)
	if err == nil {
		if resB != nil && resB.Status != tool.InvocationStatusFailed {
			t.Fatalf("expected idempotency conflict error, got success: %+v", resB)
		}
	} else if !errors.Is(err, tool.ErrIdempotencyConflict) && !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("expected ErrIdempotencyConflict, got: %v", err)
	}
}

// 3. State recovery across process restart (NewManager on existing dispatchDir)
func TestGoverned_ProcessRestartRecovery(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	patch := `diff --git a/restart.txt b/restart.txt
new file mode 100644
--- /dev/null
+++ b/restart.txt
@@ -0,0 +1 @@
+restart-test
`
	key := "restart-key-42"

	res1, err := rig.manager.ApplyPatch(ctx, rig.taskID, patch, "", key)
	if err != nil || res1.Status != tool.InvocationStatusCompleted {
		t.Fatalf("first apply failed: %v (%+v)", err, res1)
	}

	// Simulate process restart: instantiate a fresh Manager on the existing dispatchDir
	mgr2, err := governed.NewManager(rig.service, rig.dispatchDir)
	if err != nil {
		t.Fatalf("instantiate second manager: %v", err)
	}

	// Re-invoking with same key must replay from durable dispatch store
	res2, err := mgr2.ApplyPatch(ctx, rig.taskID, patch, "", key)
	if err != nil {
		t.Fatalf("second manager invoke failed: %v", err)
	}
	if res2.Status != tool.InvocationStatusCompleted {
		t.Fatalf("expected completed status, got %s", res2.Status)
	}
	if !res2.Replayed {
		t.Fatalf("expected replayed=true after restart recovery, got false")
	}
	if res2.OccurrenceID != res1.OccurrenceID {
		t.Errorf("expected same occurrence ID %s, got %s", res1.OccurrenceID, res2.OccurrenceID)
	}
}

// 4. Stale snapshot precondition rejection (ErrStaleSnapshot)
func TestGoverned_StaleSnapshotPrecondition(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	patch := `diff --git a/snapshot.txt b/snapshot.txt
new file mode 100644
--- /dev/null
+++ b/snapshot.txt
@@ -0,0 +1 @@
+snapshot test
`
	// Supply wrong / stale commit SHA
	res, err := rig.manager.ApplyPatch(ctx, rig.taskID, patch, "deadbeefcafebabe000000000000000000000000", "snap-key-1")
	if err == nil {
		if res != nil && res.Status == tool.InvocationStatusCompleted {
			t.Fatalf("expected stale snapshot rejection, but succeeded: %+v", res)
		}
		if res != nil && !strings.Contains(res.ErrorMessage, "stale snapshot") {
			t.Errorf("expected stale snapshot in error message, got %s", res.ErrorMessage)
		}
	} else if !errors.Is(err, governed.ErrStaleSnapshot) && !strings.Contains(err.Error(), "stale snapshot") {
		t.Fatalf("expected ErrStaleSnapshot, got: %v", err)
	}
}

// 5. Authority denial when Charlie or non-coder attempts apply_patch (ErrUnauthorizedMutation)
func TestGoverned_AuthorityDenial_CharlieOrNonCoder(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	// Switch active activity to Charlie (reviewer)
	_, err := rig.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    rig.taskID,
		Agent:     "charlie",
		Role:      "reviewer",
		Objective: "Review changes",
	})
	if err != nil {
		t.Fatalf("assign activity to charlie: %v", err)
	}

	patch := `diff --git a/unauth.txt b/unauth.txt
new file mode 100644
--- /dev/null
+++ b/unauth.txt
@@ -0,0 +1 @@
+unauthorized
`

	res, err := rig.manager.ApplyPatch(ctx, rig.taskID, patch, "", "unauth-key")
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res.Status != tool.InvocationStatusDenied {
		t.Fatalf("expected status denied for reviewer mutating attempt, got %s (err: %s)", res.Status, res.ErrorMessage)
	}
	if !strings.Contains(res.ErrorMessage, "allow_mutating=false") {
		t.Errorf("expected allow_mutating=false in denial message, got: %s", res.ErrorMessage)
	}
}

// 6. Filesystem traversal attempt (e.g. ../../ or symlink escape) fails closed (ErrFilesystemEscape)
func TestGoverned_FilesystemTraversal_SymlinkEscape(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	// 6a. Direct directory traversal path
	resA, errA := rig.manager.ReadFile(ctx, rig.taskID, "../../etc/passwd")
	if errA != nil {
		if !errors.Is(errA, governed.ErrFilesystemEscape) && !strings.Contains(errA.Error(), "outside worktree") {
			t.Fatalf("expected ErrFilesystemEscape, got: %v", errA)
		}
	} else {
		if resA.Status != tool.InvocationStatusFailed {
			t.Fatalf("expected failed status for directory traversal, got %s", resA.Status)
		}
		if !errors.Is(resA.Error, governed.ErrFilesystemEscape) && !strings.Contains(resA.ErrorMessage, "outside worktree") {
			t.Fatalf("expected ErrFilesystemEscape in result error, got %v (errMsg: %s)", resA.Error, resA.ErrorMessage)
		}
	}

	// 6b. Symlink escape check
	wtPath, err := rig.service.GetWorktreePath(rig.taskID)
	if err != nil {
		t.Fatalf("get worktree path: %v", err)
	}

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	_ = os.WriteFile(outsideSecret, []byte("super-secret"), 0644)

	symlinkPath := filepath.Join(wtPath, "leak_link")
	if err := os.Symlink(outsideSecret, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	resB, errB := rig.manager.ReadFile(ctx, rig.taskID, "leak_link")
	if errB != nil {
		if !errors.Is(errB, governed.ErrFilesystemEscape) && !strings.Contains(errB.Error(), "resolves outside worktree") {
			t.Fatalf("expected ErrFilesystemEscape for symlink escape, got: %v", errB)
		}
	} else {
		if resB.Status != tool.InvocationStatusFailed {
			t.Fatalf("expected failed status for symlink escape, got %s", resB.Status)
		}
		if !errors.Is(resB.Error, governed.ErrFilesystemEscape) && !strings.Contains(resB.ErrorMessage, "resolves outside worktree") {
			t.Fatalf("expected ErrFilesystemEscape in result error, got %v (errMsg: %s)", resB.Error, resB.ErrorMessage)
		}
	}
}

// 7. Deterministic validation procedure / coexistence with YAWR workflow API
func TestGoverned_WorkflowCoexistence_AndValidation(t *testing.T) {
	rig := setupTestRig(t)
	ctx := context.Background()

	// 7a. Governed ReadFile on legitimate file
	readRes, err := rig.manager.ReadFile(ctx, rig.taskID, "README.md")
	if err != nil || readRes.Status != tool.InvocationStatusCompleted {
		t.Fatalf("read_file failed: %v (%+v)", err, readRes)
	}
	if !strings.Contains(readRes.Stdout, "Governed Workspace") {
		t.Errorf("read_file stdout missing expected content: %s", readRes.Stdout)
	}

	// 7b. Governed GitInspect
	gitRes, err := rig.manager.GitInspect(ctx, rig.taskID, "status", "")
	if err != nil || gitRes.Status != tool.InvocationStatusCompleted {
		t.Fatalf("git_inspect failed: %v (%+v)", err, gitRes)
	}

	// 7c. Governed RunTests
	testRes, err := rig.manager.RunTests(ctx, rig.taskID, "./...", true, "test-key-1")
	if err != nil || testRes.Status != tool.InvocationStatusCompleted {
		t.Fatalf("run_tests failed: %v (%+v)", err, testRes)
	}
	if !strings.Contains(testRes.Stdout, "PASS") {
		t.Errorf("run_tests stdout missing PASS: %s", testRes.Stdout)
	}
	if testRes.ExitCode != 0 {
		t.Errorf("run_tests exit code = %d, want 0", testRes.ExitCode)
	}
}
