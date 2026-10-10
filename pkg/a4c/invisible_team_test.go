package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func setupInvisibleTeamHarness(t *testing.T) *serviceTestHarness {
	h := setupServiceTestHarness(t)
	// Initialize Go module and dummy test so NativeGoTestRunner succeeds
	_ = os.WriteFile(filepath.Join(h.repoDir, "go.mod"), []byte("module testrepo\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(h.repoDir, "dummy_test.go"), []byte("package main\nimport \"testing\"\nfunc TestPass(t *testing.T) {}\n"), 0644)
	runGitCmd(t, h.repoDir, "add", "go.mod", "dummy_test.go")
	runGitCmd(t, h.repoDir, "commit", "-m", "add go module")

	// Register any extra specialists in roster
	_ = h.roster.RegisterMember(a4c.AgentIdentity{
		ID:           "storage-engineer",
		Name:         "Storage Engineer",
		Role:         "specialist",
		Charter:      "Durable storage specialist",
		AllowedTools: []string{"governed_read_file"},
	})
	_ = h.roster.RegisterMember(a4c.AgentIdentity{
		ID:           "storage-specialist",
		Name:         "Storage Specialist",
		Role:         "specialist",
		Charter:      "Storage specialist",
		AllowedTools: []string{"governed_read_file"},
	})

	return h
}

// TestDirectModeExecution verifies that ordinary tasks run with exactly ONE specialist session,
// zero coordinators, and zero automatic reviewers.
func TestDirectModeExecution_SingleSpecialistZeroCoordinatorsZeroReviewers(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	var sessionsCreated []string
	fake := a4c.NewFakeProvider()
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		sessionsCreated = append(sessionsCreated, session.Config().AgentID)
		// Specialist implements code change in worktree
		wtPath := session.Config().WorkspaceDir
		if wtPath == "" {
			wtPath = h.repoDir
		}
		_ = os.WriteFile(filepath.Join(wtPath, "feature.go"), []byte("package main\nfunc Feature() string { return \"done\" }\n"), 0644)
		return &a4c.AgentMessage{Content: "Implemented Feature()"}, nil
	}
	h.coordinator.SetDefaultProvider(fake)

	// 1. Submit task with default strategy (Direct)
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Implement Feature in feature.go",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Verify only 1 activity in task tree initially
	tree := h.store.GetTaskTree(ref.TaskID)
	if len(tree) != 1 {
		t.Fatalf("expected exactly 1 activity in direct mode, got %d", len(tree))
	}
	if tree[0].Request.AgentID != "coder" {
		t.Fatalf("expected specialist agent 'coder', got %q", tree[0].Request.AgentID)
	}

	// 2. Execute task
	results, err := h.service.ExecuteTask(ctx, ref.TaskID, 10)
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 activity result, got %d", len(results))
	}

	// Verify session count: exactly 1 specialist session
	if len(sessionsCreated) != 1 {
		t.Fatalf("expected exactly 1 session created, got %d: %v", len(sessionsCreated), sessionsCreated)
	}
	if sessionsCreated[0] != "coder" {
		t.Fatalf("expected session agent to be 'coder', got %q", sessionsCreated[0])
	}

	// 3. Verify task is immediately ready for human review
	taskDetails, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskDetails.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected task state %s, got %s", a4c.TaskStateAwaitingHumanReview, taskDetails.State)
	}
}

// TestReviewedModeExecution verifies that reviewed tasks run with exactly ONE implementer
// session and ONE reviewer session, with deterministic review handoff and ZERO coordinator sessions.
func TestReviewedModeExecution_ImplementerAndReviewerZeroCoordinators(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	var sessionsCreated []string
	var promptReceivedByReviewer string

	fake := a4c.NewFakeProvider()
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		sessionsCreated = append(sessionsCreated, agentID)

		if agentID == "coder" {
			wtPath := session.Config().WorkspaceDir
			if wtPath == "" {
				wtPath = h.repoDir
			}
			_ = os.WriteFile(filepath.Join(wtPath, "lib.go"), []byte("package main\nfunc Lib() int { return 42 }\n"), 0644)
			return &a4c.AgentMessage{Content: "Implemented Lib()"}, nil
		}

		if agentID == "reviewer" {
			promptReceivedByReviewer = prompt
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Approved: true,
					Comments: "Changes are minimal and pass tests.",
				})
			}
			return &a4c.AgentMessage{Content: "Reviewed and approved"}, nil
		}

		return &a4c.AgentMessage{Content: "ok"}, nil
	}
	h.coordinator.SetDefaultProvider(fake)

	// 1. Submit task with StrategyReviewed
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Implement Lib() in lib.go with review",
		Strategy:  a4c.StrategyReviewed,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Verify tree has implementer + reviewer (zero coordinators)
	tree := h.store.GetTaskTree(ref.TaskID)
	if len(tree) != 2 {
		t.Fatalf("expected 2 activities (implementer + reviewer), got %d", len(tree))
	}

	// 2. Execute task
	results, err := h.service.ExecuteTask(ctx, ref.TaskID, 10)
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Verify session sequence: exactly [coder, reviewer] (zero coordinators)
	if len(sessionsCreated) != 2 {
		t.Fatalf("expected 2 sessions created, got %d: %v", len(sessionsCreated), sessionsCreated)
	}
	if sessionsCreated[0] != "coder" || sessionsCreated[1] != "reviewer" {
		t.Fatalf("expected [coder, reviewer], got %v", sessionsCreated)
	}

	// Verify reviewer received diff and test evidence
	if !strings.Contains(promptReceivedByReviewer, "lib.go") && !strings.Contains(promptReceivedByReviewer, "Review") {
		t.Fatalf("reviewer prompt missing expected review context: %s", promptReceivedByReviewer)
	}

	// 3. Verify task is approved and awaiting human acceptance
	taskDetails, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskDetails.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected state %s, got %s", a4c.TaskStateAwaitingHumanReview, taskDetails.State)
	}
	if taskDetails.ReviewVerdict != "approved" {
		t.Fatalf("expected review verdict 'approved', got %q", taskDetails.ReviewVerdict)
	}
}

// TestCrossHostContinuation_MidTaskWorktreeStatePreserved verifies transferring unfinished
// work between hosts using compact checkpoints without replaying transcripts.
func TestCrossHostContinuation_MidTaskWorktreeStatePreserved(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	// 1. Submit task on Copilot runtime
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective:  "Refactor durable storage engine",
		Strategy:   a4c.StrategyDirect,
		Specialist: "storage-specialist",
		Runtime:    "copilot",
		Model:      "gpt-4o",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Get worktree path
	wtPath, err := h.service.GetWorktreePath(ref.TaskID)
	if err != nil {
		t.Fatalf("GetWorktreePath failed: %v", err)
	}

	// Simulate work in progress in the worktree
	partialFile := filepath.Join(wtPath, "storage_partial.go")
	if err := os.WriteFile(partialFile, []byte("package main\n// partial implementation\n"), 0644); err != nil {
		t.Fatalf("failed to write partial file: %v", err)
	}

	// 2. Capture authoritative checkpoint mid-task
	chk, err := h.service.CaptureCheckpoint(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("CaptureCheckpoint failed: %v", err)
	}
	if chk.TaskID != ref.TaskID {
		t.Fatalf("expected task ID %s, got %s", ref.TaskID, chk.TaskID)
	}
	if chk.WorktreeBranch == "" {
		t.Fatalf("expected non-empty worktree branch")
	}
	if !strings.Contains(chk.GitStatusSummary, "storage_partial.go") {
		t.Fatalf("expected git status to reflect storage_partial.go, got: %s", chk.GitStatusSummary)
	}

	// 3. Resume task on OpenCode host
	resumedChk, err := h.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:       ref.TaskID,
		Runtime:      "opencode",
		Model:        "opencode-go/mimo-v2.5",
		Specialist:   "storage-engineer",
		Instructions: "Continue storage engine implementation without re-analyzing past files.",
	})
	if err != nil {
		t.Fatalf("ResumeTaskOnHost failed: %v", err)
	}
	if resumedChk.TargetRuntime != "opencode" {
		t.Fatalf("expected target runtime 'opencode', got %q", resumedChk.TargetRuntime)
	}
	if resumedChk.SpecialistID != "storage-engineer" {
		t.Fatalf("expected specialist 'storage-engineer', got %q", resumedChk.SpecialistID)
	}

	// 4. Build continuation prompt and verify compact representation (no transcript replay)
	continuationPrompt := a4c.BuildContinuationPrompt(*resumedChk)
	if !strings.Contains(continuationPrompt, "Task Continuation Checkpoint") {
		t.Fatalf("expected continuation header in prompt: %s", continuationPrompt)
	}
	if !strings.Contains(continuationPrompt, "Refactor durable storage engine") {
		t.Fatalf("expected objective in prompt: %s", continuationPrompt)
	}
	if !strings.Contains(continuationPrompt, "storage_partial.go") {
		t.Fatalf("expected uncommitted files in prompt: %s", continuationPrompt)
	}
	if strings.Contains(continuationPrompt, "Chat History:") || strings.Contains(continuationPrompt, "Transcript:") {
		t.Fatalf("continuation prompt should NOT replay transcripts: %s", continuationPrompt)
	}

	// 5. Complete task under new host
	fake := a4c.NewFakeProvider()
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		// Verify new host receives continuation context
		if !strings.Contains(prompt, "Continuation") && !strings.Contains(prompt, "continuation") {
			t.Fatalf("resumed session prompt missing continuation header: %s", prompt)
		}
		// Finalize worktree file
		_ = os.WriteFile(partialFile, []byte("package main\nfunc StorageComplete() bool { return true }\n"), 0644)
		return &a4c.AgentMessage{Content: "Completed storage engine continuation"}, nil
	}
	h.coordinator.RegisterProvider("opencode", fake)

	results, err := h.service.ExecuteTask(ctx, ref.TaskID, 5)
	if err != nil {
		t.Fatalf("ExecuteTask failed on resumed task: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result from resumed execution, got %d", len(results))
	}
	if results[0].Status != a4c.StatusCompleted {
		t.Fatalf("expected activity status completed, got %s", results[0].Status)
	}
}

// TestTaskEscalation_DirectToReviewed verifies that an ordinary direct task can be escalated
// to reviewed without losing worktree state or already completed work.
func TestTaskEscalation_DirectToReviewed(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	var sessionsCreated []string
	fake := a4c.NewFakeProvider()
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		sessionsCreated = append(sessionsCreated, agentID)
		if agentID == "coder" {
			wtPath := session.Config().WorkspaceDir
			if wtPath == "" {
				wtPath = h.repoDir
			}
			_ = os.WriteFile(filepath.Join(wtPath, "feature_escalated.go"), []byte("package main\nfunc Escalated() bool { return true }\n"), 0644)
			return &a4c.AgentMessage{Content: "Implemented feature"}, nil
		}
		if agentID == "reviewer" {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Approved: true,
					Comments: "Escalated review passed successfully.",
				})
			}
			return &a4c.AgentMessage{Content: "Review complete"}, nil
		}
		return &a4c.AgentMessage{Content: "ok"}, nil
	}
	h.coordinator.SetDefaultProvider(fake)

	// 1. Submit Direct task
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Implement feature with optional escalation",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// 2. Execute specialist activity
	res1, err := h.coordinator.Step(ctx)
	if err != nil || res1 == nil {
		t.Fatalf("specialist step failed: %v", err)
	}
	if res1.Status != a4c.StatusCompleted {
		t.Fatalf("expected completed status, got %s", res1.Status)
	}

	// 3. Escalate to reviewed
	escalatedRef, err := h.service.EscalateTask(ctx, a4c.EscalateTaskRequest{
		TaskID:      ref.TaskID,
		NewStrategy: a4c.StrategyReviewed,
	})
	if err != nil {
		t.Fatalf("EscalateTask failed: %v", err)
	}
	if escalatedRef.TaskID != ref.TaskID {
		t.Fatalf("expected task ID %s, got %s", ref.TaskID, escalatedRef.TaskID)
	}

	// Verify reviewer activity was enqueued
	tree := h.store.GetTaskTree(ref.TaskID)
	if len(tree) != 2 {
		t.Fatalf("expected 2 activities after escalation, got %d", len(tree))
	}

	// 4. Execute reviewer activity
	res2, err := h.coordinator.Step(ctx)
	if err != nil || res2 == nil {
		t.Fatalf("reviewer step failed: %v", err)
	}
	if res2.Status != a4c.StatusCompleted {
		t.Fatalf("expected completed status, got %s", res2.Status)
	}

	// Verify session sequence: exactly [coder, reviewer]
	if len(sessionsCreated) != 2 || sessionsCreated[0] != "coder" || sessionsCreated[1] != "reviewer" {
		t.Fatalf("unexpected sessions created: %v", sessionsCreated)
	}

	// 5. Verify task details
	details, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if details.ReviewVerdict != "approved" {
		t.Fatalf("expected approved verdict, got %q", details.ReviewVerdict)
	}
}
