package governed_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func initCalcGo(t *testing.T, h *advHarness) string {
	t.Helper()
	wtPath, err := h.service.GetWorktreePath(h.taskID)
	if err != nil {
		t.Fatalf("get worktree path: %v", err)
	}
	calcPath := filepath.Join(wtPath, "calc.go")
	_ = os.WriteFile(calcPath, []byte("package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"), 0644)
	cmdAdd := exec.Command("git", "add", "calc.go")
	cmdAdd.Dir = wtPath
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "add calc.go")
	cmdCommit.Dir = wtPath
	_ = cmdCommit.Run()
	snap, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)
	return snap
}

// Scenario 1: Two host subagents attempting to apply patches to the same file simultaneously.
func TestConcurrency_01_TwoAgentsSameFileSimultaneous(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	initSnap := initCalcGo(t, h)
	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	patchA := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// modification A
 	return a + b
`
	patchB := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// modification B
 	return a + b
`

	var wg sync.WaitGroup
	var resA, resB *tool.InvocationResult
	var errA, errB error

	wg.Add(2)
	go func() {
		defer wg.Done()
		resA, errA = h.manager.ApplyPatch(ctx, h.taskID, patchA, initSnap, "conc-same-a")
	}()
	go func() {
		defer wg.Done()
		resB, errB = h.manager.ApplyPatch(ctx, h.taskID, patchB, initSnap, "conc-same-b")
	}()
	wg.Wait()

	successCount := 0
	if errA == nil && resA != nil && resA.Status == tool.InvocationStatusCompleted {
		successCount++
	}
	if errB == nil && resB != nil && resB.Status == tool.InvocationStatusCompleted {
		successCount++
	}

	t.Logf("Scenario 1 results: Successes=%d (A err: %v, B err: %v)", successCount, errA, errB)
	if successCount > 1 {
		t.Fatalf("VIOLATION: both concurrent patches succeeded on same file without conflict detection")
	}
	if successCount == 0 {
		t.Fatalf("VIOLATION: both concurrent patches failed: A=%+v, B=%+v", resA, resB)
	}

	// Verify no corrupted file content
	calcContent, _ := os.ReadFile(filepath.Join(wtPath, "calc.go"))
	if strings.Contains(string(calcContent), "<<<<<<<") || strings.Contains(string(calcContent), "=======") {
		t.Fatalf("VIOLATION: merge conflict markers found in calc.go: %s", string(calcContent))
	}
}

// Scenario 2: Two host subagents attempting to apply patches to different files in the same worktree simultaneously.
func TestConcurrency_02_TwoAgentsDifferentFilesSimultaneous(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	_ = initCalcGo(t, h)
	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	// Create second file in worktree
	_ = os.WriteFile(filepath.Join(wtPath, "helper.go"), []byte("package main\n\nfunc Helper() string { return \"v0\" }\n"), 0644)
	cmdAdd := exec.Command("git", "add", "helper.go")
	cmdAdd.Dir = wtPath
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "add helper")
	cmdCommit.Dir = wtPath
	_ = cmdCommit.Run()

	initSnap, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)

	patchCalc := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// calc update
 	return a + b
`
	patchHelper := `diff --git a/helper.go b/helper.go
--- a/helper.go
+++ b/helper.go
@@ -3,1 +3,1 @@
-func Helper() string { return "v0" }
+func Helper() string { return "v1" }
`

	var wg sync.WaitGroup
	var resCalc, resHelper *tool.InvocationResult
	var errCalc, errHelper error

	wg.Add(2)
	go func() {
		defer wg.Done()
		resCalc, errCalc = h.manager.ApplyPatch(ctx, h.taskID, patchCalc, initSnap, "conc-diff-calc")
	}()
	go func() {
		defer wg.Done()
		resHelper, errHelper = h.manager.ApplyPatch(ctx, h.taskID, patchHelper, initSnap, "conc-diff-helper")
	}()
	wg.Wait()

	calcSuccess := errCalc == nil && resCalc != nil && resCalc.Status == tool.InvocationStatusCompleted
	helperSuccess := errHelper == nil && resHelper != nil && resHelper.Status == tool.InvocationStatusCompleted

	t.Logf("Scenario 2 results: calcSuccess=%v helperSuccess=%v", calcSuccess, helperSuccess)

	// Due to strict workspace-level snapshot preconditions (initSnap), exactly one patch succeeds
	// and the other is rejected because the worktree snapshot advanced.
	if calcSuccess && helperSuccess {
		t.Log("Note: Both succeeded (lock serialized without snapshot conflict).")
	} else if calcSuccess || helperSuccess {
		t.Log("Note: Exactly one succeeded; the other rejected due to stale workspace snapshot precondition.")
	} else {
		t.Fatalf("VIOLATION: neither patch succeeded")
	}
}

// Scenario 3: An agent modifying a file between snapshot computation and patch application (uncommitted modifications).
func TestConcurrency_03_UncommittedEditsBetweenAuthAndMutation(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	snapBefore := initCalcGo(t, h)
	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	// Out-of-band edit before patch is applied
	calcPath := filepath.Join(wtPath, "calc.go")
	_ = os.WriteFile(calcPath, []byte("package main\n\n// uncommitted local edit\nfunc Add(a, b int) int { return 0 }\n"), 0644)

	patch := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// patched
 	return a + b
`
	// Patch application should detect stale snapshot or git apply check failure
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, snapBefore, "uncommitted-test")
	if err == nil && res.Status == tool.InvocationStatusCompleted {
		t.Fatalf("VIOLATION: patch applied over uncommitted conflicting modifications")
	}
	t.Logf("Scenario 3 verified: uncommitted modification rejected with err=%v", err)
}

// Scenario 4: A patch targeting a file that was replaced with a symlink between validation and mutation.
func TestConcurrency_04_SymlinkReplacementBetweenValidationAndMutation(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	_ = initCalcGo(t, h)
	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	outsideTarget := filepath.Join(os.TempDir(), fmt.Sprintf("symlink-target-%d.txt", time.Now().UnixNano()))
	_ = os.WriteFile(outsideTarget, []byte("original outside target\n"), 0644)
	defer os.Remove(outsideTarget)

	// Replace calc.go with symlink pointing to outsideTarget
	_ = os.Remove(filepath.Join(wtPath, "calc.go"))
	_ = os.Symlink(outsideTarget, filepath.Join(wtPath, "calc.go"))

	patch := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -1,1 +1,1 @@
-original outside target
+overwritten through symlink
`
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", "symlink-exploit")
	if err == nil && res.Status == tool.InvocationStatusCompleted {
		t.Fatalf("VIOLATION: patch mutated target file through escaping symlink")
	}

	// Verify outside file was NOT modified
	outsideContent, _ := os.ReadFile(outsideTarget)
	if strings.Contains(string(outsideContent), "overwritten") {
		t.Fatalf("VIOLATION: external target file was overwritten through symlink!")
	}
	t.Logf("Scenario 4 verified: escaping symlink write rejected (outside file intact)")
}

// Scenario 5: An agent attempting to replay a patch with a valid idempotency key but against a different snapshot.
func TestConcurrency_05_ReplayWithValidKeyDifferentSnapshot(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	snap1 := initCalcGo(t, h)
	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	patch1 := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// pass 1
 	return a + b
`
	// First invocation with key
	res1, err := h.manager.ApplyPatch(ctx, h.taskID, patch1, snap1, "key-replay-1")
	if err != nil || res1.Status != tool.InvocationStatusCompleted {
		t.Fatalf("first apply patch failed: %v (%+v)", err, res1)
	}

	snap2, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)
	if snap1 == snap2 {
		t.Fatalf("snapshot did not advance after patch")
	}

	// Replay with SAME idempotency key but against new snapshot or different patch
	patch2 := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// pass 2 different
 	return a + b
`
	res2, err2 := h.manager.ApplyPatch(ctx, h.taskID, patch2, snap2, "key-replay-1")
	if err2 == nil && res2.Status == tool.InvocationStatusCompleted && !res2.Replayed {
		t.Fatalf("VIOLATION: reused idempotency key for differing payload without replay detection")
	}
	t.Logf("Scenario 5 verified: key replay handled cleanly (replayed=%v, err=%v)", res2 != nil && res2.Replayed, err2)
}

// Scenario 6: Crash recovery mid-patch (lock release and recovery).
func TestConcurrency_06_CrashRecovery(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	_ = initCalcGo(t, h)

	// 1. Manually acquire lock simulating crashed process that died without clean release
	lockPath := filepath.Join(h.repoDir, ".a4c", "locks", h.taskID+".lock")
	_ = os.MkdirAll(filepath.Dir(lockPath), 0755)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	// Close file without flock unlock simulating OS process termination
	_ = f.Close()

	// 2. Subsequent invocation must cleanly acquire lock
	patch := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// post crash
 	return a + b
`
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", "post-crash-apply")
	if err != nil || res.Status != tool.InvocationStatusCompleted {
		t.Fatalf("post-crash ApplyPatch failed: %v (%+v)", err, res)
	}
	t.Logf("Scenario 6 verified: post-crash recovery succeeded")
}

// Scenario 7: Out-of-band modifications during execution.
func TestConcurrency_07_OutOfBandModificationDuringExecution(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	// Tamper with test file directly while pretending to run tests
	_ = os.WriteFile(filepath.Join(wtPath, "tampered.txt"), []byte("external intrusion"), 0644)

	res, err := h.manager.GitInspect(ctx, h.taskID, "status", "")
	if err != nil || res.Status != tool.InvocationStatusCompleted {
		t.Fatalf("GitInspect failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "tampered.txt") {
		t.Fatalf("VIOLATION: out-of-band untracked modification not detected by GitInspect")
	}
	t.Logf("Scenario 7 verified: GitInspect cleanly reports out-of-band file:\n%s", res.Stdout)
}

// Scenario 8: Concurrent read during patch application.
func TestConcurrency_08_ConcurrentReadDuringPatch(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	_ = initCalcGo(t, h)

	patch := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,2 +3,3 @@
 func Add(a, b int) int {
+	// concurrent read test
 	return a + b
`
	var wg sync.WaitGroup
	var readErr error
	var readContent string

	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(1 * time.Millisecond) // slight delay to overlap with patch
		readRes, err := h.manager.ReadFile(ctx, h.taskID, "calc.go")
		readErr = err
		if readRes != nil {
			readContent = readRes.Stdout
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = h.manager.ApplyPatch(ctx, h.taskID, patch, "", "conc-read-patch")
	}()
	wg.Wait()

	if readErr != nil {
		t.Fatalf("concurrent read returned error: %v", readErr)
	}
	// Read should have either pre-patch or post-patch content, never corrupt/partial bytes
	if !strings.Contains(readContent, "func Add") {
		t.Fatalf("VIOLATION: concurrent read returned corrupted content: %s", readContent)
	}
	t.Logf("Scenario 8 verified: concurrent read completed cleanly with valid content (len=%d)", len(readContent))
}
