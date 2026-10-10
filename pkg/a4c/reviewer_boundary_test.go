package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestReviewerBoundaryEnforcement verifies that A4C enforces non-mutating reviewer boundaries
// by detecting any workspace changes produced by the reviewer and rejecting the review verdict.
func TestReviewerBoundaryEnforcement(t *testing.T) {
	repoDir := t.TempDir()

	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Reviewer Boundary Tester")
	runGitCmd(t, repoDir, "config", "user.email", "boundary@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Boundary Test Repo\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "init")

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
		t.Fatalf("create lifecycle: %v", err)
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

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr)
	ctx := context.Background()

	// 1. Begin task
	beginRes, err := svc.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective:   "Implement security token parser",
		Coordinator: "alice",
	})
	if err != nil {
		t.Fatalf("BeginTask failed: %v", err)
	}
	taskID := beginRes.TaskID
	worktreePath := beginRes.WorktreePath

	// 2. Bob implements valid code change
	tokenFile := filepath.Join(worktreePath, "token.go")
	_ = os.WriteFile(tokenFile, []byte("package main\n\ntype Token string\n"), 0644)
	runGitCmd(t, worktreePath, "add", "token.go")
	runGitCmd(t, worktreePath, "commit", "-m", "feat: implement Token")

	// 3. Bob completes activity
	assignBob, _ := svc.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Write Token",
	})
	_, _ = svc.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:             taskID,
		ActivityID:         assignBob.ActivityID,
		Actor:              "bob",
		Summary:            "Created token.go",
		FilesModified:      []string{"token.go"},
		ValidationEvidence: "PASS",
		Success:            true,
	})

	// 4. Case A: Charlie violates the non-mutating reviewer invariant by editing files
	violationFile := filepath.Join(worktreePath, "unauthorized_change.go")
	_ = os.WriteFile(violationFile, []byte("package main\n// Charlie unauthorized mutation\n"), 0644)

	// Attempt to record review: must fail closed!
	_, err = svc.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:   taskID,
		Reviewer: "charlie",
		Verdict:  "approved",
		Comments: "Sneaking in an unauthorized file edit",
	})
	if err == nil {
		t.Fatalf("expected RecordReview to fail due to uncommitted reviewer modifications, but it succeeded")
	}
	if !strings.Contains(err.Error(), "non-mutating reviewer invariant violated") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// 5. Case B: Remove the violation, review succeeds cleanly
	_ = os.Remove(violationFile)
	revRes, err := svc.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:   taskID,
		Reviewer: "charlie",
		Verdict:  "approved",
		Comments: "Clean read-only review. Changes verified.",
	})
	if err != nil {
		t.Fatalf("expected clean review to succeed, got %v", err)
	}
	if revRes.LifecycleState != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected awaiting_human_review, got %s", revRes.LifecycleState)
	}
}
