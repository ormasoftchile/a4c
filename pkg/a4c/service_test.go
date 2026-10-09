package a4c_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

type serviceTestHarness struct {
	repoDir        string
	store          *a4c.DurableActivityStore
	lifecycleStore *a4c.DurableLifecycleStore
	coordinator    *a4c.TeamCoordinator
	roster         *a4c.TeamRoster
	memory         *a4c.DurableMemoryStore
	journal        *a4c.DurableIntentJournal
	wtMgr          *a4c.WorktreeManager
	structStore    *a4c.DurableStructuralStore
	service        *a4c.TeamServiceImpl
}

func setupServiceTestHarness(t *testing.T) *serviceTestHarness {
	t.Helper()
	repoDir := t.TempDir()

	// Initialize git repo
	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "A4C Tester")
	runGitCmd(t, repoDir, "config", "user.email", "tester@example.com")

	readme := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "initial commit")

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")
	structDir := filepath.Join(dotDir, "structural")

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("failed to create activity store: %v", err)
	}
	lifecycleStore, err := a4c.NewDurableLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatalf("failed to create lifecycle store: %v", err)
	}
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("failed to create roster: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("failed to create journal: %v", err)
	}
	wtMgr, err := a4c.NewWorktreeManager(repoDir, wtDir)
	if err != nil {
		t.Fatalf("failed to create worktree manager: %v", err)
	}
	structStore, err := a4c.NewDurableStructuralStore(structDir)
	if err != nil {
		t.Fatalf("failed to create structural store: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr, structStore)

	return &serviceTestHarness{
		repoDir:        repoDir,
		store:          store,
		lifecycleStore: lifecycleStore,
		coordinator:    coord,
		roster:         roster,
		memory:         memory,
		journal:        journal,
		wtMgr:          wtMgr,
		structStore:    structStore,
		service:        svc,
	}
}

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
	}
	return string(out)
}

func TestTeamService_SubmitListAndInspect(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	// 1. Submit task
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective:    "Implement feature X",
		BudgetTokens: 25000,
		Strategy:     a4c.StrategyTeam,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}
	if ref.TaskID == "" {
		t.Fatalf("expected non-empty task ID")
	}
	if ref.State != a4c.TaskStateQueued {
		t.Fatalf("expected state queued, got %s", ref.State)
	}

	// 2. List tasks
	list, err := h.service.ListTasks(ctx, a4c.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 task in list, got %d", len(list))
	}
	if list[0].TaskID != ref.TaskID {
		t.Fatalf("expected task ID %s, got %s", ref.TaskID, list[0].TaskID)
	}
	if list[0].State != a4c.TaskStateQueued {
		t.Fatalf("expected state queued, got %s", list[0].State)
	}

	// 3. Inspect task
	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.TaskID != ref.TaskID {
		t.Fatalf("expected task ID %s, got %s", ref.TaskID, details.TaskID)
	}
	if details.Objective != "Implement feature X" {
		t.Fatalf("unexpected objective: %s", details.Objective)
	}
	if details.State != a4c.TaskStateQueued {
		t.Fatalf("expected state queued, got %s", details.State)
	}
}

func TestTeamService_AcceptTask_DoesNotModifyWorkingCheckout(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Refactor core loop",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Cannot accept a task that is not awaiting review
	_, err = h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{TaskID: ref.TaskID})
	if !errors.Is(err, a4c.ErrTaskNotReadyForReview) {
		t.Fatalf("expected ErrTaskNotReadyForReview, got %v", err)
	}

	// Complete the root activity so task transitions to AwaitingHumanReview
	rec, err := h.store.Get(ref.TaskID)
	if err != nil {
		t.Fatalf("store.Get failed: %v", err)
	}
	_ = h.store.Complete(rec.Request.ID, a4c.ActivityResult{
		ID: rec.Request.ID,
		Status:     a4c.StatusCompleted,
		Outputs:    map[string]any{"result": "success"},
	})

	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected state awaiting_human_review, got %s", details.State)
	}

	// Record initial working checkout state
	initialContent, _ := os.ReadFile(filepath.Join(h.repoDir, "README.md"))

	// Explicitly accept task
	accRes, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:     ref.TaskID,
		Comments:   "LGTM!",
		AcceptedBy: "human_cli",
	})
	if err != nil {
		t.Fatalf("AcceptTask failed: %v", err)
	}
	if accRes.AcceptedSnapshot == "" {
		t.Fatalf("expected recorded snapshot in accept result")
	}

	// CRITICAL CHECK: Verify working checkout was NOT altered by acceptance!
	afterContent, _ := os.ReadFile(filepath.Join(h.repoDir, "README.md"))
	if string(afterContent) != string(initialContent) {
		t.Fatalf("AcceptTask violated contract: modified working checkout!")
	}

	// Verify task state is now Accepted
	details, err = h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.State != a4c.TaskStateAccepted {
		t.Fatalf("expected state accepted, got %s", details.State)
	}

	// Idempotency check: Cannot accept already accepted task
	_, err = h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{TaskID: ref.TaskID, AcceptedBy: "human_cli"})
	if !errors.Is(err, a4c.ErrTaskAlreadyAccepted) {
		t.Fatalf("expected ErrTaskAlreadyAccepted, got %v", err)
	}
}

func TestTeamService_IntegrateTask_SuccessAndGuards(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	// 1. Submit and setup worktree with a branch modification
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Add helper utility",
		Strategy:  a4c.StrategyReviewed,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Acquire worktree for task
	wtRec, err := h.wtMgr.GetWorktree(ref.TaskID)
	if err != nil {
		t.Fatalf("AcquireWorktree failed: %v", err)
	}

	// Commit a change in the worktree branch
	helperFile := filepath.Join(wtRec.WorktreePath, "helper.go")
	if err := os.WriteFile(helperFile, []byte("package main\n\nfunc Helper() {}\n"), 0644); err != nil {
		t.Fatalf("failed to write helper file: %v", err)
	}
	runGitCmd(t, wtRec.WorktreePath, "add", "helper.go")
	runGitCmd(t, wtRec.WorktreePath, "commit", "-m", "feat: add helper")

	// Complete coder (ref.TaskID) and reviewer (ref.TaskID + "-rev") activities
	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{
		ID:     ref.TaskID,
		Status: a4c.StatusCompleted,
	})

	reviewerID := fmt.Sprintf("%s-rev", ref.TaskID)
	_ = h.store.Complete(reviewerID, a4c.ActivityResult{
		ID:      reviewerID,
		Status:  a4c.StatusCompleted,
		Outputs: map[string]any{"approved": true, "review_comments": "Looks great"},
	})

	// Attempt integration before acceptance -> Must fail with ErrTaskNotAccepted
	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if !errors.Is(err, a4c.ErrTaskNotAccepted) {
		t.Fatalf("expected ErrTaskNotAccepted, got %v", err)
	}

	// Accept task
	accRes, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{TaskID: ref.TaskID, Comments: "Approved", AcceptedBy: "human_cli"})
	if err != nil {
		t.Fatalf("AcceptTask failed: %v", err)
	}
	if accRes.AcceptedSnapshot == "" {
		t.Fatalf("expected non-empty AcceptedSnapshot")
	}

	// 2. Test Dirty Working Tree Guard
	dirtyFile := filepath.Join(h.repoDir, "dirty.txt")
	_ = os.WriteFile(dirtyFile, []byte("uncommitted change"), 0644)

	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if !errors.Is(err, a4c.ErrWorkingTreeDirty) {
		t.Fatalf("expected ErrWorkingTreeDirty when base checkout has uncommitted changes, got %v", err)
	}

	// Clean dirty file
	_ = os.Remove(dirtyFile)

	// 3. Test Snapshot Mismatch Guard
	// Simulate worktree commit after acceptance
	anotherFile := filepath.Join(wtRec.WorktreePath, "another.go")
	_ = os.WriteFile(anotherFile, []byte("package main\n"), 0644)
	runGitCmd(t, wtRec.WorktreePath, "add", "another.go")
	runGitCmd(t, wtRec.WorktreePath, "commit", "-m", "post-acceptance unapproved commit")

	// Attempt integration with modified snapshot -> Must fail with ErrSnapshotMismatch
	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if !errors.Is(err, a4c.ErrSnapshotMismatch) {
		t.Fatalf("expected ErrSnapshotMismatch when snapshot changed after acceptance, got %v", err)
	}

	// Reset worktree back to accepted commit
	runGitCmd(t, wtRec.WorktreePath, "reset", "--hard", "HEAD~1")

	// 4. Successful Integration
	intRes, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if err != nil {
		t.Fatalf("IntegrateTask failed: %v", err)
	}
	if intRes.IntegratedCommit == "" {
		t.Fatalf("expected integrated commit SHA")
	}

	// Verify helper.go is now present in main repository checkout
	integratedHelper := filepath.Join(h.repoDir, "helper.go")
	if _, err := os.Stat(integratedHelper); err != nil {
		t.Fatalf("integrated file helper.go not found in base checkout: %v", err)
	}

	// Verify task state is now integrated
	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.State != a4c.TaskStateIntegrated {
		t.Fatalf("expected state integrated, got %s", details.State)
	}

	// Cannot integrate again
	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if !errors.Is(err, a4c.ErrTaskAlreadyIntegrated) {
		t.Fatalf("expected ErrTaskAlreadyIntegrated, got %v", err)
	}
}

func TestTeamService_FeedbackAndRevision(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Initial feature",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Mark completed to simulate awaiting review
	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{
		ID: ref.TaskID,
		Status:     a4c.StatusCompleted,
	})

	// Submit human feedback requesting revisions
	feedbackRef, err := h.service.SubmitFeedback(ctx, a4c.FeedbackRequest{
		TaskID:      ref.TaskID,
		Feedback:    "Please add unit tests and error handling",
		TargetAgent: "coder",
	})
	if err != nil {
		t.Fatalf("SubmitFeedback failed: %v", err)
	}
	if feedbackRef.State != a4c.TaskStateRevisionRequested {
		t.Fatalf("expected state revision_requested, got %s", feedbackRef.State)
	}

	// Inspect details: verify revisions recorded
	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.LifecycleFacts == nil || len(details.LifecycleFacts.Revisions) != 1 {
		t.Fatalf("expected 1 revision in lifecycle facts, got %+v", details.LifecycleFacts)
	}
	if details.LifecycleFacts.Revisions[0].Feedback != "Please add unit tests and error handling" {
		t.Fatalf("unexpected revision feedback: %s", details.LifecycleFacts.Revisions[0].Feedback)
	}

	// Verify revision activities were enqueued in store
	coderRevID := details.LifecycleFacts.Revisions[0].RevisionActivityID
	coderRec, err := h.store.Get(coderRevID)
	if err != nil {
		t.Fatalf("expected enqueued coder revision activity %s: %v", coderRevID, err)
	}
	if coderRec.Status != a4c.StatusPending {
		t.Fatalf("expected status pending for coder revision, got %s", coderRec.Status)
	}

	reviewerRevID := fmt.Sprintf("%s-review", coderRevID)
	reviewerRec, err := h.store.Get(reviewerRevID)
	if err != nil {
		t.Fatalf("expected enqueued reviewer activity %s: %v", reviewerRevID, err)
	}
	if len(reviewerRec.Request.DependsOn) != 1 || reviewerRec.Request.DependsOn[0] != coderRevID {
		t.Fatalf("expected reviewer revision activity to depend on coder revision")
	}
}

func TestTeamService_RejectTask(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Deprecated approach",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Complete to make reviewable
	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{
		ID: ref.TaskID,
		Status:     a4c.StatusCompleted,
	})

	rejRes, err := h.service.RejectTask(ctx, a4c.RejectTaskRequest{
		TaskID: ref.TaskID,
		Reason: "Requirements changed, this approach is obsolete",
	})
	if err != nil {
		t.Fatalf("RejectTask failed: %v", err)
	}
	if rejRes.Reason != "Requirements changed, this approach is obsolete" {
		t.Fatalf("unexpected rejection reason: %s", rejRes.Reason)
	}

	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.State != a4c.TaskStateRejected {
		t.Fatalf("expected state rejected, got %s", details.State)
	}
	if details.LifecycleFacts == nil || !details.LifecycleFacts.Rejected {
		t.Fatalf("expected rejected flag set in lifecycle facts")
	}
}

func TestTeamService_GetTaskDiff(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Test diff calculation",
		Strategy:  a4c.StrategyReviewed,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	wtRec, err := h.wtMgr.GetWorktree(ref.TaskID)
	if err != nil {
		t.Fatalf("GetWorktree failed: %v", err)
	}

	// Create and commit a new file in the worktree
	newFile := filepath.Join(wtRec.WorktreePath, "feature.go")
	if err := os.WriteFile(newFile, []byte("package main\n\nfunc NewFeature() string {\n\treturn \"done\"\n}\n"), 0644); err != nil {
		t.Fatalf("failed to write feature file: %v", err)
	}
	runGitCmd(t, wtRec.WorktreePath, "add", "feature.go")
	runGitCmd(t, wtRec.WorktreePath, "commit", "-m", "feat: implement feature")

	diffRes, err := h.service.GetTaskDiff(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTaskDiff failed: %v", err)
	}

	if diffRes.TaskID != ref.TaskID {
		t.Fatalf("expected task ID %s, got %s", ref.TaskID, diffRes.TaskID)
	}
	if diffRes.TotalFiles != 1 {
		t.Fatalf("expected 1 modified file, got %d", diffRes.TotalFiles)
	}
	if diffRes.Files[0].Path != "feature.go" {
		t.Fatalf("expected path feature.go, got %s", diffRes.Files[0].Path)
	}
	if diffRes.TotalAdds <= 0 {
		t.Fatalf("expected positive additions, got %d", diffRes.TotalAdds)
	}
}

