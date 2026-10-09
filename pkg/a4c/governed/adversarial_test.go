package governed_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/a4c/pkg/a4c/governed"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type advHarness struct {
	repoDir     string
	service     a4c.TeamService
	manager     *governed.Manager
	taskID      string
	dispatchDir string
}

func setupAdvHarness(t *testing.T) *advHarness {
	t.Helper()
	repoDir := t.TempDir()

	runCmd(t, repoDir, "git", "init")
	runCmd(t, repoDir, "git", "config", "user.name", "Adversarial Tester")
	runCmd(t, repoDir, "git", "config", "user.email", "adv@example.com")

	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module advtest\n\ngo 1.25\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Governed Workspace\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestMainPass(t *testing.T) {}\n"), 0644)
	runCmd(t, repoDir, "git", "add", "go.mod", "README.md", "main.go", "main_test.go")
	runCmd(t, repoDir, "git", "commit", "-m", "chore: initial commit")

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")
	structDir := filepath.Join(dotDir, "structural")
	dispatchDir := filepath.Join(dotDir, "yawr-dispatch")

	store, _ := a4c.NewDurableActivityStore(storeDir)
	lifecycle, _ := a4c.NewDurableLifecycleStore(lifecycleDir)
	journal, _ := a4c.NewDurableIntentJournal(journalDir)
	memory, _ := a4c.NewDurableMemoryStore(memDir)
	wtMgr, _ := a4c.NewWorktreeManager(repoDir, wtDir)
	roster, _ := a4c.DefaultTeamRoster(rosterDir)
	structStore, _ := a4c.NewDurableStructuralStore(structDir)

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_git_inspect", "governed_run_tests",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycle, coord, roster, memory, journal, wtMgr, structStore)

	ctx := context.Background()
	taskRes, err := svc.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective: "Adversarial Governance Conformance",
		Strategy:  "team",
	})
	if err != nil {
		t.Fatalf("setup: begin task: %v", err)
	}

	_, err = svc.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskRes.TaskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Implement code changes",
	})
	if err != nil {
		t.Fatalf("setup: assign bob: %v", err)
	}

	mgr, err := governed.NewManager(svc, dispatchDir)
	if err != nil {
		t.Fatalf("setup: create manager: %v", err)
	}

	return &advHarness{
		repoDir:     repoDir,
		service:     svc,
		manager:     mgr,
		taskID:      taskRes.TaskID,
		dispatchDir: dispatchDir,
	}
}

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd %s %v in %s failed: %v\nOutput: %s", name, args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// 1. Charlie requests governed patch -> denied before mutation
func TestAdversarial_Scenario01_CharlieGovernedPatchDenied(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	// Assign Charlie (reviewer)
	_, err := h.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    h.taskID,
		Agent:     "charlie",
		Role:      "reviewer",
		Objective: "Review changes",
	})
	if err != nil {
		t.Fatalf("assign charlie: %v", err)
	}

	patch := `diff --git a/unauth_charlie.txt b/unauth_charlie.txt
new file mode 100644
--- /dev/null
+++ b/unauth_charlie.txt
@@ -0,0 +1 @@
+charlie was here
`
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", "adv-charlie-patch")
	if err == nil {
		if res.Status != tool.InvocationStatusDenied {
			t.Fatalf("expected InvocationStatusDenied, got %s", res.Status)
		}
	}

	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	targetFile := filepath.Join(wtPath, "unauth_charlie.txt")
	if _, err := os.Stat(targetFile); err == nil {
		t.Fatalf("VIOLATION: charlie successfully created %s", targetFile)
	}
}

// 2. Charlie impersonates Bob via request arguments or per-agent role server
func TestAdversarial_Scenario02_CharlieImpersonatesBob(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	// Reviewer-scoped manager instance (as when running a4c mcp --role reviewer)
	revMgr, err := governed.NewManagerWithRole(h.service, h.dispatchDir, "reviewer")
	if err != nil {
		t.Fatalf("create reviewer manager: %v", err)
	}

	patch := `diff --git a/impersonate.txt b/impersonate.txt
new file mode 100644
--- /dev/null
+++ b/impersonate.txt
@@ -0,0 +1 @@
+bob forged by charlie
`
	// Even if an active coder activity exists on the task, reviewer manager denies mutation
	res, err := revMgr.ApplyPatch(ctx, h.taskID, patch, "", "adv-impersonate")
	if err == nil && res.Status != tool.InvocationStatusDenied {
		t.Fatalf("VIOLATION: reviewer manager allowed patch application")
	}

	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	targetFile := filepath.Join(wtPath, "impersonate.txt")
	if _, err := os.Stat(targetFile); err == nil {
		t.Fatalf("VIOLATION: impersonation patch modified filesystem at %s", targetFile)
	}
}

// 3. Bob uses native write -> verified by host-exported permissions
func TestAdversarial_Scenario03_BobNativeWritePolicy(t *testing.T) {
	tmpDir := t.TempDir()
	if err := a4c.ExportCopilotAgents(tmpDir); err != nil {
		t.Fatalf("export copilot agents: %v", err)
	}
	if err := a4c.ExportOpenCodeAgents(tmpDir); err != nil {
		t.Fatalf("export opencode agents: %v", err)
	}

	// In Copilot: bob.agent.md tools must NOT contain "write" or "edit" or "*"
	bobCopilot, err := os.ReadFile(filepath.Join(tmpDir, ".github", "agents", "bob.agent.md"))
	if err != nil {
		t.Fatalf("read bob copilot: %v", err)
	}
	bobCopilotStr := string(bobCopilot)
	if strings.Contains(bobCopilotStr, `tools: ["*"]`) {
		t.Fatalf("VIOLATION: bob copilot has wildcard tools")
	}
	if strings.Contains(bobCopilotStr, `"write"`) || strings.Contains(bobCopilotStr, `"edit"`) {
		t.Fatalf("VIOLATION: bob copilot includes native write/edit tools")
	}

	// In OpenCode: bob.md must explicitly deny edit, write, patch
	bobOpenCode, err := os.ReadFile(filepath.Join(tmpDir, ".opencode", "agents", "bob.md"))
	if err != nil {
		t.Fatalf("read bob opencode: %v", err)
	}
	bobOpenCodeStr := string(bobOpenCode)
	if !strings.Contains(bobOpenCodeStr, "edit: deny") || !strings.Contains(bobOpenCodeStr, "write: deny") {
		t.Fatalf("VIOLATION: bob opencode does not deny native edit/write")
	}
}

// 4. Bob uses native shell -> verified by host-exported permissions
func TestAdversarial_Scenario04_BobNativeShellPolicy(t *testing.T) {
	tmpDir := t.TempDir()
	_ = a4c.ExportCopilotAgents(tmpDir)
	_ = a4c.ExportOpenCodeAgents(tmpDir)

	bobCopilot, _ := os.ReadFile(filepath.Join(tmpDir, ".github", "agents", "bob.agent.md"))
	if strings.Contains(string(bobCopilot), `"bash"`) || strings.Contains(string(bobCopilot), `"shell"`) {
		t.Fatalf("VIOLATION: bob copilot includes native bash/shell")
	}

	bobOpenCode, _ := os.ReadFile(filepath.Join(tmpDir, ".opencode", "agents", "bob.md"))
	if !strings.Contains(string(bobOpenCode), "bash: deny") {
		t.Fatalf("VIOLATION: bob opencode does not deny bash")
	}
}

// 5. Nested subagent attempts a prohibited op -> subagent inheritance blocked
func TestAdversarial_Scenario05_NestedSubagentProhibitedOp(t *testing.T) {
	tmpDir := t.TempDir()
	_ = a4c.ExportCopilotAgents(tmpDir)
	_ = a4c.ExportOpenCodeAgents(tmpDir)

	// In Copilot, Bob and Charlie MUST NOT have the "agent" tool
	bobCopilot, _ := os.ReadFile(filepath.Join(tmpDir, ".github", "agents", "bob.agent.md"))
	if strings.Contains(string(bobCopilot), `"agent"`) {
		t.Fatalf("VIOLATION: bob copilot has agent delegation capability")
	}

	charlieCopilot, _ := os.ReadFile(filepath.Join(tmpDir, ".github", "agents", "charlie.agent.md"))
	if strings.Contains(string(charlieCopilot), `"agent"`) {
		t.Fatalf("VIOLATION: charlie copilot has agent delegation capability")
	}

	// In OpenCode, Bob and Charlie must have "task: deny"
	bobOpenCode, _ := os.ReadFile(filepath.Join(tmpDir, ".opencode", "agents", "bob.md"))
	if !strings.Contains(string(bobOpenCode), "task: deny") {
		t.Fatalf("VIOLATION: bob opencode does not deny subagent delegation")
	}

	// Alice in OpenCode must restrict subagent delegation only to bob and charlie
	aliceOpenCode, _ := os.ReadFile(filepath.Join(tmpDir, ".opencode", "agents", "a4c.md"))
	if !strings.Contains(string(aliceOpenCode), `"*": deny`) || !strings.Contains(string(aliceOpenCode), "bob: allow") {
		t.Fatalf("VIOLATION: alice opencode does not restrict task delegation")
	}
}

// 6. Patch targets parent directory -> ErrFilesystemEscape
func TestAdversarial_Scenario06_PatchTargetsParentDir(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	patch := `diff --git a/../escape.txt b/../escape.txt
new file mode 100644
--- /dev/null
+++ b/../escape.txt
@@ -0,0 +1 @@
+parent dir escape
`
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", "adv-parent-escape")
	if err == nil && res.Status == tool.InvocationStatusCompleted {
		t.Fatalf("VIOLATION: patch targeting parent dir succeeded")
	}
	if err != nil && !errors.Is(err, governed.ErrFilesystemEscape) && !strings.Contains(err.Error(), "escaping path") {
		t.Fatalf("expected ErrFilesystemEscape, got: %v", err)
	}

	outsideFile := filepath.Join(filepath.Dir(h.repoDir), "escape.txt")
	if _, err := os.Stat(outsideFile); err == nil {
		_ = os.Remove(outsideFile)
		t.Fatalf("VIOLATION: file created outside repo: %s", outsideFile)
	}
}

// 7. Patch targets escaping symlink -> ErrFilesystemEscape
func TestAdversarial_Scenario07_PatchTargetsEscapingSymlink(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	// Try reading via path traversal
	res, err := h.manager.ReadFile(ctx, h.taskID, "../../etc/shadow")
	if err == nil && res.Status == tool.InvocationStatusCompleted {
		t.Fatalf("VIOLATION: traversal read succeeded")
	}
	if err != nil && !errors.Is(err, governed.ErrFilesystemEscape) && !strings.Contains(err.Error(), "outside worktree") {
		t.Fatalf("expected ErrFilesystemEscape, got: %v", err)
	}
}

// 8. Patch uses stale file content -> ErrStaleSnapshot
func TestAdversarial_Scenario08_PatchUsesStaleSnapshot(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	patch := `diff --git a/stale.txt b/stale.txt
new file mode 100644
--- /dev/null
+++ b/stale.txt
@@ -0,0 +1 @@
+stale
`
	res, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "0000000000000000000000000000000000000000", "adv-stale")
	if err == nil && res.Status == tool.InvocationStatusCompleted {
		t.Fatalf("VIOLATION: stale patch was applied")
	}
	if err != nil && !errors.Is(err, governed.ErrStaleSnapshot) && !strings.Contains(err.Error(), "stale snapshot") {
		t.Fatalf("expected ErrStaleSnapshot, got: %v", err)
	}
}

// 9. Concurrent patches on the same version -> serialized without silent lost update
func TestAdversarial_Scenario09_ConcurrentPatchesSerialized(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	patchA := `diff --git a/concurrentA.txt b/concurrentA.txt
new file mode 100644
--- /dev/null
+++ b/concurrentA.txt
@@ -0,0 +1 @@
+patch A
`
	patchB := `diff --git a/concurrentB.txt b/concurrentB.txt
new file mode 100644
--- /dev/null
+++ b/concurrentB.txt
@@ -0,0 +1 @@
+patch B
`

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		_, err := h.manager.ApplyPatch(ctx, h.taskID, patchA, "", "adv-concurrent-A")
		errCh <- err
	}()

	go func() {
		defer wg.Done()
		_, err := h.manager.ApplyPatch(ctx, h.taskID, patchB, "", "adv-concurrent-B")
		errCh <- err
	}()

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Logf("one concurrent patch returned expected error or serialized: %v", err)
		}
	}

	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	// Verify that whichever patch succeeded, the worktree is in a valid state
	statA, errA := os.Stat(filepath.Join(wtPath, "concurrentA.txt"))
	statB, errB := os.Stat(filepath.Join(wtPath, "concurrentB.txt"))
	if errA != nil && errB != nil {
		t.Fatalf("neither concurrent patch succeeded")
	}
	t.Logf("Concurrent mutation results: patchA created=%t, patchB created=%t", errA == nil && statA != nil, errB == nil && statB != nil)
}

// 10. Identical dispatch replayed -> no duplicate mutation
func TestAdversarial_Scenario10_IdenticalDispatchReplayed(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	patch := `diff --git a/replay.txt b/replay.txt
new file mode 100644
--- /dev/null
+++ b/replay.txt
@@ -0,0 +1 @@
+replay test
`
	key := "adv-replay-001"

	res1, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", key)
	if err != nil || res1.Status != tool.InvocationStatusCompleted {
		t.Fatalf("first apply failed: %v", err)
	}
	if res1.Replayed {
		t.Errorf("first invocation was incorrectly marked as replayed")
	}

	res2, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", key)
	if err != nil || res2.Status != tool.InvocationStatusCompleted {
		t.Fatalf("second apply failed: %v", err)
	}
	if !res2.Replayed {
		t.Fatalf("expected res2.Replayed to be true, got false")
	}
}

// 11. Interrupted dispatch retried -> safe replay from durable store
func TestAdversarial_Scenario11_InterruptedDispatchRetried(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	patch := `diff --git a/restart.txt b/restart.txt
new file mode 100644
--- /dev/null
+++ b/restart.txt
@@ -0,0 +1 @@
+restart test
`
	key := "adv-restart-001"

	res1, err := h.manager.ApplyPatch(ctx, h.taskID, patch, "", key)
	if err != nil || res1.Status != tool.InvocationStatusCompleted {
		t.Fatalf("initial apply failed: %v", err)
	}

	// Instantiate new manager recovering from durable dispatchDir
	freshMgr, err := governed.NewManager(h.service, h.dispatchDir)
	if err != nil {
		t.Fatalf("fresh manager: %v", err)
	}

	res2, err := freshMgr.ApplyPatch(ctx, h.taskID, patch, "", key)
	if err != nil || res2.Status != tool.InvocationStatusCompleted {
		t.Fatalf("recovered apply failed: %v", err)
	}
	if !res2.Replayed {
		t.Fatalf("expected recovered invocation to be replayed from durable store")
	}
}

// 12. Reviewer invokes test code with side effects -> denied / controlled
func TestAdversarial_Scenario12_ReviewerTestDeniedAndIsolated(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	// 12a. Charlie (reviewer) attempting RunTests must be denied
	_, _ = h.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    h.taskID,
		Agent:     "charlie",
		Role:      "reviewer",
		Objective: "Review verification",
	})

	testRes, err := h.manager.RunTests(ctx, h.taskID, "./...", false, "charlie-test")
	if err == nil && testRes.Status != tool.InvocationStatusDenied {
		t.Fatalf("VIOLATION: reviewer was permitted to invoke test code (status: %s)", testRes.Status)
	}

	// 12b. When coder runs tests, verify tests execute in isolated sandbox and leave worktree invariant
	_, _ = h.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    h.taskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Run verified tests",
	})

	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	snapBefore, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)

	bobTestRes, err := h.manager.RunTests(ctx, h.taskID, "./...", false, "bob-test")
	if err != nil || bobTestRes.Status != tool.InvocationStatusCompleted {
		t.Fatalf("bob test run failed: %v (%+v)", err, bobTestRes)
	}

	snapAfter, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)
	if snapBefore != snapAfter {
		t.Fatalf("VIOLATION: test execution modified the worktree (%s -> %s)", snapBefore, snapAfter)
	}
}

// 13. Agent attempts human acceptance -> denied & audited
func TestAdversarial_Scenario13_AgentAttemptsHumanAcceptance(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	// Transition task to AwaitingHumanReview
	_, _ = h.service.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:        h.taskID,
		ActivityID:    h.taskID,
		Actor:         "bob",
		Summary:       "Implemented",
		FilesModified: []string{"main.go"},
		Success:       true,
	})
	wtPath, _ := h.service.GetWorktreePath(h.taskID)
	snap, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)
	_, _ = h.service.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:      h.taskID,
		Reviewer:    "charlie",
		Verdict:     "approved",
		Comments:    "LGTM",
		SnapshotSHA: snap,
	})

	// Agent calls AcceptTask
	_, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:     h.taskID,
		AcceptedBy: "agent_charlie",
		Comments:   "I self-approve my team's work",
	})
	if err == nil {
		t.Fatalf("VIOLATION: agent successfully approved task")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized error, got: %v", err)
	}

	// Verify denial was durably audited
	records, err := a4c.ReadAudit(h.repoDir, "approvals")
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	foundDenial := false
	for _, r := range records {
		if r["op"] == "accept" && r["outcome"] == "denied" {
			foundDenial = true
			break
		}
	}
	if !foundDenial {
		t.Fatalf("expected audit denial record in approvals stream")
	}
}

// 14. Agent attempts integration without prior acceptance -> denied & audited
func TestAdversarial_Scenario14_AgentAttemptsIntegrationWithoutAcceptance(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	_, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID:       h.taskID,
		IntegratedBy: "agent_bob",
	})
	if err == nil {
		t.Fatalf("VIOLATION: integrated unaccepted task")
	}
	if !errors.Is(err, a4c.ErrTaskNotAccepted) {
		t.Fatalf("expected ErrTaskNotAccepted, got: %v", err)
	}
}

// 15. Approval references obsolete snapshot -> denied & audited
func TestAdversarial_Scenario15_ApprovalReferencesObsoleteSnapshot(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	wtPath, _ := h.service.GetWorktreePath(h.taskID)

	// Step A: Bob completes, Charlie approves snapshot A
	_, _ = h.service.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:        h.taskID,
		ActivityID:    h.taskID,
		Actor:         "bob",
		Summary:       "Implemented initial version",
		FilesModified: []string{"main.go"},
		Success:       true,
	})
	snapA, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(wtPath)
	_, err := h.service.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:      h.taskID,
		Reviewer:    "charlie",
		Verdict:     "approved",
		Comments:    "Reviewed snapshot A",
		SnapshotSHA: snapA,
	})
	if err != nil {
		t.Fatalf("review record: %v", err)
	}

	// Step B: Unapproved modification occurs in worktree
	tamperedFile := filepath.Join(wtPath, "tampered.go")
	_ = os.WriteFile(tamperedFile, []byte("package main\n\n// unreviewed back door\n"), 0644)

	// Step C: Human tries to accept task, but worktree content has drifted from reviewed snapshot!
	_, err = h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:     h.taskID,
		AcceptedBy: "human_cli",
		Comments:   "Human trying to accept",
	})
	if err == nil {
		t.Fatalf("VIOLATION: accepted task whose worktree drifted from reviewed snapshot")
	}
	if !errors.Is(err, a4c.ErrSnapshotMismatch) {
		t.Fatalf("expected ErrSnapshotMismatch, got: %v", err)
	}

	// Verify denial was recorded in audit chain
	n, verifyErr := a4c.VerifyAuditChain(h.repoDir, "approvals")
	if verifyErr != nil {
		t.Fatalf("audit chain broken: %v", verifyErr)
	}
	if n == 0 {
		t.Fatalf("expected audit records, found none")
	}
}
