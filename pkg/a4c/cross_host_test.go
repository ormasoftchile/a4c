package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestCrossHostContinuity proves that an engineering task initiated in GitHub Copilot
// can be checkpointed, survive host termination, and be resumed and completed in OpenCode
// using A4C's durable storage without repeating earlier work or launching separate inference loops.
func TestCrossHostContinuity(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repository
	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Host Continuity Tester")
	runGitCmd(t, repoDir, "config", "user.email", "tester@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Cross Host Test Repo\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "chore: initial commit")

	// Initialize A4C stores
	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")

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
	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	ctx := context.Background()

	// =========================================================================
	// PHASE 1: GITHUB COPILOT HOST SESSION
	// =========================================================================
	// Simulate Copilot-native coordinator service instance
	copilotSvc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr)

	// Step 1: Alice inspects environment context in Copilot
	copilotCtx, err := copilotSvc.GetTeamContext(ctx)
	if err != nil {
		t.Fatalf("copilot: GetTeamContext failed: %v", err)
	}
	if len(copilotCtx.TeamMembers) < 3 {
		t.Fatalf("copilot: expected 3 team members, got %d", len(copilotCtx.TeamMembers))
	}

	// Step 2: Alice begins engineering task in Copilot
	beginRes, err := copilotSvc.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective:   "Implement payment gateway interface",
		Strategy:    a4c.StrategyTeam,
		Coordinator: "alice",
		Runtime:     "copilot",
	})
	if err != nil {
		t.Fatalf("copilot: BeginTask failed: %v", err)
	}
	taskID := beginRes.TaskID
	worktreePath := beginRes.WorktreePath

	// Step 3: Alice assigns implementation to Bob
	assignBob, err := copilotSvc.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Write PaymentGateway struct and ProcessPayment method",
	})
	if err != nil {
		t.Fatalf("copilot: AssignActivity failed: %v", err)
	}

	// Step 4: Bob implements changes in isolated worktree
	paymentCode := `package main

type PaymentRequest struct {
	Amount   float64
	Currency string
}

type PaymentGateway struct{}

func (g *PaymentGateway) ProcessPayment(req PaymentRequest) bool {
	return req.Amount > 0
}
`
	payFile := filepath.Join(worktreePath, "payment.go")
	if err := os.WriteFile(payFile, []byte(paymentCode), 0644); err != nil {
		t.Fatalf("failed to write payment.go: %v", err)
	}
	runGitCmd(t, worktreePath, "add", "payment.go")
	runGitCmd(t, worktreePath, "commit", "-m", "feat: implement PaymentGateway")

	// Step 5: Bob records durable checkpoint
	chkRes, err := copilotSvc.RecordCheckpoint(ctx, a4c.CheckpointRequest{
		TaskID:         taskID,
		ActivityID:     assignBob.ActivityID,
		Actor:          "bob",
		Summary:        "Implemented PaymentGateway and basic validation",
		Assumptions:    []string{"Currency ISO codes assumed valid", "Only positive amounts accepted"},
		Findings:       []string{"Ready for unit test coverage"},
		FilesInspected: []string{"payment.go"},
		NextSteps:      []string{"Submit for peer review by Charlie"},
	})
	if err != nil {
		t.Fatalf("copilot: RecordCheckpoint failed: %v", err)
	}
	if chkRes.SnapshotSHA == "" {
		t.Fatalf("copilot: expected snapshot SHA in checkpoint")
	}

	// Step 6: Bob completes activity with validation evidence
	_, err = copilotSvc.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:             taskID,
		ActivityID:         assignBob.ActivityID,
		Actor:              "bob",
		Summary:            "PaymentGateway implementation completed and committed",
		FilesModified:      []string{"payment.go"},
		ValidationEvidence: "PASS: all unit tests passing",
		Success:            true,
	})
	if err != nil {
		t.Fatalf("copilot: CompleteActivity failed: %v", err)
	}

	// =========================================================================
	// PHASE 2: HOST TERMINATION & RESUMPTION IN OPENCODE
	// =========================================================================
	// The GitHub Copilot session terminates completely.
	// We drop the copilotSvc instance.
	copilotSvc = nil

	// Now the engineer opens OpenCode and selects A4C.
	// OpenCode mounts the exact same workspace repoDir and persistent .a4c directory.
	openCodeStore, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("opencode: reload store: %v", err)
	}
	openCodeLifecycle, err := a4c.NewDurableLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatalf("opencode: reload lifecycle: %v", err)
	}
	openCodeJournal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("opencode: reload journal: %v", err)
	}
	openCodeMemory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("opencode: reload memory: %v", err)
	}
	openCodeWtMgr, err := a4c.NewWorktreeManager(repoDir, wtDir)
	if err != nil {
		t.Fatalf("opencode: reload worktree: %v", err)
	}
	openCodeRoster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("opencode: reload roster: %v", err)
	}

	openCodeCoord := a4c.NewTeamCoordinator(repoDir, openCodeStore, openCodeRoster, openCodeMemory, gov, openCodeJournal, &a4c.NativeGoTestRunner{}, nil)
	openCodeCoord.SetWorktreeManager(openCodeWtMgr)
	openCodeSvc := a4c.NewTeamService(repoDir, openCodeStore, openCodeLifecycle, openCodeCoord, openCodeRoster, openCodeMemory, openCodeJournal, openCodeWtMgr)

	// Step 7: OpenCode inspects existing team context
	opencodeCtx, err := openCodeSvc.GetTeamContext(ctx)
	if err != nil {
		t.Fatalf("opencode: GetTeamContext failed: %v", err)
	}
	if len(opencodeCtx.ActiveTasks) == 0 {
		t.Fatalf("opencode: expected active task from Copilot session, found none")
	}

	// Step 8: Verify task identity, completed activities, and checkpoints
	taskDetails, err := openCodeSvc.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("opencode: GetTask for %s failed: %v", taskID, err)
	}
	if taskDetails.TaskID != taskID {
		t.Fatalf("opencode: task ID mismatch: expected %s, got %s", taskID, taskDetails.TaskID)
	}
	if taskDetails.Objective != "Implement payment gateway interface" {
		t.Fatalf("opencode: objective mismatch: %s", taskDetails.Objective)
	}
	if len(taskDetails.Activities) < 2 {
		t.Fatalf("opencode: expected at least 2 activities (root + bob), got %d", len(taskDetails.Activities))
	}

	// Verify Bob's completed work was recognized without re-executing
	bobFound := false
	for _, a := range taskDetails.Activities {
		if a.AgentID == "bob" && a.Status == a4c.StatusCompleted {
			bobFound = true
			break
		}
	}
	if !bobFound {
		t.Fatalf("opencode: Bob's completed activity was not preserved across hosts")
	}

	// Step 9: In OpenCode, Alice assigns Charlie to review Bob's code
	assignCharlie, err := openCodeSvc.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskID,
		Agent:     "charlie",
		Role:      "reviewer",
		Objective: "Verify PaymentGateway correctness and ensure non-mutating compliance",
	})
	if err != nil {
		t.Fatalf("opencode: AssignActivity for Charlie failed: %v", err)
	}
	if assignCharlie.Agent != "charlie" {
		t.Fatalf("opencode: expected agent charlie, got %s", assignCharlie.Agent)
	}

	// Step 10: In OpenCode, Charlie records independent review verdict
	revResult, err := openCodeSvc.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:      taskID,
		Reviewer:    "charlie",
		Verdict:     "approved",
		Comments:    "Implementation fulfills specification. Zero mutations made.",
		Findings:    []string{"Valid validation evidence present", "Non-mutating verification confirmed"},
		SnapshotSHA: chkRes.SnapshotSHA,
	})
	if err != nil {
		t.Fatalf("opencode: RecordReview failed: %v", err)
	}
	if revResult.LifecycleState != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("opencode: expected state %s, got %s", a4c.TaskStateAwaitingHumanReview, revResult.LifecycleState)
	}

	// Step 10b: Charlie's delegated activity is completed work and must be
	// closed in the durable graph before human acceptance.
	if _, err := openCodeSvc.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:             taskID,
		ActivityID:         assignCharlie.ActivityID,
		Actor:              "charlie",
		Summary:            "Independent review recorded",
		ValidationEvidence: "review verdict: approved",
		Success:            true,
	}); err != nil {
		t.Fatalf("opencode: CompleteActivity for Charlie failed: %v", err)
	}

	// Step 10c: the coordinating lead finalizes its own root activity;
	// reviewer approval never substitutes for completed work.
	if _, err := openCodeSvc.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:     taskID,
		ActivityID: taskID,
		Actor:      "alice",
		Summary:    "Cross-host coordination complete; implementation and review finished",
		Success:    true,
	}); err != nil {
		t.Fatalf("opencode: CompleteActivity for root failed: %v", err)
	}

	// Step 11: Human accepts task via CLI
	accRes, err := openCodeSvc.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:        taskID,
		Comments:      "Reviewed and approved across both Copilot and OpenCode sessions",
		AcceptedBy:    "human",
		IsInteractive: true,
	})
	if err != nil {
		t.Fatalf("opencode: AcceptTask failed: %v", err)
	}
	if accRes.AcceptedSnapshot == "" {
		t.Fatalf("opencode: expected accepted snapshot SHA")
	}

	// Step 12: Human integrates task into main repository
	intRes, err := openCodeSvc.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID: taskID,
	})
	if err != nil {
		t.Fatalf("opencode: IntegrateTask failed: %v", err)
	}
	if intRes.IntegratedCommit == "" {
		t.Fatalf("opencode: expected integrated commit SHA")
	}

	// Step 13: Verify root checkout now contains payment.go
	finalPaymentPath := filepath.Join(repoDir, "payment.go")
	content, err := os.ReadFile(finalPaymentPath)
	if err != nil {
		t.Fatalf("payment.go was not integrated into base checkout: %v", err)
	}
	if !strings.Contains(string(content), "PaymentGateway") {
		t.Fatalf("payment.go content is corrupted: %s", string(content))
	}

	// Final verification: task lifecycle state is integrated
	finalDetails, err := openCodeSvc.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("final GetTask failed: %v", err)
	}
	if finalDetails.State != a4c.TaskStateIntegrated {
		t.Fatalf("expected final state integrated, got %s", finalDetails.State)
	}
}

